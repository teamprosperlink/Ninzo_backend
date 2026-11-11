package console

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

// loginPattern is an email address (owner, 2026-11-11) or the older ID form, lower case.
var loginPattern = regexp.MustCompile(`^([a-z0-9._-]{3,80}|[a-z0-9._%+-]{1,64}@[a-z0-9-]+(\.[a-z0-9-]+)*\.[a-z]{2,63})$`)

var roles = map[string]bool{"admin": true, "ops": true, "support": true, "hub_manager": true}

// refusal is a 409 the contract names.
type refusal struct{ code, message string }

func (r *refusal) Error() string { return "console: " + r.code }

var (
	errInvalid  = errors.New("console: invalid request")
	errNotFound = errors.New("console: not found")
)

// write answers a change: its reply, or the refusal the contract names.
func (s *Service) write(w http.ResponseWriter, r *http.Request, status int, reply map[string]any, err error) {
	var no *refusal
	var bad *badSetting
	switch {
	case err == nil:
		httpserver.WriteJSON(w, status, reply)
	case errors.As(err, &bad):
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_setting", bad.message, false)
	case errors.As(err, &no):
		httpserver.WriteError(w, http.StatusConflict, no.code, no.message, false)
	case errors.Is(err, errInvalid):
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Check the fields against the contract.", false)
	case errors.Is(err, errNotFound):
		httpserver.WriteError(w, http.StatusNotFound, "not_found", "Not found.", false)
	case errors.Is(err, identity.ErrSessionInvalid):
		httpserver.WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in again.", false)
	default:
		s.unavailable(w, r, err)
	}
}

// change runs a write in one transaction with the session check, and saves its reply for the
// Idempotency-Key.
func (s *Service) change(w http.ResponseWriter, r *http.Request, status int, act func(ctx context.Context, tx pgx.Tx, u User) (map[string]any, error)) {
	actor, _ := identity.ActorFrom(r.Context())
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		if err := identity.CheckSession(r.Context(), tx, actor); err != nil {
			return err
		}
		var err error
		if reply, err = act(r.Context(), tx, UserFrom(r.Context())); err != nil {
			return err
		}
		reply["server_time"] = serverTime()
		return idempotency.Save(r.Context(), tx, status, reply)
	})
	s.write(w, r, status, reply, err)
}

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// UserView is one console user (contract: ConsoleUser).
func UserView(ctx context.Context, q querier, id string) (map[string]any, string, error) {
	var login, name, role string
	var hub *string
	var active bool
	var enrolled, locked *time.Time
	var created time.Time
	err := q.QueryRow(ctx, "SELECT login_id, name, role, hub_id::text, active, totp_enrolled_at, locked_until, created_at FROM console_users WHERE id = $1", id).
		Scan(&login, &name, &role, &hub, &active, &enrolled, &locked, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", errNotFound
	}
	if err != nil {
		return nil, "", err
	}
	if locked != nil && !locked.After(time.Now()) {
		locked = nil
	}
	return map[string]any{"id": id, "login_id": login, "name": name, "role": role, "hub_id": hub, "active": active, "totp_enrolled": enrolled != nil,
		"locked_until": stamp(locked), "created_at": created.UTC().Format(time.RFC3339Nano)}, role, nil
}

func userReply(ctx context.Context, q querier, id string) (map[string]any, error) {
	v, role, err := UserView(ctx, q, id)
	if err != nil {
		return nil, err
	}
	perms := RolePermissions[role]
	if perms == nil {
		perms = []string{}
	}
	return map[string]any{"user": v, "permissions": perms}, nil
}

// MountUsers adds getConsoleUser and the user management calls.
func (s *Service) MountUsers(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.With(s.Require("", true)).Get("/admin/me", s.me)
		g.With(s.Require("users.manage", false)).Get("/admin/users", s.listUsers)
		g.With(s.Require("users.manage", false), idem).Post("/admin/users", s.createUser)
		g.With(s.Require("users.manage", false), idem).Patch("/admin/users/{user_id}", s.updateUser)
		g.With(s.Require("users.manage", false), idem).Post("/admin/users/{user_id}/reset", s.resetUser)
	})
}

func (s *Service) me(w http.ResponseWriter, r *http.Request) {
	reply, err := userReply(r.Context(), s.Pool, UserFrom(r.Context()).ID)
	if err == nil {
		reply["server_time"] = serverTime()
	}
	s.write(w, r, http.StatusOK, reply, err)
}

// Page reads cursor and limit. The cursor is the last row's sort time and id, encoded.
type Page struct {
	Limit    int
	At       time.Time
	ID       string
	HasAfter bool
}

// ReadPage parses cursor and limit, or fails with 400.
func ReadPage(r *http.Request) (Page, bool) {
	p := Page{Limit: 50}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			return p, false
		}
		p.Limit = n
	}
	if v := r.URL.Query().Get("cursor"); v != "" {
		raw, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil {
			return p, false
		}
		at, id, ok := strings.Cut(string(raw), "|")
		t, err := time.Parse(time.RFC3339Nano, at)
		if !ok || err != nil || !idPattern.MatchString(id) {
			return p, false
		}
		p.At, p.ID, p.HasAfter = t, id, true
	}
	return p, true
}

// Cursor encodes the last row's sort time and id.
func Cursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id))
}

func (s *Service) listUsers(w http.ResponseWriter, r *http.Request) {
	p, ok := ReadPage(r)
	if !ok {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Bad cursor or limit.", false)
		return
	}
	far := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	if !p.HasAfter {
		p.At, p.ID = far, "ffffffff-ffff-ffff-ffff-ffffffffffff"
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT id::text, created_at FROM console_users WHERE (created_at, id) < ($1, $2::uuid)
		ORDER BY created_at DESC, id DESC LIMIT $3`, p.At, p.ID, p.Limit+1)
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
	users := []map[string]any{}
	for _, x := range list {
		v, _, err := UserView(r.Context(), s.Pool, x.id)
		if err != nil {
			s.unavailable(w, r, err)
			return
		}
		users = append(users, v)
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"users": users, "next_cursor": next, "server_time": serverTime()})
}

func validReason(s string) bool {
	n := utf8.RuneCountInString(strings.TrimSpace(s))
	return n >= 3 && n <= 500
}

func checkHub(ctx context.Context, tx pgx.Tx, role string, hub *string) error {
	if (role == "hub_manager") != (hub != nil) {
		return errInvalid // A hub_manager has a hub; no other role does.
	}
	if hub == nil {
		return nil
	}
	if !idPattern.MatchString(*hub) {
		return errInvalid
	}
	var ok bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM hubs WHERE id = $1)", *hub).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return errInvalid
	}
	return nil
}

// CreateUser adds a console user with a temporary password, returned once.
func CreateUser(ctx context.Context, tx pgx.Tx, by, login, name, role string, hub *string) (string, string, error) {
	login = strings.ToLower(strings.TrimSpace(login))
	name = strings.TrimSpace(name)
	if len(login) > 254 || !loginPattern.MatchString(login) || name == "" || utf8.RuneCountInString(name) > 80 || !roles[role] {
		return "", "", errInvalid
	}
	if err := checkHub(ctx, tx, role, hub); err != nil {
		return "", "", err
	}
	temp, err := TemporaryPassword()
	if err != nil {
		return "", "", err
	}
	hash, err := HashPassword(temp)
	if err != nil {
		return "", "", err
	}
	var id string
	err = tx.QueryRow(ctx, "INSERT INTO console_users (login_id, name, role, hub_id, password_hash) VALUES ($1, $2, $3, $4, $5) RETURNING id::text",
		login, name, role, hub, hash).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return "", "", &refusal{"login_id_taken", "That email or login ID is taken."}
	}
	if err != nil {
		return "", "", err
	}
	actorType, actorID := identity.TypeConsole, by
	if by == "" {
		actorType = audit.ActorSystem
	}
	err = audit.Record(ctx, tx, audit.Event{ActorType: actorType, ActorID: actorID, Action: "console.user_added", EntityType: "console_user", EntityID: id,
		After: map[string]any{"login_id": login, "role": role}})
	return id, temp, err
}

func (s *Service) createUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LoginID string  `json:"login_id"`
		Name    string  `json:"name"`
		Role    string  `json:"role"`
		HubID   *string `json:"hub_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send login_id, name and role.", false)
		return
	}
	s.change(w, r, http.StatusCreated, func(ctx context.Context, tx pgx.Tx, u User) (map[string]any, error) {
		id, temp, err := CreateUser(ctx, tx, u.ID, body.LoginID, body.Name, body.Role, body.HubID)
		if err != nil {
			return nil, err
		}
		v, _, err := UserView(ctx, tx, id)
		return map[string]any{"user": v, "temporary_password": temp}, err
	})
}

func lockUser(ctx context.Context, tx pgx.Tx, id string) (string, bool, error) {
	if !idPattern.MatchString(id) {
		return "", false, errNotFound
	}
	var role string
	var active bool
	err := tx.QueryRow(ctx, "SELECT role, active FROM console_users WHERE id = $1 FOR UPDATE", id).Scan(&role, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, errNotFound
	}
	return role, active, err
}

// endSessions ends a user's sessions through the session lock, and their open sign-in challenges,
// so neither a refresh in flight nor a challenge from before survives.
func endSessions(ctx context.Context, tx pgx.Tx, sessions *identity.Sessions, id string) error {
	if err := sessions.EndAll(ctx, tx, identity.TypeConsole, id, ""); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "UPDATE console_challenges SET used_at = now() WHERE user_id = $1 AND used_at IS NULL", id)
	return err
}

func (s *Service) updateUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Role   *string         `json:"role"`
		HubID  json.RawMessage `json:"hub_id"`
		Active *bool           `json:"active"`
		Reason string          `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !validReason(body.Reason) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send the change and a reason.", false)
		return
	}
	id := chi.URLParam(r, "user_id")
	s.change(w, r, http.StatusOK, func(ctx context.Context, tx pgx.Tx, u User) (map[string]any, error) {
		// Admins are locked in id order, so two changes never deadlock and the last-admin check holds.
		if _, err := tx.Exec(ctx, "SELECT 1 FROM console_users WHERE role = 'admin' AND active ORDER BY id FOR UPDATE"); err != nil {
			return nil, err
		}
		role, active, err := lockUser(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		var hub *string
		if err := tx.QueryRow(ctx, "SELECT hub_id::text FROM console_users WHERE id = $1", id).Scan(&hub); err != nil {
			return nil, err
		}
		newRole, newActive, newHub := role, active, hub
		if body.Role != nil {
			if !roles[*body.Role] {
				return nil, errInvalid
			}
			newRole = *body.Role
		}
		if len(body.HubID) > 0 {
			if string(body.HubID) == "null" {
				newHub = nil
			} else {
				var h string
				if json.Unmarshal(body.HubID, &h) != nil {
					return nil, errInvalid
				}
				newHub = &h
			}
		}
		if newRole != "hub_manager" && len(body.HubID) == 0 {
			newHub = nil
		}
		if body.Active != nil {
			newActive = *body.Active
		}
		if err := checkHub(ctx, tx, newRole, newHub); err != nil {
			return nil, err
		}
		if id == u.ID && (newRole != "admin" || !newActive) {
			return nil, &refusal{"last_admin", "You cannot remove your own admin access."}
		}
		if role == "admin" && active && (newRole != "admin" || !newActive) {
			var others int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM console_users WHERE role = 'admin' AND active AND id <> $1", id).Scan(&others); err != nil {
				return nil, err
			}
			if others == 0 {
				return nil, &refusal{"last_admin", "This is the last active admin."}
			}
		}
		if _, err := tx.Exec(ctx, "UPDATE console_users SET role = $2, hub_id = $3, active = $4, updated_at = now() WHERE id = $1", id, newRole, newHub, newActive); err != nil {
			return nil, err
		}
		if !newActive || newRole != role {
			if err := endSessions(ctx, tx, s.Sessions, id); err != nil {
				return nil, err
			}
		}
		if !newActive {
			// A user who leaves takes their waiting changes with them.
			if _, err := tx.Exec(ctx, "UPDATE approvals SET status = 'cancelled', decided_by = $2, decided_at = now(), decision_reason = 'the requester was deactivated' WHERE requested_by = $1 AND status = 'pending'",
				id, u.ID); err != nil {
				return nil, err
			}
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: u.ID, Action: "console.user_changed", EntityType: "console_user", EntityID: id,
			Before: map[string]any{"role": role, "active": active}, After: map[string]any{"role": newRole, "active": newActive}, Reason: body.Reason}); err != nil {
			return nil, err
		}
		return userReply(ctx, tx, id)
	})
}

// ResetUser gives a user a new temporary password, clears their authenticator and lock, and ends
// their sessions. by is empty for the command-line tool.
func ResetUser(ctx context.Context, tx pgx.Tx, sessions *identity.Sessions, by, id, reason string) (string, error) {
	if _, _, err := lockUser(ctx, tx, id); err != nil {
		return "", err
	}
	temp, err := TemporaryPassword()
	if err != nil {
		return "", err
	}
	hash, err := HashPassword(temp)
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE console_users SET password_hash = $2, must_change_password = true, totp_secret = NULL, totp_enrolled_at = NULL, totp_last_step = 0,
		failed_logins = 0, failed_since = NULL, locked_until = NULL, updated_at = now() WHERE id = $1`, id, hash); err != nil {
		return "", err
	}
	if err := endSessions(ctx, tx, sessions, id); err != nil {
		return "", err
	}
	actorType := identity.TypeConsole
	if by == "" {
		actorType = audit.ActorSystem
	}
	return temp, audit.Record(ctx, tx, audit.Event{ActorType: actorType, ActorID: by, Action: "console.user_reset", EntityType: "console_user", EntityID: id, Reason: reason})
}

func (s *Service) resetUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !validReason(body.Reason) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send a reason.", false)
		return
	}
	id := chi.URLParam(r, "user_id")
	s.change(w, r, http.StatusOK, func(ctx context.Context, tx pgx.Tx, u User) (map[string]any, error) {
		temp, err := ResetUser(ctx, tx, s.Sessions, u.ID, id, body.Reason)
		if err != nil {
			return nil, err
		}
		v, _, err := UserView(ctx, tx, id)
		return map[string]any{"user": v, "temporary_password": temp}, err
	})
}
