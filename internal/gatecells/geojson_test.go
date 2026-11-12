package gatecells

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/teamprosperlink/Ninzo_backend/internal/hexgrid"
)

func TestGeoJSONIsValidAndMatchesClassify(t *testing.T) {
	g := hexgrid.Bengaluru()
	cells, err := Build(context.Background(), &fakeRouter{unreachable: map[int]bool{4: true}}, g, gate, 600)
	if err != nil {
		t.Fatal(err)
	}
	tiers := GateTiers{Build: BuildInfo{ID: "b-1", RoadDataVersionID: "r-1"}, Budgets: start}
	for _, c := range cells {
		tiers.Cells = append(tiers.Cells, Classify(c, start))
	}
	raw, err := GeoJSON(g, tiers)
	if err != nil {
		t.Fatal(err)
	}
	var fc struct {
		Type     string  `json:"type"`
		Build    string  `json:"build_id"`
		Budgets  Budgets `json:"budgets"`
		Features []struct {
			Type     string `json:"type"`
			Geometry struct {
				Type        string         `json:"type"`
				Coordinates [][][2]float64 `json:"coordinates"`
			} `json:"geometry"`
			Properties struct {
				Cell        string   `json:"cell"`
				Reach       Reach    `json:"reach"`
				InTimerZone bool     `json:"in_timer_zone"`
				RoadSeconds *float64 `json:"road_seconds"`
			} `json:"properties"`
		} `json:"features"`
	}
	if err := json.Unmarshal(raw, &fc); err != nil {
		t.Fatal(err)
	}
	if fc.Type != "FeatureCollection" || fc.Build != "b-1" || fc.Budgets != start || len(fc.Features) != len(cells) {
		t.Fatalf("collection %s %s %+v with %d features for %d cells", fc.Type, fc.Build, fc.Budgets, len(fc.Features), len(cells))
	}
	for i, f := range fc.Features {
		want := tiers.Cells[i]
		ring := f.Geometry.Coordinates[0]
		if f.Type != "Feature" || f.Geometry.Type != "Polygon" || len(f.Geometry.Coordinates) != 1 || len(ring) != 7 || ring[0] != ring[6] {
			t.Fatalf("feature %d: not a closed hexagon: %+v", i, f.Geometry)
		}
		// Counter-clockwise exterior ring, as RFC 7946 recommends: positive signed area in lng,lat.
		area := 0.0
		for j := 0; j < 6; j++ {
			area += ring[j][0]*ring[j+1][1] - ring[j+1][0]*ring[j][1]
		}
		if area <= 0 {
			t.Fatalf("feature %d: ring is clockwise (signed area %g)", i, area)
		}
		// lng,lat order: Bengaluru's longitude is about 77.6, its latitude about 12.9.
		for _, p := range ring {
			if p[0] < 77 || p[0] > 78 || p[1] < 12 || p[1] > 14 {
				t.Fatalf("feature %d: %v is not lng,lat in Bengaluru", i, p)
			}
		}
		// The polygon is the cell: its centre (the mean of the six corners) falls in that cell.
		var lat, lng float64
		for _, p := range ring[:6] {
			lng += p[0] / 6
			lat += p[1] / 6
		}
		if c, _ := g.CellAt(hexgrid.Point{Lat: lat, Lng: lng}); g.ID(c) != f.Properties.Cell || c != want.Cell {
			t.Fatalf("feature %d: polygon is cell %s, labelled %s", i, g.ID(c), f.Properties.Cell)
		}
		if f.Properties.Reach != want.Reach || f.Properties.InTimerZone != want.InTimerZone {
			t.Fatalf("feature %d: %+v, Classify said %+v", i, f.Properties, want)
		}
		if (f.Properties.RoadSeconds == nil) != (want.Reach == ReachNone) {
			t.Fatalf("feature %d: road_seconds %v for reach %s", i, f.Properties.RoadSeconds, want.Reach)
		}
	}
}
