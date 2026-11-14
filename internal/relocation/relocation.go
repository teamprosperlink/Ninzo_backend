// Package relocation suggests moves between staging nodes, toward where orders come from, and
// serves the rider's answer. The rule is simple on purpose (milestones, v0.7.0 part D): a node
// with recent orders and nobody waiting asks the nearest node with a spare rider for one.
package relocation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/earnings"
	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/notify"
	"github.com/teamprosperlink/Ninzo_backend/internal/placementplan"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/outbox"
)

// arriveWithin is how long an accepted move waits for the rider to reach the node. After it, the
// move is closed as expired, and paid time is left to the earnings rule.
const arriveWithin = time.Hour

var idPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

var (
	errNotFound = errors.New("relocation: not found")
	errBlocked  = errors.New("relocation: blocked")
	errExpired  = errors.New("relocation: expired")
)

// Service suggests moves and serves acceptRelocation.
type Service struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
	now  func() time.Time
}

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Current is the rider's open suggestion for listNodes, or nil. An accepted move is not shown: the
// rider's assigned node already says where to go.
func Current(ctx context.Context, q querier, rider string, now time.Time) (any, error) {
	var id, node, reason string
	var expires time.Time
	var waiting *int
	err := q.QueryRow(ctx, `SELECT v.id::text, v.to_node_id::text, v.reason, v.expires_at, v.orders_waiting FROM relocations v
		JOIN shifts sh ON sh.id = v.shift_id AND sh.ended_at IS NULL
		WHERE v.rider_id = $1 AND v.state = 'suggested' AND v.expires_at > $2
			AND NOT EXISTS (SELECT 1 FROM offers f WHERE f.rider_id = v.rider_id AND f.released_at IS NULL)`, rider, now).Scan(&id, &node, &reason, &expires, &waiting)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var demand any // Null for a suggestion made before the count was kept.
	if waiting != nil {
		demand = *waiting
	}
	return map[string]any{"id": id, "node_id": node, "reason": reason, "orders_waiting": demand, "expires_at": expires.UTC().Format(time.RFC3339Nano)}, nil
}

func bump(ctx context.Context, tx pgx.Tx, rider, id string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO rider_state (rider_id, seq) VALUES ($1, 1)
		ON CONFLICT (rider_id) DO UPDATE SET seq = rider_state.seq + 1, updated_at = now()`, rider); err != nil {
		return err
	}
	if err := outbox.Add(ctx, tx, "push.rider", map[string]any{"rider_id": rider, "kind": "relocation", "relocation_id": id}); err != nil {
		return err
	}
	// The rider's list of notifications says what the move is now, and never more than the app shows.
	var state string
	if err := tx.QueryRow(ctx, "SELECT state FROM relocations WHERE id = $1", id).Scan(&state); err != nil {
		return err
	}
	title, body := "Move suggested", "Ninzo suggests moving your scooter to a busier node."
	switch state {
	case "accepted":
		title, body = "Move accepted", "Ride your scooter to the new node."
	case "arrived":
		title, body = "Move finished", "You are at the new node."
	case "expired":
		title, body = "Move expired", "The suggested move ended."
	case "cancelled":
		title, body = "Move cancelled", "The move ended."
	}
	return notify.Record(ctx, tx, rider, "relocation", title, body, "")
}

// MountRider adds acceptRelocation.
func (s *Service) MountRider(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.With(idem).Post("/riders/me/relocations/{relocation_id}/accept", s.accept)
	})
}

func (s *Service) accept(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	id := chi.URLParam(r, "relocation_id")
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		if err := identity.CheckSession(r.Context(), tx, actor); err != nil {
			return err
		}
		var status, person string
		err := tx.QueryRow(r.Context(), "SELECT r.status, p.status FROM riders r JOIN people p ON p.id = r.person_id WHERE r.id = $1", actor.ID).Scan(&status, &person)
		if errors.Is(err, pgx.ErrNoRows) {
			return identity.ErrSessionInvalid
		}
		if err != nil {
			return err
		}
		if person != "active" || status == "suspended" {
			return errBlocked
		}
		if err := s.AcceptTx(r.Context(), tx, actor.ID, id); err != nil {
			return err
		}
		return idempotency.Save(r.Context(), tx, http.StatusOK, map[string]any{})
	})
	switch {
	case err == nil:
		httpserver.WriteJSON(w, http.StatusOK, map[string]any{})
	case errors.Is(err, identity.ErrSessionInvalid):
		httpserver.WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in again.", false)
	case errors.Is(err, errBlocked):
		identity.WriteBlocked(w)
	case errors.Is(err, errNotFound):
		httpserver.WriteError(w, http.StatusNotFound, "not_found", "Not found.", false)
	case errors.Is(err, errExpired):
		httpserver.WriteError(w, http.StatusConflict, "relocation_expired", "This suggestion has ended. Stay where you are.", false)
	default:
		s.Log.Error("relocation: accept failed", "request_id", httpserver.RequestID(r.Context()), "error", err.Error())
		httpserver.WriteError(w, http.StatusInternalServerError, "internal_error", "Try again shortly.", true)
	}
}

// AcceptTx accepts a rider's suggestion: the rider's shift and scooter are now assigned to the new
// node, and paid time starts. A repeat is accepted again with no change.
func (s *Service) AcceptTx(ctx context.Context, tx pgx.Tx, rider, id string) error {
	if !idPattern.MatchString(id) {
		return errNotFound
	}
	now := s.clock()
	// The shift first, then the move: the order endShift takes them in.
	var shift string
	err := tx.QueryRow(ctx, "SELECT shift_id::text FROM relocations WHERE id = $1 AND rider_id = $2", id, rider).Scan(&shift)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "SELECT 1 FROM shifts WHERE id = $1 FOR UPDATE", shift); err != nil {
		return err
	}
	var state, to string
	var expires time.Time
	if err := tx.QueryRow(ctx, "SELECT state, to_node_id::text, expires_at FROM relocations WHERE id = $1 FOR UPDATE", id).Scan(&state, &to, &expires); err != nil {
		return err
	}
	switch {
	case state == "accepted" || state == "arrived":
		return nil
	case state != "suggested" || !now.Before(expires):
		return errExpired
	}
	var scooter string
	err = tx.QueryRow(ctx, "UPDATE shifts SET node_id = $2 WHERE id = $1 AND ended_at IS NULL RETURNING scooter_id::text", shift, to).Scan(&scooter)
	if errors.Is(err, pgx.ErrNoRows) {
		return errExpired // The shift has ended.
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE scooters SET node_id = $2, updated_at = now() WHERE id = $1", scooter, to); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE relocations SET state = 'accepted', accepted_at = $2, state_changed_at = $2 WHERE id = $1", id, now); err != nil {
		return err
	}
	if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeRider, ActorID: rider, Action: "relocation.accepted", EntityType: "relocation", EntityID: id,
		After: map[string]any{"to_node_id": to}}); err != nil {
		return err
	}
	return bump(ctx, tx, rider, id)
}

// Run checks every so often until ctx ends. A failure is logged, never fatal.
func (s *Service) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			func() {
				defer func() {
					if p := recover(); p != nil {
						s.Log.Error("relocation: check panicked")
					}
				}()
				if _, err := s.Check(ctx, s.clock()); err != nil && ctx.Err() == nil {
					s.Log.Error("relocation: check failed", "error", err.Error())
				}
			}()
		}
	}
}

// Check ends moves that are over, marks arrivals, and suggests new moves zone by zone. It returns
// how many it suggested.
func (s *Service) Check(ctx context.Context, now time.Time) (int, error) {
	if err := s.settle(ctx, now); err != nil {
		return 0, err
	}
	rows, err := s.Pool.Query(ctx, "SELECT id::text FROM zones WHERE status = 'active' ORDER BY id")
	if err != nil {
		return 0, err
	}
	zones, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	made := 0
	for _, zone := range zones {
		err := database.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
			n, err := s.suggestIn(ctx, tx, zone, now)
			made += n
			return err
		})
		if err != nil && ctx.Err() == nil {
			s.Log.Error("relocation: a zone check failed", "zone_id", zone, "error", err.Error())
		}
	}
	return made, nil
}

// maxAccuracyM is the worst GPS accuracy a ping may have to mark an arrival.
const maxAccuracyM = 100

// settle ends suggestions that are past their time or no longer fit (the rider has an offer or no
// shift), ends accepted moves that an offer interrupted or nobody finished, and marks arrivals from
// the rider's pings.
func (s *Service) settle(ctx context.Context, now time.Time) error {
	return database.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		type moved struct{ id, rider string }
		ended := []moved{}
		// Arrivals first, so a rider who reached the node before an offer is marked arrived.
		rows, err := tx.Query(ctx, `SELECT v.id::text, v.rider_id::text, n.zone_id::text FROM relocations v JOIN staging_nodes n ON n.id = v.to_node_id
			WHERE v.state = 'accepted' ORDER BY v.id FOR UPDATE OF v SKIP LOCKED`)
		if err != nil {
			return err
		}
		type open struct{ id, rider, zone string }
		moving, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (open, error) {
			var o open
			return o, r.Scan(&o.id, &o.rider, &o.zone)
		})
		if err != nil {
			return err
		}
		reach := map[string]int{}
		for _, m := range moving {
			within, ok := reach[m.zone]
			if !ok {
				settings, err := geo.LoadSettings(ctx, tx, m.zone)
				if err != nil {
					return err
				}
				within, _ = settings.Get("relocation_arrive_m")
				reach[m.zone] = within
			}
			// The first accurate ping, recorded and received since accepting, near the new node.
			var at time.Time
			err := tx.QueryRow(ctx, `SELECT lp.received_at FROM relocations v JOIN staging_nodes n ON n.id = v.to_node_id
				JOIN location_pings lp ON lp.rider_id = v.rider_id AND lp.received_at >= v.accepted_at AND lp.device_time >= v.accepted_at
					AND lp.received_at <= $2 AND lp.accuracy_m <= $3
				WHERE v.id = $1 AND extensions.ST_DWithin(
					extensions.ST_SetSRID(extensions.ST_MakePoint(n.lng, n.lat), 4326)::extensions.geography,
					extensions.ST_SetSRID(extensions.ST_MakePoint(lp.lng, lp.lat), 4326)::extensions.geography, $4)
				ORDER BY lp.received_at LIMIT 1`, m.id, now, maxAccuracyM, within).Scan(&at)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "UPDATE relocations SET state = 'arrived', arrived_at = $2, state_changed_at = $3 WHERE id = $1", m.id, at, now); err != nil {
				return err
			}
			ended = append(ended, moved{m.id, m.rider})
		}
		rows, err = tx.Query(ctx, `UPDATE relocations v SET state = CASE WHEN v.state = 'suggested' AND v.expires_at > $1 THEN 'cancelled'
					WHEN v.state = 'accepted' AND v.accepted_at > $1 - make_interval(secs => $2) THEN 'cancelled' ELSE 'expired' END,
				-- An offer ends an accepted move when it was made, so paid time stops there.
				state_changed_at = CASE WHEN v.state = 'accepted' THEN greatest(v.accepted_at, least($1, coalesce((SELECT min(f.created_at) FROM offers f
					WHERE f.rider_id = v.rider_id AND f.released_at IS NULL AND f.created_at > v.accepted_at), $1))) ELSE $1 END
			WHERE (v.state = 'suggested' AND (v.expires_at <= $1
					OR NOT EXISTS (SELECT 1 FROM shifts sh WHERE sh.id = v.shift_id AND sh.ended_at IS NULL)
					OR EXISTS (SELECT 1 FROM offers f WHERE f.rider_id = v.rider_id AND f.released_at IS NULL)))
				OR (v.state = 'accepted' AND (v.accepted_at <= $1 - make_interval(secs => $2)
					OR EXISTS (SELECT 1 FROM offers f WHERE f.rider_id = v.rider_id AND f.released_at IS NULL AND f.created_at > v.accepted_at)))
			RETURNING v.id::text, v.rider_id::text`, now, arriveWithin.Seconds())
		if err != nil {
			return err
		}
		stopped, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (moved, error) {
			var m moved
			return m, r.Scan(&m.id, &m.rider)
		})
		if err != nil {
			return err
		}
		ended = append(ended, stopped...)
		for _, m := range ended {
			if err := earnings.RecordRelocation(ctx, tx, m.id); err != nil {
				return err
			}
			if err := bump(ctx, tx, m.rider, m.id); err != nil {
				return err
			}
		}
		return nil
	})
}

type place struct {
	id       string
	lat, lng float64
	demand   int
	free     []string // Free riders waiting here who may be asked to move, longest waiting first.
	waiting  int      // Every free rider waiting here.
	incoming bool     // A move toward here is already in play.
	// The same two for riders on non-test scooters only: plans are built for those (#249).
	realFree    []string
	realWaiting int
}

func (s *Service) suggestIn(ctx context.Context, tx pgx.Tx, zone string, now time.Time) (int, error) {
	// One check per zone at a time, so two copies of the API never suggest twice.
	// An advisory lock, not a row lock on the zone, so orders in the zone never wait on it.
	var locked bool
	if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock(hashtextextended('relocation:' || $1, 0))", zone).Scan(&locked); err != nil {
		return 0, err
	}
	if !locked {
		return 0, nil
	}
	settings, err := geo.LoadSettings(ctx, tx, zone)
	if err != nil {
		return 0, err
	}
	window, _ := settings.Get("relocation_window_s")
	least, _ := settings.Get("relocation_min_orders")
	ttl, _ := settings.Get("relocation_ttl_s")
	rest, _ := settings.Get("relocation_rest_s")
	rows, err := tx.Query(ctx, `SELECT n.id::text, n.lat, n.lng, EXISTS (SELECT 1 FROM relocations v WHERE v.to_node_id = n.id AND v.state IN ('suggested', 'accepted'))
		FROM staging_nodes n WHERE n.zone_id = $1 ORDER BY n.id`, zone)
	if err != nil {
		return 0, err
	}
	places := map[string]*place{}
	order := []string{}
	var p place
	if _, err := pgx.ForEachRow(rows, []any{&p.id, &p.lat, &p.lng, &p.incoming}, func() error {
		c := p
		places[c.id] = &c
		order = append(order, c.id)
		return nil
	}); err != nil {
		return 0, err
	}
	if len(places) < 2 {
		return 0, nil
	}
	// Demand: paid orders in the window, each counted at the node nearest its gate.
	rows, err = tx.Query(ctx, `SELECT (SELECT n.id::text FROM staging_nodes n WHERE n.zone_id = $1
			ORDER BY (n.lat - g.lat) * (n.lat - g.lat) + (n.lng - g.lng) * (n.lng - g.lng), n.id LIMIT 1), count(*)::int
		FROM orders o JOIN gate_history g ON g.id = o.gate_history_id
		WHERE o.zone_id = $1 AND o.created_at > $2 AND o.paid_at IS NOT NULL GROUP BY 1`, zone, now.Add(-time.Duration(window)*time.Second))
	if err != nil {
		return 0, err
	}
	var node string
	var count int
	if _, err := pgx.ForEachRow(rows, []any{&node, &count}, func() error {
		if pl := places[node]; pl != nil {
			pl.demand = count
		}
		return nil
	}); err != nil {
		return 0, err
	}
	// Supply: riders on shift at each node with no live offer. Those with a move in play, or one
	// that ended within the rest time, are counted but never asked again.
	rows, err = tx.Query(ctx, `SELECT sh.node_id::text, sh.rider_id::text,
			NOT EXISTS (SELECT 1 FROM relocations v WHERE v.rider_id = sh.rider_id
				AND (v.state IN ('suggested', 'accepted') OR v.state_changed_at > $2)), c.is_test
		FROM shifts sh JOIN riders r ON r.id = sh.rider_id AND r.status = 'active'
			JOIN people pe ON pe.id = r.person_id AND pe.status = 'active'
			JOIN scooters c ON c.id = sh.scooter_id AND c.status = 'active'
			JOIN staging_nodes n ON n.id = sh.node_id AND n.zone_id = $1
		WHERE sh.ended_at IS NULL AND NOT EXISTS (SELECT 1 FROM offers f WHERE f.rider_id = sh.rider_id AND f.released_at IS NULL)
		ORDER BY sh.started_at, sh.rider_id`, zone, now.Add(-time.Duration(rest)*time.Second))
	if err != nil {
		return 0, err
	}
	var rider string
	var askable, test bool
	if _, err := pgx.ForEachRow(rows, []any{&node, &rider, &askable, &test}, func() error {
		if pl := places[node]; pl != nil {
			pl.waiting++
			if askable {
				pl.free = append(pl.free, rider)
			}
			if !test {
				pl.realWaiting++
				if askable {
					pl.realFree = append(pl.realFree, rider)
				}
			}
		}
		return nil
	}); err != nil {
		return 0, err
	}
	// With relocation_by_plan on, follow the zone's placement plan for now and this many free riders
	// (#226). Plans are built for non-test scooters, so only riders on those are counted for n or
	// asked to move by the plan (#249). With no plan for this moment, today's rule below runs as before.
	if byPlan, _ := settings.Get("relocation_by_plan"); byPlan == 1 {
		free := 0
		for _, pl := range places {
			free += pl.realWaiting
		}
		if free > 0 {
			plan, ok, err := placementplan.Latest(ctx, tx, zone, now, free)
			if err != nil {
				return 0, err
			}
			if ok {
				planned := map[string]*place{}
				for id, pl := range places {
					c := *pl
					c.free, c.waiting = pl.realFree, pl.realWaiting
					planned[id] = &c
				}
				return s.suggestByPlan(ctx, tx, planned, order, plan, now, ttl)
			}
		}
	}
	// Short nodes, most orders first.
	short := []*place{}
	for _, id := range order {
		if pl := places[id]; pl.demand >= least && pl.waiting == 0 && !pl.incoming {
			short = append(short, pl)
		}
	}
	sort.SliceStable(short, func(i, j int) bool { return short[i].demand > short[j].demand })
	made := 0
	for _, to := range short {
		var from *place
		best := 0.0
		for _, id := range order {
			pl := places[id]
			// A donor keeps at least one rider waiting.
			if pl.id == to.id || pl.waiting < 2 || len(pl.free) == 0 {
				continue
			}
			d := (pl.lat-to.lat)*(pl.lat-to.lat) + (pl.lng-to.lng)*(pl.lng-to.lng)
			if from == nil || d < best {
				from, best = pl, d
			}
		}
		if from == nil {
			continue
		}
		ok, err := suggest(ctx, tx, from, to, fmt.Sprintf("%d orders expected, nobody nearby", to.demand), to.demand, now, ttl)
		if err != nil {
			return made, err
		}
		if ok {
			made++
		}
	}
	return made, nil
}

// suggest asks from's longest-waiting askable rider to move to to: the same row, TTL and rider_state
// bump for either rule, with expected orders at to stored as orders_waiting. It reports false when
// the rider's shift ended during this check.
func suggest(ctx context.Context, tx pgx.Tx, from, to *place, reason string, expected int, now time.Time, ttl int) (bool, error) {
	who := from.free[0]
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO relocations (rider_id, shift_id, from_node_id, to_node_id, reason, expires_at, state_changed_at, orders_waiting)
		SELECT $1, sh.id, $2, $3, $4, $5, $6, $7 FROM shifts sh WHERE sh.rider_id = $1 AND sh.ended_at IS NULL RETURNING id::text`,
		who, from.id, to.id, reason, now.Add(time.Duration(ttl)*time.Second), now, expected).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil // The shift ended during this check.
		}
		return false, err
	}
	if err := bump(ctx, tx, who, id); err != nil {
		return false, err
	}
	from.free, from.waiting = from.free[1:], from.waiting-1
	return true, nil
}

// suggestByPlan (relocation_by_plan, #226) asks for moves toward the plan: a node is short when the
// plan wants more riders there than are waiting, most short first; the donor is the nearest node
// with more riders waiting than the plan wants there, and an askable rider. Only the choice of nodes
// differs from today's rule: one move per short node per check, none toward a node with a move in
// play, and the same suggestion row and flow. Plan nodes no longer in the zone are ignored.
func (s *Service) suggestByPlan(ctx context.Context, tx pgx.Tx, places map[string]*place, order []string, plan placementplan.Plan, now time.Time, ttl int) (int, error) {
	want := map[string]int{}
	// Forecast orders per node, from the plan; a build from before #249 has none, so its moves
	// report the node's recent orders as before.
	forecast, hasForecast := map[string]float64{}, plan.NodeOrders != nil
	for i, id := range plan.Nodes {
		if places[id] != nil {
			want[id]++
			if hasForecast {
				forecast[id] += plan.NodeOrders[i]
			}
		}
	}
	short := []*place{}
	for _, id := range order {
		if pl := places[id]; want[id] > pl.waiting && !pl.incoming {
			short = append(short, pl)
		}
	}
	sort.SliceStable(short, func(i, j int) bool {
		return want[short[i].id]-short[i].waiting > want[short[j].id]-short[j].waiting
	})
	made := 0
	for _, to := range short {
		var from *place
		best := 0.0
		for _, id := range order {
			pl := places[id]
			if pl.id == to.id || pl.waiting <= want[pl.id] || len(pl.free) == 0 {
				continue
			}
			d := (pl.lat-to.lat)*(pl.lat-to.lat) + (pl.lng-to.lng)*(pl.lng-to.lng)
			if from == nil || d < best {
				from, best = pl, d
			}
		}
		if from == nil {
			continue
		}
		expected := to.demand
		if hasForecast {
			expected = int(math.Round(forecast[to.id]))
		}
		ok, err := suggest(ctx, tx, from, to, "More orders are expected here soon", expected, now, ttl)
		if err != nil {
			return made, err
		}
		if ok {
			made++
		}
	}
	return made, nil
}
