package console

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
	"github.com/teamprosperlink/Ninzo_backend/internal/restock"
)

// defaultRestockStuck is used only when no settings row holds restock_stuck_s (the console's value
// wins). It is migration 0045's default.
const defaultRestockStuck = 600

// MountHubOps adds listHubOps, listStuckRestocks and expireRestock (v0.10.0 part D). A hub_manager
// sees only their own hub, and cannot expire (hub_ops.manage is admin and ops).
func (s *Service) MountHubOps(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		read := s.Require("hub_ops.read", false)
		g.With(read).Get("/admin/hub-ops", s.listHubOps)
		g.With(read).Get("/admin/hub-ops/stuck", s.listStuckRestocks)
		g.With(s.Require("hub_ops.manage", false), idem).Post("/admin/restock/{request_id}/expire", s.expireRestock)
	})
}

// stuckAfter is how long a request may wait in a zone before it counts as stuck.
func stuckAfter(ctx context.Context, q querier, zone string) (int, error) {
	settings, err := geo.LoadSettings(ctx, q, zone)
	if err != nil {
		return 0, err
	}
	if v, ok := settings.Get("restock_stuck_s"); ok {
		return v, nil
	}
	return defaultRestockStuck, nil
}

// onlyHub is the one hub a user is limited to: their own for a hub_manager. visible is false for a
// hub_manager with no hub, who sees nothing.
func onlyHub(u User) (hub *string, visible bool) {
	if u.Role != "hub_manager" {
		return nil, true
	}
	return u.HubID, u.HubID != nil
}

func (s *Service) listHubOps(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r.Context())
	zone := strings.ToLower(r.URL.Query().Get("zone_id"))
	if zone != "" && !idPattern.MatchString(zone) {
		badBody(w, "Bad zone_id.")
		return
	}
	only, visible := onlyHub(u)
	if !visible {
		httpserver.WriteJSON(w, http.StatusOK, map[string]any{"hubs": []any{}, "server_time": serverTime()})
		return
	}
	ctx, now := r.Context(), s.clock()
	rows, err := s.Pool.Query(ctx, `SELECT id::text, zone_id::text FROM hubs WHERE ($1 = '' OR zone_id = NULLIF($1, '')::uuid)
		AND ($2::uuid IS NULL OR id = $2::uuid) ORDER BY name, id`, zone, only)
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	type hubRow struct{ id, zone string }
	hubs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (hubRow, error) {
		var h hubRow
		return h, r.Scan(&h.id, &h.zone)
	})
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(hubs))
	for _, h := range hubs {
		after, err := stuckAfter(ctx, s.Pool, h.zone)
		if err != nil {
			s.unavailable(w, r, err)
			return
		}
		view, err := HubView(ctx, s.Pool, h.id)
		if err != nil {
			s.unavailable(w, r, err)
			return
		}
		var c [6]int
		var stuck, shortfalls, lots, staff int
		err = s.Pool.QueryRow(ctx, `SELECT
				count(*) FILTER (WHERE state = 'confirmed'), count(*) FILTER (WHERE state = 'hub_accepted'), count(*) FILTER (WHERE state = 'packing'),
				count(*) FILTER (WHERE state = 'partial'), count(*) FILTER (WHERE state = 'ready'), count(*) FILTER (WHERE state = 'picked_up'),
				count(*) FILTER (WHERE state IN ('confirmed', 'hub_accepted', 'packing') AND state_changed_at < $2::timestamptz - make_interval(secs => $3::double precision)),
				(SELECT count(*) FROM shortfalls f JOIN restock_requests q ON q.id = f.request_id WHERE q.hub_id = $1 AND f.status = 'open'),
				(SELECT count(*) FROM lots WHERE hub_id = $1 AND status = 'quarantined'),
				(SELECT count(*) FROM hub_staff WHERE hub_id = $1 AND status = 'active')
			FROM restock_requests WHERE hub_id = $1`, h.id, now, float64(after)).
			Scan(&c[0], &c[1], &c[2], &c[3], &c[4], &c[5], &stuck, &shortfalls, &lots, &staff)
		if err != nil {
			s.unavailable(w, r, err)
			return
		}
		out = append(out, map[string]any{"hub": view,
			"queue": map[string]any{"confirmed": c[0], "hub_accepted": c[1], "packing": c[2], "partial": c[3], "ready": c[4], "picked_up": c[5]},
			"stuck": stuck, "open_shortfalls": shortfalls, "quarantined_lots": lots, "active_staff": staff})
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"hubs": out, "server_time": serverTime()})
}

// AdminRestock is one restock request as the console sees it (contract: AdminRestock). It holds
// the rider's id and name as the contract asks, and nothing of a customer.
func AdminRestock(ctx context.Context, q reader, id string, now time.Time) (map[string]any, error) {
	var hub, rider, riderName, state string
	var changed, requested time.Time
	var ended *string
	err := q.QueryRow(ctx, `SELECT q.hub_id::text, q.rider_id::text, coalesce(r.name, ''), q.state, q.state_changed_at, q.created_at,
			CASE WHEN q.state = 'expired' THEN q.reason END
		FROM restock_requests q JOIN riders r ON r.id = q.rider_id WHERE q.id = $1`, id).Scan(&hub, &rider, &riderName, &state, &changed, &requested, &ended)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	return restockMap(id, hub, rider, riderName, state, changed, requested, ended, now), nil
}

func restockMap(id, hub, rider, riderName, state string, changed, requested time.Time, ended *string, now time.Time) map[string]any {
	return map[string]any{"id": id, "hub_id": hub, "rider": map[string]any{"id": rider, "name": riderName}, "state": state,
		"waiting_s": max(0, int(now.Sub(changed).Seconds())), "requested_at": requested.UTC().Format(time.RFC3339Nano), "ended_reason": ended}
}

func (s *Service) listStuckRestocks(w http.ResponseWriter, r *http.Request) {
	p, ok := ReadPage(r)
	if !ok {
		badBody(w, "Bad cursor or limit.")
		return
	}
	only, visible := onlyHub(UserFrom(r.Context()))
	if !visible {
		httpserver.WriteJSON(w, http.StatusOK, map[string]any{"requests": []any{}, "next_cursor": nil, "server_time": serverTime()})
		return
	}
	ctx, now := r.Context(), s.clock()
	// Each zone has its own limit, so find them first and join the limits in.
	rows, err := s.Pool.Query(ctx, "SELECT DISTINCT zone_id::text FROM hubs WHERE $1::uuid IS NULL OR id = $1::uuid", only)
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	zones, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	limits := make([]float64, len(zones))
	for i, z := range zones {
		n, err := stuckAfter(ctx, s.Pool, z)
		if err != nil {
			s.unavailable(w, r, err)
			return
		}
		limits[i] = float64(n)
	}
	if !p.HasAfter {
		p.At, p.ID = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), "00000000-0000-0000-0000-000000000000"
	}
	// Longest waiting first: the oldest state change, then the id.
	rows, err = s.Pool.Query(ctx, `SELECT q.id::text, q.hub_id::text, q.rider_id::text, coalesce(r.name, ''), q.state, q.state_changed_at, q.created_at
		FROM restock_requests q JOIN hubs h ON h.id = q.hub_id JOIN riders r ON r.id = q.rider_id
		JOIN unnest($1::uuid[], $2::double precision[]) AS lim(zone_id, secs) ON lim.zone_id = h.zone_id
		WHERE q.state IN ('confirmed', 'hub_accepted', 'packing') AND q.state_changed_at < $3::timestamptz - make_interval(secs => lim.secs)
			AND ($4::uuid IS NULL OR q.hub_id = $4::uuid) AND (q.state_changed_at, q.id) > ($5, $6::uuid)
		ORDER BY q.state_changed_at, q.id LIMIT $7`, zones, limits, now, only, p.At, p.ID, p.Limit+1)
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	type row struct {
		id, hub, rider, name, state string
		changed, requested          time.Time
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.id, &x.hub, &x.rider, &x.name, &x.state, &x.changed, &x.requested)
	})
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	var next any
	if len(list) > p.Limit {
		list = list[:p.Limit]
		next = Cursor(list[len(list)-1].changed, list[len(list)-1].id)
	}
	out := make([]map[string]any, 0, len(list))
	for _, x := range list {
		out = append(out, restockMap(x.id, x.hub, x.rider, x.name, x.state, x.changed, x.requested, nil, now))
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"requests": out, "next_cursor": next, "server_time": serverTime()})
}

func (s *Service) expireRestock(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(chi.URLParam(r, "request_id"))
	var body struct {
		Reason string `json:"reason"`
	}
	if !decodeStrict(r, &body) || !validReason(body.Reason) {
		badBody(w, "Send a reason of 3 to 500 characters.")
		return
	}
	if !idPattern.MatchString(id) {
		httpserver.WriteError(w, http.StatusNotFound, "not_found", "Not found.", false)
		return
	}
	reason := strings.TrimSpace(body.Reason)
	s.change(w, r, http.StatusOK, func(ctx context.Context, tx pgx.Tx, u User) (map[string]any, error) {
		now := s.clock()
		switch err := restock.ExpireTx(ctx, tx, id, now, u.ID, reason); {
		case errors.Is(err, restock.ErrNotFound):
			return nil, errNotFound
		case errors.Is(err, restock.ErrNotExpirable):
			return nil, &refusal{"restock_not_expirable", "This request is ready or past it, or has already ended, so it cannot be expired."}
		case err != nil:
			return nil, err
		}
		v, err := AdminRestock(ctx, tx, id, now)
		return map[string]any{"restock": v}, err
	})
}
