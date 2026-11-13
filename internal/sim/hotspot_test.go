package sim

import (
	"context"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/teamprosperlink/Ninzo_backend/internal/hexgrid"
	"github.com/teamprosperlink/Ninzo_backend/internal/routing"
)

var hotBox = Box{12.925, 77.612, 12.945, 77.640}

func TestNoHotspotsDrawsAsBefore(t *testing.T) {
	sc := Scenario{Area: hotBox, Orders: 50, Seed: 7}
	got, err := drawGates(context.Background(), &straightRoads{}, sc)
	if err != nil {
		t.Fatal(err)
	}
	// The draw #205 made: two uniform numbers per gate from PCG(seed, 205).
	rng := rand.New(rand.NewPCG(7, 205))
	for i, p := range got {
		want := routing.Point{Lat: hotBox.MinLat + rng.Float64()*(hotBox.MaxLat-hotBox.MinLat), Lng: hotBox.MinLng + rng.Float64()*(hotBox.MaxLng-hotBox.MinLng)}
		if p != want {
			t.Fatalf("gate %d: %v, want %v", i, p, want)
		}
	}
}

func near(p routing.Point, h Hotspot, m float64) bool {
	dy := (p.Lat - h.Lat) * 111320
	dx := (p.Lng - h.Lng) * 111320 * math.Cos(h.Lat*math.Pi/180)
	return math.Hypot(dx, dy) <= m
}

func TestHotspotsTakeTheirShare(t *testing.T) {
	h := Hotspot{Lat: 12.935, Lng: 77.620, Share: 0.6, RadiusM: 150}
	sc := Scenario{Area: hotBox, Orders: 2000, Seed: 3, Hotspots: []Hotspot{h}}
	got, err := drawGates(context.Background(), &straightRoads{}, sc)
	if err != nil {
		t.Fatal(err)
	}
	in := 0
	for _, p := range got {
		if !hotBox.contains(p) {
			t.Fatalf("%v is outside the area", p)
		}
		if near(p, h, 3*150) {
			in++
		}
	}
	// 60% from the hotspot (almost all within 3 radii), plus the uniform 40% that falls there by
	// chance: a 450 m disc is about 0.64 km² of the 4.8 km² box, so about 5% of 40%.
	share := float64(in) / float64(len(got))
	if share < 0.58 || share > 0.68 {
		t.Fatalf("%.3f of gates near the hotspot, want about 0.62", share)
	}
}

func TestBadHotspotsAreRefused(t *testing.T) {
	for name, hs := range map[string][]Hotspot{
		"no share":  {{Lat: 12.93, Lng: 77.62, Share: 0, RadiusM: 100}},
		"no radius": {{Lat: 12.93, Lng: 77.62, Share: 0.5, RadiusM: 0}},
		"over one":  {{Lat: 12.93, Lng: 77.62, Share: 0.6, RadiusM: 100}, {Lat: 12.94, Lng: 77.63, Share: 0.5, RadiusM: 100}},
	} {
		if _, err := drawGates(context.Background(), &straightRoads{}, Scenario{Area: hotBox, Orders: 5, Seed: 1, Hotspots: hs}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCellDemandTracksTheHotspot(t *testing.T) {
	h := Hotspot{Lat: 12.935, Lng: 77.620, Share: 0.7, RadiusM: 120}
	sc := Scenario{Area: hotBox, Setting: start, Hotspots: []Hotspot{h}}
	cells, w, err := CellDemand(context.Background(), &straightRoads{}, sc, 4, 300, 50, 0.8)
	if err != nil {
		t.Fatal(err)
	}
	g := hexgrid.Bengaluru()
	hot, total := 0.0, 0.0
	for i, id := range cells {
		c, err := g.Parse(id)
		if err != nil {
			t.Fatal(err)
		}
		ctr := g.Centre(c)
		if near(routing.Point{Lat: ctr.Lat, Lng: ctr.Lng}, h, 3*120) {
			hot += w[i]
		}
		total += w[i]
		if i > 0 && cells[i-1] >= id {
			t.Fatal("cells are not sorted")
		}
	}
	// Forecast weights are per-week counts: they add up to about 300, and about 70% near the hotspot.
	if math.Abs(total-300) > 1e-6 || hot/total < 0.65 || hot/total > 0.80 {
		t.Fatalf("total %.1f, near the hotspot %.2f", total, hot/total)
	}
}

func TestPlaceOnCellsFollowsTheWeights(t *testing.T) {
	sc := Scenario{Area: hotBox, Setting: start}
	g := hexgrid.Bengaluru()
	cellAt := func(lat, lng float64) string {
		c, err := g.CellAt(hexgrid.Point{Lat: lat, Lng: lng})
		if err != nil {
			t.Fatal(err)
		}
		return g.ID(c)
	}
	west, east := cellAt(12.935, 77.615), cellAt(12.935, 77.637) // about 2.4 km apart
	o := PlaceOptions{N: 1, Q: 0, Capacity: 1, GridRows: 4, GridCols: 6}
	for _, c := range []struct {
		w    []float64
		east bool
	}{{[]float64{1, 9}, true}, {[]float64{9, 1}, false}} {
		pts, _, err := PlaceOnCells(context.Background(), &straightRoads{}, sc, []string{west, east}, c.w, o)
		if err != nil {
			t.Fatal(err)
		}
		if (pts[0].Lng > 77.626) != c.east {
			t.Fatalf("weights %v: the scooter went to %v", c.w, pts[0])
		}
	}
	if _, _, err := PlaceOnCells(context.Background(), &straightRoads{}, sc, []string{west}, []float64{1, 2}, o); err == nil {
		t.Fatal("ragged weights accepted")
	}
}

// TestGoldenForecastPlacementOnRealRoads runs the chain synthetic history → forecast → placement on
// the test-zone OSRM (CI job routes).
func TestGoldenForecastPlacementOnRealRoads(t *testing.T) {
	c := goldenRouter(t)
	sc := Scenario{Area: hotBox, Setting: start, Hotspots: []Hotspot{{Lat: 12.9355, Lng: 77.6245, Share: 0.5, RadiusM: 200}}}
	cells, w, err := CellDemand(context.Background(), c, sc, 2, 40, 2001, 0.8)
	if err != nil || len(cells) == 0 {
		t.Fatalf("%d cells: %v", len(cells), err)
	}
	pts, plan, err := PlaceOnCells(context.Background(), c, sc, cells, w, PlaceOptions{N: 2, Q: 0.2, Capacity: 1, GridRows: 4, GridCols: 5})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d cells; placed %v; covered %.2f", len(cells), pts, plan.Covered)
	if len(pts) != 2 || plan.Covered < 0.05 {
		t.Fatalf("%d points, %+v", len(pts), plan)
	}
}

func TestAHotspotAtTheEdgeStaysInTheArea(t *testing.T) {
	// A hotspot on the box's corner, wide: about three quarters of its draws fall outside.
	sc := Scenario{Area: hotBox, Orders: 400, Seed: 5, Hotspots: []Hotspot{{Lat: hotBox.MinLat, Lng: hotBox.MinLng, Share: 1, RadiusM: 400}}}
	got, err := drawGates(context.Background(), &straightRoads{}, sc)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		if !hotBox.contains(p) {
			t.Fatalf("%v is outside the area", p)
		}
	}
}
