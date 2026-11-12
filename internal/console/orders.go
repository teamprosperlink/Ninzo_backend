package console

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/orders"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

// MountOrders adds the order search and the order timeline (v0.10.0 part A). Both only read, and
// reads are not audited, as for the audit log and the review queue.
func (s *Service) MountOrders(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.With(s.Require("orders.read", false)).Get("/admin/orders", s.listOrders)
		g.With(s.Require("orders.read", false)).Get("/admin/orders/{order_id}/timeline", s.orderTimeline)
	})
}

var (
	orderNumberPattern = regexp.MustCompile(`^[A-Za-z][0-9]{4}$`)
	last4Pattern       = regexp.MustCompile(`^[0-9]{4}$`)
	// orderStates is the contract's OrderState enum.
	orderStates = map[string]bool{"CREATED": true, "PAYMENT_PENDING": true, "PAYMENT_SECURED": true, "OFFERED": true, "RIDER_ACCEPTED": true,
		"PACKED": true, "TIMER_RUNNING": true, "ARRIVED": true, "HANDED_OVER": true, "COMPLETED": true, "CANCELLED": true, "PAYMENT_FAILED": true,
		"NO_SCOOTER": true, "TIMED_OUT": true, "FAILED": true}
)

// orderNumberCandidates lists the stored order_no values that print as the given short form. The
// short form repeats every 260000 orders, so more than one can match; the list stops at the
// highest number issued so far. They are looked up by the unique index on order_no.
func orderNumberCandidates(letter byte, digits int, highest int64) []int64 {
	base := int64(letter-'A')*10000 + int64(digits)
	out := []int64{}
	for n := base; n <= highest; n += 26 * 10000 {
		out = append(out, n)
	}
	return out
}

// orderSummarySelect reads what AdminOrderSummary and the head of AdminOrder need. The customer's
// phone is cut to its last four digits in the query, so the full number never reaches Go.
const orderSummarySelect = `SELECT o.id::text, o.order_no, o.state, o.zone_id::text, o.customer_id::text, coalesce(right(cp.phone, 4), ''), o.rider_id::text,
		coalesce(r.name, ''), o.total_paise, o.created_at, o.ended_reason
	FROM orders o
	JOIN customers c ON c.id = o.customer_id
	JOIN people cp ON cp.id = c.person_id
	LEFT JOIN riders r ON r.id = o.rider_id`

type summaryRow struct {
	id, state, zone, customer, last4 string
	rider                            *string
	riderName                        string
	orderNo                          int64
	total                            int
	created                          time.Time
	ended                            *string
}

func (x *summaryRow) targets() []any {
	return []any{&x.id, &x.orderNo, &x.state, &x.zone, &x.customer, &x.last4, &x.rider, &x.riderName, &x.total, &x.created, &x.ended}
}

func (x *summaryRow) view() map[string]any {
	var rider any
	if x.rider != nil {
		rider = map[string]any{"id": *x.rider, "name": x.riderName}
	}
	return map[string]any{"id": x.id, "order_number": orders.OrderNumber(x.orderNo), "state": x.state, "zone_id": x.zone,
		"customer": map[string]any{"id": x.customer, "phone_last4": x.last4}, "rider": rider, "total": money(x.total),
		"created_at": x.created.UTC().Format(time.RFC3339Nano), "ended_reason": x.ended}
}

func badOrderQuery(w http.ResponseWriter, message string) {
	httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", message, false)
}

// listOrders: newest first, paged by (created_at, id). q is one of an order number, an order id or
// the last four phone digits; anything else is refused so a typo is not mistaken for "no orders".
func (s *Service) listOrders(w http.ResponseWriter, r *http.Request) {
	p, ok := ReadPage(r)
	if !ok {
		badOrderQuery(w, "Bad cursor or limit.")
		return
	}
	q := r.URL.Query()
	var from, to *time.Time
	for _, f := range []struct {
		key string
		dst **time.Time
	}{{"from", &from}, {"to", &to}} {
		if v := q.Get(f.key); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				badOrderQuery(w, f.key+" must be a date-time.")
				return
			}
			*f.dst = &t
		}
	}
	state, zone := q.Get("state"), q.Get("zone_id")
	if state != "" && !orderStates[state] {
		badOrderQuery(w, "state is not an order state.")
		return
	}
	if zone != "" && !idPattern.MatchString(zone) {
		badOrderQuery(w, "zone_id must be an id.")
		return
	}
	ctx := r.Context()
	// The three forms of q, each turned into a filter the indexes can serve.
	var numbers []int64 // nil means no filter by number.
	var orderID, phoneLast4 string
	if text := strings.TrimSpace(q.Get("q")); q.Has("q") {
		switch {
		case text == "":
			// A cleared search box sends q with nothing in it: no filter.
		case len(text) > 64:
			badOrderQuery(w, "q must be an order number such as A1001, an order id or four phone digits.")
			return
		case orderNumberPattern.MatchString(text):
			var highest int64
			if err := s.Pool.QueryRow(ctx, "SELECT coalesce(max(order_no), 0) FROM orders").Scan(&highest); err != nil {
				s.unavailable(w, r, err)
				return
			}
			digits, _ := strconv.Atoi(text[1:])
			numbers = orderNumberCandidates(byte(strings.ToUpper(text)[0]), digits, highest)
		case idPattern.MatchString(text):
			orderID = strings.ToLower(text)
		case last4Pattern.MatchString(text):
			phoneLast4 = text
		default:
			badOrderQuery(w, "q must be an order number such as A1001, an order id or four phone digits.")
			return
		}
	}
	var customers, riders []string
	if phoneLast4 != "" {
		// A handful of people share four digits, so look them up first and filter by their ids.
		for _, side := range []struct {
			table string
			dst   *[]string
		}{{"customers", &customers}, {"riders", &riders}} {
			rows, err := s.Pool.Query(ctx, `SELECT t.id::text FROM `+side.table+` t JOIN people p ON p.id = t.person_id WHERE right(p.phone, 4) = $1`, phoneLast4)
			if err != nil {
				s.unavailable(w, r, err)
				return
			}
			ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				s.unavailable(w, r, err)
				return
			}
			*side.dst = ids
		}
	}
	far := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	if !p.HasAfter {
		p.At, p.ID = far, "ffffffff-ffff-ffff-ffff-ffffffffffff"
	}
	if customers == nil {
		customers = []string{}
	}
	if riders == nil {
		riders = []string{}
	}
	rows, err := s.Pool.Query(ctx, orderSummarySelect+`
		WHERE (o.created_at, o.id) < ($1, $2::uuid)
			AND ($3 = '' OR o.state = $3) AND ($4 = '' OR o.zone_id = NULLIF($4, '')::uuid)
			AND ($5::timestamptz IS NULL OR o.created_at >= $5) AND ($6::timestamptz IS NULL OR o.created_at < $6)
			AND ($7::bigint[] IS NULL OR o.order_no = ANY($7))
			AND ($8 = '' OR o.id = NULLIF($8, '')::uuid)
			AND ($9 = '' OR o.customer_id = ANY($10::uuid[]) OR o.rider_id = ANY($11::uuid[]))
		ORDER BY o.created_at DESC, o.id DESC LIMIT $12`,
		p.At, p.ID, state, zone, from, to, numbers, orderID, phoneLast4, customers, riders, p.Limit+1)
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (summaryRow, error) {
		var x summaryRow
		return x, r.Scan(x.targets()...)
	})
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	var next any
	if len(list) > p.Limit {
		list = list[:p.Limit]
		next = Cursor(list[len(list)-1].created, list[len(list)-1].id)
	}
	out := make([]map[string]any, 0, len(list))
	for i := range list {
		out = append(out, list[i].view())
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"orders": out, "next_cursor": next, "server_time": serverTime()})
}

// timelineEvent is one row of the merged history before it is written out.
type timelineEvent struct {
	at        time.Time
	source    string
	kind      string
	actorType string
	actorName *string
	detail    map[string]any
}

// actorNameSQL finds the name of a rider or console user from an actor_type and actor_id column.
// Customers and hub staff stay nameless in the history.
func actorNameSQL(typeCol, idCol string) string {
	return `CASE ` + typeCol + ` WHEN 'rider' THEN (SELECT name FROM riders WHERE id = ` + idCol + `)
		WHEN 'console' THEN (SELECT name FROM console_users WHERE id = ` + idCol + `) END`
}

// orderTimeline shows one order with everything that happened to it. All reads run in one
// repeatable-read snapshot, so the order and its events agree.
func (s *Service) orderTimeline(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(chi.URLParam(r, "order_id"))
	if !idPattern.MatchString(id) {
		httpserver.WriteError(w, http.StatusNotFound, "not_found", "Not found.", false)
		return
	}
	ctx := r.Context()
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	order, events, pings, err := loadTimeline(ctx, tx, id)
	if errors.Is(err, errNotFound) {
		httpserver.WriteError(w, http.StatusNotFound, "not_found", "Not found.", false)
		return
	}
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"order": order, "events": events, "pings": pings, "server_time": serverTime()})
}

func loadTimeline(ctx context.Context, tx pgx.Tx, id string) (map[string]any, []map[string]any, map[string]any, error) {
	var head summaryRow
	if err := tx.QueryRow(ctx, orderSummarySelect+" WHERE o.id = $1", id).Scan(head.targets()...); errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil, errNotFound
	} else if err != nil {
		return nil, nil, nil, err
	}
	order := head.view()
	var gateLabel string
	var gateLat, gateLng float64
	var timerStart, timerStop *time.Time
	var duration, perimeter *int
	var missed bool
	var arrival *string
	if err := tx.QueryRow(ctx, `SELECT h.label, h.lat, h.lng, o.timer_started_at, o.timer_stopped_at, o.timer_length_s, o.arrival_perimeter_m, o.timer_missed, o.arrival_source
		FROM orders o JOIN gate_history h ON h.id = o.gate_history_id WHERE o.id = $1`, id).
		Scan(&gateLabel, &gateLat, &gateLng, &timerStart, &timerStop, &duration, &perimeter, &missed, &arrival); err != nil {
		return nil, nil, nil, err
	}
	order["gate"] = map[string]any{"label": gateLabel, "lat": gateLat, "lng": gateLng}
	order["timer"] = map[string]any{"timer_started_at": stamp(timerStart), "timer_stopped_at": stamp(timerStop), "timer_duration_s": duration,
		"perimeter_m": perimeter, "timer_missed": missed, "arrival_source": arrival}

	items := []map[string]any{}
	rows, err := tx.Query(ctx, "SELECT item_id::text, name, qty, unit_paise FROM order_items WHERE order_id = $1 ORDER BY name, item_id", id)
	if err != nil {
		return nil, nil, nil, err
	}
	var itemID, itemName string
	var qty, unit int
	if _, err := pgx.ForEachRow(rows, []any{&itemID, &itemName, &qty, &unit}, func() error {
		items = append(items, map[string]any{"item_id": itemID, "name": itemName, "qty": qty, "unit_price": money(unit)})
		return nil
	}); err != nil {
		return nil, nil, nil, err
	}
	order["items"] = items

	var evs []timelineEvent
	add := func(at time.Time, source, kind, actorType string, name *string, detail map[string]any) {
		evs = append(evs, timelineEvent{at, source, kind, actorType, name, detail})
	}

	// Remedies and refunds are part of the order, and also events.
	remedies := []map[string]any{}
	rows, err = tx.Query(ctx, "SELECT id::text, type, amount_paise, status, reason, created_at FROM remedies WHERE order_id = $1 ORDER BY created_at, id", id)
	if err != nil {
		return nil, nil, nil, err
	}
	var rid, rtype, rstatus, rreason string
	var ramount *int
	var rcreated time.Time
	if _, err := pgx.ForEachRow(rows, []any{&rid, &rtype, &ramount, &rstatus, &rreason, &rcreated}, func() error {
		var amount any
		if ramount != nil {
			amount = money(*ramount)
		}
		// No remedy records who made it yet, and none has lines: the system makes every one today.
		remedies = append(remedies, map[string]any{"id": rid, "kind": rtype, "amount": amount, "items": nil, "status": rstatus, "reason": rreason,
			"created_by": nil, "created_at": rcreated.UTC().Format(time.RFC3339Nano)})
		add(rcreated, "remedy", "remedy.owed", "system", nil, map[string]any{"remedy_id": rid, "kind": rtype, "amount": amount, "status": rstatus, "reason": rreason})
		return nil
	}); err != nil {
		return nil, nil, nil, err
	}
	order["remedies"] = remedies

	refunds := []map[string]any{}
	refundIDs := []string{}
	rows, err = tx.Query(ctx, "SELECT id::text, amount_paise, status, reason_code, created_at, updated_at FROM refunds WHERE order_id = $1 ORDER BY created_at, id", id)
	if err != nil {
		return nil, nil, nil, err
	}
	var fid, fstatus, freason string
	var famount int
	var fcreated, fupdated time.Time
	if _, err := pgx.ForEachRow(rows, []any{&fid, &famount, &fstatus, &freason, &fcreated, &fupdated}, func() error {
		refundIDs = append(refundIDs, fid)
		refunds = append(refunds, map[string]any{"id": fid, "order_id": id, "amount": money(famount), "status": fstatus, "reason": freason,
			"requested_by": nil, "created_at": fcreated.UTC().Format(time.RFC3339Nano)})
		detail := map[string]any{"refund_id": fid, "amount": money(famount), "reason": freason}
		add(fcreated, "refund", "refund.requested", "system", nil, detail)
		if fstatus != "pending" {
			add(fupdated, "refund", "refund."+fstatus, "system", nil, map[string]any{"refund_id": fid, "amount": money(famount), "reason": freason})
		}
		return nil
	}); err != nil {
		return nil, nil, nil, err
	}
	order["refunds"] = refunds

	// Events of the order itself, oldest first by their own id.
	rows, err = tx.Query(ctx, `SELECT created_at, from_state, to_state, actor_type, reason, `+actorNameSQL("actor_type", "actor_id")+`
		FROM order_events WHERE order_id = $1 ORDER BY id`, id)
	if err != nil {
		return nil, nil, nil, err
	}
	var at time.Time
	var fromState, reason, name *string
	var toState, actorType string
	if _, err := pgx.ForEachRow(rows, []any{&at, &fromState, &toState, &actorType, &reason, &name}, func() error {
		kind := "order." + strings.ToLower(toState)
		switch {
		case fromState == nil:
			kind = "order.created"
		case *fromState == toState:
			// A note on the order with no change of state, such as an arrival review's decision.
			kind = "order.noted"
		}
		detail := map[string]any{"from_state": fromState, "to_state": toState}
		if reason != nil {
			detail["reason"] = *reason
		}
		add(at, "order", kind, actorType, name, detail)
		return nil
	}); err != nil {
		return nil, nil, nil, err
	}

	// Offers: made, then accepted or ended.
	rows, err = tx.Query(ctx, `SELECT o.id::text, o.state, o.ended_reason, o.final_state, o.created_at, o.rider_accepted_at, o.released_at, r.name
		FROM offers o JOIN riders r ON r.id = o.rider_id WHERE o.order_id = $1 ORDER BY o.created_at, o.id`, id)
	if err != nil {
		return nil, nil, nil, err
	}
	var oid, ostate string
	var oended, ofinal, rname *string
	var ocreated time.Time
	var oaccepted, oreleased *time.Time
	if _, err := pgx.ForEachRow(rows, []any{&oid, &ostate, &oended, &ofinal, &ocreated, &oaccepted, &oreleased, &rname}, func() error {
		add(ocreated, "offer", "offer.made", "system", nil, map[string]any{"offer_id": oid})
		if oaccepted != nil {
			add(*oaccepted, "offer", "offer.accepted", "rider", rname, map[string]any{"offer_id": oid})
		}
		if oreleased != nil {
			actor, who := "system", (*string)(nil)
			if ostate == "declined" {
				actor, who = "rider", rname
			}
			kind := "offer." + ostate
			detail := map[string]any{"offer_id": oid, "state": ostate}
			if ostate == "accepted" {
				kind = "offer.released" // The rider accepted, and the offer later ended with the order.
			}
			if oended != nil {
				detail["ended_reason"] = *oended
			}
			if ofinal != nil {
				detail["final_state"] = *ofinal
			}
			add(*oreleased, "offer", kind, actor, who, detail)
		}
		return nil
	}); err != nil {
		return nil, nil, nil, err
	}

	// Payments.
	rows, err = tx.Query(ctx, "SELECT id::text, provider, amount_paise, status, created_at, updated_at FROM payments WHERE order_id = $1 ORDER BY created_at, id", id)
	if err != nil {
		return nil, nil, nil, err
	}
	var pid, provider, pstatus string
	var pamount int
	var pcreated, pupdated time.Time
	if _, err := pgx.ForEachRow(rows, []any{&pid, &provider, &pamount, &pstatus, &pcreated, &pupdated}, func() error {
		add(pcreated, "payment", "payment.created", "customer", nil, map[string]any{"payment_id": pid, "provider": provider, "amount": money(pamount)})
		switch pstatus {
		case "paid":
			add(pupdated, "payment", "payment.captured", "system", nil, map[string]any{"payment_id": pid, "provider": provider, "amount": money(pamount)})
		case "failed", "refunded":
			add(pupdated, "payment", "payment."+pstatus, "system", nil, map[string]any{"payment_id": pid, "provider": provider, "amount": money(pamount)})
		}
		return nil
	}); err != nil {
		return nil, nil, nil, err
	}

	// Problem reports. The rider's note is free text and stays out.
	rows, err = tx.Query(ctx, `SELECT p.id::text, p.type, p.source, p.outcome, p.created_at, r.name FROM problem_reports p LEFT JOIN riders r ON r.id = p.rider_id
		WHERE p.order_id = $1 ORDER BY p.created_at, p.id`, id)
	if err != nil {
		return nil, nil, nil, err
	}
	var prid, ptype, psource, poutcome string
	var pcr time.Time
	if _, err := pgx.ForEachRow(rows, []any{&prid, &ptype, &psource, &poutcome, &pcr, &rname}, func() error {
		who := rname
		if psource != "rider" {
			who = nil
		}
		add(pcr, "problem", "problem.reported", psource, who,
			map[string]any{"problem_id": prid, "type": ptype, "outcome": poutcome})
		return nil
	}); err != nil {
		return nil, nil, nil, err
	}

	// Tickets and the senders of their messages. The customer's words stay out.
	rows, err = tx.Query(ctx, "SELECT id::text, type, status, created_at, updated_at FROM support_tickets WHERE order_id = $1 ORDER BY created_at, id", id)
	if err != nil {
		return nil, nil, nil, err
	}
	type ticket struct {
		id, kind, status string
		created, updated time.Time
	}
	tickets, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ticket, error) {
		var t ticket
		return t, r.Scan(&t.id, &t.kind, &t.status, &t.created, &t.updated)
	})
	if err != nil {
		return nil, nil, nil, err
	}
	for _, t := range tickets {
		add(t.created, "ticket", "ticket.opened", "customer", nil, map[string]any{"ticket_id": t.id, "type": t.kind})
		if t.status == "resolved" {
			add(t.updated, "ticket", "ticket.resolved", "support", nil, map[string]any{"ticket_id": t.id, "type": t.kind})
		}
		rows, err := tx.Query(ctx, "SELECT sender, created_at FROM ticket_messages WHERE ticket_id = $1 ORDER BY created_at, id", t.id)
		if err != nil {
			return nil, nil, nil, err
		}
		var sender string
		var mcreated time.Time
		if _, err := pgx.ForEachRow(rows, []any{&sender, &mcreated}, func() error {
			add(mcreated, "ticket", "ticket.message", sender, nil, map[string]any{"ticket_id": t.id})
			return nil
		}); err != nil {
			return nil, nil, nil, err
		}
	}

	// Arrival reviews: opened by the system, decided by a person or by the time limit.
	rows, err = tx.Query(ctx, `SELECT a.id::text, a.status, a.created_at, a.decided_at, a.due_at, cu.name
		FROM arrival_reviews a LEFT JOIN console_users cu ON cu.id = a.decided_by WHERE a.order_id = $1 ORDER BY a.created_at, a.id`, id)
	if err != nil {
		return nil, nil, nil, err
	}
	var avid, avstatus string
	var avcreated, avdue time.Time
	var avdecided *time.Time
	var avby *string
	if _, err := pgx.ForEachRow(rows, []any{&avid, &avstatus, &avcreated, &avdecided, &avdue, &avby}, func() error {
		add(avcreated, "review", "review.opened", "system", nil, map[string]any{"review_id": avid, "due_at": avdue.UTC().Format(time.RFC3339Nano)})
		if avdecided != nil {
			actor := "console"
			if avby == nil {
				actor = "system"
			}
			add(*avdecided, "review", "review."+avstatus, actor, avby, map[string]any{"review_id": avid, "status": avstatus})
		}
		return nil
	}); err != nil {
		return nil, nil, nil, err
	}

	// Audit entries about the order or its refunds. Only the action and the entity leave: the
	// reason is typed by staff (an arrival review's note, for one) and before and after can hold
	// more than the console should show (#146 review).
	rows, err = tx.Query(ctx, `SELECT created_at, actor_type, action, entity_type, entity_id, `+actorNameSQL("actor_type", "actor_id")+`
		FROM audit_events WHERE (entity_type = 'order' AND entity_id = $1) OR (entity_type = 'refund' AND entity_id = ANY($2::text[]))
		ORDER BY created_at, id`, id, refundIDs)
	if err != nil {
		return nil, nil, nil, err
	}
	var aat time.Time
	var aactor, aaction, aentity, aentityID string
	var aname *string
	if _, err := pgx.ForEachRow(rows, []any{&aat, &aactor, &aaction, &aentity, &aentityID, &aname}, func() error {
		detail := map[string]any{"entity_type": aentity, "entity_id": aentityID}
		add(aat, "audit", aaction, aactor, aname, detail)
		return nil
	}); err != nil {
		return nil, nil, nil, err
	}

	// Oldest first. Equal times fall back to the order of the sources in the contract, then to the
	// order they were read in (the sort is stable).
	sort.SliceStable(evs, func(i, j int) bool {
		if !evs[i].at.Equal(evs[j].at) {
			return evs[i].at.Before(evs[j].at)
		}
		return sourceRank[evs[i].source] < sourceRank[evs[j].source]
	})
	out := make([]map[string]any, 0, len(evs))
	for _, e := range evs {
		var who any
		if e.actorName != nil {
			who = *e.actorName
		}
		out = append(out, map[string]any{"at": e.at.UTC().Format(time.RFC3339Nano), "source": e.source, "kind": e.kind, "actor_type": e.actorType,
			"actor_name": who, "detail": scrub(e.detail)})
	}

	// Pings are summarised, never listed.
	var count int
	var first, last *time.Time
	if err := tx.QueryRow(ctx, "SELECT count(*), min(received_at), max(received_at) FROM location_pings WHERE order_id = $1", id).Scan(&count, &first, &last); err != nil {
		return nil, nil, nil, err
	}
	return order, out, map[string]any{"count": count, "first_at": stamp(first), "last_at": stamp(last)}, nil
}

// sourceRank breaks ties between events with the same time, in the contract's order.
var sourceRank = map[string]int{"order": 0, "offer": 1, "payment": 2, "refund": 3, "remedy": 4, "problem": 5, "ticket": 6, "review": 7, "audit": 8}
