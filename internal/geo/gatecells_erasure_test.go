package geo

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/gatecells"
	"github.com/teamprosperlink/Ninzo_backend/internal/hexgrid"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
	"github.com/teamprosperlink/Ninzo_backend/internal/routing"
)

type straightRoads struct{}

func (straightRoads) Table(_ context.Context, sources []routing.Point, dest routing.Point, _ float64) ([]routing.Leg, error) {
	out := make([]routing.Leg, len(sources))
	for i, s := range sources {
		m := routing.Metres(s, dest) * 1.3
		out[i] = routing.Leg{Seconds: m / 5, Metres: m, Reachable: true, Source: routing.SourceOSRM}
	}
	return out, nil
}

// Erasing a customer through RedactGates leaves no cells around their gates (#196).
func TestErasureRemovesTheGatesCellSets(t *testing.T) {
	pool := dbtest.Fresh(t)
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	zone := q(`INSERT INTO zones (name, city, status, is_test, area) VALUES ('Zone (test)', 'Bengaluru', 'active', true,
		extensions.ST_GeomFromText('MULTIPOLYGON(((77.612 12.925, 77.640 12.925, 77.640 12.945, 77.612 12.945, 77.612 12.925)))', 4326)) RETURNING id::text`)
	person := q("INSERT INTO people (phone) VALUES ('+919800000198') RETURNING id::text")
	customer := q("INSERT INTO customers (person_id) VALUES ($1) RETURNING id::text", person)
	pin := hexgrid.Point{Lat: 12.93545, Lng: 77.61439}
	gate := q("INSERT INTO gates (customer_id, zone_id, label, lat, lng) VALUES ($1, $2, 'Home', $3, $4) RETURNING id::text", customer, zone, pin.Lat, pin.Lng)
	gh := q("INSERT INTO gate_history (gate_id, version, label, lat, lng, verified, reason) VALUES ($1, 1, 'Home', $2, $3, false, 'created') RETURNING id::text", gate, pin.Lat, pin.Lng)
	road := q(`INSERT INTO road_data_versions (name, source, source_sha256, clip_box, osm_extract_sha256, osm_timestamp,
		profile, profile_version, profile_sha256, osrm_image, algorithm, built_at)
		VALUES ('test-zones', 'test', $1, '77.6,12.9,77.7,13.0', $1, now() - interval '1 day', 'scooter', 'scooter-2', $1, 'osrm', 'mld', now())
		RETURNING id::text`, strings.Repeat("d", 64))
	g := hexgrid.Bengaluru()
	cells, err := gatecells.Build(ctx, straightRoads{}, g, pin, 350)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.InTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := gatecells.Store(ctx, tx, gh, road, g, 350, pin, cells)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.InTx(ctx, pool, func(tx pgx.Tx) error { return RedactGates(ctx, tx, customer) }); err != nil {
		t.Fatal(err)
	}
	var left, live int
	if err := pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM gate_cells), (SELECT count(*) FROM gate_cell_builds WHERE redacted_at IS NULL)").Scan(&left, &live); err != nil {
		t.Fatal(err)
	}
	if left != 0 || live != 0 {
		t.Fatalf("after erasure: %d cells, %d live builds", left, live)
	}
}
