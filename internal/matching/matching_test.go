package matching

import (
	"fmt"
	"math"
	"testing"

	"github.com/teamprosperlink/Ninzo_backend/internal/routing"
)

var gate = routing.Point{Lat: 12.9354, Lng: 77.6144}

func east(m float64) routing.Point {
	return routing.Point{Lat: gate.Lat, Lng: gate.Lng + m/(6371000*math.Cos(gate.Lat*math.Pi/180))*180/math.Pi}
}

func cand(id string, nodeM float64) Candidate {
	return Candidate{ScooterID: id, Node: east(nodeM), Position: east(nodeM)}
}

func osrm(secs ...float64) []routing.Leg {
	out := make([]routing.Leg, len(secs))
	for i, s := range secs {
		if s < 0 {
			out[i] = routing.Leg{Source: routing.SourceOSRM}
			continue
		}
		out[i] = routing.Leg{Seconds: s, Reachable: true, Source: routing.SourceOSRM}
	}
	return out
}

func TestByStraightLineIsTodaysOrder(t *testing.T) {
	cs := []Candidate{cand("c", 300), cand("a", 100), cand("b", 100), cand("d", 50)}
	if got := fmt.Sprint(IDs(ByStraightLine(cs, gate))); got != "[d a b c]" {
		t.Fatalf("%s, want nearest node first, ties by id", got)
	}
}

func TestByRoadPutsTheFastestByRoadFirst(t *testing.T) {
	// a is nearest by line but across a median: slow by road.
	cs := []Candidate{cand("a", 100), cand("b", 300), cand("c", 500), cand("d", 200)}
	ranked, ok := ByRoad(cs, gate, osrm(240, 60, 90, -1))
	if !ok {
		t.Fatal("OSRM legs were not used")
	}
	if got := fmt.Sprint(IDs(ranked)); got != "[b c a d]" {
		t.Fatalf("%s, want fastest by road first and the unreachable one last", got)
	}
	if ranked[0].RoadSeconds != 60 || ranked[3].Reachable {
		t.Fatalf("%+v", ranked)
	}
	// The nearest by line but with no road (an island, a gated campus) still goes last.
	ranked, _ = ByRoad([]Candidate{cand("near", 20), cand("far", 600)}, gate, osrm(-1, 150))
	if got := fmt.Sprint(IDs(ranked)); got != "[far near]" {
		t.Fatalf("%s, want the unreachable one last however near", got)
	}
	// Equal road times fall back to the straight line, then the id.
	ranked, _ = ByRoad([]Candidate{cand("y", 300), cand("x", 300), cand("z", 100)}, gate, osrm(60, 60, 60))
	if got := fmt.Sprint(IDs(ranked)); got != "[z x y]" {
		t.Fatalf("%s", got)
	}
}

func TestARoutingOutageKeepsTodaysOrder(t *testing.T) {
	cs := []Candidate{cand("a", 100), cand("b", 300)}
	placeholder := []routing.Leg{{Seconds: 1, Reachable: true, Source: routing.SourceOSRM}, {Seconds: 999, Reachable: true, Source: routing.SourcePlaceholder}}
	for name, legs := range map[string][]routing.Leg{"placeholder": placeholder, "short": osrm(5)} {
		ranked, ok := ByRoad(cs, gate, legs)
		if ok || fmt.Sprint(IDs(ranked)) != "[a b]" {
			t.Errorf("%s: %v %v, want today's order and false", name, IDs(ranked), ok)
		}
	}
}

func TestSourcesAreThePositions(t *testing.T) {
	c := cand("a", 100)
	c.Position, c.Live = east(40), true
	if got := Sources([]Candidate{c}); got[0] != east(40) {
		t.Fatalf("%v", got)
	}
}
