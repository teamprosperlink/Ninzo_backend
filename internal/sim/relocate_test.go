package sim

import (
	"context"
	"math"
	"testing"

	"github.com/teamprosperlink/Ninzo_backend/internal/routing"
)

// Three nodes on a line: A at the gate, B 1 km east, C 2 km east.
var (
	nodeA = gate
	nodeB = east(gate, 1000)
	nodeC = east(gate, 2000)
)

func relocScenario(picks ...int) RelocScenario {
	return RelocScenario{FleetScenario: fleet(nodeA), Nodes: []routing.Point{nodeA, nodeB, nodeC}, PlanPicks: picks,
		CheckS: 30, WindowS: 1800, MinOrders: 2, RestS: 900}
}

func at(nodes ...int) []mover {
	out := make([]mover, len(nodes))
	for i, n := range nodes {
		out[i] = mover{node: n}
	}
	return out
}

func check(t *testing.T, rs RelocScenario, rule string, riders []mover, done []served, now float64) RelocResult {
	t.Helper()
	var res RelocResult
	if err := relocate(context.Background(), &straightRoads{}, rs, rule, riders, done, now, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func TestTodaysRuleMovesTowardRecentOrdersWithNobodyThere(t *testing.T) {
	rs := relocScenario()
	riders := at(0, 0)
	riders[1].freeAt = -10 // free longest: asked first
	res := check(t, rs, RelocToday, riders, []served{{at: 100, node: 2}, {at: 200, node: 2}}, 300)
	if res.Moves != 1 || riders[1].node != 2 || riders[0].node != 0 {
		t.Fatalf("%+v %+v", res, riders)
	}
	// 2 km at 5 m/s: busy riding until 700, then resting until 1600.
	if math.Abs(res.MoveS-400) > 1 || math.Abs(riders[1].freeAt-700) > 1 || math.Abs(riders[1].restTill-1600) > 1 || !riders[1].moving {
		t.Fatalf("%+v %+v", res, riders[1])
	}
	// The next check sees the move in play: no second move toward C.
	if again := check(t, rs, RelocToday, riders, []served{{at: 100, node: 2}, {at: 200, node: 2}}, 330); again.Moves != 0 {
		t.Fatal("a second move toward a node with one in play")
	}
}

func TestTodaysRuleNeedsADonorWithTwoWaitingAndEnoughOrders(t *testing.T) {
	rs := relocScenario()
	orders := []served{{at: 100, node: 2}, {at: 200, node: 2}}
	if res := check(t, rs, RelocToday, at(0), orders, 300); res.Moves != 0 {
		t.Fatal("a donor must keep one rider waiting")
	}
	if res := check(t, rs, RelocToday, at(0, 0), orders[:1], 300); res.Moves != 0 {
		t.Fatal("one order is below relocation_min_orders")
	}
	if res := check(t, rs, RelocToday, at(0, 0), orders, 100+1800+1); res.Moves != 0 {
		t.Fatal("an order outside the window counted")
	}
	busy := at(0, 0)
	busy[0].freeAt = 1000 // on an order: not waiting
	if res := check(t, rs, RelocToday, busy, orders, 300); res.Moves != 0 {
		t.Fatal("a busy rider counted as waiting")
	}
	resting := at(0, 0)
	resting[0].restTill, resting[1].restTill = 400, 400
	if res := check(t, rs, RelocToday, resting, orders, 300); res.Moves != 0 {
		t.Fatal("a resting rider was asked")
	}
}

func TestThePlanRuleFollowsThePlanWithoutOrders(t *testing.T) {
	// Two free riders at A; the plan for two wants one at B and one at C. No orders at all.
	rs := relocScenario(1, 2, 0)
	riders := at(0, 0)
	res := check(t, rs, RelocPlan, riders, nil, 300)
	got := map[int]int{riders[0].node: 1}
	got[riders[1].node]++
	if res.Moves != 2 || got[1] != 1 || got[2] != 1 {
		t.Fatalf("%+v %+v", res, riders)
	}
	// Today's rule, with no orders, would not move anyone.
	if res := check(t, rs, RelocToday, at(0, 0), nil, 300); res.Moves != 0 {
		t.Fatal(res)
	}
}

func TestThePlanRuleKeepsADonorAtItsTarget(t *testing.T) {
	// The plan for two wants two at B; one waits at B and one at A. A gives, B is never short of itself.
	rs := relocScenario(1, 1, 2)
	riders := at(1, 0)
	res := check(t, rs, RelocPlan, riders, nil, 300)
	if res.Moves != 1 || riders[0].node != 1 || riders[1].node != 1 {
		t.Fatalf("%+v %+v", res, riders)
	}
}

func TestThePlanRuleFallsBackWithNoPlanForN(t *testing.T) {
	// Three free riders, but the plan only goes to n = 2: today's rule decides.
	rs := relocScenario(1, 2)
	orders := []served{{at: 100, node: 2}, {at: 200, node: 2}}
	riders := at(0, 0, 0)
	res := check(t, rs, RelocPlan, riders, orders, 300)
	moved := 0
	for _, rd := range riders {
		if rd.node == 2 {
			moved++
		}
	}
	if res.Moves != 1 || moved != 1 {
		t.Fatalf("today's rule should move one to C: %+v %+v", res, riders)
	}
	if res := check(t, rs, RelocPlan, at(0, 0, 0), nil, 300); res.Moves != 0 {
		t.Fatal("with no plan and no orders, nobody moves")
	}
}

func TestTheRulesRunOnTheSameOrdersAndNoneNeverMoves(t *testing.T) {
	rs := relocScenario(1, 2, 0)
	rs.FleetScenario = fleet(nodeA, nodeA, nodeA)
	rs.OrdersPerHour, rs.Hours = 20, 2
	res, err := RunRelocation(context.Background(), &straightRoads{}, rs)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 || res[0].Relocation != RelocNone || res[1].Relocation != RelocToday || res[2].Relocation != RelocPlan {
		t.Fatalf("%+v", res)
	}
	if res[0].Moves != 0 || res[0].MoveS != 0 {
		t.Fatal("none moved someone")
	}
	if res[2].Moves == 0 {
		t.Fatal("the plan wants riders at B and C, and nobody starts there")
	}
	for _, r := range res {
		if r.Orders != res[0].Orders || r.Orders == 0 {
			t.Fatalf("different orders: %+v", res)
		}
	}
}

func TestAMovingRiderTakesNoOrder(t *testing.T) {
	// One rider, moved from A toward C at once by the plan; an order at A while they ride is refused,
	// though A is where they started.
	rs := relocScenario(2)
	rs.FleetScenario = fleet(nodeA)
	res, err := runReloc(context.Background(), &straightRoads{}, rs, RelocPlan, []float64{60}, []routing.Point{nodeA})
	if err != nil {
		t.Fatal(err)
	}
	if res.Moves != 1 || res.Served != 0 || res.Refused != 1 {
		t.Fatalf("%+v", res)
	}
	same, err := runReloc(context.Background(), &straightRoads{}, rs, RelocNone, []float64{60}, []routing.Point{nodeA})
	if err != nil || same.Served != 1 {
		t.Fatalf("without the move the order is served: %+v %v", same, err)
	}
}

func TestABadRelocationScenarioIsRefused(t *testing.T) {
	for name, rs := range map[string]RelocScenario{
		"one node":   {FleetScenario: fleet(nodeA), Nodes: []routing.Point{nodeA}, CheckS: 30, WindowS: 1800, MinOrders: 2},
		"no check":   {FleetScenario: fleet(nodeA), Nodes: []routing.Point{nodeA, nodeB}, WindowS: 1800, MinOrders: 2},
		"no minimum": {FleetScenario: fleet(nodeA), Nodes: []routing.Point{nodeA, nodeB}, CheckS: 30, WindowS: 1800},
		"bad pick":   {FleetScenario: fleet(nodeA), Nodes: []routing.Point{nodeA, nodeB}, PlanPicks: []int{2}, CheckS: 30, WindowS: 1800, MinOrders: 2},
		"no riders":  {FleetScenario: fleet(), Nodes: []routing.Point{nodeA, nodeB}, CheckS: 30, WindowS: 1800, MinOrders: 2},
		"minus rest": {FleetScenario: fleet(nodeA), Nodes: []routing.Point{nodeA, nodeB}, CheckS: 30, WindowS: 1800, MinOrders: 2, RestS: -1},
	} {
		if _, err := RunRelocation(context.Background(), &straightRoads{}, rs); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTodaysRuleSkipsANodeWithAMoveInPlay(t *testing.T) {
	// Three wait at A, so A could give twice; one move toward C is in play, so no second one.
	rs := relocScenario()
	orders := []served{{at: 100, node: 2}, {at: 200, node: 2}}
	riders := at(0, 0, 0)
	if res := check(t, rs, RelocToday, riders, orders, 300); res.Moves != 1 {
		t.Fatal(res)
	}
	if res := check(t, rs, RelocToday, riders, orders, 330); res.Moves != 0 {
		t.Fatal("a second move toward a node with one in play")
	}
}

func TestThePlanRuleSkipsANodeWithAMoveInPlay(t *testing.T) {
	// n = 3 wants B, C and A: two moves. Then one is still free at A; the plan for one wants B,
	// which has a move in play.
	rs := relocScenario(1, 2, 0)
	riders := at(0, 0, 0)
	if res := check(t, rs, RelocPlan, riders, nil, 300); res.Moves != 2 {
		t.Fatal(res)
	}
	if res := check(t, rs, RelocPlan, riders, nil, 330); res.Moves != 0 {
		t.Fatal("a move toward a node with one in play")
	}
}

func TestThePlanDonorIsTheNodeWithASurplus(t *testing.T) {
	// The plan wants two at B and one at C. B has its two and is nearer C than A is; A has a
	// surplus of one, so A gives.
	rs := relocScenario(1, 1, 2)
	riders := at(1, 1, 0)
	if res := check(t, rs, RelocPlan, riders, nil, 300); res.Moves != 1 || riders[2].node != 2 || riders[0].node != 1 || riders[1].node != 1 {
		t.Fatalf("%+v %+v", res, riders)
	}
}

func TestNoneNeverMoves(t *testing.T) {
	rs := relocScenario(1, 2, 0)
	orders := []served{{at: 100, node: 2}, {at: 200, node: 2}}
	if res := check(t, rs, RelocNone, at(0, 0, 0), orders, 300); res.Moves != 0 {
		t.Fatal(res)
	}
}
