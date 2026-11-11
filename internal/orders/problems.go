package orders

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/outbox"
)

var problemTypes = map[string]bool{"breakdown": true, "accident": true, "gate_closed": true, "customer_absent": true, "unsafe_route": true}

// shown says whether the customer can already see the rider, which decides how a failure ends.
func shown(ctx context.Context, tx pgx.Tx, orderID string, now time.Time) (string, bool, error) {
	var state string
	var minWait *time.Time
	if err := tx.QueryRow(ctx, "SELECT state, min_wait_until FROM orders WHERE id = $1", orderID).Scan(&state, &minWait); err != nil {
		return "", false, err
	}
	switch stageAt(state, minWait, now) {
	case "rider_accepted", "timer_running", "arrived":
		return state, true, nil
	}
	return state, false, nil
}

// rematch takes a locked order from its rider and searches again. The stock stays held until
// another scooter takes it over. Before the customer has seen the rider nothing they see changes,
// so it is silent (contracts/README.md). After, they see looking_for_rider again.
func rematch(ctx context.Context, tx pgx.Tx, orderID, why string, now time.Time) error {
	state, visible, err := shown(ctx, tx, orderID, now)
	if err != nil {
		return err
	}
	// Only an order still with a rider can be taken back. One that ended or was handed over, even
	// a moment ago, is left alone.
	switch state {
	case "OFFERED", "RIDER_ACCEPTED", "PACKED", "ARRIVED":
	default:
		return nil
	}
	rows, err := tx.Query(ctx, `UPDATE offers SET state = CASE WHEN state = 'open' THEN 'expired' ELSE state END,
			ended_reason = 'reassigned', final_state = $2, released_at = now(), updated_at = now()
		WHERE order_id = $1 AND released_at IS NULL RETURNING rider_id::text`, orderID, state)
	if err != nil {
		return err
	}
	riders, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	bump := 0
	if visible {
		bump = 1
	}
	if _, err := tx.Exec(ctx, `UPDATE orders SET state = 'PAYMENT_SECURED', rider_id = NULL, rider_accepted_at = NULL, packed_at = NULL,
			timer_length_s = NULL, zone_radius_m = NULL, arrival_perimeter_m = NULL, handover_otp = NULL, otp_attempts = 0, otp_released_at = NULL,
			zone_hits = 0, perimeter_hits = 0, last_ping_at = NULL, last_live_ping_at = NULL, last_device_time = NULL,
			arrived_at = NULL, arrival_source = NULL, audit_flag = NULL,
			min_wait_until_seen = min_wait_until_seen AND $2 = 0, seq = seq + $2, updated_at = now()
		WHERE id = $1`, orderID, bump); err != nil {
		return err
	}
	if err := event(ctx, tx, orderID, state, "PAYMENT_SECURED", "system", nil, why); err != nil {
		return err
	}
	for _, r := range riders {
		if err := bumpRider(ctx, tx, r); err != nil {
			return err
		}
	}
	return nil
}

// failOrder ends a locked order on Ninzo's side, with a full refund.
func failOrder(ctx context.Context, tx pgx.Tx, orderID, why string) error {
	var state string
	if err := tx.QueryRow(ctx, "SELECT state FROM orders WHERE id = $1", orderID).Scan(&state); err != nil {
		return err
	}
	switch state {
	case "OFFERED", "RIDER_ACCEPTED", "PACKED", "TIMER_RUNNING", "ARRIVED":
	default:
		return nil // Already ended or handed over.
	}
	if err := end(ctx, tx, orderID, state, "FAILED", "failed", "system", nil, why); err != nil {
		return err
	}
	return refundPaid(ctx, tx, orderID, "order_failed")
}

func (s *Service) reportProblem(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body struct {
		Type    string `json:"type"`
		OrderID string `json:"order_id"`
		Note    string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !problemTypes[body.Type] || utf8.RuneCountInString(body.Note) > 1000 ||
		(body.OrderID != "" && !idPattern.MatchString(body.OrderID)) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send a type, and optionally order_id and a note of up to 1000 characters.", false)
		return
	}
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ctx := r.Context()
		if err := identity.CheckSession(ctx, tx, actor); err != nil {
			return err
		}
		if err := riderOK(ctx, tx, actor.ID); err != nil {
			return err
		}
		var err error
		reply, err = s.problemTx(ctx, tx, actor.ID, body.Type, body.OrderID, body.Note, true)
		return err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusCreated, reply)
}

// problemTx records a rider's problem and acts on it: the core of reportProblem, shared with the
// offline batch.
func (s *Service) problemTx(ctx context.Context, tx pgx.Tx, riderID, kind, orderID, noteText string, save bool) (reply map[string]any, err error) {
	err = func() error {
		outcome := "recorded"
		var order any
		if orderID != "" {
			var mine bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM offers WHERE order_id = $1 AND rider_id = $2)", orderID, riderID).Scan(&mine); err != nil {
				return err
			}
			if !mine {
				return &refusal{"order_not_yours", "That order is not one of yours."}
			}
			order = orderID
		}
		breakdown := kind == "breakdown" || kind == "accident"
		if breakdown {
			// The scooter is taken out of service, so it gets no more offers.
			if _, err := tx.Exec(ctx, `UPDATE scooters SET status = 'maintenance', updated_at = now()
				WHERE id = (SELECT scooter_id FROM shifts WHERE rider_id = $1 AND ended_at IS NULL)`, riderID); err != nil {
				return err
			}
			var live, liveState string
			err := tx.QueryRow(ctx, "SELECT order_id::text, state FROM offers WHERE rider_id = $1 AND released_at IS NULL", riderID).Scan(&live, &liveState)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
			case err != nil:
				return err
			default:
				if _, err := tx.Exec(ctx, "SELECT 1 FROM orders WHERE id = $1 FOR UPDATE", live); err != nil {
					return err
				}
				// Read again under the lock: a cancel, a handover or a pass may have taken it.
				var still bool
				if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM offers WHERE rider_id = $1 AND order_id = $2 AND released_at IS NULL)",
					riderID, live).Scan(&still); err != nil {
					return err
				}
				if !still {
					break
				}
				now := s.clock()
				_, visible, err := shown(ctx, tx, live, now)
				if err != nil {
					return err
				}
				if visible {
					outcome = "order_failed"
					err = failOrder(ctx, tx, live, "rider "+kind)
				} else {
					outcome = "rematched"
					err = rematch(ctx, tx, live, "rider "+kind, now)
				}
				if err != nil {
					return err
				}
				order = live
			}
		}
		var note any
		if noteText != "" {
			note = noteText
		}
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO problem_reports (rider_id, order_id, type, source, note, outcome) VALUES ($1, $2, $3, 'rider', $4, $5)
			RETURNING id::text`, riderID, order, kind, note, outcome).Scan(&id); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeRider, ActorID: riderID, Action: "problem.reported", EntityType: "problem_report", EntityID: id,
			After: map[string]any{"type": kind, "outcome": outcome}}); err != nil {
			return err
		}
		reply = map[string]any{"report_id": id, "server_time": serverTime()}
		if !save {
			return nil
		}
		return idempotency.Save(ctx, tx, http.StatusCreated, reply)
	}()
	return reply, err
}

// signalLost raises one support alert for an order whose rider went quiet after the timer started.
func signalLost(ctx context.Context, tx pgx.Tx, orderID string) error {
	var rider *string
	err := tx.QueryRow(ctx, `INSERT INTO problem_reports (rider_id, order_id, type, source, outcome)
		SELECT rider_id, id, 'signal_lost', 'system', 'support_alerted' FROM orders WHERE id = $1
		ON CONFLICT (order_id, rider_id) WHERE source = 'system' AND type = 'signal_lost' DO NOTHING RETURNING rider_id::text`, orderID).Scan(&rider)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // Already reported, so support was already told.
	}
	if err != nil {
		return err
	}
	// The console's support alerts read these (listSupportAlerts).
	return outbox.Add(ctx, tx, "support.alert", map[string]any{"reason": "signal_lost", "order_id": orderID, "rider_id": rider})
}
