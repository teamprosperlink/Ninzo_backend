package gatecells

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/hexgrid"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
)

// The starting values on the Bengaluru grid: timer 90 s, zone 350 m, margin 15 s, sweet spot 80 s,
// outer ring 300 s, crossing speed 15 km/h, search 1500 m, cell edge 76 m.
var start = Budgets{ZoneRadiusM: 350, TimerS: 90, MarginS: 15, SweetSpotS: 80, OuterRingS: 300, CrossKmh: 15, MaxSearchM: 1500, CellEdgeM: 76}

// cross is the time to cross one cell edge at the crossing speed: 76 m at 15 km/h, 18.24 s.
var cross = start.CellEdgeM / (float64(start.CrossKmh) / 3.6)

func TestClassifyAtEachBoundary(t *testing.T) {
	cell := func(metres, secs float64) CellTime {
		return CellTime{Cell: hexgrid.Cell{Q: 1, R: 2}, CentreMetres: metres, Reachable: true, RoadSeconds: secs, RoadMetres: metres * 1.3}
	}
	zoneTime := float64(start.TimerS-start.MarginS) - cross  // 56.76 s
	zoneDist := float64(start.ZoneRadiusM) - start.CellEdgeM // 274 m
	cases := []struct {
		name  string
		c     CellTime
		reach Reach
		zone  bool
	}{
		{"at the gate", cell(0, 0), ReachSweetSpot, true},
		{"timer zone time edge, exactly", cell(100, zoneTime), ReachSweetSpot, true},
		{"just past the timer zone time", cell(100, zoneTime+0.01), ReachSweetSpot, false},
		{"timer zone distance edge, exactly", cell(zoneDist, 20), ReachSweetSpot, true},
		{"cell pokes out of the circle", cell(zoneDist+0.01, 20), ReachSweetSpot, false},
		{"centre inside the circle, slow by road", cell(200, 120), ReachOuterRing, false},
		{"sweet spot edge: 80 s exactly", cell(600, 80), ReachSweetSpot, false},
		{"just past the sweet spot", cell(600, 80.01), ReachOuterRing, false},
		{"outer ring edge: 300 s exactly", cell(1200, 300), ReachOuterRing, false},
		{"just past the outer ring", cell(1200, 300.01), ReachBeyond, false},
		{"search radius edge, exactly", cell(1500, 50), ReachSweetSpot, false},
		{"past a search radius that shrank", cell(1500.01, 50), ReachBeyond, false},
		{"no road", CellTime{CentreMetres: 50}, ReachNone, false},
	}
	for _, c := range cases {
		got := Classify(c.c, start)
		if got.Reach != c.reach || got.InTimerZone != c.zone {
			t.Errorf("%s: got %s, timer zone %v; want %s, %v", c.name, got.Reach, got.InTimerZone, c.reach, c.zone)
		}
	}
	if Classify(CellTime{CentreMetres: 0}, start).InTimerZone {
		t.Error("an unreachable cell is in the timer zone")
	}
}

// Every corner of every timer-zone cell is inside the circle, checked on the real grid against
// the true distance from the gate, not against the formula.
func TestATimerZoneCellLiesWhollyInsideTheCircle(t *testing.T) {
	g := hexgrid.Bengaluru()
	cells, err := Candidates(g, gate, 1500)
	if err != nil {
		t.Fatal(err)
	}
	inZone := 0
	for _, c := range cells {
		// Fast roads, so distance alone decides.
		c.Reachable, c.RoadSeconds, c.RoadMetres = true, 1, c.CentreMetres
		if !Classify(c, start).InTimerZone {
			continue
		}
		inZone++
		for _, corner := range g.Outline(c.Cell) {
			if d := metres(gate, corner); d > float64(start.ZoneRadiusM)+0.5 {
				t.Fatalf("%v: a corner is %.1f m from the gate, outside the %d m circle", c.Cell, d, start.ZoneRadiusM)
			}
		}
	}
	if inZone == 0 {
		t.Fatal("no cell in the timer zone; the test proves nothing")
	}
}

type settingsMap map[string]int

func (m settingsMap) Get(name string) (int, bool) { v, ok := m[name]; return v, ok }

var startSettings = settingsMap{"zone_radius_m": 350, "timer_length_s": 90, "timer_zone_margin_s": 15, "sweet_spot_budget_s": 80,
	"outer_ring_budget_s": 300, "straight_line_speed_kmh": 15, "max_search_radius_m": 1500}

func with(s settingsMap, changes map[string]int) settingsMap {
	out := settingsMap{}
	for k, v := range s {
		out[k] = v
	}
	for k, v := range changes {
		out[k] = v
	}
	return out
}

func TestReadBudgetsNeedsEverySetting(t *testing.T) {
	b, err := ReadBudgets(startSettings, hexgrid.Bengaluru())
	if err != nil || b != start {
		t.Fatalf("%+v %v", b, err)
	}
	for name := range startSettings {
		partial := settingsMap{}
		for k, v := range startSettings {
			if k != name {
				partial[k] = v
			}
		}
		if _, err := ReadBudgets(partial, hexgrid.Bengaluru()); err == nil {
			t.Errorf("without %s: accepted", name)
		}
	}
}

// Changing any budget moves cells between tiers at once, from one stored build.
func TestABudgetChangeNeedsNoRebuild(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	g := hexgrid.Bengaluru()
	cells, err := Build(ctx, &fakeRouter{unreachable: map[int]bool{5: true}}, g, gate, 1500)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := database.InTx(ctx, f.pool, func(tx pgx.Tx) error {
		var err error
		id, err = Store(ctx, tx, f.gh, f.roadData, g, 1500, gate, cells)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	type counts struct{ sweet, ring, beyond, zone, none int }
	count := func(s settingsMap) counts {
		t.Helper()
		gt, err := Tiers(ctx, f.pool, g, f.gh, s)
		if err != nil {
			t.Fatal(err)
		}
		if gt.Build.ID != id || len(gt.Cells) != len(cells) {
			t.Fatalf("read build %s with %d cells", gt.Build.ID, len(gt.Cells))
		}
		var c counts
		for _, x := range gt.Cells {
			switch x.Reach {
			case ReachSweetSpot:
				c.sweet++
			case ReachOuterRing:
				c.ring++
			case ReachBeyond:
				c.beyond++
			case ReachNone:
				c.none++
			}
			if x.InTimerZone {
				c.zone++
			}
		}
		return c
	}
	base := count(startSettings)
	if base.none != 1 || base.sweet == 0 || base.zone == 0 || base.beyond == 0 {
		t.Fatalf("starting tiers %+v", base)
	}
	for _, c := range []struct {
		name   string
		change map[string]int
		check  func(c counts) bool
	}{
		{"wider sweet spot", map[string]int{"sweet_spot_budget_s": 160}, func(c counts) bool { return c.sweet > base.sweet }},
		{"longer timer", map[string]int{"timer_length_s": 120}, func(c counts) bool { return c.zone > base.zone }},
		{"bigger margin", map[string]int{"timer_zone_margin_s": 40}, func(c counts) bool { return c.zone < base.zone }},
		{"tighter outer ring", map[string]int{"outer_ring_budget_s": 120}, func(c counts) bool { return c.ring < base.ring && c.beyond > base.beyond }},
		{"smaller search radius", map[string]int{"max_search_radius_m": 600}, func(c counts) bool { return c.beyond > base.beyond }},
	} {
		if got := count(with(startSettings, c.change)); !c.check(got) {
			t.Errorf("%s: %+v, from %+v", c.name, got, base)
		}
	}
	var builds int
	if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM gate_cell_builds").Scan(&builds); err != nil || builds != 1 {
		t.Fatalf("%d builds: a settings change must not rebuild", builds)
	}
}
