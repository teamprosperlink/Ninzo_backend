package sim

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/teamprosperlink/Ninzo_backend/internal/demand"
	"github.com/teamprosperlink/Ninzo_backend/internal/hexgrid"
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

func (o PlaceOptions) check() error {
	if o.N < 1 || o.GridRows < 1 || o.GridCols < 1 || o.Capacity < 1 {
		return errors.New("sim: placement needs n, a grid and capacity")
	}
	return nil
}

// candidates are a grid across the area, each snapped to its nearest road (dropped when more than
// maxSnapM away).
func candidates(ctx context.Context, r Router, sc Scenario, o PlaceOptions) ([]routing.Point, error) {
	var cands []routing.Point
	for i := range o.GridRows {
		for j := range o.GridCols {
			p := routing.Point{
				Lat: sc.Area.MinLat + (float64(i)+0.5)/float64(o.GridRows)*(sc.Area.MaxLat-sc.Area.MinLat),
				Lng: sc.Area.MinLng + (float64(j)+0.5)/float64(o.GridCols)*(sc.Area.MaxLng-sc.Area.MinLng),
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
			cands = append(cands, snap.Point)
		}
	}
	if len(cands)*o.Capacity < o.N {
		return nil, errors.New("sim: too few candidate points near a road for this many scooters")
	}
	return cands, nil
}

// placeOn places on weighted demand points; a candidate covers a point within the scenario's
// sweet_spot_budget_s by road.
func placeOn(ctx context.Context, r Router, sc Scenario, o PlaceOptions, pts []routing.Point, w []float64) ([]routing.Point, placement.Plan, error) {
	cands, err := candidates(ctx, r, sc, o)
	if err != nil {
		return nil, placement.Plan{}, err
	}
	cov, err := placement.Matrix(ctx, r, cands, pts, float64(sc.Setting.SweetSpotS), float64(sc.Setting.StraightKmh))
	if err != nil {
		return nil, placement.Plan{}, err
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

// Place chooses n waiting points for a scenario: demand is gates drawn with DemandSeed (from the
// scenario's hotspots, if any), each weighing the same.
func Place(ctx context.Context, r Router, sc Scenario, o PlaceOptions) ([]routing.Point, placement.Plan, error) {
	if err := o.check(); err != nil {
		return nil, placement.Plan{}, err
	}
	if o.Demand < 1 {
		return nil, placement.Plan{}, errors.New("sim: placement needs demand points")
	}
	dsc := sc
	dsc.Orders, dsc.Seed = o.Demand, o.DemandSeed
	gates, err := drawGates(ctx, r, dsc)
	if err != nil {
		return nil, placement.Plan{}, err
	}
	w := make([]float64, len(gates))
	for i := range w {
		w[i] = 1
	}
	return placeOn(ctx, r, sc, o, gates, w)
}

// PlaceOnCells places on demand given per blr76 cell (from demand.History.Forecast, say; issue
// 222): each cell's centre, snapped to a road, is a demand point with its weight. A cell with no road
// near its centre is left out.
func PlaceOnCells(ctx context.Context, r Router, sc Scenario, cells []string, weights []float64, o PlaceOptions) ([]routing.Point, placement.Plan, error) {
	if err := o.check(); err != nil {
		return nil, placement.Plan{}, err
	}
	if len(cells) == 0 || len(cells) != len(weights) {
		return nil, placement.Plan{}, errors.New("sim: one weight per cell, and at least one cell")
	}
	g := hexgrid.Bengaluru()
	var pts []routing.Point
	var w []float64
	for i, id := range cells {
		c, err := g.Parse(id)
		if err != nil {
			return nil, placement.Plan{}, err
		}
		centre := g.Centre(c)
		snap, err := r.Nearest(ctx, routing.Point{Lat: centre.Lat, Lng: centre.Lng})
		if err != nil {
			return nil, placement.Plan{}, err
		}
		if snap.Source != routing.SourceOSRM {
			return nil, placement.Plan{}, errSource
		}
		if snap.NoRoad || snap.Moved > maxSnapM {
			continue
		}
		pts = append(pts, snap.Point)
		w = append(w, weights[i])
	}
	if len(pts) == 0 {
		return nil, placement.Plan{}, errors.New("sim: no demand cell is near a road")
	}
	return placeOn(ctx, r, sc, o, pts, w)
}

// CellDemand makes a synthetic history like the one demand_checks holds: for each of weeks past
// weeks, perWeek gates drawn from the scenario (seeds seed, seed+1, ...) at one weekday and band,
// counted per blr76 cell. It returns, sorted by cell, the forecast for the week after, for each cell
// with demand, by demand.History.Forecast with decay.
func CellDemand(ctx context.Context, r Router, sc Scenario, weeks, perWeek int, seed uint64, decay float64) ([]string, []float64, error) {
	if weeks < 1 || perWeek < 1 {
		return nil, nil, errors.New("sim: weeks and gates per week are needed")
	}
	g := hexgrid.Bengaluru()
	target := demand.SlotOf(time.Date(2026, 11, 13, 13, 30, 0, 0, time.UTC)) // a Friday, 19:00 IST
	var cs []demand.Count
	for k := 1; k <= weeks; k++ {
		wsc := sc
		wsc.Orders, wsc.Seed = perWeek, seed+uint64(k-1)
		gates, err := drawGates(ctx, r, wsc)
		if err != nil {
			return nil, nil, err
		}
		slot := demand.Slot{Day: target.Day.AddDate(0, 0, -7*k), Band: target.Band}
		for _, p := range gates {
			c, err := g.CellAt(hexgrid.Point{Lat: p.Lat, Lng: p.Lng})
			if err != nil {
				return nil, nil, err
			}
			cs = append(cs, demand.Count{Cell: g.ID(c), Slot: slot, Asked: 1})
		}
	}
	h := demand.NewHistory(cs, target.Day.AddDate(0, 0, -7*weeks), func(c demand.Count) float64 { return float64(c.Asked) })
	cells := make([]string, 0, len(h.At))
	for cell := range h.At {
		cells = append(cells, cell)
	}
	sort.Strings(cells) // a placement is the same every run
	var outCells []string
	var w []float64
	for _, cell := range cells {
		if f, ok := h.Forecast(cell, target, weeks, decay); ok && f > 0 {
			outCells = append(outCells, cell)
			w = append(w, f)
		}
	}
	return outCells, w, nil
}
