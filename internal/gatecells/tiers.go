package gatecells

import (
	"context"
	"fmt"

	"github.com/teamprosperlink/Ninzo_backend/internal/hexgrid"
)

// Reach is how far a cell is from its gate by road, against the settings in force (W2 step 4).
type Reach string

const (
	// ReachSweetSpot: road time to the gate within sweet_spot_budget_s.
	ReachSweetSpot Reach = "sweet_spot"
	// ReachOuterRing: within outer_ring_budget_s, beyond the sweet spot.
	ReachOuterRing Reach = "outer_ring"
	// ReachBeyond: a road joins them, but it takes longer than the outer ring.
	ReachBeyond Reach = "beyond"
	// ReachNone: no road joins the cell to the gate.
	ReachNone Reach = "unreachable"
)

// Tier is one cell's place around its gate.
type Tier struct {
	Cell hexgrid.Cell
	// InTimerZone: the whole cell is inside zone_radius_m, and a rider anywhere in it can reach
	// the gate by road at least timer_zone_margin_s under the timer. Only the centre is routed, so
	// one cell edge is added to the distance and, at straight_line_speed_kmh, to the time. The time
	// counted runs to the gate, never less than to the arrival perimeter on the way. Not covered: a
	// median or one-way that splits a cell, where its corners differ by more than the edge; the
	// corner routes of the road timer-zone mode (G20) handle that.
	InTimerZone bool
	Reach       Reach
	RoadSeconds float64 // zero when unreachable
}

// Getter is the settings a zone runs with; geo.Effective is one.
type Getter interface {
	Get(name string) (int, bool)
}

// tierSettings are the settings Classify reads.
var tierSettings = []string{"zone_radius_m", "timer_length_s", "timer_zone_margin_s", "sweet_spot_budget_s", "outer_ring_budget_s", "straight_line_speed_kmh", "max_search_radius_m"}

// Budgets are the settings Classify uses, read once, and the grid's cell edge.
type Budgets struct {
	ZoneRadiusM int     `json:"zone_radius_m"`
	TimerS      int     `json:"timer_length_s"`
	MarginS     int     `json:"timer_zone_margin_s"`
	SweetSpotS  int     `json:"sweet_spot_budget_s"`
	OuterRingS  int     `json:"outer_ring_budget_s"`
	CrossKmh    int     `json:"straight_line_speed_kmh"` // crossing a cell
	MaxSearchM  int     `json:"max_search_radius_m"`     // no reach past it
	CellEdgeM   float64 `json:"cell_edge_m"`             // the grid's edge, centre to corner
}

// ReadBudgets takes the tier settings from a zone's settings, each of which must be set, and the
// cell edge of the grid the cells are on.
func ReadBudgets(s Getter, g *hexgrid.Grid) (Budgets, error) {
	v := make([]int, len(tierSettings))
	for i, name := range tierSettings {
		x, ok := s.Get(name)
		if !ok {
			return Budgets{}, fmt.Errorf("gatecells: setting %s is not set", name)
		}
		v[i] = x
	}
	if v[5] <= 0 {
		return Budgets{}, fmt.Errorf("gatecells: straight_line_speed_kmh must be positive")
	}
	return Budgets{ZoneRadiusM: v[0], TimerS: v[1], MarginS: v[2], SweetSpotS: v[3], OuterRingS: v[4],
		CrossKmh: v[5], MaxSearchM: v[6], CellEdgeM: g.Edge()}, nil
}

// Classify places one cell. It is a pure function of the stored times and the budgets, so a
// change of settings moves cells between tiers at once, with no rebuild.
func Classify(c CellTime, b Budgets) Tier {
	t := Tier{Cell: c.Cell}
	if !c.Reachable {
		t.Reach = ReachNone
		return t
	}
	t.RoadSeconds = c.RoadSeconds
	switch {
	case c.CentreMetres > float64(b.MaxSearchM):
		// The search radius shrank after the build: past it is no reach, whatever the road time.
		t.Reach = ReachBeyond
		return t
	case c.RoadSeconds <= float64(b.SweetSpotS):
		t.Reach = ReachSweetSpot
	case c.RoadSeconds <= float64(b.OuterRingS):
		t.Reach = ReachOuterRing
	default:
		t.Reach = ReachBeyond
	}
	cross := b.CellEdgeM / (float64(b.CrossKmh) / 3.6)
	t.InTimerZone = c.CentreMetres+b.CellEdgeM <= float64(b.ZoneRadiusM) &&
		c.RoadSeconds+cross <= float64(b.TimerS-b.MarginS)
	return t
}

// GateTiers are a gate version's tiers and what they were read from, so an order can keep them.
type GateTiers struct {
	Build   BuildInfo
	Budgets Budgets
	Cells   []Tier // nearest first
}

// Tiers reads the newest build of a gate version and places every cell with the settings given.
func Tiers(ctx context.Context, q querier, g *hexgrid.Grid, gateHistoryID string, s Getter) (GateTiers, error) {
	b, err := ReadBudgets(s, g)
	if err != nil {
		return GateTiers{}, err
	}
	build, cells, err := Latest(ctx, q, g, gateHistoryID)
	if err != nil {
		return GateTiers{}, err
	}
	out := GateTiers{Build: build, Budgets: b, Cells: make([]Tier, len(cells))}
	for i, c := range cells {
		out.Cells[i] = Classify(c, b)
	}
	return out, nil
}
