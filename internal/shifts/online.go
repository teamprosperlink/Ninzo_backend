package shifts

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

// maxPlannedEnd is how far past the shift's start the planned end may be (the contract's 16 hours).
const maxPlannedEnd = 16 * time.Hour

// goOnline records that the rider is online at their spot, the first time only. It records and
// nothing else: it never starts or stops offers, and asking again changes nothing.
func (s *Service) goOnline(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ctx := r.Context()
		if err := identity.CheckSession(ctx, tx, actor); err != nil {
			return err
		}
		if _, err := readRider(ctx, tx, actor.ID); err != nil {
			return err
		}
		sh, _, open, err := openShift(ctx, tx, actor.ID)
		if err != nil {
			return err
		}
		if !open {
			return refuse("no_open_shift", "Start a shift first.")
		}
		if sh.OnlineAt == nil {
			// Whole microseconds, as the database keeps them, so the reply says what is stored.
			at := s.clock().Truncate(time.Microsecond)
			if _, err := tx.Exec(ctx, "UPDATE shifts SET online_at = $2 WHERE id = $1 AND online_at IS NULL", sh.ID, at); err != nil {
				return err
			}
			sh.OnlineAt = optStamp(&at)
			if err := bump(ctx, tx, actor.ID); err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeRider, ActorID: actor.ID, Action: "shift.online", EntityType: "shift", EntityID: sh.ID,
				After: map[string]any{"online_at": stamp(at)}}); err != nil {
				return err
			}
		}
		reply = map[string]any{"shift": sh, "server_time": serverTime()}
		return idempotency.Save(ctx, tx, http.StatusOK, reply)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, reply)
}

// setPlannedEnd records the end the rider says. It must be in the future and within 16 hours of the
// shift's start. Shifts are open-ended: nothing ends the shift at that time and no reminder is sent.
func (s *Service) setPlannedEnd(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body struct {
		PlannedEndAt json.RawMessage `json:"planned_end_at"`
	}
	bad := func() {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "planned_end_at must be a time in the future, within 16 hours of the shift's start.", false)
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&body); err != nil || dec.More() {
		bad()
		return
	}
	var raw string
	var end time.Time
	if err := json.Unmarshal(body.PlannedEndAt, &raw); err != nil {
		bad() // Missing, null or not text.
		return
	}
	end, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		bad()
		return
	}
	end = end.UTC().Truncate(time.Microsecond)
	var reply map[string]any
	invalid := false
	err = database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ctx := r.Context()
		if err := identity.CheckSession(ctx, tx, actor); err != nil {
			return err
		}
		if _, err := readRider(ctx, tx, actor.ID); err != nil {
			return err
		}
		sh, _, open, err := openShift(ctx, tx, actor.ID)
		if err != nil {
			return err
		}
		if !open {
			return refuse("no_open_shift", "Start a shift first.")
		}
		started, err := time.Parse(time.RFC3339Nano, sh.StartedAt)
		if err != nil {
			return err
		}
		if !end.After(s.clock()) || end.After(started.Add(maxPlannedEnd)) {
			invalid = true
			return nil
		}
		if sh.PlannedEndAt == nil || *sh.PlannedEndAt != stamp(end) {
			if _, err := tx.Exec(ctx, "UPDATE shifts SET planned_end_at = $2 WHERE id = $1", sh.ID, end); err != nil {
				return err
			}
			if err := bump(ctx, tx, actor.ID); err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeRider, ActorID: actor.ID, Action: "shift.planned_end_set", EntityType: "shift", EntityID: sh.ID,
				Before: map[string]any{"planned_end_at": sh.PlannedEndAt}, After: map[string]any{"planned_end_at": stamp(end)}}); err != nil {
				return err
			}
			sh.PlannedEndAt = optStamp(&end)
		}
		reply = map[string]any{"shift": sh, "server_time": serverTime()}
		return idempotency.Save(ctx, tx, http.StatusOK, reply)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if invalid {
		bad()
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, reply)
}
