package sim

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"

	"github.com/teamprosperlink/Ninzo_backend/internal/routing"
)

// FleetScenario is a simulated stretch of hours with a fleet that gets busy (issue 214, W8 step 3).
type FleetScenario struct {
	Scenario              // area, rider nodes, seed and settings; Orders is ignored
	OrdersPerHour float64 // Poisson arrival rate
	Hours         float64
	PackS         float64 // packing, stopped at the node, after accepting
	HandoverS     float64 // at the gate, before riding back
}

// FleetResult sums one rule over a fleet scenario.
type FleetResult struct {
	Result
	// RefusedBusy: the rule would have taken a rider had they been free, but every rider in its
	// reach was busy. RefusedReach: nobody was within the rule's reach, free or not. Refused is
	// their sum.
	RefusedBusy, RefusedReach int
	// BusyShare is each rider's share of the simulated time spent on orders, in rider order.
	BusyShare []float64
}

// rider is one rider's state in a run.
type riderState struct {
	node   routing.Point
	freeAt float64 // seconds from the start
	busy   float64 // seconds spent on orders
}

// arrivals are the order times (seconds) and gates, the same for every rule.
func arrivals(ctx context.Context, r Router, fs FleetScenario) ([]float64, []routing.Point, error) {
	rng := rand.New(rand.NewPCG(fs.Seed, 214))
	end := fs.Hours * 3600
	var times []float64
	for t := 0.0; ; {
		t += rng.ExpFloat64() / fs.OrdersPerHour * 3600
		if t >= end {
			break
		}
		times = append(times, t)
	}
	sc := fs.Scenario
	sc.Orders = len(times)
	if sc.Orders == 0 {
		return nil, nil, nil
	}
	gates, err := drawGates(ctx, r, sc)
	return times, gates, err
}

// RunFleet simulates both rules over the same arrivals, each with its own fleet state.
func RunFleet(ctx context.Context, r Router, fs FleetScenario) ([]FleetResult, error) {
	if len(fs.Riders) == 0 || fs.OrdersPerHour <= 0 || fs.Hours <= 0 || fs.PackS < 0 || fs.HandoverS < 0 {
		return nil, errors.New("sim: a fleet scenario needs riders, a rate, hours, and no negative times")
	}
	s := fs.Setting
	if s.PerimeterM >= s.ZoneRadiusM || s.TimerS <= 0 || s.StraightKmh <= 0 {
		return nil, errors.New("sim: settings contradict each other")
	}
	times, gates, err := arrivals(ctx, r, fs)
	if err != nil {
		return nil, err
	}
	var out []FleetResult
	for _, rule := range []string{RuleToday, RuleRoad} {
		res, err := runFleetRule(ctx, r, fs, rule, times, gates)
		if err != nil {
			return nil, err
		}
		out = append(out, res)
	}
	return out, nil
}

func runFleetRule(ctx context.Context, r Router, fs FleetScenario, rule string, times []float64, gates []routing.Point) (FleetResult, error) {
	s := fs.Setting
	riders := make([]riderState, len(fs.Riders))
	for i, p := range fs.Riders {
		riders[i] = riderState{node: p}
	}
	var rides []Ride
	res := FleetResult{}
	for k, t := range times {
		gate := gates[k]
		// Road times from every rider's node, in one call: they say both whom the rule would take if
		// everyone were free, and whom it can take now.
		all := make([]routing.Point, len(riders))
		for i, rd := range riders {
			all[i] = rd.node // a free rider waits at their node
		}
		allLegs, err := r.Table(ctx, all, gate, float64(s.StraightKmh))
		if err != nil {
			return FleetResult{}, err
		}
		for _, l := range allLegs {
			if l.Source != routing.SourceOSRM {
				return FleetResult{}, errSource
			}
		}
		var free []int
		for i, rd := range riders {
			if rd.freeAt <= t {
				free = append(free, i)
			}
		}
		pts := make([]routing.Point, len(free))
		legs := make([]routing.Leg, len(free))
		for i, idx := range free {
			pts[i], legs[i] = all[idx], allLegs[idx]
		}
		pick := -1
		if len(free) > 0 {
			pick = choose(rule, pts, legs, gate, s)
		}
		if pick < 0 {
			// Busy: the rule would have taken someone had they been free. Reach: nobody is within
			// the rule's reach, free or not.
			if choose(rule, all, allLegs, gate, s) >= 0 {
				res.RefusedBusy++
			} else {
				res.RefusedReach++
			}
			rides = append(rides, Ride{Refused: true, Rider: -1})
			continue
		}
		idx := free[pick]
		ride, err := Timed(ctx, r, riders[idx].node, gate, s)
		if err != nil {
			return FleetResult{}, err
		}
		ride.Rider = idx
		// Busy: packing, the whole ride to the gate, the handover, the ride back to the node.
		there := legs[pick].Seconds
		back, err := r.Route(ctx, gate, riders[idx].node, float64(s.StraightKmh))
		if err != nil {
			return FleetResult{}, err
		}
		if back.Source != routing.SourceOSRM {
			return FleetResult{}, errSource
		}
		returnS := back.Seconds
		if !back.Reachable || !legs[pick].Reachable || ride.Stuck {
			// Stuck: the rider still spends the attempt; count a ride there and back by the
			// straight line at the slow speed, so a stuck order is not free.
			d := routing.Metres(riders[idx].node, gate)
			there, returnS = d/(float64(s.StraightKmh)/3.6), d/(float64(s.StraightKmh)/3.6)
		}
		dur := fs.PackS + there + fs.HandoverS + returnS
		riders[idx].freeAt = t + dur
		riders[idx].busy += dur
		rides = append(rides, ride)
	}
	res.Result = summarise(rule, rides)
	end := fs.Hours * 3600
	res.BusyShare = make([]float64, len(riders))
	for i, rd := range riders {
		res.BusyShare[i] = math.Min(1, rd.busy/end)
	}
	return res, nil
}
