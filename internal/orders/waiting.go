package orders

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"time"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/outbox"
)

// riderShown are the customer stages from which the customer is shown the rider, so the only ones
// where "the rider is waiting" can be told to them.
func riderShown(stage string) bool {
	switch stage {
	case "rider_accepted", "timer_running", "arrived", "handed_over":
		return true
	}
	return false
}

// markWaitingAtGate records that the rider is waiting at the gate, for an order they accepted once
// the server knows they are there: the order is ARRIVED, or the handover code was released after a
// lost signal, as for handOver. The first time is kept, so a repeat changes nothing. The customer's
// order shows the time, and the customer is pushed once the rider is shown to them.
func (s *Service) markWaitingAtGate(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	id := chi.URLParam(r, "order_id")
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ctx := r.Context()
		if err := identity.CheckSession(ctx, tx, actor); err != nil {
			return err
		}
		if err := riderOK(ctx, tx, actor.ID); err != nil {
			return err
		}
		if !idPattern.MatchString(id) {
			return errNotFound
		}
		var offer string
		err := tx.QueryRow(ctx, `SELECT id::text FROM offers WHERE order_id = $1 AND rider_id = $2 AND state = 'accepted'
			ORDER BY created_at DESC, id DESC LIMIT 1`, id, actor.ID).Scan(&offer)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		// The order's lock first, as every change to an order takes it, then the state is read under it.
		if _, err := tx.Exec(ctx, "SELECT 1 FROM orders WHERE id = $1 FOR UPDATE", id); err != nil {
			return err
		}
		now := s.clock()
		f, err := readOffer(ctx, tx, offer)
		if err != nil {
			return err
		}
		switch {
		case f.orderState == "HANDED_OVER" || f.orderState == "COMPLETED":
			return &refusal{"handover_not_allowed", "This order has already been handed over."}
		case f.releasedAt != nil && f.endedReason != nil && *f.endedReason == "cancelled":
			return &refusal{"order_cancelled", "The customer cancelled this order."}
		case f.releasedAt != nil:
			return &refusal{"order_reassigned", "This order is no longer yours."}
		}
		var released bool
		if err := tx.QueryRow(ctx, "SELECT otp_released_at IS NOT NULL FROM orders WHERE id = $1", id).Scan(&released); err != nil {
			return err
		}
		switch f.orderState {
		case "ARRIVED":
		case "RIDER_ACCEPTED", "PACKED", "TIMER_RUNNING":
			if !released {
				return &refusal{"not_arrived", "You are not at the gate yet."}
			}
		default:
			return &refusal{"handover_not_allowed", "This order cannot be handed over now."}
		}
		// The first time is kept. The customer's seq moves only once they can see the rider, as
		// arrive does, so a wait during the minimum wait shows nothing.
		var customerID string
		var minWait *time.Time
		if err := tx.QueryRow(ctx, "SELECT customer_id::text, min_wait_until FROM orders WHERE id = $1", id).Scan(&customerID, &minWait); err != nil {
			return err
		}
		// The same stage test as the customer's order view decides both the seq and the push (#159 review).
		stage := stageAt(f.orderState, minWait, now)
		shown := riderShown(stage)
		step := 0
		if shown {
			step = 1
		}
		var seq int64
		err = tx.QueryRow(ctx, `UPDATE orders SET rider_waiting_since = $2, seq = seq + $3, updated_at = now()
			WHERE id = $1 AND rider_waiting_since IS NULL RETURNING seq`, id, now, step).Scan(&seq)
		first := !errors.Is(err, pgx.ErrNoRows)
		if err != nil && first {
			return err
		}
		if first {
			if err := event(ctx, tx, id, f.orderState, f.orderState, identity.TypeRider, actor.ID, "waiting at the gate"); err != nil {
				return err
			}
			if err := bumpRiderOf(ctx, tx, id); err != nil {
				return err
			}
			if shown {
				if err := outbox.Add(ctx, tx, "push.customer", map[string]any{"customer_id": customerID, "order_id": id, "stage": stage, "kind": "rider_waiting", "seq": seq}); err != nil {
					return err
				}
			}
			if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeRider, ActorID: actor.ID, Action: "order.rider_waiting", EntityType: "order", EntityID: id}); err != nil {
				return err
			}
		}
		if f, err = readOffer(ctx, tx, offer); err != nil {
			return err
		}
		view, err := riderOrderView(ctx, tx, f)
		if err != nil {
			return err
		}
		reply = map[string]any{"order": view, "server_time": serverTime()}
		return idempotency.Save(ctx, tx, http.StatusOK, reply)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, reply)
}
