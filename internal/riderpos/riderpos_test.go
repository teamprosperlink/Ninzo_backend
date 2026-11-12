package riderpos

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/gatecells"
	"github.com/teamprosperlink/Ninzo_backend/internal/hexgrid"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
	"github.com/teamprosperlink/Ninzo_backend/internal/routing"
)

var gate = hexgrid.Point{Lat: 12.93545, Lng: 77.61439}

func TestCellIsTheGridCellOrNone(t *testing.T) {
	g := hexgrid.Bengaluru()
	c := Cell(g, gate.Lat, gate.Lng)
	want, _ := g.CellAt(gate)
	if c == nil || *c != g.ID(want) {
		t.Fatalf("%v", c)
	}
	if Cell(g, 28.6, 77.2) != nil { // Delhi: beyond the Bengaluru grid's reach
		t.Fatal("a point 1700 km away has a cell")
	}
}

type fixture struct {
	pool       *pgxpool.Pool
	rider, gh  string
	zone, road string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	f := &fixture{pool: dbtest.Fresh(t)}
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	f.zone = q(`INSERT INTO zones (name, city, status, is_test, area) VALUES ('Zone (test)', 'Bengaluru', 'active', true,
		extensions.ST_GeomFromText('MULTIPOLYGON(((77.612 12.925, 77.640 12.925, 77.640 12.945, 77.612 12.945, 77.612 12.925)))', 4326)) RETURNING id::text`)
	rp := q("INSERT INTO people (phone) VALUES ('+919800000207') RETURNING id::text")
	f.rider = q("INSERT INTO riders (person_id, name, status, zone_id) VALUES ($1, 'Test Rider', 'active', $2) RETURNING id::text", rp, f.zone)
	cp := q("INSERT INTO people (phone) VALUES ('+919800000208') RETURNING id::text")
	customer := q("INSERT INTO customers (person_id) VALUES ($1) RETURNING id::text", cp)
	g := q("INSERT INTO gates (customer_id, zone_id, label, lat, lng) VALUES ($1, $2, 'Home', $3, $4) RETURNING id::text", customer, f.zone, gate.Lat, gate.Lng)
	f.gh = q("INSERT INTO gate_history (gate_id, version, label, lat, lng, verified, reason) VALUES ($1, 1, 'Home', $2, $3, false, 'created') RETURNING id::text", g, gate.Lat, gate.Lng)
	f.road = q(`INSERT INTO road_data_versions (name, source, source_sha256, clip_box, osm_extract_sha256, osm_timestamp,
		profile, profile_version, profile_sha256, osrm_image, algorithm, built_at)
		VALUES ('test-zones', 'test', $1, '77.6,12.9,77.7,13.0', $1, now() - interval '1 day', 'scooter', 'scooter-2', $1, 'osrm', 'mld', now())
		RETURNING id::text`, strings.Repeat("a", 64))
	return f
}

func (f *fixture) record(t *testing.T, p Ping) {
	t.Helper()
	if err := database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		return Record(context.Background(), tx, hexgrid.Bengaluru(), f.rider, p)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestThePositionFollowsTheNewestPingOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := Latest(ctx, f.pool, f.rider); !errors.Is(err, ErrNoPosition) {
		t.Fatalf("before any ping: %v", err)
	}
	t0 := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	f.record(t, Ping{Lat: 12.930, Lng: 77.620, SpeedMps: 3, AccuracyM: 8, DeviceTime: t0, ReceivedAt: t0.Add(time.Second)})
	f.record(t, Ping{Lat: 12.931, Lng: 77.621, SpeedMps: 4, AccuracyM: 6, DeviceTime: t0.Add(10 * time.Second), ReceivedAt: t0.Add(11 * time.Second)})
	// A late ping, older by device time, arriving after: it must not move the rider back.
	f.record(t, Ping{Lat: 12.990, Lng: 77.690, SpeedMps: 1, AccuracyM: 5, DeviceTime: t0.Add(5 * time.Second), ReceivedAt: t0.Add(40 * time.Second)})
	// The same device time again changes nothing either.
	f.record(t, Ping{Lat: 12.990, Lng: 77.690, SpeedMps: 1, AccuracyM: 5, DeviceTime: t0.Add(10 * time.Second), ReceivedAt: t0.Add(41 * time.Second)})
	p, err := Latest(ctx, f.pool, f.rider)
	if err != nil {
		t.Fatal(err)
	}
	if p.Lat != 12.931 || p.Lng != 77.621 || p.SpeedMps != 4 || !p.DeviceTime.Equal(t0.Add(10*time.Second)) || p.Cell == nil {
		t.Fatalf("position %+v", p)
	}
	g := hexgrid.Bengaluru()
	if want := Cell(g, 12.931, 77.621); *p.Cell != *want {
		t.Fatalf("cell %s, want %s", *p.Cell, *want)
	}
	// A rider beyond the grid keeps a position with no cell.
	f.record(t, Ping{Lat: 28.6, Lng: 77.2, SpeedMps: 0, AccuracyM: 5, DeviceTime: t0.Add(time.Hour), ReceivedAt: t0.Add(time.Hour)})
	if p, _ = Latest(ctx, f.pool, f.rider); p.Cell != nil || p.Lat != 28.6 {
		t.Fatalf("off the grid: %+v", p)
	}
}

type straightRoads struct{}

func (straightRoads) Table(_ context.Context, sources []routing.Point, dest routing.Point, _ float64) ([]routing.Leg, error) {
	out := make([]routing.Leg, len(sources))
	for i, s := range sources {
		m := routing.Metres(s, dest) * 1.3
		out[i] = routing.Leg{Seconds: m / 5, Metres: m, Reachable: true, Source: routing.SourceOSRM}
	}
	return out, nil
}

type settingsMap map[string]int

func (m settingsMap) Get(n string) (int, bool) { v, ok := m[n]; return v, ok }

var startSettings = settingsMap{"zone_radius_m": 350, "timer_length_s": 90, "timer_zone_margin_s": 15, "sweet_spot_budget_s": 80,
	"outer_ring_budget_s": 300, "straight_line_speed_kmh": 15, "max_search_radius_m": 1500}

func TestTierForPlacesTheRiderAroundTheGate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	g := hexgrid.Bengaluru()
	now := time.Date(2026, 11, 13, 10, 0, 30, 0, time.UTC)
	if _, err := TierFor(ctx, f.pool, g, f.rider, f.gh, startSettings, now); !errors.Is(err, ErrNoPosition) {
		t.Fatalf("no position: %v", err)
	}
	f.record(t, Ping{Lat: gate.Lat, Lng: gate.Lng, AccuracyM: 5, DeviceTime: now.Add(-20 * time.Second), ReceivedAt: now.Add(-19 * time.Second)})
	if _, err := TierFor(ctx, f.pool, g, f.rider, f.gh, startSettings, now); !errors.Is(err, gatecells.ErrNoBuild) {
		t.Fatalf("no build: %v", err)
	}
	cells, err := gatecells.Build(ctx, straightRoads{}, g, gate, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.InTx(ctx, f.pool, func(tx pgx.Tx) error {
		_, err := gatecells.Store(ctx, tx, f.gh, f.road, g, 1500, gate, cells)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// At the gate: in its own cell, the sweet spot and the timer zone.
	rt, err := TierFor(ctx, f.pool, g, f.rider, f.gh, startSettings, now)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := g.CellAt(gate)
	if !rt.InCellSet || rt.Tier.Cell != home || rt.Tier.Reach != gatecells.ReachSweetSpot || !rt.Tier.InTimerZone || rt.Age != 20*time.Second {
		t.Fatalf("at the gate: %+v", rt)
	}
	// 1 km east: in the cell set, past the sweet spot (1300 m by road at 5 m/s is 260 s).
	f.record(t, Ping{Lat: gate.Lat, Lng: 77.6236, AccuracyM: 5, DeviceTime: now.Add(-5 * time.Second), ReceivedAt: now.Add(-4 * time.Second)})
	if rt, _ = TierFor(ctx, f.pool, g, f.rider, f.gh, startSettings, now); !rt.InCellSet || rt.Tier.Reach != gatecells.ReachOuterRing || rt.Tier.InTimerZone {
		t.Fatalf("1 km away: %+v", rt)
	}
	// 3 km away: outside the 1500 m build, so beyond and not in the set.
	f.record(t, Ping{Lat: 12.962, Lng: gate.Lng, AccuracyM: 5, DeviceTime: now, ReceivedAt: now})
	if rt, _ = TierFor(ctx, f.pool, g, f.rider, f.gh, startSettings, now); rt.InCellSet || rt.Tier.Reach != gatecells.ReachBeyond {
		t.Fatalf("3 km away: %+v", rt)
	}
}

// A ping from a phone whose clock runs fast is kept as evidence but does not move the rider, or
// it would freeze the position: no correct ping could be newer by device time.
func TestAPingFromTheFutureDoesNotFreezeThePosition(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	f.record(t, Ping{Lat: 12.930, Lng: 77.620, AccuracyM: 5, DeviceTime: t0, ReceivedAt: t0})
	f.record(t, Ping{Lat: 12.990, Lng: 77.690, AccuracyM: 5, DeviceTime: t0.Add(24 * time.Hour), ReceivedAt: t0.Add(time.Second)})
	f.record(t, Ping{Lat: 12.931, Lng: 77.621, AccuracyM: 5, DeviceTime: t0.Add(10 * time.Second), ReceivedAt: t0.Add(10 * time.Second)})
	p, err := Latest(ctx, f.pool, f.rider)
	if err != nil || p.Lat != 12.931 {
		t.Fatalf("after a ping a day ahead: %+v %v", p, err)
	}
	// Within the limit, a little ahead, still counts.
	f.record(t, Ping{Lat: 12.932, Lng: 77.622, AccuracyM: 5, DeviceTime: t0.Add(40 * time.Second), ReceivedAt: t0.Add(20 * time.Second)})
	if p, _ = Latest(ctx, f.pool, f.rider); p.Lat != 12.932 {
		t.Fatalf("a ping 20 s ahead: %+v", p)
	}
}

// Age counts from the device time: a ping queued offline is as old as when it was taken.
func TestAgeCountsFromTheDeviceTime(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	g := hexgrid.Bengaluru()
	now := time.Date(2026, 11, 13, 10, 10, 0, 0, time.UTC)
	cells, _ := gatecells.Build(ctx, straightRoads{}, g, gate, 600)
	if err := database.InTx(ctx, f.pool, func(tx pgx.Tx) error {
		_, err := gatecells.Store(ctx, tx, f.gh, f.road, g, 600, gate, cells)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Taken five minutes ago, received just now from the offline queue.
	f.record(t, Ping{Lat: gate.Lat, Lng: gate.Lng, AccuracyM: 5, DeviceTime: now.Add(-5 * time.Minute), ReceivedAt: now})
	rt, err := TierFor(ctx, f.pool, g, f.rider, f.gh, startSettings, now)
	if err != nil || rt.Age != 5*time.Minute {
		t.Fatalf("age %v, want 5m (%v)", rt.Age, err)
	}
}
