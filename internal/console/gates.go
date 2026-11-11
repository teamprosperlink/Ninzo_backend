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
	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

// MountGates adds listAdminGates, updateGate and getGateHistory.
func (s *Service) MountGates(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.With(s.Require("gates.read", false)).Get("/admin/gates", s.listGates)
		g.With(s.Require("gates.read", false)).Get("/admin/gates/{gate_id}/history", s.gateHistory)
		g.With(s.Require("gates.manage", false), idem).Patch("/admin/gates/{gate_id}", s.updateGate)
	})
}

// GateView is one gate as the console sees it (contract: AdminGate), with its newest version.
func GateView(ctx context.Context, q querier, id string) (map[string]any, error) {
	var zone, label string
	var lat, lng float64
	var notes *string
	var verified bool
	var version int
	var at time.Time
	err := q.QueryRow(ctx, `SELECT g.zone_id::text, g.label, g.lat, g.lng, g.entry_notes, g.verified, g.updated_at,
			coalesce((SELECT max(h.version) FROM gate_history h WHERE h.gate_id = g.id), 1)
		FROM gates g WHERE g.id = $1`, id).Scan(&zone, &label, &lat, &lng, &notes, &verified, &at, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "zone_id": zone, "label": label, "lat": lat, "lng": lng, "entry_notes": notes, "verified": verified,
		"version": version, "updated_at": at.UTC().Format(time.RFC3339Nano)}, nil
}

// listGates is newest first, optionally in one zone.
func (s *Service) listGates(w http.ResponseWriter, r *http.Request) {
	p, ok := ReadPage(r)
	zone := r.URL.Query().Get("zone_id")
	if !ok || (zone != "" && !idPattern.MatchString(zone)) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Bad zone_id, cursor or limit.", false)
		return
	}
	if !p.HasAfter {
		p.At, p.ID = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), "ffffffff-ffff-ffff-ffff-ffffffffffff"
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT id::text, created_at FROM gates
		WHERE (NULLIF($1, '')::uuid IS NULL OR zone_id = NULLIF($1, '')::uuid) AND (created_at, id) < ($2, $3::uuid)
		ORDER BY created_at DESC, id DESC LIMIT $4`, zone, p.At, p.ID, p.Limit+1)
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
		v, err := GateView(r.Context(), s.Pool, x.id)
		if err != nil {
			s.unavailable(w, r, err)
			return
		}
		out = append(out, v)
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"gates": out, "next_cursor": next, "server_time": serverTime()})
}

func (s *Service) gateHistory(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "gate_id")
	if !idPattern.MatchString(id) {
		httpserver.WriteError(w, http.StatusNotFound, "not_found", "Not found.", false)
		return
	}
	if _, err := GateView(r.Context(), s.Pool, id); err != nil {
		s.write(w, r, 0, nil, err)
		return
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT h.version, h.label, h.lat, h.lng, h.entry_notes, h.verified, h.reason, cu.name, h.created_at
		FROM gate_history h LEFT JOIN console_users cu ON cu.id = h.changed_by WHERE h.gate_id = $1 ORDER BY h.version`, id)
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	out := []map[string]any{}
	var version int
	var label, reason string
	var lat, lng float64
	var notes, by *string
	var verified bool
	var at time.Time
	if _, err := pgx.ForEachRow(rows, []any{&version, &label, &lat, &lng, &notes, &verified, &reason, &by, &at}, func() error {
		out = append(out, map[string]any{"version": version, "label": label, "lat": lat, "lng": lng, "entry_notes": notes, "verified": verified,
			"reason": reason, "changed_by": by, "created_at": at.UTC().Format(time.RFC3339Nano)})
		return nil
	}); err != nil {
		s.unavailable(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"gate_id": id, "versions": out, "server_time": serverTime()})
}

// updateGate writes a new version; orders keep the version they were placed with. A moved pin must
// stay inside the gate's zone.
func (s *Service) updateGate(w http.ResponseWriter, r *http.Request) {
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send a JSON object.", false)
		return
	}
	var body struct {
		Label      *string  `json:"label"`
		Lat        *float64 `json:"lat"`
		Lng        *float64 `json:"lng"`
		EntryNotes *string  `json:"entry_notes"`
		Verified   *bool    `json:"verified"`
		Reason     string   `json:"reason"`
	}
	buf, _ := json.Marshal(raw)
	if err := json.Unmarshal(buf, &body); err != nil || nullOn(raw, "label", "lat", "lng", "verified", "reason") {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "A field has the wrong type, or is null where it cannot be.", false)
		return
	}
	_, setNotes := raw["entry_notes"]
	// The same cleaning as a customer's own gate: no control or bidi characters.
	if body.Label != nil {
		clean, ok := geo.CleanText(*body.Label, 80, false)
		if !ok || clean == "" {
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "That label is not valid.", false)
			return
		}
		body.Label = &clean
	}
	if body.EntryNotes != nil {
		clean, ok := geo.CleanText(*body.EntryNotes, 500, true)
		if !ok {
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Those entry notes are not valid.", false)
			return
		}
		body.EntryNotes = &clean
	}
	if !validReason(body.Reason) ||
		(body.Lat != nil && (*body.Lat < -90 || *body.Lat > 90)) || (body.Lng != nil && (*body.Lng < -180 || *body.Lng > 180)) ||
		(body.Lat == nil) != (body.Lng == nil) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Check the fields; lat and lng go together.", false)
		return
	}
	id := chi.URLParam(r, "gate_id")
	s.change(w, r, http.StatusOK, func(ctx context.Context, tx pgx.Tx, u User) (map[string]any, error) {
		if !idPattern.MatchString(id) {
			return nil, errNotFound
		}
		var zone, label string
		var lat, lng float64
		var notes *string
		var verified, redacted bool
		err := tx.QueryRow(ctx, "SELECT zone_id::text, label, lat, lng, entry_notes, verified FROM gates WHERE id = $1 FOR UPDATE", id).
			Scan(&zone, &label, &lat, &lng, &notes, &verified)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errNotFound
		}
		if err != nil {
			return nil, err
		}
		// Read after the lock, in its own statement, so an erasure that committed while this waited
		// is seen (RedactGates changes the gate row too, so it holds the same lock).
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM gate_history WHERE gate_id = $1 AND redacted_at IS NOT NULL)", id).Scan(&redacted); err != nil {
			return nil, err
		}
		if redacted {
			return nil, &refusal{"not_allowed", "This gate's customer was erased; it cannot be changed."}
		}
		if body.Label != nil {
			label = *body.Label
		}
		if body.Lat != nil {
			lat, lng = *body.Lat, *body.Lng
			var inside bool
			if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT extensions.ST_Covers(area, extensions.ST_SetSRID(extensions.ST_MakePoint($3, $2), 4326))
				FROM zones WHERE id = $1), false)`, zone, lat, lng).Scan(&inside); err != nil {
				return nil, err
			}
			if !inside {
				return nil, &refusal{"outside_zone", "That pin is outside the gate's zone."}
			}
		}
		if setNotes {
			notes = body.EntryNotes
			if notes != nil && *notes == "" {
				notes = nil
			}
		}
		if body.Verified != nil {
			verified = *body.Verified
		}
		if _, err := tx.Exec(ctx, "UPDATE gates SET label = $2, lat = $3, lng = $4, entry_notes = $5, verified = $6, updated_at = now() WHERE id = $1",
			id, label, lat, lng, notes, verified); err != nil {
			return nil, err
		}
		var version int
		if err := tx.QueryRow(ctx, `INSERT INTO gate_history (gate_id, version, label, lat, lng, entry_notes, verified, reason, changed_by)
			SELECT $1, coalesce(max(version), 0) + 1, $2, $3, $4, $5, $6, $7, $8 FROM gate_history WHERE gate_id = $1 RETURNING version`,
			id, label, lat, lng, notes, verified, body.Reason, u.ID).Scan(&version); err != nil {
			return nil, err
		}
		// Not the pin or the label: audit rows are kept for good, and these say where someone lives.
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: u.ID, Action: "gate.updated", EntityType: "gate", EntityID: id,
			After: map[string]any{"version": version, "verified": verified}, Reason: body.Reason}); err != nil {
			return nil, err
		}
		v, err := GateView(ctx, tx, id)
		return map[string]any{"gate": v}, err
	})
}
