package console

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

// RolePermissions is decision 0015's table, and the contract's Permission schema.
var RolePermissions = map[string][]string{
	"admin": {"users.manage", "approvals.read", "approvals.decide", "riders.review", "flags.manage", "zones.read", "settings.read", "settings.request",
		"zones.kill_switch", "menu.manage", "gates.read", "gates.manage", "hubs.read", "hubs.manage", "hub_staff.manage", "audit.read", "reviews.read",
		"reviews.decide", "alerts.read"},
	"ops": {"approvals.read", "riders.review", "zones.read", "settings.read", "settings.request", "zones.kill_switch", "menu.manage", "gates.read",
		"gates.manage", "hubs.read", "hubs.manage", "hub_staff.manage", "reviews.read", "reviews.decide", "alerts.read"},
	"support":     {"zones.read", "gates.read", "reviews.read", "reviews.decide", "alerts.read"},
	"hub_manager": {"hubs.read", "hub_staff.manage"},
}

// User is the signed-in console user, as each call reads them.
type User struct {
	ID, LoginID, Name, Role string
	HubID                   *string
	MustChange              bool
}

type userKey struct{}

// UserFrom returns the console user Require put on the request.
func UserFrom(ctx context.Context) User {
	u, _ := ctx.Value(userKey{}).(User)
	return u
}

// Can says whether a role has a permission.
func Can(role, permission string) bool { return slices.Contains(RolePermissions[role], permission) }

func forbidden(w http.ResponseWriter, code, message, permission string) {
	httpserver.WriteJSON(w, http.StatusForbidden, map[string]any{"code": code, "message": message, "retryable": false,
		"missing_permission": permission, "server_time": serverTime()})
}

// Require reads the signed-in user afresh on every call and checks they may do this: an active
// account, a changed temporary password, and the permission. An empty permission only needs the
// first two; allowWithTemporary lets the calls that must work before the change through.
func (s *Service) Require(permission string, allowWithTemporary bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor, _ := identity.ActorFrom(r.Context())
			live, err := identity.SessionLive(r.Context(), s.Pool, actor)
			if err != nil {
				s.unavailable(w, r, err)
				return
			}
			if !live {
				httpserver.WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in again.", false)
				return
			}
			var u User
			var active bool
			err = s.Pool.QueryRow(r.Context(), "SELECT id::text, login_id, name, role, hub_id::text, must_change_password, active FROM console_users WHERE id = $1", actor.ID).
				Scan(&u.ID, &u.LoginID, &u.Name, &u.Role, &u.HubID, &u.MustChange, &active)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && !active) {
				httpserver.WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in again.", false)
				return
			}
			if err != nil {
				s.unavailable(w, r, err)
				return
			}
			if u.MustChange && !allowWithTemporary {
				forbidden(w, "password_change_required", "Change your temporary password first.", "password_changed")
				return
			}
			if permission != "" && !Can(u.Role, permission) {
				forbidden(w, "forbidden", "Your role cannot do this.", permission)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
		})
	}
}
