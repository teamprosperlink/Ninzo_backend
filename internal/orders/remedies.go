package orders

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

// reviewLimit is how long a late arrival waits for a decision before the remedy goes ahead, so the
// customer is never left waiting (contracts/README.md, Signal lost at the gate).
const reviewLimit = 10 * time.Minute

// owe records a remedy once, and tells the customer, since it is a field they see.
func owe(ctx context.Context, tx pgx.Tx, orderID, kind, reason string, amount any) error {
	tag, err := tx.Exec(ctx, `INSERT INTO remedies (order_id, type, reason, amount_paise) VALUES ($1, $2, $3, $4)
		ON CONFLICT (order_id, type) DO NOTHING`, orderID, kind, reason, amount)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE orders SET seq = seq + 1, updated_at = now() WHERE id = $1", orderID); err != nil {
		return err
	}
	return audit.Record(ctx, tx, audit.Event{ActorType: "system", Action: "remedy.owed", EntityType: "order", EntityID: orderID,
		After: map[string]any{"type": kind, "reason": reason}})
}

// creditForMissedTimer owes the credit the console set, or a credit with no amount yet.
func creditForMissedTimer(ctx context.Context, tx pgx.Tx, orderID string) error {
	amount, ok, err := pinned(ctx, tx, orderID, "remedy_credit_paise")
	if err != nil {
		return err
	}
	var value any
	if ok && amount > 0 {
		value = amount
	}
	return owe(ctx, tx, orderID, "credit", "timer_missed", value)
}

// refundMissingFood refunds what was ordered but not handed over: a failure on Ninzo's side, so it
// is always refunded (contracts/README.md, Cancels and refunds).
func refundMissingFood(ctx context.Context, tx pgx.Tx, orderID string) error {
	var missing int
	if err := tx.QueryRow(ctx, "SELECT coalesce(sum((qty - qty_handed) * unit_paise), 0)::int FROM order_items WHERE order_id = $1 AND qty_handed < qty",
		orderID).Scan(&missing); err != nil || missing == 0 {
		return err
	}
	var paymentID, provider string
	err := tx.QueryRow(ctx, "SELECT id::text, provider FROM payments WHERE order_id = $1 AND status = 'paid' FOR UPDATE", orderID).Scan(&paymentID, &provider)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := refund(ctx, tx, orderID, paymentID, provider, "missing_food", missing); err != nil {
		return err
	}
	return owe(ctx, tx, orderID, "refund", "missing_food", missing)
}

// remedyView is the customer's remedy: a refund if there is one, else the latest owed, or null.
func remedyView(ctx context.Context, q querier, orderID string) (any, error) {
	var kind string
	var amount *int
	// The contract has one remedy. Money already going back shows first.
	err := q.QueryRow(ctx, `SELECT type, amount_paise FROM remedies WHERE order_id = $1 AND status <> 'cancelled'
		ORDER BY (type = 'refund') DESC, created_at DESC, id DESC LIMIT 1`,
		orderID).Scan(&kind, &amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var money any
	if amount != nil {
		money = map[string]any{"amount_minor": *amount, "currency": "INR"}
	}
	return map[string]any{"type": kind, "amount": money}, nil
}

// etaOfferView is the longer ETA on offer at no_scooter, or null.
func etaOfferView(ctx context.Context, q querier, orderID string, now time.Time) (any, error) {
	var id string
	var eta int
	var expires time.Time
	err := q.QueryRow(ctx, `SELECT id::text, eta_s, expires_at FROM eta_offers WHERE order_id = $1 AND accepted_at IS NULL AND expires_at > $2
		ORDER BY created_at DESC, id DESC LIMIT 1`, orderID, now).Scan(&id, &eta, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "eta_s": eta, "expires_at": expires.UTC().Format(time.RFC3339Nano)}, nil
}

// reviewLateArrival opens a review when a ping too late to use shows the rider at the gate while
// the timer was still running. The remedy waits for it.
func reviewLateArrival(ctx context.Context, tx pgx.Tx, orderID string, p ping, now time.Time) error {
	var started *time.Time
	var length *int
	var perimeter *int
	var gateLat, gateLng float64
	if err := tx.QueryRow(ctx, `SELECT o.timer_started_at, o.timer_length_s, o.arrival_perimeter_m, h.lat, h.lng
		FROM orders o JOIN gate_history h ON h.id = o.gate_history_id WHERE o.id = $1`, orderID).Scan(&started, &length, &perimeter, &gateLat, &gateLng); err != nil {
		return err
	}
	if started == nil || length == nil || perimeter == nil {
		return nil
	}
	end := started.Add(time.Duration(*length) * time.Second)
	if p.device.Before(*started) || !p.device.Before(end) || metres(p.lat, p.lng, gateLat, gateLng) > float64(*perimeter) {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO arrival_reviews (order_id, lat, lng, device_time, due_at) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (order_id) WHERE status = 'open' DO NOTHING`, orderID, p.lat, p.lng, p.device, now.Add(reviewLimit))
	return err
}

// acceptETA is acceptLongerEta: the customer's explicit consent to a longer time at no_scooter.
func (s *Service) acceptETA(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	id := chi.URLParam(r, "order_id")
	var body struct {
		OfferID string `json:"eta_offer_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !idPattern.MatchString(body.OfferID) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send eta_offer_id.", false)
		return
	}
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ctx := r.Context()
		if err := identity.CheckSession(ctx, tx, actor); err != nil {
			return err
		}
		if _, err := customer(ctx, tx, actor.ID); err != nil {
			return err
		}
		state, err := lockOrder(ctx, tx, id, actor.ID)
		if err != nil {
			return err
		}
		now := s.clock()
		gone := &refusal{"eta_offer_expired", "That time is no longer on offer."}
		var scooter string
		var eta int
		var expires time.Time
		var accepted *time.Time
		err = tx.QueryRow(ctx, "SELECT scooter_id::text, eta_s, expires_at, accepted_at FROM eta_offers WHERE id = $1 AND order_id = $2 FOR UPDATE",
			body.OfferID, id).Scan(&scooter, &eta, &expires, &accepted)
		if errors.Is(err, pgx.ErrNoRows) {
			return gone
		}
		if err != nil {
			return err
		}
		if accepted != nil && state == "NO_SCOOTER" {
			return gone // Used once already: a new search needs a new offer.
		}
		if accepted != nil {
			order, err := view(ctx, tx, s, id, now) // Accepted already: the same answer again.
			reply = map[string]any{"order": order, "server_time": serverTime()}
			if err != nil {
				return err
			}
			return idempotency.Save(ctx, tx, http.StatusOK, reply)
		}
		if state != "NO_SCOOTER" || !now.Before(expires) {
			return gone
		}
		// Hold the stock again on the scooter the time was measured for, if it still has it all.
		basket, err := basketOf(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SELECT 1 FROM scooter_stock WHERE scooter_id = $1 ORDER BY lot_id FOR UPDATE", scooter); err != nil {
			return err
		}
		for item, qty := range basket {
			var free int
			if err := tx.QueryRow(ctx, `SELECT coalesce(sum(ss.on_hand - ss.reserved), 0)::int FROM scooter_stock ss JOIN lots l ON l.id = ss.lot_id
				WHERE ss.scooter_id = $1 AND l.item_id = $2 AND l.status = 'ok' AND l.temp_status = 'ok' AND l.expires_at > now()`, scooter, item).Scan(&free); err != nil {
				return err
			}
			if free < qty {
				return gone
			}
		}
		if _, err := tx.Exec(ctx, "DELETE FROM reservations WHERE order_id = $1 AND status = 'released'", id); err != nil {
			return err
		}
		for item, qty := range basket {
			if err := holdStock(ctx, tx, id, scooter, item, qty, identity.TypeCustomer, actor.ID); err != nil {
				return err
			}
		}
		// The agreed time replaces wait_deadline, so the customer has one clear time to wait for.
		if _, err := tx.Exec(ctx, `UPDATE orders SET state = 'PAYMENT_SECURED', scooter_id = $2, wait_deadline = $3, wait_deadline_seen = false,
				search_from = now(), seq = seq + 1, updated_at = now() WHERE id = $1`, id, scooter, now.Add(time.Duration(eta)*time.Second)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE eta_offers SET accepted_at = $2 WHERE id = $1", body.OfferID, now); err != nil {
			return err
		}
		if err := event(ctx, tx, id, "NO_SCOOTER", "PAYMENT_SECURED", identity.TypeCustomer, actor.ID, "longer ETA accepted"); err != nil {
			return err
		}
		order, err := view(ctx, tx, s, id, now)
		if err != nil {
			return err
		}
		reply = map[string]any{"order": order, "server_time": serverTime()}
		return idempotency.Save(ctx, tx, http.StatusOK, reply)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, reply)
}
