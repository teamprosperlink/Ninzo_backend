package shifts

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/notify"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

// defaultMaxBreak is used only when no settings row holds max_break_s (the console's value wins).
const defaultMaxBreak = 1800 * time.Second

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// startBreak pauses offers for the rider until they end the break or the console's longest break
// (max_break_s) passes. It is refused with a live order or an offer waiting for an answer.
func (s *Service) startBreak(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ctx := r.Context()
		if err := identity.CheckSession(ctx, tx, actor); err != nil {
			return err
		}
		rd, err := readRider(ctx, tx, actor.ID)
		if err != nil {
			return err
		}
		sh, _, open, err := openShift(ctx, tx, actor.ID)
		if err != nil {
			return err
		}
		if !open {
			return refuse("no_open_shift", "Start a shift first.")
		}
		if sh.BreakStartedAt != nil {
			return refuse("on_break", "You are already on a break.")
		}
		now := s.clock()
		// Read after the shift row is locked. A matcher that offers at the same moment either shows
		// here, or sees the break already running and passes this rider by.
		var accepted, offered bool
		if err := tx.QueryRow(ctx, `SELECT coalesce(bool_or(state = 'accepted'), false), coalesce(bool_or(state = 'open' AND expires_at > $2), false)
			FROM offers WHERE rider_id = $1 AND released_at IS NULL`, actor.ID, now).Scan(&accepted, &offered); err != nil {
			return err
		}
		if accepted {
			return refuse("order_in_progress", "Finish the current order first.")
		}
		if offered {
			return refuse("offer_open", "Answer the waiting offer first.")
		}
		zone := ""
		if rd.zoneID != nil {
			zone = *rd.zoneID
		}
		settings, err := geo.LoadSettings(ctx, tx, zone)
		if err != nil {
			return err
		}
		longest := defaultMaxBreak
		if v, ok := settings.Get("max_break_s"); ok {
			longest = time.Duration(v) * time.Second
		}
		ends := now.Add(longest)
		if _, err := tx.Exec(ctx, "INSERT INTO shift_breaks (shift_id, started_at) VALUES ($1, $2)", sh.ID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE shifts SET break_started_at = $2, break_ends_at = $3 WHERE id = $1", sh.ID, now, ends); err != nil {
			return err
		}
		if err := bump(ctx, tx, actor.ID); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeRider, ActorID: actor.ID, Action: "shift.break_started", EntityType: "shift", EntityID: sh.ID,
			After: map[string]any{"break_ends_at": stamp(ends)}}); err != nil {
			return err
		}
		sh.BreakStartedAt, sh.BreakEndsAt = optStamp(&now), optStamp(&ends)
		reply = map[string]any{"shift": sh, "server_time": serverTime()}
		return idempotency.Save(ctx, tx, http.StatusOK, reply)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, reply)
}

// endBreak lets offers come again. With no break running it returns the shift as it is.
func (s *Service) endBreak(w http.ResponseWriter, r *http.Request) {
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
		if sh.BreakStartedAt != nil {
			if err := finishBreak(ctx, tx, sh.ID, s.clock(), "rider"); err != nil {
				return err
			}
			if err := bump(ctx, tx, actor.ID); err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeRider, ActorID: actor.ID, Action: "shift.break_ended", EntityType: "shift", EntityID: sh.ID}); err != nil {
				return err
			}
			sh.BreakStartedAt, sh.BreakEndsAt = nil, nil
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

// finishBreak closes the shift's open break row and clears the shift's break fields. The caller
// holds the shift's row lock and raises the rider's seq.
func finishBreak(ctx context.Context, tx pgx.Tx, shift string, at time.Time, by string) error {
	if _, err := tx.Exec(ctx, "UPDATE shift_breaks SET ended_at = greatest($2, started_at), ended_by = $3 WHERE shift_id = $1 AND ended_at IS NULL", shift, at, by); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "UPDATE shifts SET break_started_at = NULL, break_ends_at = NULL WHERE id = $1", shift)
	return err
}

// EndDueBreaks ends every break whose time (break_ends_at) has passed, each in its own
// transaction, and tells the rider. It returns how many it ended. A break that fails is logged and
// skipped, never holding up the rest.
func (s *Service) EndDueBreaks(ctx context.Context, now time.Time) (int, error) {
	ended := 0
	skip := []string{}
	for i := 0; i < 500; i++ {
		var id string
		err := database.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
			var rider string
			var endsAt time.Time
			if err := tx.QueryRow(ctx, `SELECT id::text, rider_id::text, break_ends_at FROM shifts
				WHERE ended_at IS NULL AND break_ends_at <= $1 AND NOT (id::text = ANY($2::text[]))
				ORDER BY break_ends_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`, now, skip).Scan(&id, &rider, &endsAt); err != nil {
				return err
			}
			// The break ends when its time came, so break time is never counted past it.
			if err := finishBreak(ctx, tx, id, endsAt, "timeout"); err != nil {
				return err
			}
			if err := bump(ctx, tx, rider); err != nil {
				return err
			}
			if err := notify.Push(ctx, tx, rider, "shift", "Your break is over", "Offers can come to you again.", "", nil); err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Event{ActorType: audit.ActorSystem, Action: "shift.break_timed_out", EntityType: "shift", EntityID: id})
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows) && id == "":
			return ended, nil
		case err != nil:
			if id == "" || ctx.Err() != nil {
				return ended, err
			}
			s.Log.Error("shifts: ending a break failed", "shift_id", id, "error", err.Error())
			skip = append(skip, id)
		default:
			ended++
		}
	}
	return ended, nil
}

// Tick is one background pass: breaks past their time end, and notifications older than 30 days go.
func (s *Service) Tick(ctx context.Context, now time.Time) {
	if _, err := s.EndDueBreaks(ctx, now); err != nil && ctx.Err() == nil {
		s.Log.Error("shifts: ending breaks failed", "error", err.Error())
	}
	if _, err := notify.Purge(ctx, s.Pool); err != nil && ctx.Err() == nil {
		s.Log.Error("shifts: purging old notifications failed", "error", err.Error())
	}
}

// RunJobs runs Tick every so often until ctx ends. A panic is logged, never fatal.
func (s *Service) RunJobs(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			func() {
				defer func() {
					if p := recover(); p != nil {
						s.Log.Error("shifts: jobs panicked", "panic", fmt.Sprint(p))
					}
				}()
				s.Tick(ctx, s.clock())
			}()
		}
	}
}
