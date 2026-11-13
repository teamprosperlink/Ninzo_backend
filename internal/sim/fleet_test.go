package sim

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"github.com/teamprosperlink/Ninzo_backend/internal/routing"
)

func fleet(riders ...routing.Point) FleetScenario {
	return FleetScenario{Scenario: Scenario{Riders: riders, Setting: start, Seed: 3,
		Area: Box{12.925, 77.612, 12.945, 77.640}}, OrdersPerHour: 10, Hours: 1, PackS: 60, HandoverS: 30}
}

// On straight roads at 5 m/s, a rider 300 m from the gate is busy for 60 s packing, 60 s there,
// 30 s handing over and 60 s back: 210 s.
func TestARiderIsBusyForPackRideHandoverAndReturn(t *testing.T) {
	fs := fleet(east(gate, 300))
	res, err := runFleetRule(context.Background(), &straightRoads{}, fs, RuleRoad, []float64{0, 100, 209, 211}, []routing.Point{gate, gate, gate, gate})
	if err != nil {
		t.Fatal(err)
	}
	// Orders at 0 and 211 are served; at 100 and 209 the only rider is still busy.
	if res.Served != 2 || res.RefusedBusy != 2 || res.RefusedReach != 0 || res.Refused != 2 {
		t.Fatalf("%+v", res)
	}
	if want := 2 * 210.0 / 3600; math.Abs(res.BusyShare[0]-want) > 1e-9 {
		t.Fatalf("busy share %.4f, want %.4f", res.BusyShare[0], want)
	}
}

func TestAnotherFreeRiderTakesTheSecondOrder(t *testing.T) {
	fs := fleet(east(gate, 300), east(gate, 320))
	res, err := runFleetRule(context.Background(), &straightRoads{}, fs, RuleRoad, []float64{0, 100}, []routing.Point{gate, gate})
	if err != nil {
		t.Fatal(err)
	}
	if res.Served != 2 || res.RefusedBusy != 0 || res.BusyShare[0] == 0 || res.BusyShare[1] == 0 {
		t.Fatalf("%+v", res)
	}
}

func TestAFreeRiderOutOfReachIsARefusalForReach(t *testing.T) {
	fs := fleet(east(gate, 5000))
	for _, rule := range []string{RuleToday, RuleRoad} {
		res, err := runFleetRule(context.Background(), &straightRoads{}, fs, rule, []float64{0}, []routing.Point{gate})
		if err != nil {
			t.Fatal(err)
		}
		if res.RefusedReach != 1 || res.RefusedBusy != 0 || res.Served != 0 || res.BusyShare[0] != 0 {
			t.Fatalf("%s: %+v", rule, res)
		}
	}
}

func TestArrivalsAreDeterministicAndAtTheRate(t *testing.T) {
	fs := fleet(gate)
	fs.OrdersPerHour, fs.Hours = 30, 10
	a, _, err := arrivals(context.Background(), &straightRoads{}, fs)
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := arrivals(context.Background(), &straightRoads{}, fs)
	if len(a) != len(b) || a[0] != b[0] || a[len(a)-1] != b[len(b)-1] {
		t.Fatal("same seed, different arrivals")
	}
	// 300 expected; a Poisson count is within about 3 standard deviations (±52).
	if len(a) < 248 || len(a) > 352 {
		t.Fatalf("%d arrivals in 10 h at 30 an hour", len(a))
	}
	for i := 1; i < len(a); i++ {
		if a[i] <= a[i-1] || a[i] >= fs.Hours*3600 {
			t.Fatalf("arrival %d at %v after %v", i, a[i], a[i-1])
		}
	}
}

func TestRunFleetComparesBothRulesOnTheSameOrders(t *testing.T) {
	fs := fleet(east(gate, 300), east(gate, 600))
	res, err := RunFleet(context.Background(), &straightRoads{}, fs)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].Rule != RuleToday || res[1].Rule != RuleRoad || res[0].Orders != res[1].Orders || res[0].Orders == 0 {
		t.Fatalf("%+v", res)
	}
	for _, r := range res {
		if r.Served+r.RefusedBusy+r.RefusedReach != r.Orders || r.Refused != r.RefusedBusy+r.RefusedReach {
			t.Fatalf("%s: counts do not add up: %+v", r.Rule, r)
		}
	}
	bad := fs
	bad.OrdersPerHour = 0
	if _, err := RunFleet(context.Background(), &straightRoads{}, bad); err == nil {
		t.Fatal("a zero rate was accepted")
	}
}

// TestGoldenFleetOnRealRoads runs the fleet simulator on the test-zone OSRM (CI job routes).
func TestGoldenFleetOnRealRoads(t *testing.T) {
	c := goldenRouter(t)
	fs := FleetScenario{Scenario: Scenario{Name: "Koramangala (test)", Area: Box{12.925, 77.612, 12.945, 77.640}, Seed: 1, Setting: start,
		Riders: []routing.Point{{Lat: 12.9350, Lng: 77.6260}, {Lat: 12.9290, Lng: 77.6180}, {Lat: 12.9290, Lng: 77.6340}, {Lat: 12.9410, Lng: 77.6180}}},
		OrdersPerHour: 20, Hours: 1, PackS: 60, HandoverS: 60}
	res, err := RunFleet(context.Background(), c, fs)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		t.Logf("%s: %d orders, %d served, %d busy-refused, %d reach-refused, %d in time, busy %v", r.Rule, r.Orders, r.Served, r.RefusedBusy, r.RefusedReach, r.InTime, r.BusyShare)
		if r.Orders == 0 || r.Served+r.RefusedBusy+r.RefusedReach != r.Orders || r.InTime > r.Served {
			t.Fatalf("%s: counts do not add up: %+v", r.Rule, r)
		}
		for _, b := range r.BusyShare {
			if b < 0 || b > 1 {
				t.Fatalf("%s: busy share %v", r.Rule, b)
			}
		}
	}
}

// goldenRouter is a client on ROUTING_TEST_URL; the test skips without it, or fails when
// REQUIRE_ROUTING_TESTS=yes.
func goldenRouter(t *testing.T) *routing.Client {
	t.Helper()
	url := os.Getenv("ROUTING_TEST_URL")
	if url == "" {
		if os.Getenv("REQUIRE_ROUTING_TESTS") == "yes" {
			t.Fatal("REQUIRE_ROUTING_TESTS=yes but ROUTING_TEST_URL is not set")
		}
		t.Skip("ROUTING_TEST_URL not set")
	}
	c, err := routing.New(routing.Config{BaseURL: url, Profile: "scooter", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A rider in reach but busy is a busy refusal even when another rider is free but out of reach.
func TestABusyRiderInReachIsABusyRefusal(t *testing.T) {
	fs := fleet(east(gate, 300), east(gate, 5000))
	res, err := runFleetRule(context.Background(), &straightRoads{}, fs, RuleRoad, []float64{0, 100}, []routing.Point{gate, gate})
	if err != nil {
		t.Fatal(err)
	}
	// The near rider takes the first order and is busy for 210 s; the far one is free but 5 km off.
	if res.Served != 1 || res.RefusedBusy != 1 || res.RefusedReach != 0 {
		t.Fatalf("%+v", res)
	}
}

// A rider free at exactly the arrival time takes it.
func TestARiderFreeAtExactlyTheArrivalTakesIt(t *testing.T) {
	fs := fleet(east(gate, 300))
	res, err := runFleetRule(context.Background(), &straightRoads{}, fs, RuleRoad, []float64{0, 210}, []routing.Point{gate, gate})
	if err != nil {
		t.Fatal(err)
	}
	if res.Served != 2 || res.Refused != 0 {
		t.Fatalf("%+v", res)
	}
}

// A stuck ride still takes the rider out: there and back by the straight line at the slow speed.
func TestAStuckRideStillCostsTheRider(t *testing.T) {
	fs := fleet(east(gate, 300))
	r := &straightRoads{far: map[routing.Point]bool{gate: true}} // the route ends 300 m short
	res, err := runFleetRule(context.Background(), r, fs, RuleRoad, []float64{0, 100}, []routing.Point{gate, gate})
	if err != nil {
		t.Fatal(err)
	}
	// 300 m at 15 km/h is 72 s each way: busy 60 + 72 + 30 + 72 = 234 s, so the order at 100 s
	// finds the only rider busy.
	if res.Stuck != 1 || res.Served != 1 || res.RefusedBusy != 1 || math.Abs(res.BusyShare[0]-234.0/3600) > 1e-9 {
		t.Fatalf("%+v", res)
	}
}
