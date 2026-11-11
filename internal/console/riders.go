package console

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
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
	"github.com/teamprosperlink/Ninzo_backend/internal/shifts"
)

// MountRiders adds listRiderReviews and decideRider.
func (s *Service) MountRiders(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.With(s.Require("riders.review", false)).Get("/admin/riders/review", s.listRiders)
		g.With(s.Require("riders.review", false), idem).Post("/admin/riders/{rider_id}/decision", s.decideRider)
	})
}

// RiderView is one rider as the review queue shows them (contract: RiderReview): the last four
// digits of the phone only.
func RiderView(ctx context.Context, q querier, id string) (map[string]any, error) {
	var name, zone *string
	var phone, status string
	var verification *string
	var applied time.Time
	err := q.QueryRow(ctx, `SELECT r.name, p.phone, r.status, r.zone_id::text, r.created_at,
			(SELECT CASE WHEN v.status = 'verified' AND v.expires_at <= now() THEN 'expired' ELSE v.status END FROM rider_verifications v WHERE v.rider_id = r.id ORDER BY v.created_at DESC, (v.status = 'verified'), v.id LIMIT 1)
		FROM riders r JOIN people p ON p.id = r.person_id WHERE r.id = $1`, id).Scan(&name, &phone, &status, &zone, &applied, &verification)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	v := "none"
	if verification != nil {
		v = *verification
	}
	last4 := phone
	if len(phone) > 4 {
		last4 = phone[len(phone)-4:]
	}
	return map[string]any{"rider_id": id, "name": name, "phone_last4": last4, "status": status, "verification": v, "zone_id": zone,
		"applied_at": applied.UTC().Format(time.RFC3339Nano)}, nil
}

// listRiders is the queue: applicants, and riders whose latest verification failed or expired,
// oldest first.
func (s *Service) listRiders(w http.ResponseWriter, r *http.Request) {
	p, ok := ReadPage(r)
	if !ok {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Bad cursor or limit.", false)
		return
	}
	if !p.HasAfter {
		p.At, p.ID = time.Time{}, "00000000-0000-0000-0000-000000000000"
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT r.id::text, r.created_at FROM riders r
		WHERE (r.status = 'applicant' OR (r.status = 'active' AND (SELECT CASE WHEN v.status = 'verified' AND v.expires_at <= now() THEN 'expired' ELSE v.status END FROM rider_verifications v WHERE v.rider_id = r.id ORDER BY v.created_at DESC, (v.status = 'verified'), v.id LIMIT 1) IN ('failed', 'expired')))
			AND (r.created_at, r.id) > ($1, $2::uuid)
		ORDER BY r.created_at, r.id LIMIT $3`, p.At, p.ID, p.Limit+1)
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	type row struct {
		id string
		at time.Time
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.id, &x.at)
	})
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	var next any
	if len(list) > p.Limit {
		list = list[:p.Limit]
		next = Cursor(list[len(list)-1].at, list[len(list)-1].id)
	}
	out := []map[string]any{}
	for _, x := range list {
		v, err := RiderView(r.Context(), s.Pool, x.id)
		if err != nil {
			s.unavailable(w, r, err)
			return
		}
		out = append(out, v)
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"riders": out, "next_cursor": next, "server_time": serverTime()})
}

// DecideRider approves, suspends or reinstates a rider. Suspending ends their sessions and any
// open shift, as the rider ending it would.
func DecideRider(ctx context.Context, tx pgx.Tx, sessions *identity.Sessions, u User, rider, decision, zone, reason string) error {
	if !idPattern.MatchString(rider) {
		return errNotFound
	}
	// A suspension ends the rider's sessions first: that takes the rider's session lock, which the
	// rider's own writes take before any row, so the two never wait on each other in a circle.
	if decision == "suspend" {
		if err := sessions.EndAll(ctx, tx, identity.TypeRider, rider, ""); err != nil {
			return err
		}
	}
	var status string
	var current *string
	err := tx.QueryRow(ctx, "SELECT status, zone_id::text FROM riders WHERE id = $1 FOR NO KEY UPDATE", rider).Scan(&status, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	var verified bool
	if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT v.status = 'verified' AND (v.expires_at IS NULL OR v.expires_at > now()) FROM rider_verifications v
		WHERE v.rider_id = $1 ORDER BY v.created_at DESC, (v.status = 'verified'), v.id LIMIT 1), false)`, rider).Scan(&verified); err != nil {
		return err
	}
	to := status
	newZone := current
	switch decision {
	case "approve", "reinstate":
		if (decision == "approve") != (status == "applicant") || (decision == "reinstate" && status != "suspended") {
			return &refusal{"not_allowed", "That decision does not fit the rider's status."}
		}
		if decision == "approve" && zone == "" {
			return errInvalid // The contract: approving needs zone_id.
		}
		if zone != "" {
			if !idPattern.MatchString(zone) {
				return errInvalid
			}
			var ok bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM zones WHERE id = $1 AND status = 'active')", zone).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return errInvalid
			}
			newZone = &zone
		}
		if newZone == nil {
			return errInvalid // A working rider has a zone.
		}
		if !verified {
			return &refusal{"not_verified", "The rider's verification is not complete."}
		}
		to = "active"
	case "suspend":
		if status == "suspended" {
			return &refusal{"not_allowed", "The rider is already suspended."}
		}
		to = "suspended"
		if _, _, err := shifts.EndShiftTx(ctx, tx, rider, identity.TypeConsole, u.ID); err != nil {
			if shifts.IsOrderInProgress(err) {
				return &refusal{"order_in_progress", "The rider is carrying an order. Suspend them once it is handed over."}
			}
			return err
		}
	default:
		return errInvalid
	}
	if _, err := tx.Exec(ctx, "UPDATE riders SET status = $2, zone_id = $3, updated_at = now() WHERE id = $1", rider, to, newZone); err != nil {
		return err
	}
	return audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: u.ID, Action: "rider." + decision, EntityType: "rider", EntityID: rider,
		Before: map[string]any{"status": status, "zone_id": current}, After: map[string]any{"status": to, "zone_id": newZone}, Reason: reason})
}

func (s *Service) decideRider(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Decision string `json:"decision"`
		ZoneID   string `json:"zone_id"`
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !validReason(body.Reason) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send decision and a reason.", false)
		return
	}
	rider := chi.URLParam(r, "rider_id")
	s.change(w, r, http.StatusOK, func(ctx context.Context, tx pgx.Tx, u User) (map[string]any, error) {
		if err := DecideRider(ctx, tx, s.Sessions, u, rider, body.Decision, body.ZoneID, body.Reason); err != nil {
			return nil, err
		}
		v, err := RiderView(ctx, tx, rider)
		return map[string]any{"rider": v}, err
	})
}
