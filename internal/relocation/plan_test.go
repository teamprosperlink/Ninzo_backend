package relocation

import (
	"context"
	"testing"
	"time"
)

var ist = time.FixedZone("IST", 5*3600+1800)

// byPlan turns relocation_by_plan on for the fixture's zone.
func (f *fixture) byPlan() {
	f.t.Helper()
	f.exec("INSERT INTO zone_settings (zone_id, version, relocation_by_plan, reason, created_by) VALUES ($1, 1, 1, 'test', 'test')", f.zone)
}

// plan stores a build whose plan for now's weekday and band, with n = len(nodes), puts riders at
// nodes.
func (f *fixture) plan(now time.Time, nodes ...string) {
	f.t.Helper()
	l := now.In(ist)
	build := f.id(`INSERT INTO placement_builds (zone_id, demand_from, demand_to, busy_share_pct, budget_s, nodes, cells, plans)
		VALUES ($1, '2026-10-01', '2026-10-28', 30, 80, 3, 1, 1) RETURNING id::text`, f.zone)
	f.exec(`INSERT INTO placement_plans (build_id, weekday, band, n, node_ids, expected, covered) VALUES ($1, $2, $3, $4, $5::uuid[], 0.5, 0.6)`,
		build, int(l.Weekday()), (l.Hour()*60+l.Minute())/30, len(nodes), nodes)
}

func TestByPlanOffIsTodaysRule(t *testing.T) {
	// The setting is off by default: with a plan stored, today's rule still decides.
	f := newFixture(t)
	ctx := context.Background()
	f.rider(f.b)
	f.rider(f.b)
	now := time.Now()
	f.plan(now, f.a, f.b) // the plan would move one rider to A
	if n, err := f.svc.Check(ctx, now); err != nil || n != 0 {
		t.Fatalf("with no orders, today's rule makes no move: %d %v", n, err)
	}
}

func TestAMoveFollowsThePlan(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.byPlan()
	first, _ := f.rider(f.b) // waiting longest at B
	f.rider(f.b)
	now := time.Now()
	// Two free riders; the plan for two wants one at B and one at C. No orders at all: the plan, not
	// recent orders, decides.
	f.plan(now, f.b, f.c)
	if n, err := f.svc.Check(ctx, now); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	id := f.suggestion(first)
	if f.id("SELECT to_node_id::text FROM relocations WHERE id = $1", id) != f.c || f.id("SELECT from_node_id::text FROM relocations WHERE id = $1", id) != f.b {
		t.Error("the move must go from B's surplus to C, which the plan wants")
	}
	// The same flow: a suggestion, told to the rider, read back by Current.
	cur, err := Current(ctx, f.pool, first, now)
	if err != nil || cur == nil {
		t.Fatalf("%v %v", cur, err)
	}
	if m := cur.(map[string]any); m["node_id"] != f.c || m["reason"] != "More orders are expected here soon" {
		t.Errorf("%v", m)
	}
	if f.count("SELECT count(*) FROM outbox_events WHERE topic = 'push.rider'") != 1 {
		t.Error("the rider was not told")
	}
	if n, _ := f.svc.Check(ctx, now); n != 0 {
		t.Error("a second move toward the same node")
	}
}

func TestNoPlanForNowFallsBackToTodaysRule(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.byPlan()
	first, _ := f.rider(f.b)
	f.rider(f.b)
	f.rider(f.a)
	f.rider(f.a)
	f.orders(3, 12.9425, 77.6365, 10*time.Minute) // near C, where nobody waits
	now := time.Now()
	f.plan(now.Add(-2*time.Hour), f.a, f.a, f.b, f.b) // another band: no plan for now
	f.plan(now, f.a, f.c)                             // n = 2, but four riders are free
	if n, err := f.svc.Check(ctx, now); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if m, _ := Current(ctx, f.pool, first, now); m.(map[string]any)["reason"] != "3 orders expected, nobody nearby" {
		t.Errorf("today's rule should have made this move: %v", m)
	}
}

func TestNoMoveWhenTheRidersMatchThePlan(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.byPlan()
	f.rider(f.a)
	f.rider(f.c)
	f.orders(5, 12.9321, 77.6221, 5*time.Minute) // near B: today's rule would move someone
	now := time.Now()
	f.plan(now, f.c, f.a)
	if n, err := f.svc.Check(ctx, now); err != nil || n != 0 {
		t.Fatalf("the riders are where the plan wants them: %d %v", n, err)
	}
}

func TestADonorAtItsPlanTargetIsNotAsked(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.byPlan()
	f.rider(f.b)
	f.rider(f.b)
	f.rider(f.a)
	now := time.Now()
	// The plan wants two at B and one at C. B is at its target, and nearer C than A is; A has a
	// surplus of one, so A gives. B is never asked, though it has two and is nearer.
	f.plan(now, f.b, f.b, f.c)
	if n, err := f.svc.Check(ctx, now); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if f.id("SELECT from_node_id::text FROM relocations") != f.a {
		t.Error("the donor must be the node with a surplus")
	}
}

func TestPlanNodesThatAreGoneAreIgnored(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.byPlan()
	f.rider(f.a)
	f.rider(f.a)
	now := time.Now()
	// A node of another zone stands in for one that is gone: the plan wants one rider there and one at
	// A. Nothing in this zone is short, so there is no move.
	other := f.id(`INSERT INTO zones (name, city, status, area) VALUES ('O', 'Bengaluru', 'active',
		extensions.ST_GeomFromText('MULTIPOLYGON(((77.70 12.98, 77.71 12.98, 77.71 12.99, 77.70 12.99, 77.70 12.98)))', 4326)) RETURNING id::text`)
	gone := f.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, 'G', 12.985, 77.705, 2) RETURNING id::text", other)
	f.plan(now, f.a, gone)
	if n, err := f.svc.Check(ctx, now); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
}

func TestTheRestTimeStillHolds(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.byPlan()
	first, _ := f.rider(f.b)
	f.rider(f.b)
	now := time.Now()
	f.plan(now, f.b, f.c)
	// The first rider's last move ended a minute ago, within the rest time: only the second is asked.
	f.exec(`INSERT INTO relocations (rider_id, shift_id, from_node_id, to_node_id, reason, expires_at, state, state_changed_at)
		SELECT $1, sh.id, $2, $3, 'x', now(), 'expired', now() - interval '1 minute' FROM shifts sh WHERE sh.rider_id = $1`, first, f.a, f.b)
	if n, err := f.svc.Check(ctx, now); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if f.count("SELECT count(*) FROM relocations WHERE rider_id = $1 AND state = 'suggested'", first) != 0 {
		t.Error("a resting rider was asked to move")
	}
}
