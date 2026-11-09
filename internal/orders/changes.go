package orders

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/availability"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

// lockOrder takes the order's row lock, which every change to an order takes, so a cancel, a
// payment and a background pass can never act on the same order at once. Another customer's
// order is not found.
func lockOrder(ctx context.Context, tx pgx.Tx, orderID, customerID string) (string, error) {
	if !idPattern.MatchString(orderID) {
		return "", errNotFound
	}
	var state string
	err := tx.QueryRow(ctx, "SELECT state FROM orders WHERE id = $1 AND customer_id = $2 FOR UPDATE", orderID, customerID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errNotFound
	}
	return state, err
}

// releaseHolds gives back the stock an order still holds, changing rows in scooter and lot order.
func releaseHolds(ctx context.Context, tx pgx.Tx, orderID, actorType string, actorID any) error {
	rows, err := tx.Query(ctx, `UPDATE reservations SET status = 'released', released_at = now()
		WHERE order_id = $1 AND status = 'held' RETURNING scooter_id::text, lot_id::text, qty`, orderID)
	if err != nil {
		return err
	}
	type held struct {
		scooter, lot string
		qty          int
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (held, error) {
		var h held
		return h, r.Scan(&h.scooter, &h.lot, &h.qty)
	})
	if err != nil {
		return err
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].scooter != all[j].scooter {
			return all[i].scooter < all[j].scooter
		}
		return all[i].lot < all[j].lot
	})
	for _, h := range all {
		if _, err := tx.Exec(ctx, "UPDATE scooter_stock SET reserved = reserved - $3, updated_at = now() WHERE scooter_id = $1 AND lot_id = $2",
			h.scooter, h.lot, h.qty); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO stock_ledger (scooter_id, lot_id, delta_on_hand, delta_reserved, reason, actor_type, actor_id)
			VALUES ($1, $2, 0, $3, 'release', $4, $5)`, h.scooter, h.lot, -h.qty, actorType, actorID); err != nil {
			return err
		}
	}
	return nil
}

// end moves a locked order to an ended state, and gives back its stock.
func end(ctx context.Context, tx pgx.Tx, orderID, from, to, endedReason, actorType string, actorID any, why string) error {
	if _, err := tx.Exec(ctx, `UPDATE orders SET state = $2, ended_reason = $3, ended_at = now(), seq = seq + 1, handover_otp = NULL, updated_at = now()
		WHERE id = $1`, orderID, to, endedReason); err != nil {
		return err
	}
	if err := endOffers(ctx, tx, orderID, to); err != nil {
		return err
	}
	if err := releaseHolds(ctx, tx, orderID, actorType, actorID); err != nil {
		return err
	}
	return event(ctx, tx, orderID, from, to, actorType, actorID, why)
}

// refundPaid gives back the whole payment of an order.
func refundPaid(ctx context.Context, tx pgx.Tx, orderID, reason string) error {
	var paymentID, provider string
	var amount int
	if err := tx.QueryRow(ctx, "SELECT id::text, provider, amount_paise FROM payments WHERE order_id = $1 AND status = 'paid' FOR UPDATE", orderID).
		Scan(&paymentID, &provider, &amount); err != nil {
		return err
	}
	return refund(ctx, tx, orderID, paymentID, provider, reason, amount)
}

// cancel follows the table in contracts/README.md, "Cancels and refunds". The boundary is decided
// here, under the order's row lock, by the server's clock.
func (s *Service) cancel(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	id := chi.URLParam(r, "order_id")
	var body struct {
		Reason string `json:"reason"`
	}
	// The reason is checked but not kept: it is free text, which could hold personal details.
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || utf8.RuneCountInString(body.Reason) > 200 {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send a reason of up to 200 characters, or none.", false)
		return
	}
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		if err := identity.CheckSession(r.Context(), tx, actor); err != nil {
			return err
		}
		if _, err := customer(r.Context(), tx, actor.ID); err != nil {
			return err
		}
		state, err := lockOrder(r.Context(), tx, id, actor.ID)
		if err != nil {
			return err
		}
		// The clock is read once the lock is held, so the boundary is decided under it.
		now := s.clock()
		var cancelUntil, waitDeadline, minWait *time.Time
		if err := tx.QueryRow(r.Context(), "SELECT cancel_until, wait_deadline, min_wait_until FROM orders WHERE id = $1", id).Scan(&cancelUntil, &waitDeadline, &minWait); err != nil {
			return err
		}
		refundReason, again := "", false
		switch stageAt(state, minWait, now) {
		case "payment", "payment_failed":
		case "no_scooter":
			refundReason = "no_rider_found"
		case "looking_for_rider":
			switch {
			case cancelUntil != nil && now.Before(*cancelUntil):
				refundReason = "customer_cancelled"
			case waitDeadline != nil && !now.Before(*waitDeadline):
				refundReason = "wait_deadline_passed"
			default:
				return &refusal{"cancel_not_allowed", "This order can no longer be cancelled."}
			}
		case "rider_accepted":
			if waitDeadline == nil || now.Before(*waitDeadline) {
				return &refusal{"cancel_not_allowed", "This order can no longer be cancelled."}
			}
			refundReason = "wait_deadline_passed"
		case "timer_running", "arrived", "handed_over":
			return &refusal{"cancel_not_allowed", "This order can no longer be cancelled."}
		default:
			// A cancel of an order already cancelled succeeds, so a retry after a lost reply is safe.
			if state != "CANCELLED" {
				return &refusal{"cancel_not_allowed", "This order can no longer be cancelled."}
			}
			again = true
		}
		if !again {
			if err := end(r.Context(), tx, id, state, "CANCELLED", "cancelled", identity.TypeCustomer, actor.ID, "customer cancelled"); err != nil {
				return err
			}
			if refundReason != "" {
				if err := refundPaid(r.Context(), tx, id, refundReason); err != nil {
					return err
				}
			}
			if err := audit.Record(r.Context(), tx, audit.Event{
				ActorType: identity.TypeCustomer, ActorID: actor.ID, Action: "order.cancelled", EntityType: "order", EntityID: id,
				Before: map[string]any{"state": state}, After: map[string]any{"refund_reason": refundReason},
			}); err != nil {
				return err
			}
		}
		order, err := view(r.Context(), tx, s, id, now)
		if err != nil {
			return err
		}
		reply = map[string]any{"order": order, "server_time": serverTime()}
		return idempotency.Save(r.Context(), tx, http.StatusOK, reply)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, reply)
}

// retry makes a new payment for an order whose payment failed. Its stock is still held.
func (s *Service) retry(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	id := chi.URLParam(r, "order_id")
	if !idPattern.MatchString(id) {
		s.fail(w, r, errNotFound)
		return
	}
	// The provider call comes before the transaction. An order's total never changes.
	var total int
	err := s.Pool.QueryRow(r.Context(), "SELECT total_paise FROM orders WHERE id = $1 AND customer_id = $2", id, actor.ID).Scan(&total)
	if errors.Is(err, pgx.ErrNoRows) {
		err = errNotFound
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	providerOrder, err := s.Payments.NewOrder(r.Context(), total)
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
		if _, err := customer(r.Context(), tx, actor.ID); err != nil {
			return err
		}
		state, err := lockOrder(r.Context(), tx, id, actor.ID)
		if err != nil {
			return err
		}
		if state != "PAYMENT_FAILED" {
			return &refusal{"payment_retry_not_allowed", "Payment can be tried again only after it failed."}
		}
		var holdUntil time.Time
		if err := tx.QueryRow(r.Context(), "SELECT hold_until FROM orders WHERE id = $1", id).Scan(&holdUntil); err != nil {
			return err
		}
		if !now.Before(holdUntil) {
			// The background pass ends the order and gives back its stock.
			return &refusal{"sold_out", "The items are no longer held. Start a new order."}
		}
		if _, err := tx.Exec(r.Context(), "INSERT INTO payments (order_id, provider, provider_order_id, amount_paise) VALUES ($1, $2, $3, $4)",
			id, s.Payments.Name(), providerOrder, total); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), "UPDATE orders SET state = 'PAYMENT_PENDING', seq = seq + 1, updated_at = now() WHERE id = $1", id); err != nil {
			return err
		}
		if err := event(r.Context(), tx, id, state, "PAYMENT_PENDING", identity.TypeCustomer, actor.ID, "payment retried"); err != nil {
			return err
		}
		order, err := view(r.Context(), tx, s, id, now)
		if err != nil {
			return err
		}
		reply = map[string]any{"order": order, "payment": order["payment_request"], "server_time": serverTime()}
		return idempotency.Save(r.Context(), tx, http.StatusOK, reply)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, reply)
}

// changeGate moves an unpaid order to another of the customer's gates. The stock is given back
// and held again on the one scooter that can reach the new gate with the whole basket. If none
// can, nothing changes.
func (s *Service) changeGate(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	id := chi.URLParam(r, "order_id")
	var body struct {
		GateID string `json:"gate_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !idPattern.MatchString(body.GateID) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send gate_id.", false)
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
		state, err := lockOrder(r.Context(), tx, id, actor.ID)
		if err != nil {
			return err
		}
		if state != "PAYMENT_PENDING" {
			return &refusal{"gate_change_not_allowed", "The gate can change only before paying."}
		}
		var lat, lng float64
		var verified bool
		var historyID, current string
		err = tx.QueryRow(r.Context(), `SELECT g.lat, g.lng, g.verified,
				(SELECT h.id::text FROM gate_history h WHERE h.gate_id = g.id ORDER BY h.version DESC LIMIT 1),
				(SELECT o.gate_id::text FROM orders o WHERE o.id = $3)
			FROM gates g WHERE g.id = $1 AND g.customer_id = $2`, body.GateID, actor.ID, id).Scan(&lat, &lng, &verified, &historyID, &current)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		if !verified {
			return &refusal{"gate_not_verified", "This gate is not verified."}
		}
		if current != body.GateID {
			if err := moveOrder(r.Context(), tx, id, actor.ID, lat, lng, historyID, body.GateID, now); err != nil {
				return err
			}
		}
		order, err := view(r.Context(), tx, s, id, now)
		if err != nil {
			return err
		}
		reply = map[string]any{"order": order, "payment": order["payment_request"], "server_time": serverTime()}
		return idempotency.Save(r.Context(), tx, http.StatusOK, reply)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, reply)
}

func moveOrder(ctx context.Context, tx pgx.Tx, orderID, customerID string, lat, lng float64, historyID, gateID string, now time.Time) error {
	basket, err := basketOf(ctx, tx, orderID)
	if err != nil {
		return err
	}
	if err := releaseHolds(ctx, tx, orderID, identity.TypeCustomer, customerID); err != nil {
		return err
	}
	pick, err := availability.PickScooter(ctx, tx, lat, lng, now, basket)
	if err != nil {
		return err
	}
	switch pick.Reason {
	case "":
	case availability.ReasonSoldOut:
		return &refusal{"sold_out", "Not enough of these items is in reach of that gate."}
	default:
		return &refusal{"no_offer", "Nothing can be delivered to that gate right now."}
	}
	// A reservation is unique per order and lot, so the released rows make way for the new ones.
	if _, err := tx.Exec(ctx, "DELETE FROM reservations WHERE order_id = $1 AND status = 'released'", orderID); err != nil {
		return err
	}
	for item, qty := range basket {
		if err := holdStock(ctx, tx, orderID, pick.ScooterID, item, qty, identity.TypeCustomer, customerID); err != nil {
			return err
		}
	}
	var zoneRow any
	if pick.Settings.ZoneRowID != "" {
		zoneRow = pick.Settings.ZoneRowID
	}
	var before string
	if err := tx.QueryRow(ctx, "SELECT scooter_id::text FROM orders WHERE id = $1", orderID).Scan(&before); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE orders SET gate_id = $2, gate_history_id = $3, zone_id = $4, scooter_id = $5, global_settings = $6,
			zone_settings = $7, seq = seq + 1, updated_at = now() WHERE id = $1`,
		orderID, gateID, historyID, pick.ZoneID, pick.ScooterID, pick.Settings.GlobalRowID, zoneRow); err != nil {
		return err
	}
	return audit.Record(ctx, tx, audit.Event{
		ActorType: identity.TypeCustomer, ActorID: customerID, Action: "order.gate_changed", EntityType: "order", EntityID: orderID,
		Before: map[string]any{"scooter_id": before}, After: map[string]any{"scooter_id": pick.ScooterID, "gate_id": gateID},
	})
}

// getRefund says why and how the customer is refunded.
func (s *Service) getRefund(w http.ResponseWriter, r *http.Request) {
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
	var reason, method, status string
	var amount int
	var expected *time.Time
	err := s.Pool.QueryRow(r.Context(), `SELECT reason_code, method, status, amount_paise, expected_by FROM refunds WHERE order_id = $1
		ORDER BY (status <> 'failed') DESC, created_at DESC, id DESC LIMIT 1`, id).Scan(&reason, &method, &status, &amount, &expected)
	if errors.Is(err, pgx.ErrNoRows) {
		err = errNotFound
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"refund": map[string]any{
		"reason_code": reason, "method": method, "status": status,
		"amount": map[string]any{"amount_minor": amount, "currency": "INR"}, "expected_by": stamp(expected),
	}, "server_time": serverTime()})
}
