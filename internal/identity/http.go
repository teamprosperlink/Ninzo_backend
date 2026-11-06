package identity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

type actorKey struct{}

// ActorFrom returns the signed-in actor, set by RequireActor.
func ActorFrom(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(Actor)
	return a, ok
}

// ActorID is what the idempotency middleware needs: it runs after RequireActor.
func ActorID(r *http.Request) (string, bool) {
	a, ok := ActorFrom(r.Context())
	return a.ID, ok
}

// RequireActor lets through only a valid access token for this API's user type.
func RequireActor(signer Signer, actorType string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !found || token == "" {
				httpserver.WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in first.", false)
				return
			}
			actor, err := signer.Verify(token, actorType)
			switch {
			case errors.Is(err, ErrTokenExpired):
				httpserver.WriteError(w, http.StatusUnauthorized, "token_expired", "The access token has expired.", false)
			case err != nil:
				httpserver.WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in first.", false)
			default:
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, actor)))
			}
		})
	}
}

// MountSessions adds refreshToken and signOut for one API's user type.
func MountSessions(r chi.Router, s *Sessions, actorType string) {
	r.Post("/auth/refresh", s.handleRefresh(actorType))
	r.With(RequireActor(s.Signer, actorType)).Post("/auth/signout", s.handleSignOut())
}

func serverTime() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (s *Sessions) handleRefresh(actorType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.RefreshToken == "" {
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send refresh_token.", false)
			return
		}
		pair, _, err := s.Refresh(r.Context(), body.RefreshToken, actorType)
		switch {
		case errors.Is(err, ErrSessionInvalid):
			httpserver.WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in again.", false)
		case err != nil:
			s.Log.Error("identity: refresh failed", "request_id", httpserver.RequestID(r.Context()), "error", err.Error())
			w.Header().Set("Retry-After", "5")
			httpserver.WriteError(w, http.StatusServiceUnavailable, "service_unavailable", "Try again shortly.", true)
		default:
			httpserver.WriteJSON(w, http.StatusOK, map[string]any{
				"access_token":  pair.AccessToken,
				"refresh_token": pair.RefreshToken,
				"expires_in_s":  pair.ExpiresInS,
				"server_time":   serverTime(),
			})
		}
	}
}

func (s *Sessions) handleSignOut() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := ActorFrom(r.Context())
		if !ok {
			httpserver.WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in first.", false)
			return
		}
		if err := s.SignOut(r.Context(), actor); err != nil {
			s.Log.Error("identity: sign-out failed", "request_id", httpserver.RequestID(r.Context()), "error", err.Error())
			w.Header().Set("Retry-After", "5")
			httpserver.WriteError(w, http.StatusServiceUnavailable, "service_unavailable", "Try again shortly.", true)
			return
		}
		httpserver.WriteJSON(w, http.StatusOK, map[string]any{"server_time": serverTime()})
	}
}
