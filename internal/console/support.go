package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/orders"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/outbox"
)

// Limits from the contract (RefundRequest, CreditRequest, ReplacementRequest, TicketReply).
const (
	maxMoneyPaise   = 10_000_000
	maxTicketText   = 2000
	maxReplacements = 20
	maxQty          = 20
	// defaultRefundApprovalPaise is used only when no settings row holds refund_approval_paise
	// (the console's value wins). It is the owner's Rs 200.
	defaultRefundApprovalPaise = 20000
	subjectRunes               = 80
)

var ticketKinds = map[string]bool{"missing_item": true, "cold_food": true, "wrong_item": true, "payment": true, "other": true}

// MountSupport adds the ticket calls (v0.10.0 part B), requestRefund, issueReplacement and
// issueCredit. Every write needs an Idempotency-Key and is audited.
func (s *Service) MountSupport(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		tickets := s.Require("tickets.manage", false)
		g.With(tickets).Get("/admin/tickets", s.listTickets)
		g.With(tickets).Get("/admin/tickets/{ticket_id}", s.getTicket)
		g.With(tickets, idem).Post("/admin/tickets/{ticket_id}/messages", s.replyTicket)
		g.With(tickets, idem).Post("/admin/tickets/{ticket_id}/close", s.closeTicket)
		g.With(s.Require("refunds.request", false), idem).Post("/admin/orders/{order_id}/refund", s.requestRefund)
		g.With(s.Require("remedies.issue", false), idem).Post("/admin/orders/{order_id}/replacement", s.issueReplacement)
		g.With(s.Require("remedies.issue", false), idem).Post("/admin/orders/{order_id}/credit", s.issueCredit)
	})
}

// changeWith runs a write whose status depends on what it did (a refund is 200 or 202), in one
// transaction with the session check, and saves its reply for the Idempotency-Key.
func (s *Service) changeWith(w http.ResponseWriter, r *http.Request, act func(ctx context.Context, tx pgx.Tx, u User) (int, map[string]any, error)) {
	actor, _ := identity.ActorFrom(r.Context())
	var reply map[string]any
	var status int
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		if err := identity.CheckSession(r.Context(), tx, actor); err != nil {
			return err
		}
		var err error
		if status, reply, err = act(r.Context(), tx, UserFrom(r.Context())); err != nil {
			return err
		}
		reply["server_time"] = serverTime()
		return idempotency.Save(r.Context(), tx, status, reply)
	})
	s.write(w, r, status, reply, err)
}

func badBody(w http.ResponseWriter, message string) {
	httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", message, false)
}

// decodeStrict reads one JSON object, and refuses anything after it.
func decodeStrict(r *http.Request, into any) bool {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(into); err != nil {
		return false
	}
	return !dec.More()
}

// ---- Tickets ----

// reader is what the ticket and remedy views need: a pool or a transaction.
type reader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// TicketView is one ticket (contract: Ticket). Messages are filled only when asked: the list
// leaves them out.
func TicketView(ctx context.Context, q reader, id string, withMessages bool) (map[string]any, error) {
	var order *string
	var kind, status, subject string
	var opened time.Time
	var closed *time.Time
	var closeReason *string
	err := q.QueryRow(ctx, `SELECT t.order_id::text, t.type, t.status, t.created_at, t.closed_at, t.close_reason,
			coalesce(nullif(left(btrim(regexp_replace((SELECT m.text FROM ticket_messages m WHERE m.ticket_id = t.id AND m.sender = 'customer'
				ORDER BY m.created_at, m.id LIMIT 1), '\s+', ' ', 'g')), $2), ''), 'Support request')
		FROM support_tickets t WHERE t.id = $1`, id, subjectRunes).Scan(&order, &kind, &status, &opened, &closed, &closeReason, &subject)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	messages := []map[string]any{}
	if withMessages {
		rows, err := q.Query(ctx, `SELECT m.id::text, m.sender, cu.name, m.text, m.created_at FROM ticket_messages m
			LEFT JOIN console_users cu ON cu.id = m.author_id WHERE m.ticket_id = $1 ORDER BY m.created_at, m.id`, id)
		if err != nil {
			return nil, err
		}
		var mid, sender, text string
		var author *string
		var at time.Time
		if _, err := pgx.ForEachRow(rows, []any{&mid, &sender, &author, &text, &at}, func() error {
			var name any
			if author != nil && sender == "support" {
				name = *author
			}
			messages = append(messages, map[string]any{"id": mid, "author_type": sender, "author_name": name, "text": text, "created_at": at.UTC().Format(time.RFC3339Nano)})
			return nil
		}); err != nil {
			return nil, err
		}
	}
	var orderValue, reason any
	if order != nil {
		orderValue = *order
	}
	if closeReason != nil {
		reason = *closeReason
	}
	return map[string]any{"id": id, "order_id": orderValue, "type": kind, "status": status, "subject": subject, "opened_at": opened.UTC().Format(time.RFC3339Nano),
		"closed_at": stamp(closed), "close_reason": reason, "messages": messages}, nil
}

func (s *Service) listTickets(w http.ResponseWriter, r *http.Request) {
	p, ok := ReadPage(r)
	status, kind := r.URL.Query().Get("status"), r.URL.Query().Get("type")
	if !ok || (status != "" && status != "open" && status != "resolved") || (kind != "" && !ticketKinds[kind]) {
		badBody(w, "Bad status, type, cursor or limit.")
		return
	}
	if !p.HasAfter {
		p.At, p.ID = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), "ffffffff-ffff-ffff-ffff-ffffffffffff"
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT id::text, created_at FROM support_tickets
		WHERE ($1 = '' OR status = $1) AND ($2 = '' OR type = $2) AND (created_at, id) < ($3, $4::uuid)
		ORDER BY created_at DESC, id DESC LIMIT $5`, status, kind, p.At, p.ID, p.Limit+1)
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	type row struct {
		id string
		at time.Time
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.id, &x.at)
	})
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	var next any
	if len(list) > p.Limit {
		list = list[:p.Limit]
		next = Cursor(list[len(list)-1].at, list[len(list)-1].id)
	}
	out := make([]map[string]any, 0, len(list))
	for _, x := range list {
		v, err := TicketView(r.Context(), s.Pool, x.id, false)
		if err != nil {
			s.unavailable(w, r, err)
			return
		}
		out = append(out, v)
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"tickets": out, "next_cursor": next, "server_time": serverTime()})
}

func (s *Service) getTicket(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(chi.URLParam(r, "ticket_id"))
	if !idPattern.MatchString(id) {
		httpserver.WriteError(w, http.StatusNotFound, "not_found", "Not found.", false)
		return
	}
	v, err := TicketView(r.Context(), s.Pool, id, true)
	if errors.Is(err, errNotFound) {
		httpserver.WriteError(w, http.StatusNotFound, "not_found", "Not found.", false)
		return
	}
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"ticket": v, "server_time": serverTime()})
}

// lockTicket locks an open ticket. A resolved one is ticket_closed (409); an unknown id is 404.
func lockTicket(ctx context.Context, tx pgx.Tx, id string) (customer string, err error) {
	var status string
	err = tx.QueryRow(ctx, "SELECT status, customer_id::text FROM support_tickets WHERE id = $1 FOR UPDATE", id).Scan(&status, &customer)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errNotFound
	}
	if err != nil {
		return "", err
	}
	if status != "open" {
		return "", &refusal{"ticket_closed", "This ticket is already closed."}
	}
	return customer, nil
}

// replyTicket adds a support message the customer reads in the app (it is a ticket_messages row,
// as getTicket in the customer API reads) and queues a push. The push names the ticket, never the
// text, and never a phone or address.
func (s *Service) replyTicket(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if !decodeStrict(r, &body) || strings.TrimSpace(body.Text) == "" || utf8.RuneCountInString(body.Text) > maxTicketText || strings.ContainsRune(body.Text, 0) {
		badBody(w, "Send text of 1 to 2000 characters.")
		return
	}
	id := strings.ToLower(chi.URLParam(r, "ticket_id"))
	s.changeWith(w, r, func(ctx context.Context, tx pgx.Tx, u User) (int, map[string]any, error) {
		if !idPattern.MatchString(id) {
			return 0, nil, errNotFound
		}
		customer, err := lockTicket(ctx, tx, id)
		if err != nil {
			return 0, nil, err
		}
		var message string
		if err := tx.QueryRow(ctx, "INSERT INTO ticket_messages (ticket_id, sender, text, author_id) VALUES ($1, 'support', $2, $3) RETURNING id::text",
			id, body.Text, u.ID).Scan(&message); err != nil {
			return 0, nil, err
		}
		if _, err := tx.Exec(ctx, "UPDATE support_tickets SET updated_at = now() WHERE id = $1", id); err != nil {
			return 0, nil, err
		}
		if err := outbox.Add(ctx, tx, "push.customer", map[string]any{"customer_id": customer, "ticket_id": id, "stage": "ticket_reply"}); err != nil {
			return 0, nil, err
		}
		// The audit row keeps the ids, not the words: the reply is in ticket_messages.
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: u.ID, Action: "ticket.replied", EntityType: "ticket", EntityID: id,
			After: map[string]any{"message_id": message}}); err != nil {
			return 0, nil, err
		}
		v, err := TicketView(ctx, tx, id, true)
		return http.StatusCreated, map[string]any{"ticket": v}, err
	})
}

// closeTicket sets the ticket to resolved and keeps who, when and why.
func (s *Service) closeTicket(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	if !decodeStrict(r, &body) || !validReason(body.Reason) {
		badBody(w, "Send a reason of 3 to 500 characters.")
		return
	}
	id := strings.ToLower(chi.URLParam(r, "ticket_id"))
	s.changeWith(w, r, func(ctx context.Context, tx pgx.Tx, u User) (int, map[string]any, error) {
		if !idPattern.MatchString(id) {
			return 0, nil, errNotFound
		}
		if _, err := lockTicket(ctx, tx, id); err != nil {
			return 0, nil, err
		}
		if _, err := tx.Exec(ctx, "UPDATE support_tickets SET status = 'resolved', closed_at = now(), closed_by = $2, close_reason = $3, updated_at = now() WHERE id = $1",
			id, u.ID, strings.TrimSpace(body.Reason)); err != nil {
			return 0, nil, err
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: u.ID, Action: "ticket.closed", EntityType: "ticket", EntityID: id,
			Before: map[string]any{"status": "open"}, After: map[string]any{"status": "resolved"}, Reason: body.Reason}); err != nil {
			return 0, nil, err
		}
		v, err := TicketView(ctx, tx, id, true)
		return http.StatusOK, map[string]any{"ticket": v}, err
	})
}

// ---- Money owed on an order ----

// paidOrder is an order with its paid payment, both locked.
type paidOrder struct {
	id, zone, paymentID, provider string
	orderNo                       int64
	paid                          int
}

// lockPaid locks an order and its paid payment, in that order, which is also the order the
// automatic refunds take them in. Two requests on one order therefore run one after the other, and
// the amounts read afterwards are final for as long as the transaction lasts. An unknown order is
// errNotFound; one with no paid payment is not_paid.
func lockPaid(ctx context.Context, tx pgx.Tx, id string) (paidOrder, error) {
	o := paidOrder{id: id}
	if !idPattern.MatchString(id) {
		return o, errNotFound
	}
	err := tx.QueryRow(ctx, "SELECT order_no, zone_id::text FROM orders WHERE id = $1 FOR UPDATE", id).Scan(&o.orderNo, &o.zone)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, errNotFound
	}
	if err != nil {
		return o, err
	}
	err = tx.QueryRow(ctx, "SELECT id::text, provider, amount_paise FROM payments WHERE order_id = $1 AND status = 'paid' FOR UPDATE", id).Scan(&o.paymentID, &o.provider, &o.paid)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, &refusal{"not_paid", "This order was never paid."}
	}
	return o, err
}

// noteOrder writes a same-state event on the order's own history. The words are fixed, never what
// staff typed, as the timeline shows order events.
func noteOrder(ctx context.Context, tx pgx.Tx, orderID, actorID, text string) error {
	_, err := tx.Exec(ctx, `INSERT INTO order_events (order_id, from_state, to_state, actor_type, actor_id, reason)
		SELECT id, state, state, $2, $3, $4 FROM orders WHERE id = $1`, orderID, identity.TypeConsole, actorID, text)
	return err
}

// RefundView is one refund (contract: Refund). A console refund shows the reason staff gave; an
// automatic one shows its code.
func RefundView(ctx context.Context, q reader, id string) (map[string]any, error) {
	var order, status, reason string
	var amount int
	var by, byName *string
	var created time.Time
	err := q.QueryRow(ctx, `SELECT r.order_id::text, r.amount_paise, r.status, coalesce(r.note, r.reason_code), r.requested_by::text, cu.name, r.created_at
		FROM refunds r LEFT JOIN console_users cu ON cu.id = r.requested_by WHERE r.id = $1`, id).Scan(&order, &amount, &status, &reason, &by, &byName, &created)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "order_id": order, "amount": money(amount), "status": status, "reason": reason, "requested_by": actorOrNil(by, byName),
		"created_at": created.UTC().Format(time.RFC3339Nano)}, nil
}

// actorOrNil is the contract's Actor, or null when nobody asked (the system did).
func actorOrNil(id, name *string) any {
	if id == nil {
		return nil
	}
	n := ""
	if name != nil {
		n = *name
	}
	return map[string]any{"id": *id, "name": n}
}

// refundTotals reads what has gone back on a payment (not counting failed refunds), and how much of
// that the console made. Callers hold the payment locked.
func refundTotals(ctx context.Context, tx pgx.Tx, paymentID string) (all, console int, err error) {
	err = tx.QueryRow(ctx, `SELECT coalesce(sum(amount_paise), 0)::int, coalesce(sum(amount_paise) FILTER (WHERE requested_by IS NOT NULL), 0)::int
		FROM refunds WHERE payment_id = $1 AND status <> 'failed'`, paymentID).Scan(&all, &console)
	return
}

// refundApprovalLimit is the setting refund_approval_paise as the order's zone runs with it.
func refundApprovalLimit(ctx context.Context, tx pgx.Tx, zone string) (int, error) {
	eff, err := geo.LoadSettings(ctx, tx, zone)
	if err != nil {
		return 0, err
	}
	if v, ok := eff.Get("refund_approval_paise"); ok {
		return v, nil
	}
	return defaultRefundApprovalPaise, nil
}

// requestRefund refunds part or all of what was paid. Up to the setting, cumulative per order, it
// is made at once; above it, it waits for a second admin and is made by ApplyRefund.
func (s *Service) requestRefund(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Amount *int64 `json:"amount_paise"`
		Reason string `json:"reason"`
	}
	if !decodeStrict(r, &body) || body.Amount == nil || *body.Amount < 1 || *body.Amount > maxMoneyPaise || !validReason(body.Reason) {
		badBody(w, "Send amount_paise (1 to 10000000) and a reason of 3 to 500 characters.")
		return
	}
	amount, id := int(*body.Amount), strings.ToLower(chi.URLParam(r, "order_id"))
	s.changeWith(w, r, func(ctx context.Context, tx pgx.Tx, u User) (int, map[string]any, error) {
		// Lapsing touches approvals of every kind, so it runs before the order is locked, not under
		// it. The sum below also ignores a lapsed one that a racing request has not yet marked.
		now := s.clock()
		if err := lapse(ctx, tx, now); err != nil {
			return 0, nil, err
		}
		o, err := lockPaid(ctx, tx, id)
		if err != nil {
			return 0, nil, err
		}
		made, byConsole, err := refundTotals(ctx, tx, o.paymentID)
		if err != nil {
			return 0, nil, err
		}
		// A refund waiting for approval is money spoken for.
		var waiting int
		if err := tx.QueryRow(ctx, `SELECT coalesce(sum((change->'amount'->>'amount_minor')::int), 0)::int FROM approvals
			WHERE kind = 'refund' AND target = $1 AND status = 'pending' AND expires_at > $2`, id, now).Scan(&waiting); err != nil {
			return 0, nil, err
		}
		if amount > o.paid-made-waiting {
			return 0, nil, &refusal{"refund_too_large", "That is more than is left to refund on this order."}
		}
		limit, err := refundApprovalLimit(ctx, tx, o.zone)
		if err != nil {
			return 0, nil, err
		}
		if byConsole+waiting+amount > limit {
			approval, err := RequestChange(ctx, tx, u, "refund", id, "Order "+orders.OrderNumber(o.orderNo)+": refund "+strconv.Itoa(amount)+" paise",
				map[string]any{"kind": "refund", "order_id": id, "amount": money(amount)}, body.Reason, now)
			if err != nil {
				return 0, nil, err
			}
			v, err := ApprovalView(ctx, tx, approval)
			return http.StatusAccepted, map[string]any{"approval": v}, err
		}
		refund, err := orders.RecordSupportRefund(ctx, tx, id, o.paymentID, o.provider, amount, u.ID, strings.TrimSpace(body.Reason))
		if err != nil {
			return 0, nil, err
		}
		if err := noteOrder(ctx, tx, id, u.ID, "support refund"); err != nil {
			return 0, nil, err
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: u.ID, Action: "refund.issued", EntityType: "refund", EntityID: refund,
			After: map[string]any{"order_id": id, "amount_paise": amount}, Reason: body.Reason}); err != nil {
			return 0, nil, err
		}
		v, err := RefundView(ctx, tx, refund)
		return http.StatusOK, map[string]any{"refund": v}, err
	})
}

// ApplyRefund is the refund kind's Applier. The second admin's approval makes the refund, if it
// still fits: the order may have been refunded by other means since it was asked for. It is made
// in the name of the person who asked, with the reason they gave.
func ApplyRefund(ctx context.Context, tx pgx.Tx, change json.RawMessage, by string, _ time.Time) (string, error) {
	var c struct {
		OrderID string `json:"order_id"`
		Amount  struct {
			Minor int `json:"amount_minor"`
		} `json:"amount"`
	}
	if err := json.Unmarshal(change, &c); err != nil || c.Amount.Minor < 1 {
		return "not_applied", nil
	}
	o, err := lockPaid(ctx, tx, c.OrderID)
	var no *refusal
	switch {
	case errors.Is(err, errNotFound):
		return "not_applied", nil
	case errors.As(err, &no):
		return no.code, nil
	case err != nil:
		return "", err
	}
	made, _, err := refundTotals(ctx, tx, o.paymentID)
	if err != nil {
		return "", err
	}
	// The approval being applied is the order's only waiting one, so nothing else is spoken for.
	if c.Amount.Minor > o.paid-made {
		return "refund_too_large", nil
	}
	var requester, why string
	if err := tx.QueryRow(ctx, "SELECT requested_by::text, request_reason FROM approvals WHERE kind = 'refund' AND target = $1 AND status = 'pending'", c.OrderID).Scan(&requester, &why); err != nil {
		return "", err
	}
	refund, err := orders.RecordSupportRefund(ctx, tx, c.OrderID, o.paymentID, o.provider, c.Amount.Minor, requester, why)
	if err != nil {
		return "", err
	}
	if err := noteOrder(ctx, tx, c.OrderID, by, "support refund approved"); err != nil {
		return "", err
	}
	return "", audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: by, Action: "refund.issued", EntityType: "refund", EntityID: refund,
		After: map[string]any{"order_id": c.OrderID, "amount_paise": c.Amount.Minor, "requested_by": requester}, Reason: why})
}

// RemedyView is one remedy (contract: Remedy).
func RemedyView(ctx context.Context, q reader, id string) (map[string]any, error) {
	var kind, status, reason string
	var amount *int
	var items []byte
	var by, byName *string
	var created time.Time
	err := q.QueryRow(ctx, `SELECT m.type, m.amount_paise, m.items, m.status, m.reason, m.created_by::text, cu.name, m.created_at
		FROM remedies m LEFT JOIN console_users cu ON cu.id = m.created_by WHERE m.id = $1`, id).Scan(&kind, &amount, &items, &status, &reason, &by, &byName, &created)
	if err != nil {
		return nil, err
	}
	return remedyMap(id, kind, amount, items, status, reason, by, byName, created), nil
}

// remedyMap builds the contract's Remedy from a row; the timeline uses it too.
func remedyMap(id, kind string, amount *int, items []byte, status, reason string, by, byName *string, created time.Time) map[string]any {
	var amt, lines any
	if amount != nil {
		amt = money(*amount)
	}
	if items != nil {
		_ = json.Unmarshal(items, &lines)
	}
	return map[string]any{"id": id, "kind": kind, "amount": amt, "items": lines, "status": status, "reason": reason, "created_by": actorOrNil(by, byName),
		"created_at": created.UTC().Format(time.RFC3339Nano)}
}

// owe records one remedy a person decided on. An order has one of each type, so a second is
// remedy_exists. The order's seq moves, so the customer's order view refreshes.
func owe(ctx context.Context, tx pgx.Tx, u User, o paidOrder, kind string, amount *int, items any, reason string) (map[string]any, error) {
	var id string
	err := tx.QueryRow(ctx, `INSERT INTO remedies (order_id, type, reason, amount_paise, items, created_by) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (order_id, type) DO NOTHING RETURNING id::text`, o.id, kind, strings.TrimSpace(reason), amount, items, u.ID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, &refusal{"remedy_exists", "This order already has a " + kind + "."}
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, "UPDATE orders SET seq = seq + 1, updated_at = now() WHERE id = $1", o.id); err != nil {
		return nil, err
	}
	if err := noteOrder(ctx, tx, o.id, u.ID, "support "+kind); err != nil {
		return nil, err
	}
	after := map[string]any{"remedy_id": id, "type": kind}
	if amount != nil {
		after["amount_paise"] = *amount
	}
	if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: u.ID, Action: "remedy.issued", EntityType: "order", EntityID: o.id,
		After: after, Reason: reason}); err != nil {
		return nil, err
	}
	return RemedyView(ctx, tx, id)
}

// issueCredit owes the customer an amount.
func (s *Service) issueCredit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Amount *int64 `json:"amount_paise"`
		Reason string `json:"reason"`
	}
	if !decodeStrict(r, &body) || body.Amount == nil || *body.Amount < 1 || *body.Amount > maxMoneyPaise || !validReason(body.Reason) {
		badBody(w, "Send amount_paise (1 to 10000000) and a reason of 3 to 500 characters.")
		return
	}
	amount, id := int(*body.Amount), strings.ToLower(chi.URLParam(r, "order_id"))
	s.changeWith(w, r, func(ctx context.Context, tx pgx.Tx, u User) (int, map[string]any, error) {
		o, err := lockPaid(ctx, tx, id)
		if err != nil {
			return 0, nil, err
		}
		v, err := owe(ctx, tx, u, o, "credit", &amount, nil, body.Reason)
		return http.StatusCreated, map[string]any{"remedy": v}, err
	})
}

// issueReplacement owes the customer the items listed, which must be on the order.
func (s *Service) issueReplacement(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Items []struct {
			ItemID string `json:"item_id"`
			Qty    *int   `json:"qty"`
		} `json:"items"`
		Reason string `json:"reason"`
	}
	if !decodeStrict(r, &body) || len(body.Items) < 1 || len(body.Items) > maxReplacements || !validReason(body.Reason) {
		badBody(w, "Send 1 to 20 items and a reason of 3 to 500 characters.")
		return
	}
	lines := make([]map[string]any, 0, len(body.Items))
	seen := map[string]bool{}
	for _, it := range body.Items {
		item := strings.ToLower(it.ItemID)
		if !idPattern.MatchString(item) || it.Qty == nil || *it.Qty < 1 || *it.Qty > maxQty || seen[item] {
			badBody(w, "Each item needs its own item_id and a qty of 1 to 20.")
			return
		}
		seen[item] = true
		lines = append(lines, map[string]any{"item_id": item, "qty": *it.Qty})
	}
	id := strings.ToLower(chi.URLParam(r, "order_id"))
	s.changeWith(w, r, func(ctx context.Context, tx pgx.Tx, u User) (int, map[string]any, error) {
		o, err := lockPaid(ctx, tx, id)
		if err != nil {
			return 0, nil, err
		}
		for _, l := range lines {
			var ordered int
			err := tx.QueryRow(ctx, "SELECT qty FROM order_items WHERE order_id = $1 AND item_id = $2", id, l["item_id"]).Scan(&ordered)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && l["qty"].(int) > ordered) {
				return 0, nil, &refusal{"item_not_on_order", "A replacement can only be for items on the order, and no more than were ordered."}
			}
			if err != nil {
				return 0, nil, err
			}
		}
		raw, err := json.Marshal(lines)
		if err != nil {
			return 0, nil, err
		}
		v, err := owe(ctx, tx, u, o, "replacement", nil, raw, body.Reason)
		return http.StatusCreated, map[string]any{"remedy": v}, err
	})
}
