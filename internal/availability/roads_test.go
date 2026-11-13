package availability

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/routing"
)

// byLat is a fake router: the road time from a source depends on its latitude, as listed.
type byLat struct {
	secs   map[float64]float64
	source routing.Source
	calls  int
}

func (b *byLat) Table(_ context.Context, sources []routing.Point, _ routing.Point, _ float64) ([]routing.Leg, error) {
	b.calls++
	src := b.source
	if src == "" {
		src = routing.SourceOSRM
	}
	out := make([]routing.Leg, len(sources))
	for i, s := range sources {
		secs, ok := b.secs[math.Round(s.Lat*1e4)/1e4]
		out[i] = routing.Leg{Seconds: secs, Reachable: ok, Source: src}
	}
	return out, nil
}

// Road-time matching (#209): with matching_by_road on, the scooter whose rider is faster by road
// is tried first, even when the other is nearer in a straight line; off, or with no routing, the
// order is today's.
func TestMatchingByRoadPicksTheFasterRider(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	t.Cleanup(func() { SetRouter(nil) })
	// A second scooter 300 m south of the customer, nearer by road than the first (220 m north,
	// across a median, say). Both are within today's 333 m straight-line reach.
	node2 := f.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, 'N2', 12.9323, 77.625, 4) RETURNING id::text", f.zone)
	scooter2 := f.id("INSERT INTO scooters (code, zone_id, node_id) VALUES ('SC-2', $1, $2) RETURNING id::text", f.zone, node2)
	person := f.id("INSERT INTO people (phone) VALUES ('+919800000509') RETURNING id::text")
	rider2 := f.id("INSERT INTO riders (person_id, status, zone_id) VALUES ($1, 'active', $2) RETURNING id::text", person, f.zone)
	f.exec("UPDATE scooters SET current_rider_id = $1 WHERE id = $2", rider2, scooter2)
	f.exec("INSERT INTO shifts (rider_id, node_id, scooter_id) VALUES ($1, $2, $3)", rider2, node2, scooter2)
	lot := f.id("INSERT INTO lots (code, item_id, qty, expires_at) VALUES ('L-CHAI-9', $1, 5, now() + interval '1 day') RETURNING id::text", f.chai)
	f.exec("INSERT INTO scooter_stock (scooter_id, lot_id, on_hand) VALUES ($1, $2, 5)", scooter2, lot)
	now := time.Now().UTC()
	// Both riders pinged just now from beside their scooters.
	f.exec(`INSERT INTO rider_positions (rider_id, lat, lng, speed_mps, accuracy_m, device_time, received_at) VALUES
		($1, 12.9370, 77.625, 0, 5, $3, $3), ($2, 12.9322, 77.625, 0, 5, $3, $3)`, f.rider, rider2, now)
	pick := func() string {
		t.Helper()
		var p Pick
		if err := database.InTx(ctx, f.pool, func(tx pgx.Tx) error {
			var err error
			p, err = PickScooter(ctx, tx, custLat, custLng, now, map[string]int{f.chai: 1})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return p.ScooterID
	}
	setting := func(v int) {
		t.Helper()
		if err := database.InTx(ctx, f.pool, func(tx pgx.Tx) error {
			_, err := geo.ChangeSettings(ctx, tx, "", []geo.Change{{Name: "matching_by_road", Value: &v}}, "matching test", "test")
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	roads := &byLat{secs: map[float64]float64{12.937: 200, 12.9322: 50, 12.9323: 300}}
	SetRouter(roads)

	if got := pick(); got != f.scooter || roads.calls != 0 {
		t.Fatalf("setting off: picked %s with %d routing calls, want today's nearest %s and none", got, roads.calls, f.scooter)
	}
	setting(1)
	if got := pick(); got != scooter2 || roads.calls != 1 {
		t.Fatalf("setting on: picked %s with %d calls, want %s, faster by road", got, roads.calls, scooter2)
	}
	// Rider 2's ping goes stale: they are taken to be at their node, which is slow by road here.
	f.exec("UPDATE rider_positions SET device_time = $2, received_at = $2 WHERE rider_id = $1", rider2, now.Add(-10*time.Minute))
	if got := pick(); got != f.scooter {
		t.Fatalf("stale position: picked %s, want %s (node 2 is 300 s by road)", got, f.scooter)
	}
	f.exec("UPDATE rider_positions SET device_time = $2, received_at = $2 WHERE rider_id = $1", rider2, now)
	// Routing answering straight lines: today's order, never a guess.
	SetRouter(&byLat{secs: roads.secs, source: routing.SourcePlaceholder})
	if got := pick(); got != f.scooter {
		t.Fatalf("routing down: picked %s, want today's %s", got, f.scooter)
	}
	// Routing failing outright, or answering for fewer riders than asked: today's order.
	SetRouter(failing{})
	if got := pick(); got != f.scooter {
		t.Fatalf("routing error: picked %s, want today's %s", got, f.scooter)
	}
	SetRouter(short{})
	if got := pick(); got != f.scooter {
		t.Fatalf("short answer: picked %s, want today's %s", got, f.scooter)
	}
	// No routing configured: today's order.
	SetRouter(nil)
	if got := pick(); got != f.scooter {
		t.Fatalf("no routing: picked %s, want today's %s", got, f.scooter)
	}
}

func TestConfigureRouting(t *testing.T) {
	t.Cleanup(func() { SetRouter(nil) })
	if err := ConfigureRouting(func(string) string { return "" }); err != nil || router() != nil {
		t.Fatalf("empty URL: %v %v", err, router())
	}
	if err := ConfigureRouting(func(k string) string { return map[string]string{"ROUTING_ENGINE_URL": "localhost:5000"}[k] }); err == nil {
		t.Fatal("a URL with no scheme was accepted")
	}
	if err := ConfigureRouting(func(k string) string { return map[string]string{"ROUTING_ENGINE_URL": "http://osrm:5000"}[k] }); err != nil || router() == nil {
		t.Fatalf("a good URL: %v", err)
	}
}

type failing struct{}

func (failing) Table(context.Context, []routing.Point, routing.Point, float64) ([]routing.Leg, error) {
	return nil, context.DeadlineExceeded
}

// short answers for one rider fewer than asked, the fastest first, as a broken router might.
type short struct{}

func (short) Table(_ context.Context, sources []routing.Point, _ routing.Point, _ float64) ([]routing.Leg, error) {
	out := make([]routing.Leg, len(sources)-1)
	for i := range out {
		out[i] = routing.Leg{Seconds: 1, Reachable: true, Source: routing.SourceOSRM}
	}
	return out, nil
}
