// Package matching orders the scooters that may take an order (issue 209, geo record W5). It is
// pure: no database and no network, so availability, dispatch and the simulator share it. The
// filters (zone, sale hours, reach, stock, a free rider) stay where they are; this only decides
// the order in which the passing scooters are tried.
package matching

import (
	"sort"

	"github.com/teamprosperlink/Ninzo_backend/internal/routing"
)

// Candidate is a scooter that passed the filters.
type Candidate struct {
	ScooterID string
	Node      routing.Point // its staging node
	// Position is where its rider is; Live says it is a fresh ping. When not live, Position is
	// the node: a rider with no recent ping is taken to be waiting there.
	Position routing.Point
	Live     bool
}

// Ranked is one candidate in order, with what it was ranked by.
type Ranked struct {
	Candidate
	StraightMetres float64 // from the staging node to the gate (today's rule)
	RoadSeconds    float64 // from Position to the gate by road; zero when not asked or unreachable
	Reachable      bool    // by road; true for today's rule
}

// ByStraightLine is today's order: the nearest staging node to the gate first, ties by scooter id.
func ByStraightLine(cs []Candidate, gate routing.Point) []Ranked {
	out := make([]Ranked, len(cs))
	for i, c := range cs {
		out[i] = Ranked{Candidate: c, StraightMetres: routing.Metres(c.Node, gate), Reachable: true}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StraightMetres != out[j].StraightMetres {
			return out[i].StraightMetres < out[j].StraightMetres
		}
		return out[i].ScooterID < out[j].ScooterID
	})
	return out
}

// Sources are the points to ask road times from, in candidate order (for one Table call).
func Sources(cs []Candidate) []routing.Point {
	out := make([]routing.Point, len(cs))
	for i, c := range cs {
		out[i] = c.Position
	}
	return out
}

// ByRoad orders candidates by road time from their position to the gate, given legs from one
// Table call over Sources(cs). Unreachable ones go last, in today's order. If the legs are not
// from OSRM (routing fell back to straight lines) or do not match, it returns today's order and
// false, so a routing outage never decides by a guess dressed as a road time.
func ByRoad(cs []Candidate, gate routing.Point, legs []routing.Leg) ([]Ranked, bool) {
	if len(legs) != len(cs) {
		return ByStraightLine(cs, gate), false
	}
	for _, l := range legs {
		if l.Source != routing.SourceOSRM {
			return ByStraightLine(cs, gate), false
		}
	}
	out := make([]Ranked, len(cs))
	for i, c := range cs {
		out[i] = Ranked{Candidate: c, StraightMetres: routing.Metres(c.Node, gate), Reachable: legs[i].Reachable}
		if legs[i].Reachable {
			out[i].RoadSeconds = legs[i].Seconds
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Reachable != b.Reachable {
			return a.Reachable
		}
		if a.Reachable && a.RoadSeconds != b.RoadSeconds {
			return a.RoadSeconds < b.RoadSeconds
		}
		if a.StraightMetres != b.StraightMetres {
			return a.StraightMetres < b.StraightMetres
		}
		return a.ScooterID < b.ScooterID
	})
	return out, true
}

// IDs are the scooter ids in ranked order.
func IDs(r []Ranked) []string {
	out := make([]string, len(r))
	for i, x := range r {
		out[i] = x.ScooterID
	}
	return out
}
