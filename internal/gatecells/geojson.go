package gatecells

import (
	"encoding/json"

	"github.com/teamprosperlink/Ninzo_backend/internal/hexgrid"
)

// GeoJSON writes a gate's tiers as a GeoJSON FeatureCollection, one polygon per cell, for viewing
// in geojson.io, kepler.gl or QGIS (W2 step 7, issue 203). The cells give the gate's location
// away: the output is for the owner and ops, never to commit or share.
func GeoJSON(g *hexgrid.Grid, t GateTiers) ([]byte, error) {
	type geometry struct {
		Type        string         `json:"type"`
		Coordinates [][][2]float64 `json:"coordinates"`
	}
	type props struct {
		Cell        string   `json:"cell"`
		Reach       Reach    `json:"reach"`
		InTimerZone bool     `json:"in_timer_zone"`
		RoadSeconds *float64 `json:"road_seconds"` // null when unreachable
	}
	type feature struct {
		Type       string   `json:"type"`
		Geometry   geometry `json:"geometry"`
		Properties props    `json:"properties"`
	}
	out := struct {
		Type     string    `json:"type"`
		Build    string    `json:"build_id"`
		RoadData string    `json:"road_data_version_id"`
		Grid     string    `json:"grid"`
		Budgets  Budgets   `json:"budgets"`
		Features []feature `json:"features"`
	}{Type: "FeatureCollection", Build: t.Build.ID, RoadData: t.Build.RoadDataVersionID, Grid: g.Name(), Budgets: t.Budgets,
		Features: make([]feature, 0, len(t.Cells))}
	for _, c := range t.Cells {
		o := g.Outline(c.Cell)
		ring := make([][2]float64, 0, 7)
		for _, p := range o {
			ring = append(ring, [2]float64{p.Lng, p.Lat}) // GeoJSON is lng,lat
		}
		ring = append(ring, ring[0]) // a closed ring
		f := feature{Type: "Feature", Geometry: geometry{Type: "Polygon", Coordinates: [][][2]float64{ring}},
			Properties: props{Cell: g.ID(c.Cell), Reach: c.Reach, InTimerZone: c.InTimerZone}}
		if c.Reach != ReachNone {
			s := c.RoadSeconds
			f.Properties.RoadSeconds = &s
		}
		out.Features = append(out.Features, f)
	}
	return json.Marshal(out)
}
