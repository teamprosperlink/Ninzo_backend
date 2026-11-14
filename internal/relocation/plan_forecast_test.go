package relocation

import (
	"context"
	"testing"
	"time"
)

// planWith stores a build whose plan for now, with n = len(nodes), carries forecast orders per entry.
func (f *fixture) planWith(now time.Time, nodes []string, orders []float64) {
	f.t.Helper()
	l := now.In(ist)
	build := f.id(`INSERT INTO placement_builds (zone_id, demand_from, demand_to, busy_share_pct, budget_s, nodes, cells, plans)
		VALUES ($1, '2026-10-01', '2026-10-28', 30, 80, 3, 1, 1) RETURNING id::text`, f.zone)
	f.exec(`INSERT INTO placement_plans (build_id, weekday, band, n, node_ids, expected, covered, node_orders)
		VALUES ($1, $2, $3, $4, $5::uuid[], 0.5, 0.6, $6::float8[])`,
		build, int(l.Weekday()), (l.Hour()*60+l.Minute())/30, len(nodes), nodes, orders)
}

// testRider starts a shift on a test scooter.
func (f *fixture) testRider(node string) string {
	f.t.Helper()
	rider, _ := f.rider(node)
	f.exec("UPDATE scooters SET is_test = true WHERE current_rider_id = $1", rider)
	return rider
}

func TestRidersOnTestScootersAreLeftOutOfThePlan(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.byPlan()
	tester := f.testRider(f.b) // waiting longest at B
	f.rider(f.b)
	f.rider(f.b)
	now := time.Now()
	// Three riders are free, but one is on a test scooter. Plans count non-test scooters only, so the
	// plan for two applies: one at B and one at C. With three counted, no plan would match and
	// today's rule, with no orders, would make no move.
	f.plan(now, f.b, f.c)
	if n, err := f.svc.Check(ctx, now); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if f.count("SELECT count(*) FROM relocations WHERE rider_id = $1", tester) != 0 {
		t.Error("the rider on a test scooter was asked to move by the plan")
	}
	if f.id("SELECT to_node_id::text FROM relocations") != f.c {
		t.Error("the move must go to C")
	}
}

func TestOnlyTestRidersMeansNoPlanMove(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.byPlan()
	f.testRider(f.b)
	f.testRider(f.b)
	now := time.Now()
	f.plan(now, f.b, f.c) // would move one, if test riders counted
	if n, err := f.svc.Check(ctx, now); err != nil || n != 0 {
		t.Fatalf("no free rider on a real scooter, no plan move: %d %v", n, err)
	}
}

func TestTodaysRuleStillCountsTestScooters(t *testing.T) {
	// The setting is off: today's rule is unchanged and still moves a rider on a test scooter.
	f := newFixture(t)
	ctx := context.Background()
	first := f.testRider(f.b)
	f.testRider(f.b)
	f.orders(3, 12.9425, 77.6365, 10*time.Minute) // near C, where nobody waits
	now := time.Now()
	if n, err := f.svc.Check(ctx, now); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if f.count("SELECT count(*) FROM relocations WHERE rider_id = $1", first) != 1 {
		t.Error("today's rule should ask the longest-waiting rider, test scooter or not")
	}
}

func TestAPlanMoveReportsTheForecast(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.byPlan()
	first, _ := f.rider(f.b)
	f.rider(f.b)
	now := time.Now()
	// The plan for two wants both at C, with 1.2 and 0.9 orders forecast: about 2 near C.
	f.planWith(now, []string{f.c, f.c}, []float64{1.2, 0.9})
	if n, err := f.svc.Check(ctx, now); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	cur, err := Current(ctx, f.pool, first, now)
	if err != nil || cur == nil {
		t.Fatalf("%v %v", cur, err)
	}
	if m := cur.(map[string]any); m["orders_waiting"] != 2 || m["reason"] != "More orders are expected here soon" {
		t.Errorf("the forecast should be reported, rounded: %v", m)
	}
}

func TestAPlanMoveWithNoForecastOrdersReportsZero(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.byPlan()
	first, _ := f.rider(f.b)
	f.rider(f.b)
	f.orders(4, 12.9425, 77.6365, 10*time.Minute) // recent orders near C do not replace the forecast
	now := time.Now()
	f.planWith(now, []string{f.b, f.c}, []float64{3, 0.4})
	if n, err := f.svc.Check(ctx, now); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if m, _ := Current(ctx, f.pool, first, now); m.(map[string]any)["orders_waiting"] != 0 {
		t.Errorf("C's forecast rounds to 0, and B's is not C's: %v", m)
	}
}

func TestAnOlderPlanReportsRecentOrders(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.byPlan()
	first, _ := f.rider(f.b)
	f.rider(f.b)
	f.orders(1, 12.9425, 77.6365, 10*time.Minute) // one recent order near C
	now := time.Now()
	f.plan(now, f.b, f.c) // a build from before #249: no forecast stored
	if n, err := f.svc.Check(ctx, now); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if m, _ := Current(ctx, f.pool, first, now); m.(map[string]any)["orders_waiting"] != 1 {
		t.Errorf("with no forecast, the node's recent orders are reported as before: %v", m)
	}
}
