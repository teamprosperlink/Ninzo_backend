package placementplan

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/teamprosperlink/Ninzo_backend/internal/demand"
	"github.com/teamprosperlink/Ninzo_backend/internal/hexgrid"
)

// paid adds n paid orders at a gate at a point, weeksBack weeks before now, in band 38.
func (f *fixture) paid(lat, lng float64, weeksBack, n int) {
	f.t.Helper()
	at := time.Date(2026, 11, 13, 19, 10, 0, 0, now.Location()).AddDate(0, 0, -7*weeksBack)
	scooter := f.id("SELECT id::text FROM scooters WHERE NOT is_test LIMIT 1")
	for i := range n {
		person := f.id("INSERT INTO people (phone) VALUES ($1) RETURNING id::text", fmt.Sprintf("+9198%02d%02d%04d", weeksBack, i, int(lng*1000)%10000))
		cust := f.id("INSERT INTO customers (person_id) VALUES ($1) RETURNING id::text", person)
		gate := f.id("INSERT INTO gates (customer_id, zone_id, label, lat, lng) VALUES ($1, $2, 'G', $3, $4) RETURNING id::text", cust, f.zone, lat, lng)
		hist := f.id("INSERT INTO gate_history (gate_id, version, label, lat, lng, verified, reason) VALUES ($1, 1, 'G', $2, $3, false, 'test') RETURNING id::text", gate, lat, lng)
		f.id(`INSERT INTO orders (customer_id, idempotency_key, gate_id, gate_history_id, zone_id, scooter_id, global_settings, state, total_paise, hold_until, paid_at)
			VALUES ($1, 'k', $2, $3, $4, $5, (SELECT id FROM zone_settings WHERE zone_id IS NULL ORDER BY version DESC LIMIT 1), 'COMPLETED', 9900, now(), $6) RETURNING id::text`,
			cust, gate, hist, f.zone, scooter, at)
	}
}

func TestPlansCarryTheForecastOrdersOfTheCellsEachEntryCovers(t *testing.T) {
	f := newFixture(t, 3)
	ctx := context.Background()
	for wk := 1; wk <= 2; wk++ {
		f.ask(12.935, 77.617, wk, 6) // heavy, near W
		f.ask(12.935, 77.635, wk, 2) // light, near E
		f.paid(12.935, 77.617, wk, 3)
		f.paid(12.935, 77.635, wk, 1)
	}
	if _, err := Build(ctx, f.pool, roads{}, f.zone, now); err != nil {
		t.Fatal(err)
	}
	// The expected forecast per side, by hand, from the same paid history.
	counts, err := demand.Counts(ctx, f.pool, now.AddDate(0, 0, -28), now.AddDate(0, 0, -1))
	if err != nil {
		t.Fatal(err)
	}
	h := demand.NewHistory(counts, demand.SlotOf(now).Day.AddDate(0, 0, -28), func(c demand.Count) float64 { return float64(c.Paid) })
	g := hexgrid.Bengaluru()
	forecast := func(lat, lng float64) float64 {
		c, err := g.CellAt(hexgrid.Point{Lat: lat, Lng: lng})
		if err != nil {
			t.Fatal(err)
		}
		v, ok := h.Forecast(g.ID(c), demand.Slot{Day: demand.SlotOf(now).Day, Band: 38}, 4, decay)
		if !ok || v <= 0 {
			t.Fatalf("no paid forecast at %v,%v", lat, lng)
		}
		return v
	}
	west, east := forecast(12.935, 77.617), forecast(12.935, 77.635)
	at := now.Add(-50 * time.Minute) // 19:10 IST, a Friday
	p1, ok, err := Latest(ctx, f.pool, f.zone, at, 1)
	if err != nil || !ok || p1.Nodes[0] != f.west || len(p1.NodeOrders) != 1 || math.Abs(p1.NodeOrders[0]-west) > 1e-9 {
		t.Fatalf("one scooter at W carries W's forecast %v: %+v %v %v", west, p1, ok, err)
	}
	p2, _, _ := Latest(ctx, f.pool, f.zone, at, 2)
	if len(p2.NodeOrders) != 2 {
		t.Fatalf("%+v", p2)
	}
	// The plan for two is the plan for one plus E; E's entry has E's cells, W's entry keeps W's.
	got := map[string]float64{}
	for i, id := range p2.Nodes {
		got[id] += p2.NodeOrders[i]
	}
	if math.Abs(got[f.west]-west) > 1e-9 || math.Abs(got[f.east]-east) > 1e-9 {
		t.Fatalf("want W %v E %v, got %+v", west, east, p2)
	}
	// A third scooter covers nothing new: its entry has no orders, and the total is unchanged.
	p3, _, _ := Latest(ctx, f.pool, f.zone, at, 3)
	total := 0.0
	for _, o := range p3.NodeOrders {
		total += o
	}
	if len(p3.NodeOrders) != 3 || p3.NodeOrders[2] != 0 || math.Abs(total-west-east) > 1e-9 {
		t.Fatalf("%+v", p3)
	}
}

func TestOrdersAreForecastOnlyFromPaidOrders(t *testing.T) {
	f := newFixture(t, 2)
	for wk := 1; wk <= 2; wk++ {
		f.ask(12.935, 77.617, wk, 6) // asked, but nobody paid
	}
	if _, err := Build(context.Background(), f.pool, roads{}, f.zone, now); err != nil {
		t.Fatal(err)
	}
	p, ok, err := Latest(context.Background(), f.pool, f.zone, now.Add(-50*time.Minute), 1)
	if err != nil || !ok || len(p.NodeOrders) != 1 || p.NodeOrders[0] != 0 {
		t.Fatalf("asking is not ordering: %+v %v %v", p, ok, err)
	}
}

func TestAPlanFromBeforeTheForecastReadsAsNone(t *testing.T) {
	f := newFixture(t, 1)
	l := now.Add(-50 * time.Minute)
	build := f.id(`INSERT INTO placement_builds (zone_id, demand_from, demand_to, busy_share_pct, budget_s, nodes, cells, plans)
		VALUES ($1, '2026-10-01', '2026-10-28', 30, 80, 2, 1, 1) RETURNING id::text`, f.zone)
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO placement_plans (build_id, weekday, band, n, node_ids, expected, covered)
		VALUES ($1, 5, 38, 1, $2::uuid[], 0.5, 0.6)`, build, []string{f.west}); err != nil {
		t.Fatal(err)
	}
	p, ok, err := Latest(context.Background(), f.pool, f.zone, l, 1)
	if err != nil || !ok || p.NodeOrders != nil {
		t.Fatalf("%+v %v %v", p, ok, err)
	}
	// The column refuses a forecast of the wrong length, or below zero.
	for _, bad := range [][]float64{{1, 2}, {-1}} {
		if _, err := f.pool.Exec(context.Background(), `INSERT INTO placement_plans (build_id, weekday, band, n, node_ids, expected, covered, node_orders)
			VALUES ($1, 5, 39, 1, $2::uuid[], 0.5, 0.6, $3::float8[])`, build, []string{f.west}, bad); err == nil {
			t.Errorf("stored node_orders %v for n = 1", bad)
		}
	}
}

func TestNodeOrdersRefuseNaNAndInfinity(t *testing.T) {
	f := newFixture(t, 1)
	build := f.id(`INSERT INTO placement_builds (zone_id, demand_from, demand_to, busy_share_pct, budget_s, nodes, cells, plans)
		VALUES ($1, '2026-10-01', '2026-10-28', 30, 80, 2, 1, 1) RETURNING id::text`, f.zone)
	for _, bad := range []float64{math.NaN(), math.Inf(1)} {
		if _, err := f.pool.Exec(context.Background(), `INSERT INTO placement_plans (build_id, weekday, band, n, node_ids, expected, covered, node_orders)
			VALUES ($1, 5, 40, 1, $2::uuid[], 0.5, 0.6, $3::float8[])`, build, []string{f.west}, []float64{bad}); err == nil {
			t.Errorf("stored node_orders %v", bad)
		}
	}
}
