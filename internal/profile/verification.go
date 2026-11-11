package profile

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

// latestVerification is the rider's verification as shift start reads it: the newest row, a
// verified one first on a tie, and a verified one past its expiry counts as expired.
func latestVerification(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, rider string) (status string, reason *string, err error) {
	err = q.QueryRow(ctx, `SELECT CASE WHEN status = 'verified' AND expires_at <= now() THEN 'expired' ELSE status END, reason
		FROM rider_verifications WHERE rider_id = $1 ORDER BY created_at DESC, (status = 'verified'), id LIMIT 1`, rider).Scan(&status, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		// Nothing started yet: pending, with a reason the app can show.
		why := "not_started"
		return "pending", &why, nil
	}
	return status, reason, err
}

func (s *Service) getVerification(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	if !s.requireLive(w, r, actor) {
		return
	}
	status, reason, err := latestVerification(r.Context(), s.Pool, actor.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"status": status, "reason": reason, "server_time": serverTime()})
}

func (s *Service) startVerification(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body struct {
		Method string `json:"method"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || (body.Method != "digilocker" && body.Method != "manual_upload") {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send method: digilocker or manual_upload.", false)
		return
	}
	var reply map[string]any
	var conflict string
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		if err := identity.CheckSession(r.Context(), tx, actor); err != nil {
			return err
		}
		var personStatus string
		if err := tx.QueryRow(r.Context(), "SELECT p.status FROM riders r JOIN people p ON p.id = r.person_id WHERE r.id = $1 FOR UPDATE OF r", actor.ID).Scan(&personStatus); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errGone
			}
			return err
		}
		if personStatus != "active" {
			return errBlocked
		}
		status, reason, err := latestVerification(r.Context(), tx, actor.ID)
		if err != nil {
			return err
		}
		switch {
		case status == "verified":
			conflict = "already_verified"
			return nil
		case status == "pending" && (reason == nil || *reason != "not_started"):
			conflict = "verification_in_progress"
			return nil
		}
		var id string
		if err := tx.QueryRow(r.Context(), "INSERT INTO rider_verifications (rider_id, status, method) VALUES ($1, 'pending', $2) RETURNING id::text", actor.ID, body.Method).Scan(&id); err != nil {
			return err
		}
		if err := audit.Record(r.Context(), tx, audit.Event{ActorType: identity.TypeRider, ActorID: actor.ID, Action: "verification.started", EntityType: "rider_verification", EntityID: id,
			After: map[string]any{"method": body.Method}}); err != nil {
			return err
		}
		// No provider yet: nowhere to send the rider. Ops check the documents by hand.
		reply = map[string]any{"redirect_url": nil, "upload_url": nil, "server_time": time.Now().UTC().Format(time.RFC3339Nano)}
		return idempotency.Save(r.Context(), tx, http.StatusOK, reply)
	})
	switch {
	case err != nil:
		s.fail(w, r, err)
	case conflict == "already_verified":
		httpserver.WriteError(w, http.StatusConflict, "already_verified", "Your documents are already verified.", false)
	case conflict != "":
		httpserver.WriteError(w, http.StatusConflict, "verification_in_progress", "Your documents are being checked.", false)
	default:
		httpserver.WriteJSON(w, http.StatusOK, reply)
	}
}
