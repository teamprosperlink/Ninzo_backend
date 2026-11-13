package availability

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
	"github.com/teamprosperlink/Ninzo_backend/internal/matching"
	"github.com/teamprosperlink/Ninzo_backend/internal/routing"
)

// Router is what road-time matching needs from internal/routing.
type Router interface {
	Table(ctx context.Context, sources []routing.Point, dest routing.Point, speedKmh float64) ([]routing.Leg, error)
}

var (
	roadsMu sync.RWMutex
	roads   Router
)

// ConfigureRouting sets the routing client matching uses, from ROUTING_ENGINE_URL and
// ROUTING_PROFILE, once at start-up. An empty URL leaves matching on straight lines; a bad one is
// an error, so a typo fails the start instead of silently falling back.
func ConfigureRouting(getenv func(string) string) error {
	url := getenv("ROUTING_ENGINE_URL")
	if url == "" {
		SetRouter(nil)
		return nil
	}
	c, err := routing.New(routing.Config{BaseURL: url, Profile: getenv("ROUTING_PROFILE")})
	if err != nil {
		return err
	}
	SetRouter(c)
	return nil
}

// SetRouter sets (or, with nil, clears) the routing client; for start-up and tests.
func SetRouter(r Router) {
	roadsMu.Lock()
	roads = r
	roadsMu.Unlock()
}

func router() Router {
	roadsMu.RLock()
	defer roadsMu.RUnlock()
	return roads
}

// straightReach is today's reach: the nearer of the widest search and what the budget allows at
// the straight-line speed.
func straightReach(s geo.Effective) float64 {
	budget, _ := s.Get("sweet_spot_budget_s")
	speed, _ := s.Get("straight_line_speed_kmh")
	radius, _ := s.Get("max_search_radius_m")
	return math.Min(float64(radius), float64(speed)/3.6*float64(budget))
}

// inReach lists the scooters that can serve a point, in the order to try them, leaving out the
// except ones (W5, #209 and #211).
//
// Today (matching_by_road 0, or no routing): every scooter whose node is within the straight-line
// reach, nearest node first.
//
// With matching_by_road 1 and routing: every scooter within max_search_radius_m whose rider is
// within sweet_spot_budget_s by road, from the rider's live position (the node when the ping is
// older than signal_release_s by the server's clock), fastest first. If routing fails or answers
// straight lines, today's reach and order apply, so an outage never widens reach on guesses.
func inReach(ctx context.Context, q querier, zone string, lat, lng float64, now time.Time, s geo.Effective, except []string) ([]string, error) {
	reach := straightReach(s)
	byRoad, _ := s.Get("matching_by_road")
	r := router()
	road := byRoad == 1 && r != nil
	query := reach
	if road {
		radius, _ := s.Get("max_search_radius_m")
		query = float64(radius)
	}
	fresh, _ := s.Get("signal_release_s")
	rows, err := q.Query(ctx, `SELECT r.id::text, n.lat, n.lng, p.lat, p.lng, p.received_at
		FROM (`+reachable+`) r JOIN scooters s ON s.id = r.id JOIN staging_nodes n ON n.id = s.node_id
		LEFT JOIN shifts sh ON sh.scooter_id = s.id AND sh.ended_at IS NULL
		LEFT JOIN rider_positions p ON p.rider_id = sh.rider_id
		ORDER BY extensions.ST_Distance(
			extensions.ST_SetSRID(extensions.ST_MakePoint(n.lng, n.lat), 4326)::extensions.geography,
			extensions.ST_SetSRID(extensions.ST_MakePoint($3, $2), 4326)::extensions.geography), r.id`, zone, lat, lng, query)
	if err != nil {
		return nil, err
	}
	left := map[string]bool{}
	for _, id := range except {
		left[id] = true
	}
	var cs []matching.Candidate
	var id string
	var nlat, nlng float64
	var plat, plng *float64
	var at *time.Time
	if _, err := pgx.ForEachRow(rows, []any{&id, &nlat, &nlng, &plat, &plng, &at}, func() error {
		if left[id] {
			return nil
		}
		c := matching.Candidate{ScooterID: id, Node: routing.Point{Lat: nlat, Lng: nlng}}
		c.Position = c.Node
		if plat != nil && plng != nil && at != nil && now.Sub(*at) <= time.Duration(fresh)*time.Second {
			c.Position, c.Live = routing.Point{Lat: *plat, Lng: *plng}, true
		}
		cs = append(cs, c)
		return nil
	}); err != nil {
		return nil, err
	}
	gate := routing.Point{Lat: lat, Lng: lng}
	// today keeps today's rule: nodes within the straight-line reach, in the query's order (nearest
	// first). The query already used that reach unless road matching widened it.
	today := func() []string {
		var out []string
		for _, c := range cs {
			if !road || routing.Metres(c.Node, gate) <= reach {
				out = append(out, c.ScooterID)
			}
		}
		return out
	}
	if !road || len(cs) == 0 {
		return today(), nil
	}
	speed, _ := s.Get("straight_line_speed_kmh")
	legs, err := r.Table(ctx, matching.Sources(cs), gate, float64(max(speed, 1)))
	if err != nil {
		return today(), nil // the caller's context ended or bad input: today's reach and order
	}
	ranked, ok := matching.ByRoad(cs, gate, legs)
	if !ok {
		return today(), nil
	}
	// OSRM answering that no rider can reach the point (a snap failure, say) gives no coverage, not
	// today's list: a real answer of "no road" is not an outage, and must never widen reach.
	budget, _ := s.Get("sweet_spot_budget_s")
	var out []string
	for _, x := range ranked {
		if x.Reachable && x.RoadSeconds <= float64(budget) {
			out = append(out, x.ScooterID)
		}
	}
	return out, nil
}
