package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
	"github.com/teamprosperlink/Ninzo_backend/internal/routing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// osrm answers every source at 18 km/h on a road 1.3 times the straight line.
type osrm struct{}

func (osrm) Table(_ context.Context, sources []routing.Point, dest routing.Point, _ float64) ([]routing.Leg, error) {
	out := make([]routing.Leg, len(sources))
	for i, s := range sources {
		m := routing.Metres(s, dest) * 1.3
		out[i] = routing.Leg{Seconds: m / 5, Metres: m, Reachable: true, Source: routing.SourceOSRM}
	}
	return out, nil
}

const someID = "8f14e45f-ceea-4e7a-9d1c-6c1b2b3a4d5e"

func TestBadArgumentsTouchNothing(t *testing.T) {
	for _, args := range [][]string{
		nil, {"-local"}, {"-local", "build"}, {"-local", "build", "-road-data", someID},
		{"-local", "build-all"}, {"-local", "build-all", "-road-data", "not-a-uuid"},
		{"-local", "build-all", "-road-data", someID, "-gate-history", someID},
		{"-local", "build", "-road-data", someID, "-gate-history", "x"},
		{"-local", "build", "-road-data", someID, "-gate-history", someID, "extra"},
		{"-staging", "build-all", "-road-data", someID}, {"-local", "delete"},
	} {
		if err := run(context.Background(), args, env(nil), osrm{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestEachModeRefusesTheOtherTarget(t *testing.T) {
	remote := "postgres://postgres.chyejfgsipcsdxnginei:secret-pass@aws-0-ap-south-1.pooler.supabase.com:5432/postgres"
	err := run(context.Background(), []string{"-local", "build-all", "-road-data", someID}, env(map[string]string{"DATABASE_URL": remote}), osrm{}, &bytes.Buffer{})
	if err == nil {
		t.Error("-local would have touched Supabase")
	} else if strings.Contains(err.Error(), "secret-pass") {
		t.Error("the error shows the password")
	}
	local := "postgres://ninzo:ninzo_local@localhost:5432/ninzo?sslmode=disable"
	if err := run(context.Background(), []string{"-remote", "build-all", "-road-data", someID}, env(map[string]string{"DATABASE_URL": local}), osrm{}, &bytes.Buffer{}); err == nil {
		t.Error("-remote accepted a local database")
	}
	// No router given and no URL: refuse rather than store straight lines.
	err = run(context.Background(), []string{"-local", "build-all", "-road-data", someID}, env(map[string]string{"DATABASE_URL": local}), nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "ROUTING_ENGINE_URL") {
		t.Errorf("no OSRM: %v", err)
	}
}

func TestBuildAllBuildsEachCurrentGateVersionOnce(t *testing.T) {
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
	person := q("INSERT INTO people (phone) VALUES ('+919800000197') RETURNING id::text")
	customer := q("INSERT INTO customers (person_id) VALUES ($1) RETURNING id::text", person)
	gate := q("INSERT INTO gates (customer_id, zone_id, label, lat, lng) VALUES ($1, $2, 'Home', 12.9354, 77.6144) RETURNING id::text", customer, zone)
	old := q("INSERT INTO gate_history (gate_id, version, label, lat, lng, verified, reason) VALUES ($1, 1, 'Home', 12.9350, 77.6140, false, 'created') RETURNING id::text", gate)
	current := q("INSERT INTO gate_history (gate_id, version, label, lat, lng, verified, reason) VALUES ($1, 2, 'Home', 12.9354, 77.6144, false, 'moved') RETURNING id::text", gate)
	road := q(`INSERT INTO road_data_versions (name, source, source_sha256, clip_box, osm_extract_sha256, osm_timestamp,
		profile, profile_version, profile_sha256, osrm_image, algorithm, built_at)
		VALUES ('test-zones', 'test', $1, '77.6,12.9,77.7,13.0', $1, now() - interval '1 day', 'scooter', 'scooter-2', $1, 'osrm', 'mld', now())
		RETURNING id::text`, strings.Repeat("c", 64))
	e := env(map[string]string{"DATABASE_URL": pool.Config().ConnString()})

	var out bytes.Buffer
	if err := run(ctx, []string{"-local", "build-all", "-road-data", road}, e, osrm{}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "1 gate versions to build") || !strings.Contains(out.String(), "built "+current) || strings.Contains(out.String(), old) {
		t.Fatalf("%s", out.String())
	}
	out.Reset()
	if err := run(ctx, []string{"-local", "build-all", "-road-data", road}, e, osrm{}, &out); err != nil || !strings.Contains(out.String(), "0 gate versions to build") {
		t.Fatalf("second run: %s %v", out.String(), err)
	}
	// One gate by hand, even an old version, and an unknown road data version refused.
	out.Reset()
	if err := run(ctx, []string{"-local", "build", "-road-data", road, "-gate-history", old}, e, osrm{}, &out); err != nil {
		t.Fatalf("%s %v", out.String(), err)
	}
	if err := run(ctx, []string{"-local", "build", "-road-data", someID, "-gate-history", old}, e, osrm{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "register the build first") {
		t.Fatalf("unknown road data: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM gate_cell_builds").Scan(&n); err != nil || n != 2 {
		t.Fatalf("%d builds, %v", n, err)
	}
	// The zone's max_search_radius_m (1500 m by default) decides the size.
	var radius, cells int
	if err := pool.QueryRow(ctx, "SELECT max_radius_m, cells FROM gate_cell_builds WHERE gate_history_id = $1", current).Scan(&radius, &cells); err != nil || radius != 1500 || cells < 400 {
		t.Fatalf("radius %d, cells %d, %v", radius, cells, err)
	}
}

func TestBuildAllSkipsErasedGatesAndCarriesOnPastAFailure(t *testing.T) {
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
	gateFor := func(phone string, lat, lng float64) (string, string) {
		person := q("INSERT INTO people (phone) VALUES ($1) RETURNING id::text", phone)
		customer := q("INSERT INTO customers (person_id) VALUES ($1) RETURNING id::text", person)
		gate := q("INSERT INTO gates (customer_id, zone_id, label, lat, lng) VALUES ($1, $2, 'Home', $3, $4) RETURNING id::text", customer, zone, lat, lng)
		return q("INSERT INTO gate_history (gate_id, version, label, lat, lng, verified, reason) VALUES ($1, 1, 'Home', $2, $3, false, 'created') RETURNING id::text", gate, lat, lng), customer
	}
	// The first gate is out of the grid's reach (Chennai), so its build fails; the second builds.
	far, _ := gateFor("+919800000201", 13.0827, 80.2707)
	good, _ := gateFor("+919800000202", 12.9354, 77.6144)
	erased, _ := gateFor("+919800000203", 12.9360, 77.6150)
	if _, err := pool.Exec(ctx, `UPDATE gate_history SET label = '(removed)', entry_notes = NULL, landmark = NULL,
		lat = round(lat::numeric, 2)::double precision, lng = round(lng::numeric, 2)::double precision, redacted_at = now()
		WHERE id = $1`, erased); err != nil {
		t.Fatal(err)
	}
	road := q(`INSERT INTO road_data_versions (name, source, source_sha256, clip_box, osm_extract_sha256, osm_timestamp,
		profile, profile_version, profile_sha256, osrm_image, algorithm, built_at)
		VALUES ('test-zones', 'test', $1, '77.6,12.9,77.7,13.0', $1, now() - interval '1 day', 'scooter', 'scooter-2', $1, 'osrm', 'mld', now())
		RETURNING id::text`, strings.Repeat("e", 64))
	e := env(map[string]string{"DATABASE_URL": pool.Config().ConnString()})
	var out bytes.Buffer
	err := run(ctx, []string{"-local", "build-all", "-road-data", road}, e, osrm{}, &out)
	if err == nil || !strings.Contains(err.Error(), "1 of 2 gate versions failed") {
		t.Fatalf("got %v\n%s", err, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "2 gate versions to build") || !strings.Contains(s, "failed "+far) || !strings.Contains(s, "built "+good) || strings.Contains(s, erased) || !strings.Contains(s, "1 built, 1 failed") {
		t.Fatalf("%s", s)
	}
	// Building the erased version by hand is refused too.
	if err := run(ctx, []string{"-local", "build", "-road-data", road, "-gate-history", erased}, e, osrm{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "erased") {
		t.Fatalf("erased by hand: %v", err)
	}
}
