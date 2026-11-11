package orders

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/availability"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/outbox"
)

var settingName = regexp.MustCompile(`^[a-z_]+$`)

// pinned reads a setting as fixed on the order when it was created: the zone row's value, else
// the global one. ok is false when neither sets it.
func pinned(ctx context.Context, q querier, orderID, name string) (int, bool, error) {
	if !settingName.MatchString(name) {
		return 0, false, fmt.Errorf("orders: bad setting name %q", name)
	}
	var v *int
	err := q.QueryRow(ctx, `SELECT coalesce(z.`+name+`, g.`+name+`) FROM orders o JOIN zone_settings g ON g.id = o.global_settings
		LEFT JOIN zone_settings z ON z.id = o.zone_settings WHERE o.id = $1`, orderID).Scan(&v)
	if err != nil || v == nil {
		return 0, false, err
	}
	return *v, true, nil
}

// bumpRider raises the rider's seq, so the rider app knows something it can see changed.
func bumpRider(ctx context.Context, tx pgx.Tx, riderID string) error {
	_, err := tx.Exec(ctx, `INSERT INTO rider_state (rider_id, seq) VALUES ($1, 1)
		ON CONFLICT (rider_id) DO UPDATE SET seq = rider_state.seq + 1, updated_at = now()`, riderID)
	return err
}

func newToken() (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "ft_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

func basketOf(ctx context.Context, tx pgx.Tx, orderID string) (map[string]int, error) {
	rows, err := tx.Query(ctx, "SELECT item_id::text, qty FROM order_items WHERE order_id = $1", orderID)
	if err != nil {
		return nil, err
	}
	basket := map[string]int{}
	var item string
	var qty int
	_, err = pgx.ForEachRow(rows, []any{&item, &qty}, func() error { basket[item] += qty; return nil })
	return basket, err
}

// freeRider is the rider on an open shift with this scooter, if they are active and have no live
// offer. One order per rider (decision 0011).
func freeRider(ctx context.Context, tx pgx.Tx, scooter string) (string, bool, error) {
	var rider string
	err := tx.QueryRow(ctx, `SELECT sh.rider_id::text FROM shifts sh
		JOIN scooters s ON s.id = sh.scooter_id AND s.status = 'active'
		JOIN riders r ON r.id = sh.rider_id AND r.status = 'active'
		JOIN people p ON p.id = r.person_id AND p.status = 'active'
		WHERE sh.scooter_id = $1 AND sh.ended_at IS NULL
			AND NOT EXISTS (SELECT 1 FROM offers f WHERE f.rider_id = sh.rider_id AND f.released_at IS NULL)`, scooter).Scan(&rider)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return rider, err == nil, err
}

// dispatch offers a locked, paid order to one rider. The order's own scooter, which holds its
// stock, goes first. If its rider is busy or has already passed on it, the nearest other scooter
// with a free rider and the whole basket takes over the hold. If none can, the order waits while
// its own rider is only busy, and otherwise moves to NO_SCOOTER.
func dispatch(ctx context.Context, tx pgx.Tx, orderID string, now time.Time) error {
	var state, own, gateHistory string
	if err := tx.QueryRow(ctx, "SELECT state, scooter_id::text, gate_history_id::text FROM orders WHERE id = $1", orderID).Scan(&state, &own, &gateHistory); err != nil {
		return err
	}
	if state != "PAYMENT_SECURED" {
		return nil
	}
	// Offers made before a longer ETA was accepted no longer count as tried.
	rows, err := tx.Query(ctx, `SELECT DISTINCT scooter_id::text FROM offers WHERE order_id = $1
		AND created_at >= coalesce((SELECT search_from FROM orders WHERE id = $1), '-infinity')`, orderID)
	if err != nil {
		return err
	}
	tried, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	triedOwn := false
	for _, s := range tried {
		triedOwn = triedOwn || s == own
	}
	scooter, rider := "", ""
	if !triedOwn {
		r, free, err := freeRider(ctx, tx, own)
		if err != nil {
			return err
		}
		if free {
			scooter, rider = own, r
		}
	}
	if scooter == "" {
		rows, err := tx.Query(ctx, `SELECT sh.scooter_id::text FROM shifts sh JOIN offers f ON f.rider_id = sh.rider_id AND f.released_at IS NULL
			WHERE sh.ended_at IS NULL`)
		if err != nil {
			return err
		}
		busy, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		basket, err := basketOf(ctx, tx, orderID)
		if err != nil {
			return err
		}
		var lat, lng float64
		if err := tx.QueryRow(ctx, "SELECT lat, lng FROM gate_history WHERE id = $1", gateHistory).Scan(&lat, &lng); err != nil {
			return err
		}
		// The order's own scooter is left out, so its held units never count twice.
		pick, err := availability.PickScooterExcept(ctx, tx, lat, lng, now, basket, append(append(busy, tried...), own))
		if err != nil {
			return err
		}
		if pick.Reason != "" {
			// Wait only for an active rider who is on this scooter now and holds another live
			// order, and never past the wait deadline.
			wait := false
			if !triedOwn {
				var deadline *time.Time
				err := tx.QueryRow(ctx, `SELECT o.wait_deadline FROM orders o WHERE o.id = $2 AND EXISTS (
						SELECT 1 FROM shifts sh
						JOIN riders r ON r.id = sh.rider_id AND r.status = 'active'
						JOIN people p ON p.id = r.person_id AND p.status = 'active'
						JOIN offers f ON f.rider_id = sh.rider_id AND f.released_at IS NULL
						WHERE sh.scooter_id = $1 AND sh.ended_at IS NULL)`, own, orderID).Scan(&deadline)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
				wait = err == nil && deadline != nil && now.Before(*deadline)
			}
			if wait {
				return nil // Its rider is finishing another order. Try again on the next pass.
			}
			return noScooter(ctx, tx, orderID, now)
		}
		if err := releaseHolds(ctx, tx, orderID, "system", nil); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM reservations WHERE order_id = $1 AND status = 'released'", orderID); err != nil {
			return err
		}
		for item, qty := range basket {
			if err := holdStock(ctx, tx, orderID, pick.ScooterID, item, qty, "system", nil); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, "UPDATE orders SET scooter_id = $2, updated_at = now() WHERE id = $1", orderID, pick.ScooterID); err != nil {
			return err
		}
		r, free, err := freeRider(ctx, tx, pick.ScooterID)
		if err != nil {
			return err
		}
		if !free {
			return errors.New("orders: the picked scooter has no free rider")
		}
		scooter, rider = pick.ScooterID, r
	}
	timeout, ok, err := pinned(ctx, tx, orderID, "offer_timeout_s")
	if err != nil {
		return err
	}
	if !ok {
		timeout = 30
	}
	token, err := newToken()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO offers (order_id, rider_id, scooter_id, fencing_token, expires_at) VALUES ($1, $2, $3, $4, $5)`,
		orderID, rider, scooter, token, now.Add(time.Duration(timeout)*time.Second)); err != nil {
		return err
	}
	// Hidden from the customer: the stage stays looking_for_rider, so seq does not move.
	if _, err := tx.Exec(ctx, "UPDATE orders SET state = 'OFFERED', updated_at = now() WHERE id = $1", orderID); err != nil {
		return err
	}
	if err := event(ctx, tx, orderID, "PAYMENT_SECURED", "OFFERED", "system", nil, "offered"); err != nil {
		return err
	}
	if err := outbox.Add(ctx, tx, "push.rider", map[string]any{"rider_id": rider, "kind": "offer"}); err != nil {
		return err
	}
	return bumpRider(ctx, tx, rider)
}

// noScooter tells the customer no scooter can take the order. They have no_scooter_choice_s to
// choose before an automatic refund. Its stock goes back.
func noScooter(ctx context.Context, tx pgx.Tx, orderID string, now time.Time) error {
	choose, ok, err := pinned(ctx, tx, orderID, "no_scooter_choice_s")
	if err != nil {
		return err
	}
	if !ok {
		choose = 300 // Not set at launch. The same as the maximum wait.
	}
	if _, err := tx.Exec(ctx, "UPDATE orders SET state = 'NO_SCOOTER', seq = seq + 1, wait_deadline = $2, updated_at = now() WHERE id = $1",
		orderID, now.Add(time.Duration(choose)*time.Second)); err != nil {
		return err
	}
	if err := releaseHolds(ctx, tx, orderID, "system", nil); err != nil {
		return err
	}
	return event(ctx, tx, orderID, "PAYMENT_SECURED", "NO_SCOOTER", "system", nil, "no scooter can take it")
}

// releaseOffer ends a rider's live offer on a locked order, which goes back to be offered again.
func releaseOffer(ctx context.Context, tx pgx.Tx, offerID, to, reason, actorType string, actorID any) error {
	var orderID, rider string
	if err := tx.QueryRow(ctx, `UPDATE offers SET state = $2, ended_reason = $3, final_state = 'OFFERED', released_at = now(), updated_at = now()
		WHERE id = $1 AND state = 'open' RETURNING order_id::text, rider_id::text`, offerID, to, reason).Scan(&orderID, &rider); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE orders SET state = 'PAYMENT_SECURED', updated_at = now() WHERE id = $1 AND state = 'OFFERED'", orderID); err != nil {
		return err
	}
	if err := event(ctx, tx, orderID, "OFFERED", "PAYMENT_SECURED", actorType, actorID, "offer "+to); err != nil {
		return err
	}
	return bumpRider(ctx, tx, rider)
}

// endOffers releases an ending order's live offer. An open offer shows as cancelled, and an
// accepted one keeps its state with the reason the rider no longer has it.
func endOffers(ctx context.Context, tx pgx.Tx, orderID, to string) error {
	reason := map[string]string{"CANCELLED": "cancelled", "TIMED_OUT": "timed_out", "FAILED": "failed"}[to]
	rows, err := tx.Query(ctx, `UPDATE offers SET state = CASE WHEN state = 'open' THEN 'cancelled' ELSE state END,
			ended_reason = $2, final_state = $3, released_at = now(), updated_at = now()
		WHERE order_id = $1 AND released_at IS NULL RETURNING rider_id::text`, orderID, reason, to)
	if err != nil {
		return err
	}
	riders, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, r := range riders {
		if err := bumpRider(ctx, tx, r); err != nil {
			return err
		}
	}
	return nil
}

// markPacked records an accepted order as packed, by the rider or the packing limit. It does not
// start the timer: that needs GPS.
func markPacked(ctx context.Context, tx pgx.Tx, orderID, actorType string, actorID any) error {
	var rider string
	err := tx.QueryRow(ctx, `UPDATE orders SET state = 'PACKED', packed_at = now(), updated_at = now()
		WHERE id = $1 AND state = 'RIDER_ACCEPTED' RETURNING rider_id::text`, orderID).Scan(&rider)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := event(ctx, tx, orderID, "RIDER_ACCEPTED", "PACKED", actorType, actorID, "packed"); err != nil {
		return err
	}
	return bumpRiderOf(ctx, tx, orderID)
}

// Dispatch is the background pass for riders: it expires unanswered offers, offers paid orders
// once their cancel window has closed, packs orders past the packing limit, refunds orders left at
// NO_SCOOTER past their deadline, and raises seq when the minimum wait ends. Each order is changed
// in its own transaction under its row lock.
func (s *Service) Dispatch(ctx context.Context, now time.Time) (int, error) {
	total := 0
	// A late-arrival review not decided in time lets the remedy go ahead.
	if _, err := s.Pool.Exec(ctx, "UPDATE arrival_reviews SET status = 'timed_out', decided_at = $1 WHERE status = 'open' AND due_at <= $1", now); err != nil {
		return 0, err
	}
	for _, step := range []struct {
		what, pick string
		act        func(context.Context, pgx.Tx, string) error
	}{
		{"expiring an offer", `SELECT f.id::text FROM offers f JOIN orders o ON o.id = f.order_id
			WHERE f.state = 'open' AND f.expires_at <= $2 AND NOT (f.id::text = ANY($1::text[]))
			ORDER BY f.expires_at, f.id LIMIT 1 FOR UPDATE OF o, f SKIP LOCKED`,
			func(ctx context.Context, tx pgx.Tx, id string) error {
				return releaseOffer(ctx, tx, id, "expired", "reassigned", "system", nil)
			}},
		{"offering an order", `SELECT id::text FROM orders WHERE state = 'PAYMENT_SECURED' AND cancel_until <= $2
			AND NOT (id::text = ANY($1::text[])) ORDER BY paid_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`,
			func(ctx context.Context, tx pgx.Tx, id string) error {
				var before string
				if err := tx.QueryRow(ctx, "SELECT state FROM orders WHERE id = $1", id).Scan(&before); err != nil {
					return err
				}
				if err := dispatch(ctx, tx, id, now); err != nil {
					return err
				}
				var after string
				if err := tx.QueryRow(ctx, "SELECT state FROM orders WHERE id = $1", id).Scan(&after); err != nil {
					return err
				}
				if after == before {
					return errUnchanged
				}
				return nil
			}},
		{"packing an order", `SELECT o.id::text FROM orders o JOIN zone_settings g ON g.id = o.global_settings
			LEFT JOIN zone_settings z ON z.id = o.zone_settings
			WHERE o.state = 'RIDER_ACCEPTED' AND o.rider_accepted_at + make_interval(secs => coalesce(z.packing_limit_s, g.packing_limit_s)) <= $2
			AND NOT (o.id::text = ANY($1::text[])) ORDER BY o.rider_accepted_at, o.id LIMIT 1 FOR UPDATE OF o SKIP LOCKED`,
			func(ctx context.Context, tx pgx.Tx, id string) error { return markPacked(ctx, tx, id, "system", nil) }},
		{"releasing the code after the signal was lost", `SELECT o.id::text FROM orders o JOIN zone_settings g ON g.id = o.global_settings
			LEFT JOIN zone_settings z ON z.id = o.zone_settings
			WHERE o.otp_released_at IS NULL AND o.handover_otp IS NOT NULL
				AND coalesce(o.last_ping_at, o.rider_accepted_at) + make_interval(secs => coalesce(z.signal_release_s, g.signal_release_s)) <= $2
				AND ((o.state = 'TIMER_RUNNING' AND o.timer_started_at + make_interval(secs => o.timer_length_s) <= $2)
					OR (o.state IN ('RIDER_ACCEPTED', 'PACKED') AND o.min_wait_until <= $2 AND o.wait_deadline <= $2))
				AND NOT (o.id::text = ANY($1::text[])) ORDER BY o.id LIMIT 1 FOR UPDATE OF o SKIP LOCKED`,
			func(ctx context.Context, tx pgx.Tx, id string) error {
				if _, err := tx.Exec(ctx, "UPDATE orders SET otp_released_at = $2, seq = seq + 1, updated_at = now() WHERE id = $1", id, now); err != nil {
					return err
				}
				return bumpRiderOf(ctx, tx, id)
			}},
		{"owing a credit for a missed timer", `SELECT o.id::text FROM orders o JOIN zone_settings g ON g.id = o.global_settings
			LEFT JOIN zone_settings z ON z.id = o.zone_settings
			WHERE o.timer_missed
				AND NOT EXISTS (SELECT 1 FROM remedies m WHERE m.order_id = o.id AND m.type = 'credit')
				AND NOT EXISTS (SELECT 1 FROM arrival_reviews a WHERE a.order_id = o.id AND a.status IN ('open', 'arrived_in_time'))
				AND o.state NOT IN ('CANCELLED', 'TIMED_OUT', 'FAILED')
				AND (o.last_live_ping_at >= $2::timestamptz - make_interval(secs => coalesce(z.signal_release_s, g.signal_release_s))
					OR $2::timestamptz >= o.timer_started_at + make_interval(secs => o.timer_length_s) + interval '10 minutes')
				AND NOT (o.id::text = ANY($1::text[])) ORDER BY o.timer_started_at, o.id LIMIT 1 FOR UPDATE OF o SKIP LOCKED`,
			func(ctx context.Context, tx pgx.Tx, id string) error { return creditForMissedTimer(ctx, tx, id) }},
		// The rider's on-time pings stop for reassign_after_s before the timer: the order goes to
		// another rider (decision 0011), unless the customer already has the code. Once the timer
		// runs the rider is near and the code is released after 30 s, so support is alerted instead.
		{"reassigning after no ping", `SELECT o.id::text FROM orders o JOIN zone_settings g ON g.id = o.global_settings
			LEFT JOIN zone_settings z ON z.id = o.zone_settings
			WHERE o.state IN ('RIDER_ACCEPTED', 'PACKED') AND o.otp_released_at IS NULL
				AND o.last_live_ping_at + make_interval(secs => coalesce(z.reassign_after_s, g.reassign_after_s)) <= $2
				AND NOT (o.id::text = ANY($1::text[])) ORDER BY o.id LIMIT 1 FOR UPDATE OF o SKIP LOCKED`,
			func(ctx context.Context, tx pgx.Tx, id string) error {
				if err := signalLost(ctx, tx, id); err != nil {
					return err
				}
				return rematch(ctx, tx, id, "no ping", now)
			}},
		{"alerting support after no ping", `SELECT o.id::text FROM orders o JOIN zone_settings g ON g.id = o.global_settings
			LEFT JOIN zone_settings z ON z.id = o.zone_settings
			WHERE o.state IN ('TIMER_RUNNING', 'ARRIVED')
				AND o.last_live_ping_at + make_interval(secs => coalesce(z.reassign_after_s, g.reassign_after_s)) <= $2
				AND NOT EXISTS (SELECT 1 FROM problem_reports p WHERE p.order_id = o.id AND p.rider_id = o.rider_id AND p.source = 'system' AND p.type = 'signal_lost')
				AND NOT (o.id::text = ANY($1::text[])) ORDER BY o.id LIMIT 1 FOR UPDATE OF o SKIP LOCKED`,
			func(ctx context.Context, tx pgx.Tx, id string) error { return signalLost(ctx, tx, id) }},
		{"ending an order with no scooter", `SELECT id::text FROM orders WHERE state = 'NO_SCOOTER' AND wait_deadline <= $2
			AND NOT (id::text = ANY($1::text[])) ORDER BY wait_deadline, id LIMIT 1 FOR UPDATE SKIP LOCKED`,
			func(ctx context.Context, tx pgx.Tx, id string) error {
				if err := end(ctx, tx, id, "NO_SCOOTER", "TIMED_OUT", "timed_out", "system", nil, "no scooter, and no choice made"); err != nil {
					return err
				}
				return refundPaid(ctx, tx, id, "no_rider_found")
			}},
	} {
		n, err := s.sweep(ctx, step.what, step.pick, now, step.act)
		total += n
		if err != nil {
			return total, err
		}
	}
	tag, err := s.Pool.Exec(ctx, `WITH due AS (
			SELECT id FROM orders WHERE state IN ('RIDER_ACCEPTED', 'PACKED', 'ARRIVED') AND NOT min_wait_until_seen AND min_wait_until <= $1
			FOR UPDATE SKIP LOCKED)
		UPDATE orders o SET seq = o.seq + 1, min_wait_until_seen = true, updated_at = now() FROM due WHERE o.id = due.id`, now)
	if err != nil {
		return total, err
	}
	total += int(tag.RowsAffected())
	// The timer's length passed before the perimeter: missed, once. The order carries on.
	tag, err = s.Pool.Exec(ctx, `WITH due AS (
			SELECT id FROM orders WHERE state = 'TIMER_RUNNING' AND NOT timer_missed AND timer_stopped_at IS NULL
				AND timer_started_at + make_interval(secs => timer_length_s) <= $1
			FOR UPDATE SKIP LOCKED)
		UPDATE orders o SET seq = o.seq + 1, timer_missed = true, updated_at = now() FROM due WHERE o.id = due.id`, now)
	if err != nil {
		return total, err
	}
	return total + int(tag.RowsAffected()), nil
}

// errUnchanged marks an order a pass looked at and left as it was, so it is not counted or
// picked again in the same pass.
var errUnchanged = errors.New("orders: unchanged")
