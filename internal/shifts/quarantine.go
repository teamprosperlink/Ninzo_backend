package shifts

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

// riderQuarantineReasons are the reasons the rider app sends (the contract's enum). Hub staff have one
// more, contamination, that a rider never sends.
var riderQuarantineReasons = map[string]bool{"temperature": true, "damaged": true, "expired": true, "other": true}

// quarantineLot takes a lot off the rider's own scooter (Issue 235, part B3).
//
// It reuses the quarantine the hub uses (migration 0032): the lot's status becomes 'quarantined', which
// every stock count in the system already skips (availability, order holds, restock suggestions), so the
// food stops being offered in the same transaction. On top of that the units on the scooter that no
// order holds leave its stock: on_hand drops by them and the stock ledger gets one 'quarantine' entry, so
// the rider's stock list, the stock count and the shift summary stop counting them too, and the ledger
// still rebuilds scooter_stock. The scooter_stock row stays, so the lot is never loaded again (a scan says
// lot_quarantined).
//
// Units an order holds stay with that order, since taking them would break an order in flight. The lot is
// quarantined and its free units leave anyway (no new order can be promised any of it), and the answer is
// 409 lot_reserved, as the contract says, so the app tells the rider to report a problem on that order if
// those units are bad. Asking again changes nothing while the order holds them, and takes out whatever
// the order gave back once it releases them.
//
// A lot of another rider's scooter, one on no scooter, or one that does not exist is 404, so a rider
// cannot learn about another rider's stock. The other 409s are the contract's: no_open_shift and
// lot_not_on_scooter (the lot is the rider's own but is not on the scooter now: sold out, or still in
// their cassette). A lot this rider already quarantined answers 200 with the first time, wherever it is now.
//
// Locks: the shift, then the scooter's stock rows in lot order, as an order's hold takes them
// (availability.PickScooter), then the lot, FOR NO KEY UPDATE. An order's hold and a handover write
// the ledger and reservations while holding the stock rows, and those inserts take FOR KEY SHARE on the
// lot, which FOR NO KEY UPDATE does not conflict with. Taking the lot first would deadlock with them.
func (s *Service) quarantineLot(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body struct {
		Reason string `json:"reason"`
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !riderQuarantineReasons[body.Reason] || utf8.RuneCountInString(body.Note) > 500 {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send a reason: temperature, damaged, expired or other.", false)
		return
	}
	lot := chi.URLParam(r, "lot_id")
	var reply map[string]any
	// held is true when some of the lot is held for an order: the work is kept and the answer is 409.
	var held bool
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		held, reply = false, nil
		ctx := r.Context()
		if err := identity.CheckSession(ctx, tx, actor); err != nil {
			return err
		}
		if _, err := readRider(ctx, tx, actor.ID); err != nil {
			return err
		}
		_, scooter, open, err := openShift(ctx, tx, actor.ID)
		if err != nil {
			return err
		}
		if !open {
			return refuse("no_open_shift", "Start a shift first.")
		}
		if !idPattern.MatchString(lot) {
			return errNotFound
		}
		if _, err := tx.Exec(ctx, "SELECT 1 FROM scooter_stock WHERE scooter_id = $1 ORDER BY lot_id FOR UPDATE", scooter); err != nil {
			return err
		}
		var status string
		var by *string
		var at *time.Time
		err = tx.QueryRow(ctx, "SELECT status, quarantined_by_rider::text, quarantined_at FROM lots WHERE id = $1 FOR NO KEY UPDATE", lot).Scan(&status, &by, &at)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		var holder string
		var onHand, reserved int
		hasRow := true
		err = tx.QueryRow(ctx, "SELECT scooter_id::text, on_hand, reserved FROM scooter_stock WHERE lot_id = $1", lot).Scan(&holder, &onHand, &reserved)
		if errors.Is(err, pgx.ErrNoRows) {
			hasRow = false
		} else if err != nil {
			return err
		}
		// Asked again by the rider who did it: whatever happened to the scooter or the stock row since, it
		// is 200 with the first time. Only the lot's own quarantining rider gets this, so it tells nobody
		// anything about another rider's lot.
		mine := status == "quarantined" && by != nil && *by == actor.ID && at != nil
		now := s.clock().Truncate(time.Microsecond)
		if mine {
			now = *at
		} else {
			if !hasRow {
				// On no scooter. It is this rider's own only while it sits in their packed cassette.
				var inCassette bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM restock_lots x JOIN restock_requests q ON q.id = x.request_id
					WHERE x.lot_id = $1 AND x.released_at IS NULL AND q.rider_id = $2 AND q.state IN ('ready', 'partial'))`, lot, actor.ID).Scan(&inCassette); err != nil {
					return err
				}
				if inCassette {
					return refuse("lot_not_on_scooter", "That lot is not on your scooter.")
				}
				return errNotFound
			}
			if holder != scooter {
				return errNotFound
			}
			if onHand == 0 {
				return refuse("lot_not_on_scooter", "That lot is not on your scooter.")
			}
			if status == "ok" {
				var note any
				if body.Note != "" {
					note = body.Note
				}
				if _, err := tx.Exec(ctx, `UPDATE lots SET status = 'quarantined', quarantine_reason = $2, quarantine_note = $3,
					quarantined_by_rider = $4, quarantined_at = $5 WHERE id = $1`, lot, body.Reason, note, actor.ID, now); err != nil {
					return err
				}
			}
		}
		// The units no order holds leave the scooter. On a repeat there are some only if an order gave
		// its units back since.
		free := 0
		if hasRow && holder == scooter {
			free = onHand - reserved
			held = reserved > 0
		}
		if free > 0 {
			if _, err := tx.Exec(ctx, "UPDATE scooter_stock SET on_hand = on_hand - $3, updated_at = now() WHERE scooter_id = $1 AND lot_id = $2", scooter, lot, free); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO stock_ledger (scooter_id, lot_id, delta_on_hand, reason, actor_type, actor_id)
				VALUES ($1, $2, $3, 'quarantine', 'rider', $4)`, scooter, lot, -free, actor.ID); err != nil {
				return err
			}
		}
		if !mine || free > 0 {
			if err := bump(ctx, tx, actor.ID); err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeRider, ActorID: actor.ID, Action: "lot.quarantined", EntityType: "lot", EntityID: lot,
				After: map[string]any{"reason": body.Reason, "qty": free, "held_for_orders": reserved, "scooter_id": scooter, "by": "rider"}}); err != nil {
				return err
			}
		}
		reply = map[string]any{"lot_id": lot, "quarantined_at": stamp(now), "server_time": serverTime()}
		if held {
			// What was done stays. Only a 2xx reply is saved for a replay, so asking again with the same
			// key runs again, finds nothing more to take out, and answers 409 again.
			return nil
		}
		return idempotency.Save(ctx, tx, http.StatusOK, reply)
	})
	switch {
	case err == nil && held:
		httpserver.WriteError(w, http.StatusConflict, "lot_reserved", "Part of that lot is held for an order. The rest is out of sale. If the held units are bad, report a problem on that order.", false)
	case err != nil:
		s.fail(w, r, err)
	default:
		httpserver.WriteJSON(w, http.StatusOK, reply)
	}
}
