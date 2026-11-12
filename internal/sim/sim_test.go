package sim

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"github.com/teamprosperlink/Ninzo_backend/internal/routing"
)

// straightRoads is a fake router: every road is the straight line at 18 km/h (5 m/s), drawn as a
// line of points every few metres; far marks destinations whose route ends 300 m short.
type straightRoads struct {
	far    map[routing.Point]bool
	routes int
	vias   int
	cut    bool // Via answers no road
}

func line(a, b routing.Point, n int) []routing.Point {
	out := make([]routing.Point, n+1)
	for i := range out {
		f := float64(i) / float64(n)
		out[i] = routing.Point{Lat: a.Lat + f*(b.Lat-a.Lat), Lng: a.Lng + f*(b.Lng-a.Lng)}
	}
	return out
}

func (s *straightRoads) Route(_ context.Context, from, to routing.Point, _ float64) (routing.Route, error) {
	s.routes++
	end := to
	if s.far[to] {
		// End 300 m short of the gate, as OSRM does behind a one-way.
		d := routing.Metres(from, to)
		f := (d - 300) / d
		end = routing.Point{Lat: from.Lat + f*(to.Lat-from.Lat), Lng: from.Lng + f*(to.Lng-from.Lng)}
	}
	m := routing.Metres(from, end)
	return routing.Route{Leg: routing.Leg{Seconds: m / 5, Metres: m, Reachable: true, Source: routing.SourceOSRM}, Line: line(from, end, 200)}, nil
}

func (s *straightRoads) Via(_ context.Context, wps []routing.Waypoint, _ float64) ([]routing.Leg, error) {
	s.vias++
	out := make([]routing.Leg, len(wps)-1)
	for i := range out {
		m := routing.Metres(wps[i].Point, wps[i+1].Point)
		out[i] = routing.Leg{Seconds: m / 5, Metres: m, Reachable: !s.cut, Source: routing.SourceOSRM}
	}
	return out, nil
}

func (s *straightRoads) Table(_ context.Context, sources []routing.Point, dest routing.Point, _ float64) ([]routing.Leg, error) {
	out := make([]routing.Leg, len(sources))
	for i, p := range sources {
		m := routing.Metres(p, dest)
		out[i] = routing.Leg{Seconds: m / 5, Metres: m, Reachable: true, Source: routing.SourceOSRM}
	}
	return out, nil
}

func (s *straightRoads) Nearest(_ context.Context, p routing.Point) (routing.Snap, error) {
	return routing.Snap{Point: p, Source: routing.SourceOSRM}, nil
}

var start = Settings{TimerS: 90, ZoneRadiusM: 350, PerimeterM: 100, SweetSpotS: 80, StraightKmh: 15, MaxSearchM: 1500}

// east is the point d metres east of p.
func east(p routing.Point, d float64) routing.Point {
	return routing.Point{Lat: p.Lat, Lng: p.Lng + d/(6371000*math.Cos(p.Lat*math.Pi/180))*180/math.Pi}
}

var gate = routing.Point{Lat: 12.9354, Lng: 77.6144}

func TestTheTimerStartsAtTheZoneEdgeAndStopsAtThePerimeter(t *testing.T) {
	r := &straightRoads{}
	ride, err := Timed(context.Background(), r, east(gate, 1000), gate, start)
	if err != nil {
		t.Fatal(err)
	}
	// Straight at 5 m/s: 650 m to the zone edge (130 s), then 250 m to the perimeter (50 s).
	if math.Abs(ride.WaitS-130) > 0.5 || math.Abs(ride.TimedS-50) > 0.5 || !ride.InTime || ride.Stuck {
		t.Fatalf("%+v, want wait 130 s, timed 50 s, in time", ride)
	}
	// Already inside the zone: no wait, the timer runs from where the rider is.
	ride, _ = Timed(context.Background(), r, east(gate, 300), gate, start)
	if ride.WaitS != 0 || math.Abs(ride.TimedS-40) > 0.5 {
		t.Fatalf("inside the zone: %+v, want wait 0, timed 40 s", ride)
	}
	// Already inside the perimeter: nothing to time.
	ride, _ = Timed(context.Background(), r, east(gate, 50), gate, start)
	if ride.WaitS != 0 || ride.TimedS != 0 || !ride.InTime {
		t.Fatalf("inside the perimeter: %+v", ride)
	}
	// A 600 s timer: the same 250 m is 50 s, in time; a 40 s timer: late.
	late := start
	late.TimerS = 40
	if ride, _ = Timed(context.Background(), r, east(gate, 1000), gate, late); ride.InTime {
		t.Fatalf("50 s against a 40 s timer counted in time: %+v", ride)
	}
	// A route that ends short of the gate is stuck: served, never in time.
	far := &straightRoads{far: map[routing.Point]bool{gate: true}}
	if ride, _ = Timed(context.Background(), far, east(gate, 1000), gate, start); !ride.Stuck || ride.InTime {
		t.Fatalf("a route ending 300 m short: %+v", ride)
	}
}

func TestFirstWithinIsAccurateToTheMetre(t *testing.T) {
	from := east(gate, 2000)
	for _, n := range []int{1, 3, 50} { // even a line of one long segment
		p, _, ok := firstWithin(line(from, gate, n), gate, 350)
		if !ok || math.Abs(routing.Metres(p, gate)-350) > 1 {
			t.Fatalf("n=%d: %v %v at %.1f m", n, p, ok, routing.Metres(p, gate))
		}
	}
	if _, _, ok := firstWithin(line(from, east(gate, 500), 10), gate, 350); ok {
		t.Fatal("a line that stays 500 m away was found within 350 m")
	}
	if _, _, ok := firstWithin(nil, gate, 350); ok {
		t.Fatal("an empty line")
	}
}

func TestTheRulesChooseAsDesigned(t *testing.T) {
	riders := []routing.Point{east(gate, 300), east(gate, 200), east(gate, 600)}
	// Road times: the nearest rider by line is slow by road (a detour); the second is fast.
	legs := []routing.Leg{
		{Seconds: 200, Reachable: true, Source: routing.SourceOSRM},
		{Seconds: 60, Reachable: true, Source: routing.SourceOSRM},
		{Seconds: 70, Reachable: true, Source: routing.SourceOSRM},
	}
	// today: within 15 km/h × 80 s = 333 m, the nearest by line is rider 1 (200 m).
	if got := choose(RuleToday, riders, legs, gate, start); got != 1 {
		t.Fatalf("today chose %d, want 1", got)
	}
	// road: within 80 s by road, the fastest is rider 1 (60 s).
	if got := choose(RuleRoad, riders, legs, gate, start); got != 1 {
		t.Fatalf("road chose %d, want 1", got)
	}
	legs[1] = routing.Leg{Seconds: 300, Reachable: true, Source: routing.SourceOSRM}
	// Now rider 1 is slow by road: today still takes it; road takes rider 2 (600 m away, 70 s).
	if got := choose(RuleToday, riders, legs, gate, start); got != 1 {
		t.Fatalf("today chose %d, want 1", got)
	}
	if got := choose(RuleRoad, riders, legs, gate, start); got != 2 {
		t.Fatalf("road chose %d, want 2", got)
	}
	// A rider 500 m away by line but 60 s by road: past today's 333 m straight-line reach (15 km/h
	// for 80 s), though inside the 1500 m search: today refuses, road takes it.
	beyond := []routing.Point{east(gate, 500)}
	fast := []routing.Leg{{Seconds: 60, Reachable: true, Source: routing.SourceOSRM}}
	if got := choose(RuleToday, beyond, fast, gate, start); got != -1 {
		t.Fatalf("today took a rider 500 m away, past its 333 m reach")
	}
	if got := choose(RuleRoad, beyond, fast, gate, start); got != 0 {
		t.Fatalf("road refused a rider 60 s away")
	}
	// Nobody within reach: both refuse.
	far := []routing.Point{east(gate, 5000)}
	if choose(RuleToday, far, []routing.Leg{{Seconds: 900, Reachable: true}}, gate, start) != -1 ||
		choose(RuleRoad, far, []routing.Leg{{Seconds: 900, Reachable: true}}, gate, start) != -1 {
		t.Fatal("a rider 5 km away was chosen")
	}
	// Unreachable by road is never chosen by road.
	if choose(RuleRoad, riders[:1], []routing.Leg{{Reachable: false}}, gate, start) != -1 {
		t.Fatal("an unreachable rider was chosen")
	}
}

func TestPercentiles(t *testing.T) {
	v := []float64{10, 1, 9, 2, 8, 3, 7, 4, 6, 5}
	if percentile(v, 0.5) != 5 || percentile(v, 0.9) != 9 || percentile(nil, 0.5) != 0 || percentile([]float64{7}, 0.9) != 7 {
		t.Fatalf("p50 %v p90 %v", percentile(v, 0.5), percentile(v, 0.9))
	}
	if v[0] != 10 {
		t.Fatal("percentile sorted its input")
	}
}

func TestARunIsDeterministicBySeed(t *testing.T) {
	sc := Scenario{Name: "t", Area: Box{12.925, 77.612, 12.945, 77.640}, Orders: 40, Seed: 7, Setting: start,
		Riders: []routing.Point{{Lat: 12.935, Lng: 77.626}, {Lat: 12.929, Lng: 77.618}}}
	a, err := Run(context.Background(), &straightRoads{}, sc)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Run(context.Background(), &straightRoads{}, sc)
	if a[0] != b[0] || a[1] != b[1] {
		t.Fatalf("same seed, different results:\n%+v\n%+v", a, b)
	}
	sc.Seed = 8
	c, _ := Run(context.Background(), &straightRoads{}, sc)
	if c[0] == a[0] && c[1] == a[1] {
		t.Fatal("a different seed gave the same day")
	}
	for _, res := range a {
		if res.Orders != 40 || res.Served+res.Refused != 40 {
			t.Fatalf("%+v", res)
		}
	}
	// On straight roads the two rules see the same distances, so the road rule never does worse.
	if a[1].InTimeShare < a[0].InTimeShare {
		t.Fatalf("road %.2f below today %.2f on straight roads", a[1].InTimeShare, a[0].InTimeShare)
	}
}

func TestBadScenariosAreRefused(t *testing.T) {
	bad := start
	bad.PerimeterM = 400
	for _, sc := range []Scenario{
		{Orders: 0, Riders: []routing.Point{gate}, Setting: start},
		{Orders: 5, Setting: start},
		{Orders: 5, Riders: []routing.Point{gate}, Setting: bad},
	} {
		if _, err := Run(context.Background(), &straightRoads{}, sc); err == nil {
			t.Errorf("%+v accepted", sc)
		}
	}
}

// TestGoldenSimOnRealRoads runs the simulator against OSRM on the test-zone data (CI job routes).
func TestGoldenSimOnRealRoads(t *testing.T) {
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
	sc := Scenario{Name: "Koramangala (test)", Area: Box{12.925, 77.612, 12.945, 77.640}, Orders: 40, Seed: 1, Setting: start,
		Riders: []routing.Point{{Lat: 12.9350, Lng: 77.6260}, {Lat: 12.9290, Lng: 77.6180}, {Lat: 12.9290, Lng: 77.6340},
			{Lat: 12.9410, Lng: 77.6180}, {Lat: 12.9410, Lng: 77.6340}, {Lat: 12.9350, Lng: 77.6150}}}
	res, err := Run(context.Background(), c, sc)
	if err != nil {
		t.Fatal(err)
	}
	today, road := res[0], res[1]
	t.Logf("today: %+v", today)
	t.Logf("road:  %+v", road)
	// What a run on real roads must hold, whatever the numbers: the counts add up, the road rule's
	// picks are within its budget by road so they cannot be refused on reach, and every in-time
	// leg fits the timer. Which rule is better is for the comparison in w8, not for this test.
	for _, x := range res {
		if x.Served+x.Refused != x.Orders || x.InTime > x.Served || x.Stuck > x.Served {
			t.Fatalf("counts do not add up: %+v", x)
		}
		if x.InTime > 0 && x.TimedP50 > float64(start.TimerS) && x.InTimeShare > 0.5 {
			t.Fatalf("half in time but the median leg is over the timer: %+v", x)
		}
	}
	if today.Served == 0 || road.Served == 0 {
		t.Fatalf("a rule served nothing: today %d, road %d", today.Served, road.Served)
	}
	// The road rule takes only riders within the budget by road, a subset of what today's reach
	// takes when the budget is short, so on the test zones it serves fewer orders at 80 s.
	if road.Served > today.Served {
		t.Fatalf("road served %d, today %d", road.Served, today.Served)
	}
}

func TestTheTimedLegIsOneRouteAndAPickWithNoRoadFails(t *testing.T) {
	r := &straightRoads{}
	if _, err := Timed(context.Background(), r, east(gate, 1000), gate, start); err != nil {
		t.Fatal(err)
	}
	// One line to find the points, then one route through rider, start, stop and gate.
	if r.routes != 1 || r.vias != 1 {
		t.Fatalf("%d routes and %d via routes, want 1 and 1", r.routes, r.vias)
	}
	// The picked rider has no road through the points: in service the order fails, so it is
	// served and stuck, not refused.
	cut := &straightRoads{cut: true}
	ride, err := Timed(context.Background(), cut, east(gate, 1000), gate, start)
	if err != nil || !ride.Stuck || ride.Refused || ride.InTime || !math.IsInf(ride.TimedS, 1) {
		t.Fatalf("%+v %v", ride, err)
	}
	// Stuck rides count as endless legs in the percentiles, and in time per order counts refusals.
	res := summarise(RuleToday, []Ride{{TimedS: 10, InTime: true}, {Stuck: true, TimedS: math.Inf(1)}, {Refused: true}, {Refused: true}})
	if res.Served != 2 || res.Stuck != 1 || res.InTime != 1 || res.InTimeShare != 0.5 || res.InTimePerOrder != 0.25 || !math.IsInf(res.TimedP90, 1) {
		t.Fatalf("%+v", res)
	}
}

func TestWilson(t *testing.T) {
	// 17 of 17: about 81.6% to 100%; 47 of 200: about 18.1% to 29.9%.
	if lo, hi := Wilson(17, 17); math.Abs(lo-0.816) > 0.002 || math.Abs(hi-1) > 1e-9 {
		t.Fatalf("17/17: %.4f-%.4f", lo, hi)
	}
	if lo, hi := Wilson(47, 200); math.Abs(lo-0.181) > 0.002 || math.Abs(hi-0.299) > 0.002 {
		t.Fatalf("47/200: %.4f-%.4f", lo, hi)
	}
	if lo, hi := Wilson(0, 0); lo != 0 || hi != 1 {
		t.Fatal("no trials")
	}
}

func TestARepeatedPointGivesNoFalseHeading(t *testing.T) {
	a, b := gate, east(gate, 100)
	if got := headingAt([]routing.Point{a, a, b}, 0); got != 90 {
		t.Fatalf("heading %d over a repeated point, want 90", got)
	}
	if got := headingAt([]routing.Point{a, a}, 0); got != -1 {
		t.Fatalf("heading %d with nowhere to go, want -1", got)
	}
}
