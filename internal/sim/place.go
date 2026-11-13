package sim

import (
	"context"
	"errors"

	"github.com/teamprosperlink/Ninzo_backend/internal/placement"
	"github.com/teamprosperlink/Ninzo_backend/internal/routing"
)

// PlaceOptions say how Place chooses waiting points (issue 216, W4).
type PlaceOptions struct {
	N          int     // scooters to place
	Q          float64 // busy share for expected covering (0 for maximal covering)
	Capacity   int     // scooters per point
	GridRows   int     // candidate grid across the area
	GridCols   int
	Demand     int    // demand points drawn to place on
	DemandSeed uint64 // a seed of its own: placement is never judged on the demand it was fitted to
}

// Place chooses n waiting points for a scenario: candidates are a grid across the area, each
// snapped to its nearest road (dropped when more than maxSnapM away); demand is gates drawn with
// DemandSeed; a candidate covers a gate within the scenario's sweet_spot_budget_s by road.
func Place(ctx context.Context, r Router, sc Scenario, o PlaceOptions) ([]routing.Point, placement.Plan, error) {
	if o.N < 1 || o.GridRows < 1 || o.GridCols < 1 || o.Demand < 1 || o.Capacity < 1 {
		return nil, placement.Plan{}, errors.New("sim: placement needs n, a grid, demand and capacity")
	}
	var cands []routing.Point
	for i := range o.GridRows {
		for j := range o.GridCols {
			p := routing.Point{
				Lat: sc.Area.MinLat + (float64(i)+0.5)/float64(o.GridRows)*(sc.Area.MaxLat-sc.Area.MinLat),
				Lng: sc.Area.MinLng + (float64(j)+0.5)/float64(o.GridCols)*(sc.Area.MaxLng-sc.Area.MinLng),
			}
			snap, err := r.Nearest(ctx, p)
			if err != nil {
				return nil, placement.Plan{}, err
			}
			if snap.Source != routing.SourceOSRM {
				return nil, placement.Plan{}, errSource
			}
			if snap.NoRoad || snap.Moved > maxSnapM {
				continue
			}
			cands = append(cands, snap.Point)
		}
	}
	if len(cands)*o.Capacity < o.N {
		return nil, placement.Plan{}, errors.New("sim: too few candidate points near a road for this many scooters")
	}
	dsc := sc
	dsc.Orders, dsc.Seed = o.Demand, o.DemandSeed
	demand, err := drawGates(ctx, r, dsc)
	if err != nil {
		return nil, placement.Plan{}, err
	}
	cov, err := placement.Matrix(ctx, r, cands, demand, float64(sc.Setting.SweetSpotS), float64(sc.Setting.StraightKmh))
	if err != nil {
		return nil, placement.Plan{}, err
	}
	w := make([]float64, len(demand))
	for i := range w {
		w[i] = 1 // uniform demand until W7
	}
	plan, err := placement.Greedy(cov, w, o.N, o.Q, o.Capacity)
	if err != nil {
		return nil, placement.Plan{}, err
	}
	var out []routing.Point
	for c, n := range plan.Count {
		for range n {
			out = append(out, cands[c])
		}
	}
	return out, plan, nil
}
