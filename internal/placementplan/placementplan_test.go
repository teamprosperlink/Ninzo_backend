package placementplan

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/demand"
	"github.com/teamprosperlink/Ninzo_backend/internal/placement"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
	"github.com/teamprosperlink/Ninzo_backend/internal/routing"
)

// roads answers straight lines at 5 m/s, marked as from OSRM (or not, when fake is false).
type roads struct{ placeholder bool }

func (r roads) Table(_ context.Context, sources []routing.Point, dest routing.Point, _ float64) ([]routing.Leg, error) {
	out := make([]routing.Leg, len(sources))
	src := routing.SourceOSRM
	if r.placeholder {
		src = routing.SourcePlaceholder
	}
	for i, s := range sources {
		dy := (s.Lat - dest.Lat) * 111320
		dx := (s.Lng - dest.Lng) * 111320 * math.Cos(dest.Lat*math.Pi/180)
		m := math.Hypot(dx, dy)
		out[i] = routing.Leg{Seconds: m / 5, Metres: m, Reachable: true, Source: src}
	}
	return out, nil
}

// A Friday evening in IST: band 38 is 19:00–19:30.
var now = time.Date(2026, 11, 13, 20, 0, 0, 0, time.FixedZone("IST", 5*3600+1800))

type fixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	zone string
	west string // node near the west demand
	east string // node near the east demand
	test string // a test node, never used
}

func (f *fixture) id(sql string, args ...any) string {
	f.t.Helper()
	var s string
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

func newFixture(t *testing.T, scooters int) *fixture {
	t.Helper()
	f := &fixture{t: t, pool: dbtest.Fresh(t)}
	f.zone = f.id(`INSERT INTO zones (name, city, status, area) VALUES ('Z', 'Bengaluru', 'active',
		extensions.ST_GeomFromText('MULTIPOLYGON(((77.612 12.925, 77.640 12.925, 77.640 12.945, 77.612 12.945, 77.612 12.925)))', 4326)) RETURNING id::text`)
	f.west = f.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, 'W', 12.935, 77.616, 2) RETURNING id::text", f.zone)
	f.east = f.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, 'E', 12.935, 77.636, 1) RETURNING id::text", f.zone)
	f.test = f.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity, is_test) VALUES ($1, 'T', 12.935, 77.626, 5, true) RETURNING id::text", f.zone)
	for i := range scooters {
		f.id("INSERT INTO scooters (code, zone_id, node_id) VALUES ($1, $2, $3) RETURNING id::text", fmt.Sprintf("SC-%d", i), f.zone, f.west)
	}
	f.id("INSERT INTO scooters (code, zone_id, node_id, is_test) VALUES ('SC-T', $1, $2, true) RETURNING id::text", f.zone, f.test)
	return f
}

// ask records n customers asking at a point, weeksBack weeks before now, in band 38.
func (f *fixture) ask(lat, lng float64, weeksBack, n int) {
	f.t.Helper()
	at := time.Date(2026, 11, 13, 19, 5, 0, 0, now.Location()).AddDate(0, 0, -7*weeksBack)
	for i := range n {
		if err := demand.Record(context.Background(), f.pool, fmt.Sprintf("c-%v-%v-%d-%d", lat, lng, weeksBack, i), lat, lng, at, demand.Offered); err != nil {
			f.t.Fatal(err)
		}
	}
}

func TestABuildPlansOnlyWhereAndWhenDemandIs(t *testing.T) {
	f := newFixture(t, 4)
	for wk := 1; wk <= 3; wk++ {
		f.ask(12.935, 77.617, wk, 6) // heavy, near W
		f.ask(12.935, 77.635, wk, 2) // light, near E
	}
	f.ask(12.99, 77.70, 1, 9) // outside the zone: never counted
	res, err := Build(context.Background(), f.pool, roads{}, f.zone, now)
	if err != nil {
		t.Fatal(err)
	}
	// Capacity 2 + 1 = 3 (the test node is left out), scooters 4: plans for n = 1, 2, 3, one slot.
	if res.Nodes != 2 || res.Cells != 2 || res.MaxN != 3 || res.PlanCount != 3 {
		t.Fatalf("%+v", res)
	}
	for _, p := range res.Plans {
		if p.Weekday != 5 || p.Band != 38 || len(p.Nodes) != p.N || slices.Contains(p.Nodes, f.test) {
			t.Fatalf("%+v", p)
		}
	}
	p1, ok, err := Latest(context.Background(), f.pool, f.zone, now.Add(-50*time.Minute), 1) // 19:10 IST, a Friday
	if err != nil || !ok || p1.Nodes[0] != f.west {
		t.Fatalf("one scooter goes to the heavy side: %+v %v %v", p1, ok, err)
	}
	p2, _, _ := Latest(context.Background(), f.pool, f.zone, now.Add(-50*time.Minute), 2)
	if !slices.Contains(p2.Nodes, f.east) || !slices.Contains(p2.Nodes, f.west) || p2.Covered != 1 {
		t.Fatalf("two scooters cover both sides: %+v", p2)
	}
	if _, ok, _ := Latest(context.Background(), f.pool, f.zone, now.Add(-50*time.Minute), 4); ok {
		t.Fatal("a plan for more scooters than the nodes hold")
	}
	if _, ok, _ := Latest(context.Background(), f.pool, f.zone, now.Add(-50*time.Minute).AddDate(0, 0, 1), 1); ok {
		t.Fatal("a plan on a Saturday, with no Saturday demand")
	}
}

func TestTheStoredPlanIsTheGreedyPlan(t *testing.T) {
	f := newFixture(t, 3)
	for wk := 1; wk <= 2; wk++ {
		f.ask(12.935, 77.617, wk, 3)
		f.ask(12.940, 77.630, wk, 4)
		f.ask(12.935, 77.635, wk, 2)
	}
	res, err := Build(context.Background(), f.pool, roads{}, f.zone, now)
	if err != nil {
		t.Fatal(err)
	}
	// The same inputs by hand: nodes sorted by id, cells sorted, forecast over 4 weeks (only 2 known).
	nodes, _ := zoneNodes(context.Background(), f.pool, f.zone)
	counts, _ := demand.Counts(context.Background(), f.pool, now.AddDate(0, 0, -28), now.AddDate(0, 0, -1))
	h := demand.NewHistory(counts, demand.SlotOf(now).Day.AddDate(0, 0, -28), func(c demand.Count) float64 { return float64(c.Asked) })
	cells, centres, _ := zoneCells(context.Background(), f.pool, f.zone, h)
	at := []routing.Point{nodes[0].at, nodes[1].at}
	cov, _ := placement.Matrix(context.Background(), roads{}, at, centres, 80, 15)
	w := make([]float64, len(cells))
	for i, c := range cells {
		w[i], _ = h.Forecast(c, demand.Slot{Day: demand.SlotOf(now).Day, Band: 38}, 4, decay)
	}
	for n := 1; n <= 3; n++ {
		want, err := placement.GreedyCaps(cov, w, n, 0.3, []int{nodes[0].capacity, nodes[1].capacity})
		if err != nil {
			t.Fatal(err)
		}
		got, ok, err := Latest(context.Background(), f.pool, f.zone, now.Add(-50*time.Minute), n)
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
		var ids []string
		for _, c := range want.Picks {
			ids = append(ids, nodes[c].id)
		}
		if !slices.Equal(got.Nodes, ids) || math.Abs(got.Expected-want.Expected) > 1e-12 || math.Abs(got.Covered-want.Covered) > 1e-12 {
			t.Fatalf("n %d: stored %+v, greedy %v %+v", n, got, ids, want)
		}
	}
	if res.PlanCount != 3 {
		t.Fatalf("%+v", res)
	}
}

func TestTheLatestBuildWins(t *testing.T) {
	f := newFixture(t, 1)
	f.ask(12.935, 77.635, 1, 3) // only east demand: one scooter goes east
	if _, err := Build(context.Background(), f.pool, roads{}, f.zone, now); err != nil {
		t.Fatal(err)
	}
	f.ask(12.935, 77.617, 2, 20) // later, much west demand two weeks back
	if _, err := Build(context.Background(), f.pool, roads{}, f.zone, now); err != nil {
		t.Fatal(err)
	}
	p, ok, err := Latest(context.Background(), f.pool, f.zone, now.Add(-50*time.Minute), 1)
	if err != nil || !ok || p.Nodes[0] != f.west {
		t.Fatalf("%+v %v %v", p, ok, err)
	}
	var builds int
	if err := f.pool.QueryRow(context.Background(), "SELECT count(*) FROM placement_builds").Scan(&builds); err != nil || builds != 2 {
		t.Fatalf("%d builds: %v", builds, err)
	}
	if _, err := f.pool.Exec(context.Background(), "DELETE FROM placement_plans"); err == nil {
		t.Fatal("plans are not append-only")
	}
}

func TestABuildIsRefusedWithoutWhatItNeeds(t *testing.T) {
	f := newFixture(t, 2)
	if _, err := Build(context.Background(), f.pool, roads{}, f.zone, now); !errors.Is(err, ErrNoDemand) {
		t.Fatalf("no demand: %v", err)
	}
	f.ask(12.935, 77.620, 1, 2)
	if _, err := Build(context.Background(), f.pool, roads{placeholder: true}, f.zone, now); !errors.Is(err, placement.ErrNoRoads) {
		t.Fatalf("straight lines: %v", err)
	}
	empty := f.id(`INSERT INTO zones (name, city, status, area) VALUES ('E', 'Bengaluru', 'active',
		extensions.ST_GeomFromText('MULTIPOLYGON(((77.70 12.98, 77.71 12.98, 77.71 12.99, 77.70 12.99, 77.70 12.98)))', 4326)) RETURNING id::text`)
	if _, err := Build(context.Background(), f.pool, roads{}, empty, now); !errors.Is(err, ErrNoNodes) {
		t.Fatalf("no nodes: %v", err)
	}
	f.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, 'N', 12.985, 77.705, 2) RETURNING id::text", empty)
	if _, err := Build(context.Background(), f.pool, roads{}, empty, now); !errors.Is(err, ErrNoScooters) {
		t.Fatalf("no scooters: %v", err)
	}
	var builds int
	if err := f.pool.QueryRow(context.Background(), "SELECT count(*) FROM placement_builds").Scan(&builds); err != nil || builds != 0 {
		t.Fatalf("a refused build stored %d: %v", builds, err)
	}
}

func TestTheWindowIsPlacementWeeksLong(t *testing.T) {
	f := newFixture(t, 1)
	f.ask(12.935, 77.635, 4, 2) // four weeks back: inside placement_weeks (4)
	f.ask(12.935, 77.617, 5, 9) // five weeks back: outside
	if _, err := Build(context.Background(), f.pool, roads{}, f.zone, now); err != nil {
		t.Fatal(err)
	}
	var from, to string
	if err := f.pool.QueryRow(context.Background(), "SELECT demand_from::text, demand_to::text FROM placement_builds").Scan(&from, &to); err != nil {
		t.Fatal(err)
	}
	if from != "2026-10-16" || to != "2026-11-12" {
		t.Fatalf("window %s to %s, want 2026-10-16 to 2026-11-12", from, to)
	}
	if p, ok, _ := Latest(context.Background(), f.pool, f.zone, now.Add(-50*time.Minute), 1); !ok || p.Nodes[0] != f.east {
		t.Fatalf("only the demand inside the window counts: %+v %v", p, ok)
	}
}
