// Package hub serves the partner (hub) app: the signed-in staff member, and from part C the hub's
// restock queue. Its contract is contracts/partner/openapi.yaml.
package hub

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

var errBlocked = errors.New("hub: blocked")

// Service serves the hub endpoints.
type Service struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
}

func serverTime() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// Staff is the signed-in staff member, with their hub.
type Staff struct {
	ID, Role, HubID, HubName string
	Name                     *string
	HubOpen                  bool
}

// MountStaff adds getStaff.
func (s *Service) MountStaff(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.Get("/hub/me", s.me)
	})
}

// staff checks the session is live and the staff member may act, and returns them.
func (s *Service) staff(r *http.Request, a identity.Actor) (Staff, error) {
	live, err := identity.SessionLive(r.Context(), s.Pool, a)
	if err != nil {
		return Staff{}, err
	}
	if !live {
		return Staff{}, identity.ErrSessionInvalid
	}
	var st Staff
	var status, person string
	err = s.Pool.QueryRow(r.Context(), `SELECT s.id::text, s.name, s.role, s.status, p.status, h.id::text, h.name, h.status = 'active'
		FROM hub_staff s JOIN people p ON p.id = s.person_id JOIN hubs h ON h.id = s.hub_id WHERE s.id = $1`, a.ID).
		Scan(&st.ID, &st.Name, &st.Role, &status, &person, &st.HubID, &st.HubName, &st.HubOpen)
	if errors.Is(err, pgx.ErrNoRows) {
		return Staff{}, identity.ErrSessionInvalid
	}
	if err == nil && (person != "active" || status != "active") {
		err = errBlocked
	}
	return st, err
}

func (s *Service) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, identity.ErrSessionInvalid):
		httpserver.WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in again.", false)
	case errors.Is(err, errBlocked):
		identity.WriteBlocked(w)
	default:
		s.Log.Error("hub: request failed", "request_id", httpserver.RequestID(r.Context()), "error", err.Error())
		httpserver.WriteError(w, http.StatusInternalServerError, "internal_error", "Try again shortly.", true)
	}
}

func (s *Service) me(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	st, err := s.staff(r, actor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"staff": map[string]any{
		"id": st.ID, "name": st.Name, "role": st.Role, "hub": map[string]any{"id": st.HubID, "name": st.HubName}, "hub_open": st.HubOpen,
	}, "server_time": serverTime()})
}
