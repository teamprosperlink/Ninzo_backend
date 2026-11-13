// Package sim is the geo simulator (issue 205, geo record W8): it runs pick rules over the same
// simulated orders on real roads, through the real routing client, and measures what the product
// promises: once the timer starts at the zone edge, does the rider reach the perimeter in time?
//
// v1 treats every rider as free for every order (no fleet state) and models no traffic beyond the
// routing profile. It answers "which rule picks the rider who makes the timer", not "how many
// riders are needed".
package sim

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"sort"

	"github.com/teamprosperlink/Ninzo_backend/internal/routing"
)

// Router is what the simulator needs from internal/routing.
type Router interface {
	Route(ctx context.Context, from, to routing.Point, speedKmh float64) (routing.Route, error)
	Via(ctx context.Context, wps []routing.Waypoint, speedKmh float64) ([]routing.Leg, error)
	Table(ctx context.Context, sources []routing.Point, dest routing.Point, speedKmh float64) ([]routing.Leg, error)
	Nearest(ctx context.Context, p routing.Point) (routing.Snap, error)
}

// Settings are the order settings the rules and the timer use, as in geo.
type Settings struct {
	TimerS, ZoneRadiusM, PerimeterM, SweetSpotS, StraightKmh, MaxSearchM int
}

// Box is a rectangle in degrees; orders are drawn uniformly inside it.
type Box struct{ MinLat, MinLng, MaxLat, MaxLng float64 }

// Scenario is one simulated day: where orders come from and where riders wait.
type Scenario struct {
	Name    string
	Area    Box
	Riders  []routing.Point
	Orders  int
	Seed    uint64
	Setting Settings
	// Hotspots make demand uneven (issue 222). None: gates are uniform across the area.
	Hotspots []Hotspot
}

// Hotspot is a place orders come from: a share of all gates, spread around it with a Gaussian of
// RadiusM metres in each direction. Gates not from a hotspot are uniform across the area.
type Hotspot struct {
	Lat, Lng, Share, RadiusM float64
}

func checkHotspots(hs []Hotspot) error {
	sum := 0.0
	for _, h := range hs {
		if h.Share <= 0 || h.RadiusM <= 0 || math.IsNaN(h.Lat) || math.IsNaN(h.Lng) {
			return errors.New("sim: a hotspot needs a share and a radius above zero")
		}
		sum += h.Share
	}
	if sum > 1+1e-9 {
		return errors.New("sim: hotspot shares add up to more than 1")
	}
	return nil
}

// point draws one candidate gate. With no hotspots it makes exactly the two draws it always made, so
// earlier runs and goldens are unchanged.
func (sc Scenario) point(rng *rand.Rand) routing.Point {
	if len(sc.Hotspots) > 0 {
		u := rng.Float64()
		for _, h := range sc.Hotspots {
			if u < h.Share {
				return routing.Point{
					Lat: h.Lat + rng.NormFloat64()*h.RadiusM/111320,
					Lng: h.Lng + rng.NormFloat64()*h.RadiusM/(111320*math.Cos(h.Lat*math.Pi/180)),
				}
			}
			u -= h.Share
		}
	}
	return routing.Point{
		Lat: sc.Area.MinLat + rng.Float64()*(sc.Area.MaxLat-sc.Area.MinLat),
		Lng: sc.Area.MinLng + rng.Float64()*(sc.Area.MaxLng-sc.Area.MinLng),
	}
}

func (b Box) contains(p routing.Point) bool {
	return p.Lat >= b.MinLat && p.Lat <= b.MaxLat && p.Lng >= b.MinLng && p.Lng <= b.MaxLng
}

// Rule names.
const (
	// RuleToday is the distance rule of availability.PickScooter: the nearest by straight line
	// within min(max_search_radius_m, straight_line_speed_kmh × sweet_spot_budget_s). Its stock,
	// basket and zone checks are not modelled: every rider here has what every order needs.
	RuleToday = "today"
	// RuleRoad is the fastest by road within sweet_spot_budget_s.
	RuleRoad = "road"
)

// Ride is one order's outcome under one rule.
type Ride struct {
	Refused bool
	Rider   int
	// WaitS is the time from the rider to where the timer starts; TimedS from there to the
	// perimeter. Both are legs of one OSRM route through those points, so signal and turn costs
	// count and the path is the one the rider rides.
	WaitS, TimedS float64
	// InTime: the timed leg fits the timer. A rider already inside the zone starts the timer at
	// once (no wait); one already inside the perimeter has nothing to time.
	InTime bool
	// Stuck: the rider cannot get within the perimeter of the gate by road (the route ends on the
	// nearest road it can reach, behind a one-way or a barrier). Served, never in time.
	Stuck bool
}

// Result sums one rule over a scenario.
type Result struct {
	Rule                    string
	Orders, Served, Refused int
	InTime, Stuck           int
	// InTimeShare is of served orders; InTimePerOrder of every order placed, which is what a
	// customer sees: a refused order is not served in time either.
	InTimeShare, InTimePerOrder float64
	// Percentiles over served orders; a stuck ride counts as an endless leg.
	TimedP50, TimedP90 float64
	WaitP50, WaitP90   float64
}

// maxSnapM is how far a drawn point may be from its road to become a gate.
const maxSnapM = 60

// errSource is returned when an answer is a straight-line placeholder: a simulation on straight
// lines would prove nothing about roads.
var errSource = errors.New("sim: routing answered a straight line, not OSRM; is OSRM up?")

// Run simulates one scenario under both rules, on the same orders.
func Run(ctx context.Context, r Router, sc Scenario) ([]Result, error) {
	if sc.Orders <= 0 || len(sc.Riders) == 0 {
		return nil, errors.New("sim: a scenario needs orders and riders")
	}
	s := sc.Setting
	if s.PerimeterM >= s.ZoneRadiusM || s.TimerS <= 0 || s.StraightKmh <= 0 {
		return nil, errors.New("sim: settings contradict each other")
	}
	gates, err := drawGates(ctx, r, sc)
	if err != nil {
		return nil, err
	}
	rides := map[string][]Ride{RuleToday: {}, RuleRoad: {}}
	for _, gate := range gates {
		legs, err := r.Table(ctx, sc.Riders, gate, float64(s.StraightKmh))
		if err != nil {
			return nil, err
		}
		for _, l := range legs {
			if l.Source != routing.SourceOSRM {
				return nil, errSource
			}
		}
		for _, rule := range []string{RuleToday, RuleRoad} {
			pick := choose(rule, sc.Riders, legs, gate, s)
			if pick < 0 {
				rides[rule] = append(rides[rule], Ride{Refused: true, Rider: -1})
				continue
			}
			ride, err := Timed(ctx, r, sc.Riders[pick], gate, s)
			if err != nil {
				return nil, err
			}
			ride.Rider = pick
			rides[rule] = append(rides[rule], ride)
		}
	}
	return []Result{summarise(RuleToday, rides[RuleToday]), summarise(RuleRoad, rides[RuleRoad])}, nil
}

// choose picks a rider under a rule, or -1 when the rule refuses the order.
func choose(rule string, riders []routing.Point, legs []routing.Leg, gate routing.Point, s Settings) int {
	best, bestV := -1, math.Inf(1)
	switch rule {
	case RuleToday:
		reach := math.Min(float64(s.MaxSearchM), float64(s.StraightKmh)/3.6*float64(s.SweetSpotS))
		for i, p := range riders {
			if d := routing.Metres(p, gate); d <= reach && d < bestV {
				best, bestV = i, d
			}
		}
	case RuleRoad:
		for i, l := range legs {
			if l.Reachable && l.Seconds <= float64(s.SweetSpotS) && l.Seconds < bestV {
				best, bestV = i, l.Seconds
			}
		}
	}
	return best
}

// Timed rides from a rider to a gate. The rider's route line gives where it first comes within
// the zone radius of the gate (the timer starts) and then within the perimeter (it stops); one
// OSRM route through rider, start, stop and gate, with the line's heading at start and stop, gives
// the wait and the timed leg as two of its legs.
func Timed(ctx context.Context, r Router, rider, gate routing.Point, s Settings) (Ride, error) {
	route, err := r.Route(ctx, rider, gate, float64(s.StraightKmh))
	if err != nil {
		return Ride{}, err
	}
	if route.Source != routing.SourceOSRM {
		return Ride{}, errSource
	}
	if !route.Reachable || len(route.Line) < 2 {
		// Picked, but no road: in service this order would fail, so it is not a refusal.
		return Ride{Stuck: true, TimedS: math.Inf(1)}, nil
	}
	stop, stopSeg, okStop := firstWithin(route.Line, gate, float64(s.PerimeterM))
	if !okStop {
		return Ride{Stuck: true, TimedS: math.Inf(1)}, nil
	}
	start, startSeg, _ := firstWithin(route.Line, gate, float64(s.ZoneRadiusM))
	// Whether the rider is already inside is judged where the route starts (the snapped rider),
	// the same point the line is measured from.
	if stopSeg < 0 {
		return Ride{InTime: true}, nil // starts inside the perimeter: nothing to time
	}
	wps := []routing.Waypoint{{Point: route.Line[0], Bearing: -1}}
	insideZone := startSeg < 0
	if !insideZone {
		wps = append(wps, routing.Waypoint{Point: start, Bearing: headingAt(route.Line, startSeg)})
	}
	wps = append(wps,
		routing.Waypoint{Point: stop, Bearing: headingAt(route.Line, stopSeg)},
		routing.Waypoint{Point: route.Line[len(route.Line)-1], Bearing: -1})
	legs, err := r.Via(ctx, wps, float64(s.StraightKmh))
	if err != nil {
		return Ride{}, err
	}
	for _, l := range legs {
		if l.Source != routing.SourceOSRM {
			return Ride{}, errSource
		}
		if !l.Reachable {
			return Ride{Stuck: true, TimedS: math.Inf(1)}, nil
		}
	}
	ride := Ride{TimedS: legs[0].Seconds}
	if !insideZone {
		ride.WaitS, ride.TimedS = legs[0].Seconds, legs[1].Seconds
	}
	ride.InTime = ride.TimedS <= float64(s.TimerS)
	return ride, nil
}

// firstWithin is the first point along a line within radius metres of target, to about a metre,
// and the index of the segment it lies on (-1 when the line starts within the radius).
func firstWithin(line []routing.Point, target routing.Point, radius float64) (routing.Point, int, bool) {
	if len(line) == 0 {
		return routing.Point{}, 0, false
	}
	if routing.Metres(line[0], target) <= radius {
		return line[0], -1, true
	}
	for i := 1; i < len(line); i++ {
		a, b := line[i-1], line[i]
		// Sample the segment, so one that dips inside between its ends is found too.
		const samples = 16
		found := -1
		for k := 1; k <= samples; k++ {
			f := float64(k) / samples
			p := routing.Point{Lat: a.Lat + f*(b.Lat-a.Lat), Lng: a.Lng + f*(b.Lng-a.Lng)}
			if routing.Metres(p, target) <= radius {
				found = k
				break
			}
		}
		if found < 0 {
			continue
		}
		f0, f1 := float64(found-1)/samples, float64(found)/samples
		at := func(f float64) routing.Point {
			return routing.Point{Lat: a.Lat + f*(b.Lat-a.Lat), Lng: a.Lng + f*(b.Lng-a.Lng)}
		}
		// Bisect between the last outside sample and the first inside one.
		for range 40 {
			m := (f0 + f1) / 2
			if routing.Metres(at(m), target) <= radius {
				f1 = m
			} else {
				f0 = m
			}
			if routing.Metres(at(f0), at(f1)) < 0.5 {
				break
			}
		}
		return at(f1), i - 1, true
	}
	return routing.Point{}, 0, false
}

func summarise(rule string, rides []Ride) Result {
	res := Result{Rule: rule, Orders: len(rides)}
	var timed, wait []float64
	for _, r := range rides {
		if r.Refused {
			res.Refused++
			continue
		}
		res.Served++
		if r.Stuck {
			res.Stuck++
		}
		if r.InTime {
			res.InTime++
		}
		timed = append(timed, r.TimedS) // +Inf when stuck
		wait = append(wait, r.WaitS)
	}
	if res.Served > 0 {
		res.InTimeShare = float64(res.InTime) / float64(res.Served)
	}
	if res.Orders > 0 {
		res.InTimePerOrder = float64(res.InTime) / float64(res.Orders)
	}
	res.TimedP50, res.TimedP90 = percentile(timed, 0.5), percentile(timed, 0.9)
	res.WaitP50, res.WaitP90 = percentile(wait, 0.5), percentile(wait, 0.9)
	return res
}

// percentile is the nearest-rank percentile; zero for no values.
func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	k := int(math.Ceil(p*float64(len(s)))) - 1
	return s[max(0, min(k, len(s)-1))]
}

// Wilson is the 95% Wilson score interval for k successes in n trials.
func Wilson(k, n int) (lo, hi float64) {
	if n == 0 {
		return 0, 1
	}
	const z = 1.959963984540054
	p, nf := float64(k)/float64(n), float64(n)
	d := 1 + z*z/nf
	c := p + z*z/(2*nf)
	w := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf))
	return (c - w) / d, (c + w) / d
}

// headingAt is the line's heading on segment i, taken to the next point at least a metre on, so a
// repeated point does not give a false heading of north; -1 (any heading) if there is none.
func headingAt(line []routing.Point, i int) int {
	for j := i + 1; j < len(line); j++ {
		if routing.Metres(line[i], line[j]) >= 1 {
			return routing.Bearing(line[i], line[j])
		}
	}
	return -1
}

// drawGates draws sc.Orders gates in the area, deterministic by seed. A gate is a customer's
// entrance, on a road: draw a point, snap it to the nearest road, and draw again when the nearest
// road is far (the point fell in a park or a large building).
func drawGates(ctx context.Context, r Router, sc Scenario) ([]routing.Point, error) {
	if err := checkHotspots(sc.Hotspots); err != nil {
		return nil, err
	}
	rng := rand.New(rand.NewPCG(sc.Seed, 205))
	gates := make([]routing.Point, 0, sc.Orders)
	for tries := 0; len(gates) < sc.Orders; tries++ {
		if tries > 20*sc.Orders {
			return nil, errors.New("sim: too few points in the area are near a road")
		}
		p := sc.point(rng)
		if !sc.Area.contains(p) {
			continue // a hotspot's tail outside the area
		}
		snap, err := r.Nearest(ctx, p)
		if err != nil {
			return nil, err
		}
		if snap.Source != routing.SourceOSRM {
			return nil, errSource
		}
		if snap.NoRoad || snap.Moved > maxSnapM {
			continue
		}
		gates = append(gates, snap.Point)
	}
	return gates, nil
}
