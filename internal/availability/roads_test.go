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
	// All within the 80 s budget (#211 refuses past it), so this checks the order: rider 2 live is
	// fastest; at its node (stale) it is slowest.
	roads := &byLat{secs: map[float64]float64{12.937: 70, 12.9322: 50, 12.9323: 75}}
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
		t.Fatalf("stale position: picked %s, want %s (node 2 is 75 s by road, scooter 1 70 s)", got, f.scooter)
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

// With matching_by_road on, reach is by road time (#211): a scooter past today's straight-line
// reach but inside the sweet spot by road can take the order, one inside the line but past the
// budget by road cannot, and the menu (At) agrees with the pick.
func TestWithMatchingByRoadReachIsByRoadTime(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	t.Cleanup(func() { SetRouter(nil) })
	// A second scooter 500 m south: past today's 333 m reach, but 60 s by road.
	node2 := f.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, 'N2', 12.9305, 77.625, 4) RETURNING id::text", f.zone)
	scooter2 := f.id("INSERT INTO scooters (code, zone_id, node_id) VALUES ('SC-2', $1, $2) RETURNING id::text", f.zone, node2)
	person := f.id("INSERT INTO people (phone) VALUES ('+919800000511') RETURNING id::text")
	rider2 := f.id("INSERT INTO riders (person_id, status, zone_id) VALUES ($1, 'active', $2) RETURNING id::text", person, f.zone)
	f.exec("UPDATE scooters SET current_rider_id = $1 WHERE id = $2", rider2, scooter2)
	f.exec("INSERT INTO shifts (rider_id, node_id, scooter_id) VALUES ($1, $2, $3)", rider2, node2, scooter2)
	lot := f.id("INSERT INTO lots (code, item_id, qty, expires_at) VALUES ('L-CHAI-8', $1, 2, now() + interval '1 day') RETURNING id::text", f.chai)
	f.exec("INSERT INTO scooter_stock (scooter_id, lot_id, on_hand) VALUES ($1, $2, 2)", scooter2, lot)
	now := time.Now().UTC()
	pick := func() (string, string) {
		t.Helper()
		var p Pick
		if err := database.InTx(ctx, f.pool, func(tx pgx.Tx) error {
			var err error
			p, err = PickScooter(ctx, tx, custLat, custLng, now, map[string]int{f.chai: 1})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return p.ScooterID, p.Reason
	}
	menu := func() (int, string) {
		t.Helper()
		res, err := At(ctx, f.pool, custLat, custLng, now)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range res.Lines {
			if l.Item.ID == f.chai {
				return l.QtyLeft, res.Reason
			}
		}
		return 0, res.Reason
	}
	setting := func(v int) {
		t.Helper()
		if err := database.InTx(ctx, f.pool, func(tx pgx.Tx) error {
			_, err := geo.ChangeSettings(ctx, tx, "", []geo.Change{{Name: "matching_by_road", Value: &v}}, "reach test", "test")
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Nodes: scooter 1 at 12.937 (220 m north, 200 s by road), scooter 2 at 12.9305 (500 m, 60 s).
	roads := &byLat{secs: map[float64]float64{12.937: 200, 12.9305: 60}}
	SetRouter(roads)

	// Off: today's reach. Only scooter 1 (8 chai) is in reach; scooter 2 is too far by line.
	if got, _ := pick(); got != f.scooter {
		t.Fatalf("off: picked %s, want %s", got, f.scooter)
	}
	if qty, _ := menu(); qty != 8 {
		t.Fatalf("off: the menu offers %d chai, want 8 (scooter 1 only)", qty)
	}
	if roads.calls != 0 {
		t.Fatalf("off: %d routing calls from the pick and the menu, want none", roads.calls)
	}
	// On: by road. Scooter 2 is within 80 s; scooter 1 (200 s) is past the budget.
	setting(1)
	if got, _ := pick(); got != scooter2 {
		t.Fatalf("on: picked %s, want %s (60 s by road, 500 m by line)", got, scooter2)
	}
	if qty, _ := menu(); qty != 2 {
		t.Fatalf("on: the menu offers %d chai, want 2 (scooter 2 only: scooter 1 is past the budget)", qty)
	}
	// On, but scooter 2 slower too: nobody within the budget, so no coverage, in both.
	SetRouter(&byLat{secs: map[float64]float64{12.937: 200, 12.9305: 120}})
	if got, reason := pick(); got != "" || reason != ReasonNoCoverage {
		t.Fatalf("nobody within the budget: %q %q", got, reason)
	}
	if _, reason := menu(); reason != ReasonNoCoverage {
		t.Fatalf("menu with nobody in budget: %q", reason)
	}
	// On, routing down: today's reach again, never a guess at wider reach.
	SetRouter(&byLat{secs: map[float64]float64{12.937: 1, 12.9305: 1}, source: routing.SourcePlaceholder})
	if got, _ := pick(); got != f.scooter {
		t.Fatalf("routing down: picked %s, want today's %s", got, f.scooter)
	}
	if qty, _ := menu(); qty != 8 {
		t.Fatalf("routing down: the menu offers %d, want today's 8", qty)
	}
	// An item only the far scooter holds: with routing down it is past today's reach, so it is
	// neither offered nor picked.
	vada := f.id("INSERT INTO menu_items (name, price_paise, status, sort_order) VALUES ('Vada', 2900, 'active', 3) RETURNING id::text")
	vlot := f.id("INSERT INTO lots (code, item_id, qty, expires_at) VALUES ('L-VADA-1', $1, 3, now() + interval '1 day') RETURNING id::text", vada)
	f.exec("INSERT INTO scooter_stock (scooter_id, lot_id, on_hand) VALUES ($1, $2, 3)", scooter2, vlot)
	res, err := At(ctx, f.pool, custLat, custLng, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range res.Lines {
		if l.Item.ID == vada {
			t.Fatal("routing down: the menu offers an item only a scooter past today's reach holds")
		}
	}
	var p Pick
	if err := database.InTx(ctx, f.pool, func(tx pgx.Tx) error {
		var err error
		p, err = PickScooter(ctx, tx, custLat, custLng, now, map[string]int{vada: 1})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if p.ScooterID != "" {
		t.Fatalf("routing down: picked %s for an item only past today's reach", p.ScooterID)
	}
}
