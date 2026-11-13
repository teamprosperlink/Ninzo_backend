// Package placement chooses where scooters wait (issue 216, geo record W4): the maximum expected
// covering model from emergency services (research.md section 2), solved greedily, on road times.
//
// A demand point d is covered by a waiting point when a scooter there reaches d within the budget
// by road. A scooter is busy a share q of the time, so a point covered by k scooters is served with
// chance 1 − q^k. Greedy adds one scooter at a time where it raises Σ w_d (1 − q^{k_d}) the most;
// the objective is submodular, so greedy is within (1 − 1/e) of the best and, at pilot size, close
// to it in practice.
package placement

import (
	"context"
	"errors"
	"math"

	"github.com/teamprosperlink/Ninzo_backend/internal/routing"
)

// Coverage says which waiting points (rows) cover which demand points (columns).
type Coverage [][]bool

// Plan is a placement: how many scooters wait at each candidate point.
type Plan struct {
	Count []int // per candidate
	// Expected is Σ w_d (1 − q^{k_d}) / Σ w_d: the share of demand expected to find a free scooter in
	// reach; Covered is the share of demand within reach of at least one scooter.
	Expected, Covered float64
	// Gains are each pick's marginal gain, in pick order.
	Gains []float64
}

// Greedy places n scooters on the candidates, at most capacity per candidate, to maximise the
// expected covered demand with busy share q (0 ≤ q < 1). weights are demand per point.
func Greedy(cov Coverage, weights []float64, n int, q float64, capacity int) (Plan, error) {
	if len(cov) == 0 || n < 0 || capacity < 1 || q < 0 || q >= 1 {
		return Plan{}, errors.New("placement: candidates, n ≥ 0, capacity ≥ 1 and 0 ≤ q < 1 are needed")
	}
	for _, row := range cov {
		if len(row) != len(weights) {
			return Plan{}, errors.New("placement: every coverage row needs one entry per demand point")
		}
	}
	for _, w := range weights {
		if w < 0 || math.IsNaN(w) || math.IsInf(w, 0) {
			return Plan{}, errors.New("placement: demand weights must be finite and not negative")
		}
	}
	if n > len(cov)*capacity {
		return Plan{}, errors.New("placement: more scooters than the candidates can hold")
	}
	k := make([]int, len(weights)) // scooters covering each demand point
	plan := Plan{Count: make([]int, len(cov))}
	// The gain of one more scooter covering d, which already has k_d: w_d q^{k_d} (1 − q).
	gain := func(c int) float64 {
		g := 0.0
		for d, covers := range cov[c] {
			if covers {
				g += weights[d] * math.Pow(q, float64(k[d])) * (1 - q)
			}
		}
		return g
	}
	for range n {
		best, bestG := -1, -1.0
		for c := range cov {
			if plan.Count[c] >= capacity {
				continue
			}
			// Ties go to the lower index, so a plan is the same every run.
			if g := gain(c); g > bestG {
				best, bestG = c, g
			}
		}
		plan.Count[best]++
		plan.Gains = append(plan.Gains, bestG)
		for d, covers := range cov[best] {
			if covers {
				k[d]++
			}
		}
	}
	total, exp, covered := 0.0, 0.0, 0.0
	for d, w := range weights {
		total += w
		exp += w * (1 - math.Pow(q, float64(k[d])))
		if k[d] > 0 {
			covered += w
		}
	}
	if total > 0 {
		plan.Expected, plan.Covered = exp/total, covered/total
	}
	return plan, nil
}

// Router is what Matrix needs from internal/routing.
type Router interface {
	Table(ctx context.Context, sources []routing.Point, dest routing.Point, speedKmh float64) ([]routing.Leg, error)
}

// ErrNoRoads is returned when routing answers straight lines: a placement on guesses is no plan.
var ErrNoRoads = errors.New("placement: routing answered a straight line, not OSRM")

// Matrix builds the coverage of demand by candidates: a candidate covers a demand point when the
// road time from it is within budgetS. One Table call per demand point; an answer that falls back to
// straight lines (a timeout, say) is asked again, up to three attempts in all, before the matrix is refused.
func Matrix(ctx context.Context, r Router, candidates, demand []routing.Point, budgetS, speedKmh float64) (Coverage, error) {
	cov := make(Coverage, len(candidates))
	for c := range cov {
		cov[c] = make([]bool, len(demand))
	}
	for d, p := range demand {
		var legs []routing.Leg
		for try := 0; ; try++ {
			var err error
			if legs, err = r.Table(ctx, candidates, p, speedKmh); err != nil {
				return nil, err
			}
			if len(legs) != len(candidates) {
				return nil, errors.New("placement: routing answered the wrong number of legs")
			}
			if allOSRM(legs) {
				break
			}
			if try == 2 {
				return nil, ErrNoRoads
			}
		}
		for c, l := range legs {
			cov[c][d] = l.Reachable && l.Seconds <= budgetS
		}
	}
	return cov, nil
}

func allOSRM(legs []routing.Leg) bool {
	for _, l := range legs {
		if l.Source != routing.SourceOSRM {
			return false
		}
	}
	return true
}
