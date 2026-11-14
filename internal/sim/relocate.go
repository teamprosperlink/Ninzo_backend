package sim

import (
	"context"
	"errors"
	"math"
	"sort"

	"github.com/teamprosperlink/Ninzo_backend/internal/placement"
	"github.com/teamprosperlink/Ninzo_backend/internal/routing"
)

// Relocation rules compared by RunRelocation (issue 255).
const (
	RelocNone  = "none"  // riders stay at the node they start at
	RelocToday = "today" // internal/relocation's rule with relocation_by_plan off
	RelocPlan  = "plan"  // the placement-plan rule (#226, #249), falling back to today's
)

// RelocScenario is a fleet scenario whose riders can be moved between staging nodes.
type RelocScenario struct {
	FleetScenario // Riders are start points; each rider starts at the node nearest theirs
	Nodes         []routing.Point
	// PlanPicks is the greedy pick order over Nodes (placement.Plan.Picks): the plan for n free
	// riders is its first n entries, as in placement_plans.
	PlanPicks []int
	CheckS    float64 // seconds between relocation checks (customer-api: 30)
	WindowS   float64 // relocation_window_s
	MinOrders int     // relocation_min_orders
	RestS     float64 // relocation_rest_s, counted from arrival
	// MoverTakesOrders is the optimistic bound on a move's cost: a rider with a move in play counts
	// as waiting at the target (as production does once the move is accepted) and can take orders as
	// if already there. Off: a moving rider takes no order until arrival (the pessimistic bound).
	MoverTakesOrders bool
}

// RelocResult sums one relocation rule over a scenario. Orders are matched by today's rule.
type RelocResult struct {
	FleetResult
	Relocation string
	Moves      int
	MoveS      float64 // riding between nodes, paid in production
}

type mover struct {
	node     int
	freeAt   float64 // free from then: after an order or a move
	busy     float64
	restTill float64
	moving   bool // a move toward node started; it is in play until arriveAt
	arriveAt float64
}

// PlanPicks fits the plan the way placementplan does, on demand drawn with o.DemandSeed: candidate
// nodes on a grid, maximum expected covering on road times. It returns the nodes and the pick order.
func PlanPicks(ctx context.Context, r Router, sc Scenario, o PlaceOptions) ([]routing.Point, []int, placement.Plan, error) {
	if err := o.check(); err != nil {
		return nil, nil, placement.Plan{}, err
	}
	if o.Demand < 1 {
		return nil, nil, placement.Plan{}, errors.New("sim: a plan needs demand points")
	}
	dsc := sc
	dsc.Orders, dsc.Seed = o.Demand, o.DemandSeed
	gates, err := drawGates(ctx, r, dsc)
	if err != nil {
		return nil, nil, placement.Plan{}, err
	}
	w := make([]float64, len(gates))
	for i := range w {
		w[i] = 1
	}
	all, err := candidates(ctx, r, sc, o)
	if err != nil {
		return nil, nil, placement.Plan{}, err
	}
	// Two grid points can snap to the same road point; keep one, so no node is a copy of another.
	seen := map[routing.Point]bool{}
	var cands []routing.Point
	for _, p := range all {
		if !seen[p] {
			seen[p] = true
			cands = append(cands, p)
		}
	}
	cov, err := placement.Matrix(ctx, r, cands, gates, float64(sc.Setting.SweetSpotS), float64(sc.Setting.StraightKmh))
	if err != nil {
		return nil, nil, placement.Plan{}, err
	}
	plan, err := placement.Greedy(cov, w, o.N, o.Q, o.Capacity)
	if err != nil {
		return nil, nil, placement.Plan{}, err
	}
	return cands, plan.Picks, plan, nil
}

func (rs RelocScenario) check() error {
	if len(rs.Nodes) < 2 || rs.CheckS <= 0 || rs.WindowS <= 0 || rs.MinOrders < 1 || rs.RestS < 0 {
		return errors.New("sim: relocation needs two nodes, a check interval, a window, a minimum and no negative rest")
	}
	for _, p := range rs.PlanPicks {
		if p < 0 || p >= len(rs.Nodes) {
			return errors.New("sim: a plan pick is not a node")
		}
	}
	return nil
}

// RunRelocation simulates the three relocation rules on the same orders.
func RunRelocation(ctx context.Context, r Router, rs RelocScenario) ([]RelocResult, error) {
	fs := rs.FleetScenario
	if len(fs.Riders) == 0 || fs.OrdersPerHour <= 0 || fs.Hours <= 0 || fs.PackS < 0 || fs.HandoverS < 0 {
		return nil, errors.New("sim: a fleet scenario needs riders, a rate, hours, and no negative times")
	}
	if s := fs.Setting; s.PerimeterM >= s.ZoneRadiusM || s.TimerS <= 0 || s.StraightKmh <= 0 {
		return nil, errors.New("sim: settings contradict each other")
	}
	if err := rs.check(); err != nil {
		return nil, err
	}
	times, gates, err := arrivals(ctx, r, fs)
	if err != nil {
		return nil, err
	}
	var out []RelocResult
	for _, rule := range []string{RelocNone, RelocToday, RelocPlan} {
		res, err := runReloc(ctx, r, rs, rule, times, gates)
		if err != nil {
			return nil, err
		}
		out = append(out, res)
	}
	return out, nil
}

func nearestNode(nodes []routing.Point, p routing.Point) int {
	best, bd := 0, math.Inf(1)
	for i, n := range nodes {
		if d := sq(n, p); d < bd {
			best, bd = i, d
		}
	}
	return best
}

// sq is the squared degree difference, the straight-line measure internal/relocation uses.
func sq(a, b routing.Point) float64 {
	return (a.Lat-b.Lat)*(a.Lat-b.Lat) + (a.Lng-b.Lng)*(a.Lng-b.Lng)
}

func runReloc(ctx context.Context, r Router, rs RelocScenario, rule string, times []float64, gates []routing.Point) (RelocResult, error) {
	fs := rs.FleetScenario
	s := fs.Setting
	riders := make([]mover, len(fs.Riders))
	for i, p := range fs.Riders {
		riders[i] = mover{node: nearestNode(rs.Nodes, p)}
	}
	res := RelocResult{Relocation: rule}
	var rides []Ride
	var done []served // for today's demand count
	next := rs.CheckS
	for k, t := range times {
		for rule != RelocNone && next <= t {
			if err := relocate(ctx, r, rs, rule, riders, done, next, &res); err != nil {
				return RelocResult{}, err
			}
			next += rs.CheckS
		}
		gate := gates[k]
		all := make([]routing.Point, len(riders))
		for i, rd := range riders {
			all[i] = rs.Nodes[rd.node]
		}
		allLegs, err := r.Table(ctx, all, gate, float64(s.StraightKmh))
		if err != nil {
			return RelocResult{}, err
		}
		for _, l := range allLegs {
			if l.Source != routing.SourceOSRM {
				return RelocResult{}, errSource
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
			pick = choose(RuleToday, pts, legs, gate, s)
		}
		if pick < 0 {
			if choose(RuleToday, all, allLegs, gate, s) >= 0 {
				res.RefusedBusy++
			} else {
				res.RefusedReach++
			}
			rides = append(rides, Ride{Refused: true, Rider: -1})
			continue
		}
		idx := free[pick]
		home := rs.Nodes[riders[idx].node]
		ride, err := Timed(ctx, r, home, gate, s)
		if err != nil {
			return RelocResult{}, err
		}
		ride.Rider = idx
		there := legs[pick].Seconds
		back, err := r.Route(ctx, gate, home, float64(s.StraightKmh))
		if err != nil {
			return RelocResult{}, err
		}
		if back.Source != routing.SourceOSRM {
			return RelocResult{}, errSource
		}
		returnS := back.Seconds
		if !back.Reachable || !legs[pick].Reachable || ride.Stuck {
			d := routing.Metres(home, gate)
			there, returnS = d/(float64(s.StraightKmh)/3.6), d/(float64(s.StraightKmh)/3.6)
		}
		dur := fs.PackS + there + fs.HandoverS + returnS
		riders[idx].freeAt, riders[idx].moving = t+dur, false
		riders[idx].busy += dur
		rides = append(rides, ride)
		done = append(done, served{at: t, node: nearestNode(rs.Nodes, gate)})
	}
	res.Result = summarise(RuleToday, rides)
	end := fs.Hours * 3600
	res.BusyShare = make([]float64, len(riders))
	for i, rd := range riders {
		res.BusyShare[i] = math.Min(1, rd.busy/end)
	}
	return res, nil
}

// served is a paid order, counted at the node nearest its gate, as internal/relocation counts.
type served struct {
	at   float64
	node int
}

type spot struct {
	id       int
	demand   int
	waiting  int
	free     []int // askable riders waiting here, longest free first
	incoming bool
}

// relocate runs one check at t, as internal/relocation.suggestIn does, and starts the moves.
func relocate(ctx context.Context, r Router, rs RelocScenario, rule string, riders []mover, done []served, t float64, res *RelocResult) error {
	if rule == RelocNone {
		return nil
	}
	spots := make([]*spot, len(rs.Nodes))
	for i := range spots {
		spots[i] = &spot{id: i}
	}
	for _, o := range done {
		if o.at > t-rs.WindowS && o.at <= t {
			spots[o.node].demand++
		}
	}
	var waitingIdx []int
	for i, rd := range riders {
		if rd.moving && rd.arriveAt > t {
			spots[rd.node].incoming = true
			if !rs.MoverTakesOrders {
				continue
			}
		}
		if rd.freeAt <= t {
			waitingIdx = append(waitingIdx, i)
		}
	}
	// Longest free first, as production asks the longest-waiting rider.
	sort.SliceStable(waitingIdx, func(a, b int) bool { return riders[waitingIdx[a]].freeAt < riders[waitingIdx[b]].freeAt })
	free := 0
	for _, i := range waitingIdx {
		sp := spots[riders[i].node]
		sp.waiting++
		free++
		if riders[i].restTill <= t {
			sp.free = append(sp.free, i)
		}
	}
	move := func(from, to *spot) error {
		who := from.free[0]
		leg, err := r.Route(ctx, rs.Nodes[from.id], rs.Nodes[to.id], float64(rs.Setting.StraightKmh))
		if err != nil {
			return err
		}
		if leg.Source != routing.SourceOSRM {
			return errSource
		}
		d := leg.Seconds
		if !leg.Reachable {
			d = routing.Metres(rs.Nodes[from.id], rs.Nodes[to.id]) / (float64(rs.Setting.StraightKmh) / 3.6)
		}
		riders[who].node, riders[who].moving, riders[who].arriveAt = to.id, true, t+d
		if !rs.MoverTakesOrders {
			riders[who].freeAt = t + d
		}
		riders[who].restTill = t + d + rs.RestS
		res.Moves++
		res.MoveS += d
		from.free, from.waiting = from.free[1:], from.waiting-1
		to.incoming = true
		return nil
	}
	if rule == RelocPlan && free > 0 && free <= len(rs.PlanPicks) {
		want := make([]int, len(rs.Nodes))
		for _, p := range rs.PlanPicks[:free] {
			want[p]++
		}
		var short []*spot
		for _, sp := range spots {
			if want[sp.id] > sp.waiting && !sp.incoming {
				short = append(short, sp)
			}
		}
		sort.SliceStable(short, func(a, b int) bool {
			return want[short[a].id]-short[a].waiting > want[short[b].id]-short[b].waiting
		})
		for _, to := range short {
			var from *spot
			best := 0.0
			for _, sp := range spots {
				if sp.id == to.id || sp.waiting <= want[sp.id] || len(sp.free) == 0 {
					continue
				}
				if d := sq(rs.Nodes[sp.id], rs.Nodes[to.id]); from == nil || d < best {
					from, best = sp, d
				}
			}
			if from != nil {
				if err := move(from, to); err != nil {
					return err
				}
			}
		}
		return nil
	}
	// Today's rule (and the plan rule's fallback with no plan for this n).
	var short []*spot
	for _, sp := range spots {
		if sp.demand >= rs.MinOrders && sp.waiting == 0 && !sp.incoming {
			short = append(short, sp)
		}
	}
	sort.SliceStable(short, func(a, b int) bool { return short[a].demand > short[b].demand })
	for _, to := range short {
		var from *spot
		best := 0.0
		for _, sp := range spots {
			if sp.id == to.id || sp.waiting < 2 || len(sp.free) == 0 {
				continue
			}
			if d := sq(rs.Nodes[sp.id], rs.Nodes[to.id]); from == nil || d < best {
				from, best = sp, d
			}
		}
		if from != nil {
			if err := move(from, to); err != nil {
				return err
			}
		}
	}
	return nil
}
