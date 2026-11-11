package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

var indianMobile = regexp.MustCompile(`^\+91[6-9][0-9]{9}$`)

// MountHubs adds the hub and hub staff calls. A hub_manager sees and changes only their own hub.
func (s *Service) MountHubs(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.With(s.Require("hubs.read", false)).Get("/admin/hubs", s.listHubs)
		g.With(s.Require("hubs.manage", false), idem).Post("/admin/hubs", s.createHub)
		g.With(s.Require("hubs.manage", false), idem).Patch("/admin/hubs/{hub_id}", s.updateHub)
		g.With(s.Require("hubs.read", false)).Get("/admin/hubs/{hub_id}/staff", s.listStaff)
		g.With(s.Require("hub_staff.manage", false), idem).Post("/admin/hubs/{hub_id}/staff", s.addStaff)
		g.With(s.Require("hub_staff.manage", false), idem).Patch("/admin/hub-staff/{staff_id}", s.updateStaff)
	})
}

// ownHub says whether the user may see a hub: everyone with the permission, except a hub_manager,
// who sees only theirs.
func ownHub(u User, hub string) bool {
	return u.Role != "hub_manager" || (u.HubID != nil && *u.HubID == hub)
}

// HubView is one hub (contract: AdminHub).
func HubView(ctx context.Context, q querier, id string) (map[string]any, error) {
	var zone, name, status string
	var lat, lng float64
	var staff int
	var test bool
	err := q.QueryRow(ctx, `SELECT h.zone_id::text, h.name, h.lat, h.lng, h.status, h.is_test,
			(SELECT count(*) FROM hub_staff s WHERE s.hub_id = h.id AND s.status = 'active')
		FROM hubs h WHERE h.id = $1`, id).Scan(&zone, &name, &lat, &lng, &status, &test, &staff)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "zone_id": zone, "name": name, "lat": lat, "lng": lng, "status": status, "staff_count": staff, "is_test": test}, nil
}

func (s *Service) listHubs(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r.Context())
	zone := r.URL.Query().Get("zone_id")
	if zone != "" && !idPattern.MatchString(zone) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Bad zone_id.", false)
		return
	}
	var only *string
	if u.Role == "hub_manager" {
		only = u.HubID
		if only == nil {
			httpserver.WriteJSON(w, http.StatusOK, map[string]any{"hubs": []any{}, "server_time": serverTime()})
			return
		}
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT id::text FROM hubs WHERE (NULLIF($1, '')::uuid IS NULL OR zone_id = NULLIF($1, '')::uuid)
		AND ($2::uuid IS NULL OR id = $2::uuid) ORDER BY name, id`, zone, only)
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	out := []map[string]any{}
	for _, id := range ids {
		v, err := HubView(r.Context(), s.Pool, id)
		if err != nil {
			s.unavailable(w, r, err)
			return
		}
		out = append(out, v)
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"hubs": out, "server_time": serverTime()})
}

func validPoint(lat, lng *float64) bool {
	return lat != nil && lng != nil && *lat >= -90 && *lat <= 90 && *lng >= -180 && *lng <= 180
}

func (s *Service) createHub(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ZoneID string   `json:"zone_id"`
		Name   string   `json:"name"`
		Lat    *float64 `json:"lat"`
		Lng    *float64 `json:"lng"`
	}
	err := json.NewDecoder(r.Body).Decode(&body)
	body.Name = strings.TrimSpace(body.Name)
	if err != nil || !idPattern.MatchString(body.ZoneID) || body.Name == "" || chars(body.Name) > 80 || !validPoint(body.Lat, body.Lng) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send zone_id, name, lat and lng.", false)
		return
	}
	s.change(w, r, http.StatusCreated, func(ctx context.Context, tx pgx.Tx, u User) (map[string]any, error) {
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM zones WHERE id = $1)", body.ZoneID).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			return nil, errInvalid
		}
		var id string
		err := tx.QueryRow(ctx, "INSERT INTO hubs (zone_id, name, lat, lng) VALUES ($1, $2, $3, $4) RETURNING id::text", body.ZoneID, body.Name, *body.Lat, *body.Lng).Scan(&id)
		if uniqueViolation(err) {
			return nil, &refusal{"name_taken", "A hub in that zone already has that name."}
		}
		if err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: u.ID, Action: "hub.created", EntityType: "hub", EntityID: id,
			After: map[string]any{"zone_id": body.ZoneID, "name": body.Name}}); err != nil {
			return nil, err
		}
		v, err := HubView(ctx, tx, id)
		return map[string]any{"hub": v}, err
	})
}

func (s *Service) updateHub(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   *string  `json:"name"`
		Lat    *float64 `json:"lat"`
		Lng    *float64 `json:"lng"`
		Status *string  `json:"status"`
		Reason string   `json:"reason"`
	}
	err := json.NewDecoder(r.Body).Decode(&body)
	if err != nil || !validReason(body.Reason) || (body.Name != nil && (strings.TrimSpace(*body.Name) == "" || chars(*body.Name) > 80)) ||
		(body.Lat == nil) != (body.Lng == nil) || (body.Lat != nil && !validPoint(body.Lat, body.Lng)) ||
		(body.Status != nil && *body.Status != "active" && *body.Status != "paused") {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Check the fields; lat and lng go together.", false)
		return
	}
	id := chi.URLParam(r, "hub_id")
	s.change(w, r, http.StatusOK, func(ctx context.Context, tx pgx.Tx, u User) (map[string]any, error) {
		if !idPattern.MatchString(id) {
			return nil, errNotFound
		}
		var zone string
		err := tx.QueryRow(ctx, "SELECT zone_id::text FROM hubs WHERE id = $1 FOR UPDATE", id).Scan(&zone)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errNotFound
		}
		if err != nil {
			return nil, err
		}
		before, err := HubView(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if body.Lat != nil {
			// A moved hub stays inside its zone, when the zone has an area drawn.
			var inside bool
			if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT area IS NULL OR extensions.ST_Covers(area, extensions.ST_SetSRID(extensions.ST_MakePoint($3, $2), 4326))
				FROM zones WHERE id = $1), false)`, zone, *body.Lat, *body.Lng).Scan(&inside); err != nil {
				return nil, err
			}
			if !inside {
				return nil, &refusal{"outside_zone", "That pin is outside the hub's zone."}
			}
		}
		var name *string
		if body.Name != nil {
			n := strings.TrimSpace(*body.Name)
			name = &n
		}
		_, err = tx.Exec(ctx, `UPDATE hubs SET name = coalesce($2, name), lat = coalesce($3, lat), lng = coalesce($4, lng), status = coalesce($5, status),
			updated_at = now() WHERE id = $1`, id, name, body.Lat, body.Lng, body.Status)
		if uniqueViolation(err) {
			return nil, &refusal{"name_taken", "A hub in that zone already has that name."}
		}
		if err != nil {
			return nil, err
		}
		after, err := HubView(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: u.ID, Action: "hub.updated", EntityType: "hub", EntityID: id,
			Before: before, After: after, Reason: body.Reason}); err != nil {
			return nil, err
		}
		return map[string]any{"hub": after}, nil
	})
}

// StaffView is one hub staff member (contract: HubStaffMember): the last four phone digits only.
func StaffView(ctx context.Context, q querier, id string) (map[string]any, error) {
	var hub, role, status, phone string
	var name *string
	err := q.QueryRow(ctx, `SELECT s.hub_id::text, s.name, p.phone, s.role, s.status FROM hub_staff s JOIN people p ON p.id = s.person_id WHERE s.id = $1`, id).
		Scan(&hub, &name, &phone, &role, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(phone) > 4 {
		phone = phone[len(phone)-4:]
	}
	return map[string]any{"id": id, "hub_id": hub, "name": name, "phone_last4": phone, "role": role, "status": status}, nil
}

func (s *Service) listStaff(w http.ResponseWriter, r *http.Request) {
	hub := chi.URLParam(r, "hub_id")
	if !idPattern.MatchString(hub) || !ownHub(UserFrom(r.Context()), hub) {
		httpserver.WriteError(w, http.StatusNotFound, "not_found", "Not found.", false)
		return
	}
	if _, err := HubView(r.Context(), s.Pool, hub); err != nil {
		s.write(w, r, 0, nil, err)
		return
	}
	rows, err := s.Pool.Query(r.Context(), "SELECT id::text FROM hub_staff WHERE hub_id = $1 ORDER BY role DESC, name, id", hub)
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	out := []map[string]any{}
	for _, id := range ids {
		v, err := StaffView(r.Context(), s.Pool, id)
		if err != nil {
			s.unavailable(w, r, err)
			return
		}
		out = append(out, v)
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"staff": out, "server_time": serverTime()})
}

func (s *Service) addStaff(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone string `json:"phone"`
		Name  string `json:"name"`
		Role  string `json:"role"`
	}
	err := json.NewDecoder(r.Body).Decode(&body)
	body.Name = strings.TrimSpace(body.Name)
	if err != nil || !indianMobile.MatchString(body.Phone) || body.Name == "" || chars(body.Name) > 80 || (body.Role != "hub_staff" && body.Role != "hub_lead") {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send phone (+91), name and role.", false)
		return
	}
	hub := chi.URLParam(r, "hub_id")
	s.change(w, r, http.StatusCreated, func(ctx context.Context, tx pgx.Tx, u User) (map[string]any, error) {
		if !idPattern.MatchString(hub) || !ownHub(u, hub) {
			return nil, errNotFound
		}
		if _, err := HubView(ctx, tx, hub); err != nil {
			return nil, err
		}
		// The person may already exist (a customer, say); a person is hub staff at most once.
		var person string
		if err := tx.QueryRow(ctx, `WITH ins AS (INSERT INTO people (phone) VALUES ($1) ON CONFLICT (phone) DO NOTHING RETURNING id)
			SELECT id::text FROM ins UNION ALL SELECT id::text FROM people WHERE phone = $1 LIMIT 1`, body.Phone).Scan(&person); err != nil {
			return nil, err
		}
		var id string
		err := tx.QueryRow(ctx, "INSERT INTO hub_staff (person_id, hub_id, role, name) VALUES ($1, $2, $3, $4) RETURNING id::text", person, hub, body.Role, body.Name).Scan(&id)
		if uniqueViolation(err) {
			return nil, &refusal{"phone_taken", "That phone is already hub staff."}
		}
		if err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: u.ID, Action: "hub_staff.added", EntityType: "hub_staff", EntityID: id,
			After: map[string]any{"hub_id": hub, "role": body.Role}}); err != nil {
			return nil, err
		}
		v, err := StaffView(ctx, tx, id)
		return map[string]any{"staff": v}, err
	})
}

func (s *Service) updateStaff(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Role   *string `json:"role"`
		Status *string `json:"status"`
		HubID  *string `json:"hub_id"`
		Reason string  `json:"reason"`
	}
	err := json.NewDecoder(r.Body).Decode(&body)
	if err != nil || !validReason(body.Reason) || (body.Role != nil && *body.Role != "hub_staff" && *body.Role != "hub_lead") ||
		(body.Status != nil && *body.Status != "active" && *body.Status != "suspended") || (body.HubID != nil && !idPattern.MatchString(*body.HubID)) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Check the fields against the contract.", false)
		return
	}
	id := chi.URLParam(r, "staff_id")
	s.change(w, r, http.StatusOK, func(ctx context.Context, tx pgx.Tx, u User) (map[string]any, error) {
		if !idPattern.MatchString(id) {
			return nil, errNotFound
		}
		// The session lock first, as the staff member's own writes take it first. Moving someone to
		// another hub ends their sessions too, so they sign in again to the new hub's work.
		if (body.Status != nil && *body.Status == "suspended") || body.HubID != nil {
			if err := s.Sessions.EndAll(ctx, tx, identity.TypeStaff, id, ""); err != nil {
				return nil, err
			}
		}
		var hub, role, status string
		err := tx.QueryRow(ctx, "SELECT hub_id::text, role, status FROM hub_staff WHERE id = $1 FOR NO KEY UPDATE", id).Scan(&hub, &role, &status)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !ownHub(u, hub)) {
			return nil, errNotFound
		}
		if err != nil {
			return nil, err
		}
		if body.HubID != nil && *body.HubID != hub {
			if !ownHub(u, *body.HubID) {
				return nil, errNotFound // A hub_manager cannot move staff out of their hub.
			}
			if _, err := HubView(ctx, tx, *body.HubID); err != nil {
				if errors.Is(err, errNotFound) {
					return nil, errInvalid
				}
				return nil, err
			}
		}
		before, err := StaffView(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, "UPDATE hub_staff SET role = coalesce($2, role), status = coalesce($3, status), hub_id = coalesce($4::uuid, hub_id), updated_at = now() WHERE id = $1",
			id, body.Role, body.Status, body.HubID); err != nil {
			return nil, err
		}
		after, err := StaffView(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: u.ID, Action: "hub_staff.updated", EntityType: "hub_staff", EntityID: id,
			Before: before, After: after, Reason: body.Reason}); err != nil {
			return nil, err
		}
		return map[string]any{"staff": after}, nil
	})
}
