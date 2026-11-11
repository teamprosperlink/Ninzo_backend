// Package orders creates orders, holds their stock on one scooter, takes payment, and shows the
// customer only their stage, never the true state.
package orders

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/availability"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/config"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

const (
	maxLines   = 10
	maxQtyLine = 20
)

var idPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Payments is where money is taken. The test one stands in until Razorpay (decision 0013).
type Payments interface {
	// Name is stored on each payment.
	Name() string
	// KeyID is the public key the app opens checkout with.
	KeyID() string
	// NewOrder makes the provider's order for an amount. It is called before any transaction.
	NewOrder(ctx context.Context, amountPaise int) (string, error)
}

// TestPayments takes no money. A staging-only endpoint says whether a payment worked.
type TestPayments struct{}

// Name implements Payments.
func (TestPayments) Name() string { return "test" }

// KeyID implements Payments. It is not a Razorpay key, so nothing real can be charged with it.
func (TestPayments) KeyID() string { return "rzp_test_ninzo_stand_in" }

// NewOrder implements Payments.
func (TestPayments) NewOrder(context.Context, int) (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "test_order_" + hex.EncodeToString(raw), nil
}

// RequirePayments stops production starting with the test payment, so nobody gets free food.
func RequirePayments(cfg config.Config) error {
	if cfg.Env == "production" {
		return errors.New("payments: Razorpay is not built yet, and production refuses the test payment")
	}
	return nil
}

// refusal is a 409 the contract names.
type refusal struct{ code, message string }

func (r *refusal) Error() string { return "orders: " + r.code }

var (
	errNotFound = errors.New("orders: not found")
	errBlocked  = errors.New("orders: blocked")
	errProvider = errors.New("orders: the payment provider did not answer")
)

// Service serves orders.
type Service struct {
	Pool     *pgxpool.Pool
	Log      *slog.Logger
	Payments Payments
	now      func() time.Time
}

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// MountCustomer adds the customer's order endpoints. testPayments adds the staging-only
// endpoint that marks a test payment paid or failed.
func (s *Service) MountCustomer(r chi.Router, testPayments bool, signedIn ...func(http.Handler) http.Handler) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.With(idem).Post("/orders", s.create)
		g.Get("/orders", s.list)
		g.Get("/orders/{order_id}", s.get)
		g.With(idem).Post("/orders/{order_id}/cancel", s.cancel)
		g.With(idem).Patch("/orders/{order_id}/gate", s.changeGate)
		g.With(idem).Post("/orders/{order_id}/payment/retry", s.retry)
		g.Get("/orders/{order_id}/refund", s.getRefund)
		g.With(idem).Post("/orders/{order_id}/accept-eta", s.acceptETA)
		g.With(idem).Post("/orders/{order_id}/call", s.startCustomerCall)
		if testPayments {
			g.Post("/test/payments/{order_id}", s.testPayment)
		}
	})
}

func serverTime() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func stamp(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func (s *Service) fail(w http.ResponseWriter, r *http.Request, err error) {
	var no *refusal
	switch {
	case errors.As(err, &no):
		httpserver.WriteError(w, http.StatusConflict, no.code, no.message, false)
	case errors.Is(err, identity.ErrSessionInvalid):
		httpserver.WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in again.", false)
	case errors.Is(err, errBlocked):
		identity.WriteBlocked(w)
	case errors.Is(err, errNotFound):
		httpserver.WriteError(w, http.StatusNotFound, "not_found", "Not found.", false)
	case errors.Is(err, errProvider):
		s.Log.Error("orders: payment provider failed", "request_id", httpserver.RequestID(r.Context()), "error", err.Error())
		httpserver.WriteError(w, http.StatusServiceUnavailable, "payments_unavailable", "Payment is not available right now. Try again shortly.", true)
	default:
		s.Log.Error("orders: request failed", "request_id", httpserver.RequestID(r.Context()), "error", err.Error())
		httpserver.WriteError(w, http.StatusInternalServerError, "internal_error", "Try again shortly.", true)
	}
}

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// customer checks the account can order: active, and with a verified phone.
func customer(ctx context.Context, q querier, id string) (bool, error) {
	var active, hasPhone bool
	err := q.QueryRow(ctx, `SELECT p.status = 'active', p.phone IS NOT NULL FROM customers c JOIN people p ON p.id = c.person_id
		WHERE c.id = $1`, id).Scan(&active, &hasPhone)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, identity.ErrSessionInvalid
	}
	if err == nil && !active {
		err = errBlocked
	}
	return hasPhone, err
}

// event records a true change of state, which is kept for good.
func event(ctx context.Context, tx pgx.Tx, orderID, from, to, actorType string, actorID any, reason string) error {
	_, err := tx.Exec(ctx, `INSERT INTO order_events (order_id, from_state, to_state, actor_type, actor_id, reason)
		VALUES ($1, NULLIF($2, ''), $3, $4, $5, NULLIF($6, ''))`, orderID, from, to, actorType, actorID, reason)
	return err
}

type line struct {
	ItemID string `json:"item_id"`
	Qty    int    `json:"qty"`
}

func (s *Service) create(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body struct {
		GateID string `json:"gate_id"`
		Items  []line `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !idPattern.MatchString(body.GateID) || len(body.Items) == 0 || len(body.Items) > maxLines {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send gate_id and up to 10 items.", false)
		return
	}
	basket := map[string]int{}
	for _, l := range body.Items {
		if !idPattern.MatchString(l.ItemID) || l.Qty < 1 || l.Qty > maxQtyLine {
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Each item needs item_id and a qty from 1 to 20.", false)
			return
		}
		basket[strings.ToLower(l.ItemID)] += l.Qty
		if basket[strings.ToLower(l.ItemID)] > maxQtyLine {
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "At most 20 of one item.", false)
			return
		}
	}
	// The payment provider is an outside call, so it comes before the transaction, with the total
	// at today's prices. The transaction checks the prices have not changed since.
	quoted, err := quote(r.Context(), s.Pool, basket)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	providerOrder, err := s.Payments.NewOrder(r.Context(), quoted)
	if err != nil {
		s.fail(w, r, errors.Join(errProvider, err))
		return
	}
	now := s.clock()
	var reply map[string]any
	err = database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		if err := identity.CheckSession(r.Context(), tx, actor); err != nil {
			return err
		}
		hasPhone, err := customer(r.Context(), tx, actor.ID)
		if err != nil {
			return err
		}
		if !hasPhone {
			return &refusal{"phone_required", "Verify a phone first."}
		}
		var lat, lng float64
		var verified bool
		var historyID string
		err = tx.QueryRow(r.Context(), `SELECT g.lat, g.lng, g.verified,
				(SELECT h.id::text FROM gate_history h WHERE h.gate_id = g.id ORDER BY h.version DESC LIMIT 1)
			FROM gates g WHERE g.id = $1 AND g.customer_id = $2`, body.GateID, actor.ID).Scan(&lat, &lng, &verified, &historyID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		if !verified {
			return &refusal{"gate_not_verified", "This gate is not verified."}
		}
		pick, err := availability.PickScooter(r.Context(), tx, lat, lng, now, basket)
		if err != nil {
			return err
		}
		switch pick.Reason {
		case "":
		case availability.ReasonSoldOut:
			return &refusal{"sold_out", "Not enough of these items is in reach."}
		default:
			return &refusal{"no_offer", "Nothing can be delivered here right now."}
		}
		holdFor, _ := pick.Settings.Get("payment_hold_s")
		var zoneRow any
		if pick.Settings.ZoneRowID != "" {
			zoneRow = pick.Settings.ZoneRowID
		}
		var orderID string
		if err := tx.QueryRow(r.Context(), `INSERT INTO orders (customer_id, idempotency_key, gate_id, gate_history_id, zone_id, scooter_id,
				global_settings, zone_settings, state, total_paise, hold_until)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'PAYMENT_PENDING', 1, $9) RETURNING id::text`,
			actor.ID, r.Header.Get("Idempotency-Key"), body.GateID, historyID, pick.ZoneID, pick.ScooterID,
			pick.Settings.GlobalRowID, zoneRow, now.Add(time.Duration(holdFor)*time.Second)).Scan(&orderID); err != nil {
			return err
		}
		total := 0
		for item, qty := range basket {
			var name, priceID string
			var unit int
			if err := tx.QueryRow(r.Context(), `SELECT m.name, m.price_paise,
					(SELECT p.id::text FROM price_history p WHERE p.item_id = m.id ORDER BY p.created_at DESC, p.id DESC LIMIT 1)
				FROM menu_items m WHERE m.id = $1`, item).Scan(&name, &unit, &priceID); err != nil {
				return err
			}
			if _, err := tx.Exec(r.Context(), `INSERT INTO order_items (order_id, item_id, price_history_id, name, qty, unit_paise)
				VALUES ($1, $2, $3, $4, $5, $6)`, orderID, item, priceID, name, qty, unit); err != nil {
				return err
			}
			total += unit * qty
			if err := holdStock(r.Context(), tx, orderID, pick.ScooterID, item, qty, identity.TypeCustomer, actor.ID); err != nil {
				return err
			}
		}
		if total != quoted {
			return &refusal{"price_changed", "A price changed. Load the menu and try again."}
		}
		if _, err := tx.Exec(r.Context(), "UPDATE orders SET total_paise = $2 WHERE id = $1", orderID, total); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `INSERT INTO payments (order_id, provider, provider_order_id, amount_paise) VALUES ($1, $2, $3, $4)`,
			orderID, s.Payments.Name(), providerOrder, total); err != nil {
			return err
		}
		if err := event(r.Context(), tx, orderID, "", "PAYMENT_PENDING", identity.TypeCustomer, actor.ID, "created"); err != nil {
			return err
		}
		if err := audit.Record(r.Context(), tx, audit.Event{
			ActorType: identity.TypeCustomer, ActorID: actor.ID, Action: "order.created", EntityType: "order", EntityID: orderID,
			After: map[string]any{"scooter_id": pick.ScooterID, "total_paise": total},
		}); err != nil {
			return err
		}
		order, err := view(r.Context(), tx, s, orderID, now)
		if err != nil {
			return err
		}
		reply = map[string]any{"order": order, "payment": order["payment_request"], "server_time": serverTime()}
		return idempotency.Save(r.Context(), tx, http.StatusCreated, reply)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusCreated, reply)
}

// quote prices a basket now. An item not found or not on sale is left for the order to refuse.
func quote(ctx context.Context, q querier, basket map[string]int) (int, error) {
	total := 0
	for item, qty := range basket {
		var price int
		err := q.QueryRow(ctx, "SELECT price_paise FROM menu_items WHERE id = $1", item).Scan(&price)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, err
		}
		total += price * qty
	}
	return max(total, 1), nil
}

// holdStock reserves units of an item on the scooter, from the lots expiring first. PickScooter already
// locked these rows and counted enough.
func holdStock(ctx context.Context, tx pgx.Tx, orderID, scooter, item string, qty int, actorType string, actorID any) error {
	rows, err := tx.Query(ctx, `SELECT ss.lot_id::text, ss.on_hand - ss.reserved FROM scooter_stock ss JOIN lots l ON l.id = ss.lot_id
		WHERE ss.scooter_id = $1 AND l.item_id = $2 AND l.status = 'ok' AND l.temp_status = 'ok' AND l.expires_at > now()
			AND ss.on_hand > ss.reserved
		ORDER BY l.expires_at, ss.lot_id`, scooter, item)
	if err != nil {
		return err
	}
	type lot struct {
		id   string
		free int
	}
	lots, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (lot, error) {
		var l lot
		return l, r.Scan(&l.id, &l.free)
	})
	if err != nil {
		return err
	}
	for _, l := range lots {
		if qty == 0 {
			break
		}
		take := min(qty, l.free)
		if _, err := tx.Exec(ctx, "UPDATE scooter_stock SET reserved = reserved + $3, updated_at = now() WHERE scooter_id = $1 AND lot_id = $2", scooter, l.id, take); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO stock_ledger (scooter_id, lot_id, delta_on_hand, delta_reserved, reason, actor_type, actor_id)
			VALUES ($1, $2, 0, $3, 'reserve', $4, $5)`, scooter, l.id, take, actorType, actorID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO reservations (order_id, scooter_id, lot_id, item_id, qty) VALUES ($1, $2, $3, $4, $5)", orderID, scooter, l.id, item, take); err != nil {
			return err
		}
		qty -= take
	}
	if qty != 0 {
		return errors.New("orders: the stock counted for this order is gone")
	}
	return nil
}

// Stage maps the true state to what the customer sees. Offers and riders come in v0.5.0, so the
// states after OFFERED are mapped for completeness only.
func Stage(state string) string {
	switch state {
	case "PAYMENT_PENDING":
		return "payment"
	case "PAYMENT_FAILED":
		return "payment_failed"
	case "PAYMENT_SECURED", "OFFERED", "RIDER_ACCEPTED", "PACKED":
		return "looking_for_rider"
	case "NO_SCOOTER":
		return "no_scooter"
	case "TIMER_RUNNING":
		return "timer_running"
	case "ARRIVED":
		return "arrived"
	case "HANDED_OVER", "COMPLETED":
		return "handed_over"
	default:
		return "ended"
	}
}

// stageAt is Stage with the minimum wait: until it passes, a rider who has accepted stays hidden.
func stageAt(state string, minWait *time.Time, now time.Time) string {
	shown := minWait != nil && !now.Before(*minWait)
	switch state {
	case "RIDER_ACCEPTED", "PACKED":
		if shown {
			return "rider_accepted"
		}
		return "looking_for_rider"
	case "ARRIVED":
		if shown {
			return "arrived"
		}
		return "looking_for_rider"
	}
	return Stage(state)
}

// view builds the customer's Order. It never includes the true state.
func view(ctx context.Context, q querier, s *Service, orderID string, now time.Time) (map[string]any, error) {
	var state, gateID, label, currency string
	var ended *string
	var seq int64
	var total int
	var cancelUntil, waitDeadline, minWait, endedAt *time.Time
	var created time.Time
	var orderNo int64
	var riderName *string
	var hasRider, waitSeen bool
	err := q.QueryRow(ctx, `SELECT o.state, o.seq, o.total_paise, o.currency, o.ended_reason, o.cancel_until, o.wait_deadline, o.created_at,
			o.gate_id::text, h.label, o.min_wait_until, o.ended_at, o.rider_id IS NOT NULL, r.name, o.min_wait_until_seen, o.order_no
		FROM orders o JOIN gate_history h ON h.id = o.gate_history_id LEFT JOIN riders r ON r.id = o.rider_id WHERE o.id = $1`, orderID).
		Scan(&state, &seq, &total, &currency, &ended, &cancelUntil, &waitDeadline, &created, &gateID, &label, &minWait, &endedAt, &hasRider, &riderName, &waitSeen, &orderNo)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, "SELECT item_id::text, name, qty, unit_paise FROM order_items WHERE order_id = $1 ORDER BY name, item_id", orderID)
	if err != nil {
		return nil, err
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (map[string]any, error) {
		var id, name string
		var qty, unit int
		err := r.Scan(&id, &name, &qty, &unit)
		return map[string]any{"item_id": id, "name": name, "qty": qty, "unit_price": map[string]any{"amount_minor": unit, "currency": currency}}, err
	})
	if err != nil {
		return nil, err
	}
	var paymentStatus, providerOrder, provider string
	err = q.QueryRow(ctx, `SELECT status, provider_order_id, provider FROM payments WHERE order_id = $1
		ORDER BY (status = 'paid') DESC, created_at DESC, id DESC LIMIT 1`, orderID).Scan(&paymentStatus, &providerOrder, &provider)
	if err != nil {
		return nil, err
	}
	var refunded bool
	if err := q.QueryRow(ctx, `SELECT CASE WHEN EXISTS (SELECT 1 FROM payments WHERE order_id = $1 AND status = 'paid')
		THEN (SELECT coalesce(sum(r.amount_paise), 0) FROM refunds r JOIN payments p ON p.id = r.payment_id AND p.status = 'paid'
				WHERE r.order_id = $1 AND r.status = 'processed')
			>= (SELECT amount_paise FROM payments WHERE order_id = $1 AND status = 'paid')
		ELSE EXISTS (SELECT 1 FROM refunds WHERE order_id = $1 AND status = 'processed') END`, orderID).Scan(&refunded); err != nil {
		return nil, err
	}
	stage := stageAt(state, minWait, now)
	status := "pending"
	switch {
	case refunded:
		status = "refunded"
	case paymentStatus == "paid":
		status = "paid"
	case stage == "ended":
		status = "not_paid"
	case paymentStatus == "failed":
		status = "failed"
	}
	var request any
	if stage == "payment" {
		request = map[string]any{"razorpay_key_id": s.Payments.KeyID(), "razorpay_order_id": providerOrder, "amount": map[string]any{"amount_minor": total, "currency": currency}}
	}
	cancellable := false
	switch stage {
	case "payment", "payment_failed", "no_scooter":
		cancellable = true
	case "looking_for_rider":
		cancellable = (cancelUntil != nil && now.Before(*cancelUntil)) || (waitDeadline != nil && !now.Before(*waitDeadline))
	case "rider_accepted":
		cancellable = waitDeadline != nil && !now.Before(*waitDeadline)
	}
	// The timer is only ever set by the server. The code shows at arrived, or when the signal was
	// lost after the timer or the wait deadline.
	timer := map[string]any{"timer_started_at": nil, "timer_duration_s": nil, "timer_stopped_at": nil, "timer_missed": false}
	var otp any
	var started, stopped, released *time.Time
	var length *int
	var missed bool
	var code *string
	if err := q.QueryRow(ctx, "SELECT timer_started_at, timer_stopped_at, timer_length_s, timer_missed, handover_otp, otp_released_at FROM orders WHERE id = $1", orderID).
		Scan(&started, &stopped, &length, &missed, &code, &released); err != nil {
		return nil, err
	}
	if started != nil {
		timer = map[string]any{"timer_started_at": stamp(started), "timer_duration_s": length, "timer_stopped_at": stamp(stopped), "timer_missed": missed}
	}
	if code != nil && (stage == "arrived" || (released != nil && (stage == "rider_accepted" || stage == "timer_running"))) {
		otp = *code
	}
	remedy, err := remedyView(ctx, q, orderID)
	if err != nil {
		return nil, err
	}
	var etaOffer any
	if stage == "no_scooter" {
		if etaOffer, err = etaOfferView(ctx, q, orderID, now); err != nil {
			return nil, err
		}
	}
	// The rider is shown from rider_accepted on, and on an ended order only if they were shown.
	var rider any
	shown := hasRider && minWait != nil
	switch stage {
	case "rider_accepted", "timer_running", "arrived", "handed_over":
	case "ended":
		// Shown if the wait's end was recorded, or the order ended after it.
		shown = shown && (waitSeen || (endedAt != nil && !endedAt.Before(*minWait)))
	default:
		shown = false
	}
	if shown {
		first := "Your rider"
		if riderName != nil && strings.TrimSpace(*riderName) != "" {
			first = strings.Fields(*riderName)[0]
		}
		rider = map[string]any{"first_name": first}
	}
	var endedReason any
	if ended != nil {
		endedReason = *ended
	}
	return map[string]any{
		"id": orderID, "seq": seq, "customer_stage": stage, "ended_reason": endedReason,
		"gate":  map[string]any{"id": gateID, "label": label},
		"items": items, "total": map[string]any{"amount_minor": total, "currency": currency},
		"payment_status": status, "payment_request": request, "cancellable": cancellable,
		"cancel_until": stamp(cancelUntil), "wait_deadline": stamp(waitDeadline),
		// No rider is ever shown before v0.5.0, so these stay at their "not yet" values.
		"eta_source": "placeholder", "eta_s": nil, "eta_offer": etaOffer, "rider": rider,
		"timer":  timer,
		"remedy": remedy, "handover_otp": otp, "created_at": created.UTC().Format(time.RFC3339Nano), "order_number": OrderNumber(orderNo),
	}, nil
}

// owned reads an order only if this customer placed it. Another customer's order is not found.
func owned(ctx context.Context, q querier, orderID, customerID string) error {
	if !idPattern.MatchString(orderID) {
		return errNotFound
	}
	var mine bool
	if err := q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM orders WHERE id = $1 AND customer_id = $2)", orderID, customerID).Scan(&mine); err != nil {
		return err
	}
	if !mine {
		return errNotFound
	}
	return nil
}

func (s *Service) readable(r *http.Request, actor identity.Actor) error {
	live, err := identity.SessionLive(r.Context(), s.Pool, actor)
	if err != nil {
		return err
	}
	if !live {
		return identity.ErrSessionInvalid
	}
	_, err = customer(r.Context(), s.Pool, actor.ID)
	return err
}

func (s *Service) get(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	id := chi.URLParam(r, "order_id")
	if err := s.readable(r, actor); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := owned(r.Context(), s.Pool, id, actor.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	order, err := view(r.Context(), s.Pool, s, id, s.clock())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"order": order, "server_time": serverTime()})
}

// The cursor is the last order's time and ID, so a new order never shifts a page.
func encodeCursor(t time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(t.UnixMicro(), 10) + "|" + id))
}

func decodeCursor(raw string) (time.Time, string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return time.Time{}, "", false
	}
	micros, id, ok := strings.Cut(string(b), "|")
	n, err := strconv.ParseInt(micros, 10, 64)
	if !ok || err != nil || !idPattern.MatchString(id) {
		return time.Time{}, "", false
	}
	return time.UnixMicro(n), id, true
}

func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "limit is from 1 to 100.", false)
			return
		}
		limit = n
	}
	after, afterID := time.Now().Add(time.Hour), "ffffffff-ffff-ffff-ffff-ffffffffffff"
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		var ok bool
		if after, afterID, ok = decodeCursor(raw); !ok {
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "That cursor is not valid.", false)
			return
		}
	}
	if err := s.readable(r, actor); err != nil {
		s.fail(w, r, err)
		return
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT id::text, created_at FROM orders WHERE customer_id = $1
		AND (created_at, id) < ($2, $3::uuid) ORDER BY created_at DESC, id DESC LIMIT $4`,
		actor.ID, after, afterID, limit+1)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	type ref struct {
		id string
		at time.Time
	}
	refs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ref, error) {
		var x ref
		return x, r.Scan(&x.id, &x.at)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var next any
	if len(refs) > limit {
		refs = refs[:limit]
		last := refs[len(refs)-1]
		next = encodeCursor(last.at, last.id)
	}
	out := make([]map[string]any, 0, len(refs))
	now := s.clock()
	for _, x := range refs {
		order, err := view(r.Context(), s.Pool, s, x.id, now)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		out = append(out, order)
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"orders": out, "next_cursor": next, "server_time": serverTime()})
}

// testPayment stands in for the payment provider's webhook, on staging only (decision 0013).
func (s *Service) testPayment(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	id := chi.URLParam(r, "order_id")
	var body struct {
		Result string `json:"result"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || (body.Result != "paid" && body.Result != "failed") {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send result: paid or failed.", false)
		return
	}
	now := s.clock()
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		if err := identity.CheckSession(r.Context(), tx, actor); err != nil {
			return err
		}
		if _, err := customer(r.Context(), tx, actor.ID); err != nil {
			return err
		}
		if err := owned(r.Context(), tx, id, actor.ID); err != nil {
			return err
		}
		var providerOrder string
		err := tx.QueryRow(r.Context(), `SELECT provider_order_id FROM payments WHERE order_id = $1 AND status = 'created'
			ORDER BY created_at DESC, id DESC LIMIT 1`, id).Scan(&providerOrder)
		if errors.Is(err, pgx.ErrNoRows) {
			// Nothing is waiting for a result, so there is nothing to do. A repeat lands here.
			order, err := view(r.Context(), tx, s, id, now)
			reply = map[string]any{"order": order, "server_time": serverTime()}
			return err
		}
		if err != nil {
			return err
		}
		if err := Confirm(r.Context(), tx, id, providerOrder, body.Result == "paid", now); err != nil {
			return err
		}
		order, err := view(r.Context(), tx, s, id, now)
		if err != nil {
			return err
		}
		reply = map[string]any{"order": order, "server_time": serverTime()}
		return nil
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, reply)
}

// Confirm applies a payment result for one provider order, as Razorpay's webhook will. A repeat
// has no second effect. Money taken for an order that has ended, or is already paid, goes back.
func Confirm(ctx context.Context, tx pgx.Tx, orderID, providerOrder string, paid bool, now time.Time) error {
	var state string
	var cancelWindow, maxWait, minWait int
	if err := tx.QueryRow(ctx, `SELECT o.state,
			coalesce(z.cancel_window_s, g.cancel_window_s), coalesce(z.max_wait_before_timer_s, g.max_wait_before_timer_s),
			coalesce(z.min_search_wait_s, g.min_search_wait_s, 0)
		FROM orders o JOIN zone_settings g ON g.id = o.global_settings LEFT JOIN zone_settings z ON z.id = o.zone_settings
		WHERE o.id = $1 FOR UPDATE OF o`, orderID).Scan(&state, &cancelWindow, &maxWait, &minWait); err != nil {
		return err
	}
	var paymentID, status, provider string
	var amount int
	err := tx.QueryRow(ctx, `SELECT id::text, status, amount_paise, provider FROM payments WHERE order_id = $1 AND provider_order_id = $2 FOR UPDATE`,
		orderID, providerOrder).Scan(&paymentID, &status, &amount, &provider)
	if err != nil {
		return err
	}
	if status != "created" {
		return nil // Already decided. A repeated webhook changes nothing.
	}
	if !paid {
		if _, err := tx.Exec(ctx, "UPDATE payments SET status = 'failed', updated_at = now() WHERE id = $1", paymentID); err != nil {
			return err
		}
		if state != "PAYMENT_PENDING" {
			return nil
		}
		if _, err := tx.Exec(ctx, "UPDATE orders SET state = 'PAYMENT_FAILED', seq = seq + 1, updated_at = now() WHERE id = $1", orderID); err != nil {
			return err
		}
		return event(ctx, tx, orderID, state, "PAYMENT_FAILED", "system", nil, "payment failed")
	}
	if state != "PAYMENT_PENDING" && state != "PAYMENT_FAILED" {
		// The order has ended, or another payment already paid for it, so this money goes back.
		// Only one row per order may be paid, so a second one is recorded as refunded.
		var alreadyPaid bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM payments WHERE order_id = $1 AND status = 'paid')", orderID).Scan(&alreadyPaid); err != nil {
			return err
		}
		next := "paid"
		if alreadyPaid {
			next = "refunded"
		}
		if _, err := tx.Exec(ctx, "UPDATE payments SET status = $2, updated_at = now() WHERE id = $1", paymentID, next); err != nil {
			return err
		}
		reason := "customer_cancelled"
		switch {
		case alreadyPaid:
			reason = "duplicate_payment"
		case state == "TIMED_OUT":
			reason = "payment_after_hold"
		}
		return refund(ctx, tx, orderID, paymentID, provider, reason, amount)
	}
	if _, err := tx.Exec(ctx, "UPDATE payments SET status = 'paid', updated_at = now() WHERE id = $1", paymentID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE orders SET state = 'PAYMENT_SECURED', seq = seq + 1, paid_at = $2::timestamptz,
			cancel_until = $2::timestamptz + make_interval(secs => $3::int), wait_deadline = $2::timestamptz + make_interval(secs => $4::int),
			min_wait_until = $2::timestamptz + make_interval(secs => $5::int), updated_at = now()
		WHERE id = $1`, orderID, now, cancelWindow, maxWait, minWait); err != nil {
		return err
	}
	return event(ctx, tx, orderID, state, "PAYMENT_SECURED", "system", nil, fmt.Sprintf("payment %s", paymentID))
}

// refund records money going back. The test payment moved no money, so its refund is done at
// once. A real provider's refund is pending until the provider says it is processed.
func refund(ctx context.Context, tx pgx.Tx, orderID, paymentID, provider, reason string, amount int) error {
	status := "pending"
	if provider == "test" {
		status = "processed"
	}
	_, err := tx.Exec(ctx, `INSERT INTO refunds (order_id, payment_id, reason_code, amount_paise, status, expected_by)
		VALUES ($1, $2, $3, $4, $5, now() + interval '7 days') ON CONFLICT (payment_id, reason_code) DO NOTHING`,
		orderID, paymentID, reason, amount, status)
	return err
}
