// Package earnings records what riders earn and serves getEarnings. Pay is recorded in the same
// transaction as the event that earns it: a handover, or a finished relocation. Rates are zone
// settings; each row keeps the amount at the rate of the time.
package earnings

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

// Local is the zones' time zone. Every zone is in India for now; a day is a local day.
var Local = time.FixedZone("IST", 5*3600+1800)

// Day is the local date of t.
func Day(t time.Time) string { return t.In(Local).Format(time.DateOnly) }

// RecordOrder records the pay for a handed-over order, and the day's incentive once the rider's
// handed-over orders reach the zone's level. A repeat records nothing more.
func RecordOrder(ctx context.Context, tx pgx.Tx, rider, order string, at time.Time) error {
	var zone string
	if err := tx.QueryRow(ctx, "SELECT zone_id::text FROM orders WHERE id = $1", order).Scan(&zone); err != nil {
		return err
	}
	settings, err := geo.LoadSettings(ctx, tx, zone)
	if err != nil {
		return err
	}
	pay, _ := settings.Get("pay_per_order_paise")
	level, _ := settings.Get("incentive_orders")
	bonus, _ := settings.Get("incentive_paise")
	day := Day(at)
	tag, err := tx.Exec(ctx, `INSERT INTO earnings (rider_id, kind, order_id, amount_paise, day, earned_at) VALUES ($1, 'order', $2, $3, $4, $5)
		ON CONFLICT (order_id) WHERE kind = 'order' DO NOTHING`, rider, order, pay, day, at)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	// One rider's handovers are counted one at a time, so the count below is exact.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('earnings:' || $1, 0))", rider); err != nil {
		return err
	}
	var done int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM earnings WHERE rider_id = $1 AND day = $2 AND kind = 'order'", rider, day).Scan(&done); err != nil {
		return err
	}
	if done < level {
		return nil
	}
	_, err = tx.Exec(ctx, `INSERT INTO earnings (rider_id, kind, amount_paise, day, earned_at) VALUES ($1, 'incentive', $2, $3, $4)
		ON CONFLICT (rider_id, day) WHERE kind = 'incentive' DO NOTHING`, rider, bonus, day, at)
	return err
}

// RecordRelocation records the paid time of an accepted move once it has ended: from accepting
// until the rider arrived, an offer interrupted it, the shift ended, or the hour ran out, up to the
// zone's cap, paid by the started minute. A move never accepted, or not yet ended, records nothing,
// and neither does a repeat.
func RecordRelocation(ctx context.Context, tx pgx.Tx, relocation string) error {
	var rider, zone string
	var accepted, ended time.Time
	err := tx.QueryRow(ctx, `SELECT v.rider_id::text, n.zone_id::text, v.accepted_at, coalesce(v.arrived_at, v.state_changed_at) FROM relocations v
		JOIN staging_nodes n ON n.id = v.to_node_id
		WHERE v.id = $1 AND v.accepted_at IS NOT NULL AND v.state IN ('arrived', 'cancelled', 'expired')`, relocation).Scan(&rider, &zone, &accepted, &ended)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	settings, err := geo.LoadSettings(ctx, tx, zone)
	if err != nil {
		return err
	}
	rate, _ := settings.Get("relocation_paise_per_minute")
	most, _ := settings.Get("relocation_max_paid_s")
	seconds := max(0, min(int(ended.Sub(accepted)/time.Second), most))
	amount := (seconds + 59) / 60 * rate
	_, err = tx.Exec(ctx, `INSERT INTO earnings (rider_id, kind, relocation_id, amount_paise, paid_seconds, day, earned_at) VALUES ($1, 'relocation', $2, $3, $4, $5, $6)
		ON CONFLICT (relocation_id) WHERE kind = 'relocation' DO NOTHING`, rider, relocation, amount, seconds, Day(ended), ended)
	return err
}

// Service serves getEarnings.
type Service struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
}

// MountRider adds getEarnings.
func (s *Service) MountRider(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.Get("/riders/me/earnings", s.get)
	})
}

func money(paise int64) map[string]any {
	return map[string]any{"amount_minor": paise, "currency": "INR"}
}

func (s *Service) get(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	date := r.URL.Query().Get("date")
	if d, err := time.Parse(time.DateOnly, date); err != nil || d.Format(time.DateOnly) != date {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send date as YYYY-MM-DD.", false)
		return
	}
	live, err := identity.SessionLive(r.Context(), s.Pool, actor)
	if err == nil && !live {
		err = identity.ErrSessionInvalid
	}
	if err == nil {
		var status, person string
		err = s.Pool.QueryRow(r.Context(), "SELECT r.status, p.status FROM riders r JOIN people p ON p.id = r.person_id WHERE r.id = $1", actor.ID).Scan(&status, &person)
		if errors.Is(err, pgx.ErrNoRows) {
			err = identity.ErrSessionInvalid
		}
		// A suspended rider may still see what they earned.
		if err == nil && person != "active" {
			identity.WriteBlocked(w)
			return
		}
	}
	if errors.Is(err, identity.ErrSessionInvalid) {
		httpserver.WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in again.", false)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT kind, order_id::text, amount_paise, paid_seconds FROM earnings WHERE rider_id = $1 AND day = $2
		ORDER BY earned_at, id`, actor.ID, date)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	orders := []map[string]any{}
	var total, incentives, seconds, moving int64
	var kind string
	var order *string
	var amount, paid int64
	if _, err := pgx.ForEachRow(rows, []any{&kind, &order, &amount, &paid}, func() error {
		total += amount
		switch kind {
		case "order":
			orders = append(orders, map[string]any{"order_id": *order, "amount": money(amount)})
		case "incentive":
			incentives += amount
		case "relocation":
			seconds += paid
			moving += amount
		}
		return nil
	}); err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"date": date, "orders": len(orders), "order_earnings": orders, "incentives": money(incentives),
		"total": money(total), "repositioning": money(moving), "repositioning_seconds": seconds, "server_time": time.Now().UTC().Format(time.RFC3339Nano)})
}

func (s *Service) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.Error("earnings: request failed", "request_id", httpserver.RequestID(r.Context()), "error", err.Error())
	httpserver.WriteError(w, http.StatusInternalServerError, "internal_error", "Try again shortly.", true)
}
