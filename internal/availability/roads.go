package availability

import (
	"context"
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

// orderByRoad reorders the scooters that passed the filters by road time from where each rider is
// to the gate. A rider whose last ping reached the server longer ago than signal_release_s (the
// server's clock, not the phone's) is taken to be at the
// scooter's node. Without a routing client, or when routing answers straight lines, the order
// stays today's.
func orderByRoad(ctx context.Context, tx pgx.Tx, ids []string, lat, lng float64, now time.Time, s geo.Effective) ([]string, error) {
	r := router()
	if r == nil {
		return ids, nil
	}
	fresh, _ := s.Get("signal_release_s")
	speed, _ := s.Get("straight_line_speed_kmh")
	rows, err := tx.Query(ctx, `
		SELECT s.id::text, n.lat, n.lng, p.lat, p.lng, p.received_at
		FROM scooters s JOIN staging_nodes n ON n.id = s.node_id
		LEFT JOIN shifts sh ON sh.scooter_id = s.id AND sh.ended_at IS NULL
		LEFT JOIN rider_positions p ON p.rider_id = sh.rider_id
		WHERE s.id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	byID := map[string]matching.Candidate{}
	var id string
	var nlat, nlng float64
	var plat, plng *float64
	var at *time.Time
	if _, err := pgx.ForEachRow(rows, []any{&id, &nlat, &nlng, &plat, &plng, &at}, func() error {
		c := matching.Candidate{ScooterID: id, Node: routing.Point{Lat: nlat, Lng: nlng}}
		c.Position = c.Node
		if plat != nil && plng != nil && at != nil && now.Sub(*at) <= time.Duration(fresh)*time.Second {
			c.Position, c.Live = routing.Point{Lat: *plat, Lng: *plng}, true
		}
		byID[id] = c
		return nil
	}); err != nil {
		return nil, err
	}
	cs := make([]matching.Candidate, 0, len(ids))
	for _, id := range ids {
		if c, ok := byID[id]; ok {
			cs = append(cs, c)
		}
	}
	if len(cs) != len(ids) {
		return ids, nil // a scooter vanished between the two reads: keep today's order
	}
	gate := routing.Point{Lat: lat, Lng: lng}
	legs, err := r.Table(ctx, matching.Sources(cs), gate, float64(max(speed, 1)))
	if err != nil {
		// The caller's context ended, or bad input: keep today's order rather than fail the order.
		return ids, nil
	}
	ranked, _ := matching.ByRoad(cs, gate, legs)
	return matching.IDs(ranked), nil
}
