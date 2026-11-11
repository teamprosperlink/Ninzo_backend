// Package profile serves what customers and riders read and change about themselves: their profile
// and the devices that get their push notifications. Each is a separate component of its API.
package profile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

const (
	// maxDevices is per person and app. A new one past this replaces the one unused for longest.
	maxDevices     = 10
	maxPushToken   = 4096
	maxNameRunes   = 80
	maxEmailLength = 254
)

var idPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// errBlocked means this person or rider may not use the API.
var errBlocked = errors.New("profile: blocked")

// errGone means the access token outlived its profile.
var errGone = errors.New("profile: no such profile")

// Service holds the database and logger.
type Service struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
}

// Middleware is what guards a signed-in route.
type Middleware = func(http.Handler) http.Handler

// MountCustomer adds getMe, updateMe, registerDevice and removeDevice.
func (s *Service) MountCustomer(r chi.Router, signedIn ...Middleware) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.Get("/me", s.getCustomer)
		g.With(idem).Patch("/me", s.updateCustomer)
		g.With(idem).Post("/me/devices", s.registerDevice)
		g.With(idem).Delete("/me/devices/{device_id}", s.removeDevice)
	})
}

// MountRider adds getRider, registerRiderDevice, removeRiderDevice, getVerification and
// startVerification. Until DigiLocker, starting only records a pending request; ops check the
// documents by hand and mark them (cmd/riders).
func (s *Service) MountRider(r chi.Router, signedIn ...Middleware) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.Get("/riders/me", s.getRider)
		g.With(idem).Post("/riders/me/devices", s.registerDevice)
		g.With(idem).Delete("/riders/me/devices/{device_id}", s.removeDevice)
		g.Get("/riders/me/verification", s.getVerification)
		g.With(idem).Post("/riders/me/verification/start", s.startVerification)
	})
}

func serverTime() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// fail sends the reply for an error, and logs only the unexpected.
func (s *Service) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, identity.ErrSessionInvalid), errors.Is(err, errGone):
		httpserver.WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in again.", false)
	case errors.Is(err, errBlocked):
		identity.WriteBlocked(w)
	default:
		s.Log.Error("profile: request failed", "request_id", httpserver.RequestID(r.Context()), "error", err.Error())
		httpserver.WriteError(w, http.StatusInternalServerError, "internal_error", "Try again shortly.", true)
	}
}

// requireLive stops an access token outliving its session from reading personal data, such as after
// someone is removed from an account by a change of sign-in method.
func (s *Service) requireLive(w http.ResponseWriter, r *http.Request, a identity.Actor) bool {
	live, err := identity.SessionLive(r.Context(), s.Pool, a)
	if err != nil {
		s.fail(w, r, err)
		return false
	}
	if !live {
		s.fail(w, r, identity.ErrSessionInvalid)
		return false
	}
	return true
}

// ---------- Customer

type customerRow struct {
	id, status, method string
	phone, name, email *string
}

func readCustomer(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id string) (customerRow, error) {
	var c customerRow
	err := q.QueryRow(ctx, `SELECT c.id::text, p.status, c.sign_in_method, p.phone, c.name, c.email
		FROM customers c JOIN people p ON p.id = c.person_id WHERE c.id = $1`, id).
		Scan(&c.id, &c.status, &c.method, &c.phone, &c.name, &c.email)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, errGone
	}
	if err == nil && c.status != "active" {
		return c, errBlocked
	}
	return c, err
}

func (c customerRow) reply() map[string]any {
	return map[string]any{
		"customer": map[string]any{
			"id": c.id, "phone": c.phone, "name": c.name, "email": c.email, "sign_in_method": c.method,
		},
		"server_time": serverTime(),
	}
}

func (s *Service) getCustomer(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	if !s.requireLive(w, r, actor) {
		return
	}
	c, err := readCustomer(r.Context(), s.Pool, actor.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, c.reply())
}

// cleanName returns a name fit to store and to show to a rider.
func cleanName(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if !utf8.ValidString(name) || name == "" || utf8.RuneCountInString(name) > maxNameRunes {
		return "", false
	}
	if strings.ContainsFunc(name, func(c rune) bool { return unicode.IsControl(c) || isBidiControl(c) }) {
		return "", false
	}
	// Joiners are needed in Indian scripts, so they stay, but a name must hold something visible.
	return name, strings.ContainsFunc(name, func(c rune) bool { return unicode.IsLetter(c) || unicode.IsNumber(c) })
}

// isBidiControl finds what can make a name read backwards on a rider's screen.
func isBidiControl(c rune) bool {
	return (c >= 0x202A && c <= 0x202E) || (c >= 0x2066 && c <= 0x2069)
}

// cleanEmail accepts one plain address. It is contact details only, and never signs anyone in.
func cleanEmail(raw string) (string, bool) {
	email := strings.TrimSpace(raw)
	if email == "" || len(email) > maxEmailLength || strings.ContainsFunc(email, unicode.IsControl) {
		return "", false
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return "", false
	}
	return email, true
}

func (s *Service) updateCustomer(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "The body must be JSON.", false)
		return
	}
	rawName, hasName := body["name"]
	rawEmail, hasEmail := body["email"]
	if !hasName && !hasEmail {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send name or email.", false)
		return
	}
	var name string
	if hasName {
		var v string
		ok := json.Unmarshal(rawName, &v) == nil && string(rawName) != "null"
		if ok {
			name, ok = cleanName(v)
		}
		if !ok {
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "That name is not valid.", false)
			return
		}
	}
	var email *string // Null clears it.
	if hasEmail && string(rawEmail) != "null" {
		var v string
		ok := json.Unmarshal(rawEmail, &v) == nil
		if ok {
			v, ok = cleanEmail(v)
		}
		if !ok {
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "That email is not valid.", false)
			return
		}
		email = &v
	}

	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		if err := identity.CheckSession(r.Context(), tx, actor); err != nil {
			return err
		}
		if _, err := readCustomer(r.Context(), tx, actor.ID); err != nil {
			return err
		}
		var changed []string
		if hasName {
			if _, err := tx.Exec(r.Context(), "UPDATE customers SET name = $2, updated_at = now() WHERE id = $1", actor.ID, name); err != nil {
				return err
			}
			changed = append(changed, "name")
		}
		if hasEmail {
			if _, err := tx.Exec(r.Context(), "UPDATE customers SET email = $2, updated_at = now() WHERE id = $1", actor.ID, email); err != nil {
				return err
			}
			changed = append(changed, "email")
		}
		// Which fields, and not their values: audit rows are kept for good, and these are personal.
		if err := audit.Record(r.Context(), tx, audit.Event{
			ActorType: identity.TypeCustomer, ActorID: actor.ID, Action: "customer.updated", EntityType: "customer", EntityID: actor.ID,
			After: map[string]any{"fields": changed},
		}); err != nil {
			return err
		}
		c, err := readCustomer(r.Context(), tx, actor.ID)
		if err != nil {
			return err
		}
		reply = c.reply()
		return idempotency.Save(r.Context(), tx, http.StatusOK, reply)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, reply)
}

// ---------- Rider

func (s *Service) getRider(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	if !s.requireLive(w, r, actor) {
		return
	}
	var id, status, personStatus string
	var name, zone *string
	var created time.Time
	var approved *time.Time
	// approved_at is the newest approval in the audit trail: the command line writes rider.approved,
	// the console writes rider.approve. Null before the first approval.
	err := s.Pool.QueryRow(r.Context(), `SELECT r.id::text, r.name, r.status, p.status, r.zone_id::text, r.created_at,
			(SELECT max(a.created_at) FROM audit_events a WHERE a.entity_type = 'rider' AND a.entity_id = r.id::text
				AND a.action IN ('rider.approved', 'rider.approve'))
		FROM riders r JOIN people p ON p.id = r.person_id WHERE r.id = $1`, actor.ID).Scan(&id, &name, &status, &personStatus, &zone, &created, &approved)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		err = errGone
	case err == nil && personStatus != "active":
		err = errBlocked
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// A suspended rider can still read this, so the app can say why they cannot work.
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{
		"rider": map[string]any{"id": id, "name": name, "status": status, "zone_id": zone,
			"created_at": created.UTC().Format(time.RFC3339Nano), "approved_at": optionalStamp(approved)},
		"server_time": serverTime(),
	})
}

// ---------- Devices, for customers and riders alike

// requireActive refuses a person who is blocked, or a rider who is suspended.
func requireActive(ctx context.Context, tx pgx.Tx, a identity.Actor) error {
	var query string
	switch a.Type {
	case identity.TypeCustomer:
		query = `SELECT p.status = 'active' FROM customers c JOIN people p ON p.id = c.person_id WHERE c.id = $1`
	case identity.TypeRider:
		query = `SELECT p.status = 'active' AND r.status <> 'suspended' FROM riders r JOIN people p ON p.id = r.person_id WHERE r.id = $1`
	case identity.TypeStaff:
		query = `SELECT p.status = 'active' AND s.status = 'active' FROM hub_staff s JOIN people p ON p.id = s.person_id WHERE s.id = $1`
	default:
		return fmt.Errorf("profile: unknown user type %q", a.Type)
	}
	var active bool
	err := tx.QueryRow(ctx, query, a.ID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return errGone
	}
	if err == nil && !active {
		return errBlocked
	}
	return err
}

func (s *Service) registerDevice(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body struct {
		Platform  string `json:"platform"`
		PushToken string `json:"push_token"`
	}
	err := json.NewDecoder(r.Body).Decode(&body)
	if err != nil || (body.Platform != "android" && body.Platform != "ios") || body.PushToken == "" ||
		len(body.PushToken) > maxPushToken || strings.ContainsFunc(body.PushToken, unicode.IsControl) || !utf8.ValidString(body.PushToken) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send platform and push_token.", false)
		return
	}
	var reply map[string]any
	err = database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		// An ended session must not add a device, or a stranger removed from an account would keep
		// getting its notifications.
		if err := identity.CheckSession(r.Context(), tx, actor); err != nil {
			return err
		}
		if err := requireActive(r.Context(), tx, actor); err != nil {
			return err
		}
		var id string
		// One phone, one owner: whoever signed in on it last gets its notifications.
		err := tx.QueryRow(r.Context(), `INSERT INTO devices (actor_type, actor_id, platform, push_token) VALUES ($1, $2, $3, $4)
			ON CONFLICT (push_token) DO UPDATE SET actor_type = EXCLUDED.actor_type, actor_id = EXCLUDED.actor_id,
				platform = EXCLUDED.platform, updated_at = now()
			RETURNING id::text`, actor.Type, actor.ID, body.Platform, body.PushToken).Scan(&id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `DELETE FROM devices WHERE id IN (
				SELECT id FROM devices WHERE actor_type = $1 AND actor_id = $2 ORDER BY updated_at DESC, id OFFSET $3)`,
			actor.Type, actor.ID, maxDevices); err != nil {
			return err
		}
		reply = map[string]any{"device_id": id, "server_time": serverTime()}
		return idempotency.Save(r.Context(), tx, http.StatusCreated, reply)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusCreated, reply)
}

// removeDevice works after sign-out too, while the access token lasts, because apps call it then.
func (s *Service) removeDevice(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	id := chi.URLParam(r, "device_id")
	if !idPattern.MatchString(id) {
		httpserver.WriteError(w, http.StatusNotFound, "not_found", "No such device.", false)
		return
	}
	found := true
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), "DELETE FROM devices WHERE id = $1 AND actor_type = $2 AND actor_id = $3", id, actor.Type, actor.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			found = false
			return nil
		}
		reply = map[string]any{"server_time": serverTime()}
		return idempotency.Save(r.Context(), tx, http.StatusOK, reply)
	})
	switch {
	case err != nil:
		s.fail(w, r, err)
	case !found:
		httpserver.WriteError(w, http.StatusNotFound, "not_found", "No such device.", false)
	default:
		httpserver.WriteJSON(w, http.StatusOK, reply)
	}
}

// optionalStamp is a time as the apps read it, or null when there is none.
func optionalStamp(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}
