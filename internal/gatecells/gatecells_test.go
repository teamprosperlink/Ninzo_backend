package gatecells

import (
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/hexgrid"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
	"github.com/teamprosperlink/Ninzo_backend/internal/routing"
)

// A gate on 80 Feet Road, Koramangala test zone.
var gate = hexgrid.Point{Lat: 12.93545, Lng: 77.61439}

// fakeRouter answers every source with seconds = metres / 5 (18 km/h), except the ones listed.
type fakeRouter struct {
	unreachable map[int]bool
	source      routing.Source
	calls       int
	short       bool
}

func (f *fakeRouter) Table(_ context.Context, sources []routing.Point, dest routing.Point, _ float64) ([]routing.Leg, error) {
	f.calls++
	src := f.source
	if src == "" {
		src = routing.SourceOSRM
	}
	out := make([]routing.Leg, len(sources))
	for i, s := range sources {
		if f.unreachable[i] {
			out[i] = routing.Leg{Source: src}
			continue
		}
		m := routing.Metres(s, dest) * 1.3
		out[i] = routing.Leg{Seconds: m / 5, Metres: m, Reachable: true, Source: src}
	}
	if f.short {
		return out[1:], nil
	}
	return out, nil
}

func TestCandidatesAreExactlyTheCellsInsideTheRadius(t *testing.T) {
	g := hexgrid.Bengaluru()
	for _, radius := range []int{100, 350, 1500, 3000} {
		cells, err := Candidates(g, gate, radius)
		if err != nil {
			t.Fatal(err)
		}
		in := map[hexgrid.Cell]bool{}
		for i, c := range cells {
			if c.CentreMetres > float64(radius) {
				t.Fatalf("r=%d: %v is %.0f m out", radius, c.Cell, c.CentreMetres)
			}
			if i > 0 && hexgrid.Distance(c.Cell, cells[0].Cell) < hexgrid.Distance(cells[i-1].Cell, cells[0].Cell) {
				t.Fatalf("r=%d: not in ring order", radius)
			}
			in[c.Cell] = true
		}
		// Brute force: every cell in a much larger disk whose centre is inside must be there.
		home, _ := g.CellAt(gate)
		big, _ := hexgrid.Disk(home, int(float64(radius)/(1.5*g.Edge()))+5)
		for _, c := range big {
			if metres(gate, g.Centre(c)) <= float64(radius) && !in[c] {
				t.Fatalf("r=%d: %v missing", radius, c)
			}
		}
		// About the circle's area in cells.
		want := math.Pi * float64(radius*radius) / (3 * math.Sqrt(3) / 2 * g.Edge() * g.Edge())
		if radius >= 350 && math.Abs(float64(len(cells))-want)/want > 0.15 {
			t.Fatalf("r=%d: %d cells, want about %.0f", radius, len(cells), want)
		}
		t.Logf("radius %d m: %d cells", radius, len(cells))
	}
	for _, r := range []int{99, 5001} {
		if _, err := Candidates(g, gate, r); err == nil {
			t.Errorf("radius %d accepted", r)
		}
	}
}

func TestBuildKeepsUnreachableCellsAndRefusesStraightLines(t *testing.T) {
	g := hexgrid.Bengaluru()
	ctx := context.Background()
	f := &fakeRouter{unreachable: map[int]bool{3: true, 7: true}}
	cells, err := Build(ctx, f, g, gate, 350)
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 {
		t.Fatalf("%d table calls, want 1", f.calls)
	}
	for i, c := range cells {
		if c.Reachable == f.unreachable[i] {
			t.Fatalf("cell %d reachable=%v", i, c.Reachable)
		}
		if c.Reachable && (c.RoadSeconds <= 0 || c.RoadMetres < c.CentreMetres) {
			t.Fatalf("cell %d: %+v", i, c)
		}
	}
	if _, err := Build(ctx, &fakeRouter{source: routing.SourcePlaceholder}, g, gate, 350); !errors.Is(err, ErrNoRoads) {
		t.Fatalf("a straight-line answer: %v", err)
	}
	all := map[int]bool{}
	for i := range 200 {
		all[i] = true
	}
	if _, err := Build(ctx, &fakeRouter{unreachable: all}, g, gate, 350); err == nil || !strings.Contains(err.Error(), "no cell reaches") {
		t.Fatalf("nothing reachable: %v", err)
	}
	if _, err := Build(ctx, &fakeRouter{short: true}, g, gate, 350); err == nil {
		t.Fatal("a short answer was accepted")
	}
}

// --- database ---

type fixture struct {
	pool                *pgxpool.Pool
	customer, gate, gh  string
	roadData, roadData2 string
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
	zone := q(`INSERT INTO zones (name, city, status, is_test, area) VALUES ('Zone (test)', 'Bengaluru', 'active', true,
		extensions.ST_GeomFromText('MULTIPOLYGON(((77.612 12.925, 77.640 12.925, 77.640 12.945, 77.612 12.945, 77.612 12.925)))', 4326)) RETURNING id::text`)
	person := q("INSERT INTO people (phone) VALUES ('+919800000196') RETURNING id::text")
	f.customer = q("INSERT INTO customers (person_id) VALUES ($1) RETURNING id::text", person)
	f.gate = q("INSERT INTO gates (customer_id, zone_id, label, lat, lng) VALUES ($1, $2, 'Home', $3, $4) RETURNING id::text", f.customer, zone, gate.Lat, gate.Lng)
	f.gh = q("INSERT INTO gate_history (gate_id, version, label, lat, lng, verified, reason) VALUES ($1, 1, 'Home', $2, $3, false, 'created') RETURNING id::text", f.gate, gate.Lat, gate.Lng)
	road := func(profile string) string {
		return q(`INSERT INTO road_data_versions (name, source, source_sha256, clip_box, osm_extract_sha256, osm_timestamp,
			profile, profile_version, profile_sha256, osrm_image, algorithm, built_at)
			VALUES ('test-zones', 'test', $1, '77.6,12.9,77.7,13.0', $1, now() - interval '1 day', 'scooter', $2, $1, 'osrm', 'mld', now())
			RETURNING id::text`, strings.Repeat(profile[len(profile)-1:], 64), profile)
	}
	f.roadData, f.roadData2 = road("scooter-1"), road("scooter-2")
	return f
}

func (f *fixture) store(t *testing.T, roadData string, cells []CellTime) string {
	t.Helper()
	var id string
	if err := database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		var err error
		id, err = Store(context.Background(), tx, f.gh, roadData, hexgrid.Bengaluru(), 350, gate, cells)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestStoreAndLatest(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	g := hexgrid.Bengaluru()
	if _, _, err := Latest(ctx, f.pool, g, f.gh); !errors.Is(err, ErrNoBuild) {
		t.Fatalf("before any build: %v", err)
	}
	cells, err := Build(ctx, &fakeRouter{unreachable: map[int]bool{2: true}}, g, gate, 350)
	if err != nil {
		t.Fatal(err)
	}
	first := f.store(t, f.roadData, cells)
	time.Sleep(10 * time.Millisecond)
	second := f.store(t, f.roadData2, cells)
	b, got, err := Latest(ctx, f.pool, g, f.gh)
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != second || b.ID == first || b.RoadDataVersionID != f.roadData2 || b.Cells != len(cells) || len(got) != len(cells) {
		t.Fatalf("latest %+v with %d cells, want build %s with %d", b, len(got), second, len(cells))
	}
	byCell := map[hexgrid.Cell]CellTime{}
	for _, c := range cells {
		byCell[c.Cell] = c
	}
	for _, c := range got {
		w := byCell[c.Cell]
		if c.Reachable != w.Reachable || math.Abs(c.RoadSeconds-w.RoadSeconds) > 0.01 || math.Abs(c.CentreMetres-w.CentreMetres) > 0.01 {
			t.Fatalf("%v: stored %+v, built %+v", c.Cell, c, w)
		}
	}
	other, _ := hexgrid.New("mys76", 12.3, 76.6, 76)
	if _, _, err := Latest(ctx, f.pool, other, f.gh); err == nil {
		t.Fatal("a build on another grid was read")
	}
}

func refusedAppendOnly(t *testing.T, err error, what string) {
	t.Helper()
	var pgErr *pgconn.PgError
	// 23001 is the append-only trigger. A service login may not TRUNCATE at all (42501), which
	// refuses even earlier; the no-truncate trigger is there for the migration login, as on zone_settings.
	ok := errors.As(err, &pgErr) && (pgErr.Code == "23001" || (strings.HasPrefix(what, "truncate") && pgErr.Code == "42501"))
	if !ok {
		t.Errorf("%s: want restrict_violation (23001), got %v", what, err)
	}
}

func TestBuildsCannotChange(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cells, _ := Build(ctx, &fakeRouter{}, hexgrid.Bengaluru(), gate, 350)
	id := f.store(t, f.roadData, cells)
	for what, sql := range map[string]string{
		"change a build":      "UPDATE gate_cell_builds SET max_radius_m = 999 WHERE id = '" + id + "'",
		"redact and change":   "UPDATE gate_cell_builds SET redacted_at = now(), cells = 1 WHERE id = '" + id + "'",
		"delete a build":      "DELETE FROM gate_cell_builds WHERE id = '" + id + "'",
		"change a cell":       "UPDATE gate_cells SET road_seconds = 1 WHERE build_id = '" + id + "'",
		"delete a live cell":  "DELETE FROM gate_cells WHERE build_id = '" + id + "'",
		"truncate the builds": "TRUNCATE gate_cell_builds CASCADE",
		"truncate the cells":  "TRUNCATE gate_cells",
	} {
		_, err := f.pool.Exec(ctx, sql)
		refusedAppendOnly(t, err, what)
	}
	if _, _, err := Latest(ctx, f.pool, hexgrid.Bengaluru(), f.gh); err != nil {
		t.Fatal(err)
	}
}

func TestErasureRemovesTheCells(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cells, _ := Build(ctx, &fakeRouter{}, hexgrid.Bengaluru(), gate, 350)
	f.store(t, f.roadData, cells)
	f.store(t, f.roadData2, cells)
	if err := database.InTx(ctx, f.pool, func(tx pgx.Tx) error { return Redact(ctx, tx, f.customer) }); err != nil {
		t.Fatal(err)
	}
	var builds, redacted, left int
	if err := f.pool.QueryRow(ctx, "SELECT count(*), count(redacted_at) FROM gate_cell_builds").Scan(&builds, &redacted); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM gate_cells").Scan(&left); err != nil {
		t.Fatal(err)
	}
	if builds != 2 || redacted != 2 || left != 0 {
		t.Fatalf("builds %d, redacted %d, cells left %d", builds, redacted, left)
	}
	if _, _, err := Latest(ctx, f.pool, hexgrid.Bengaluru(), f.gh); !errors.Is(err, ErrNoBuild) {
		t.Fatalf("after erasure: %v", err)
	}
	// Redacting again changes nothing and fails nothing.
	if err := database.InTx(ctx, f.pool, func(tx pgx.Tx) error { return Redact(ctx, tx, f.customer) }); err != nil {
		t.Fatal(err)
	}
}

// TestGoldenCellsOnRealRoads builds a real gate's cells against OSRM on the test-zone data.
// It runs when ROUTING_TEST_URL is set (the CI job routes).
func TestGoldenCellsOnRealRoads(t *testing.T) {
	url := os.Getenv("ROUTING_TEST_URL")
	if url == "" {
		if os.Getenv("REQUIRE_ROUTING_TESTS") == "yes" {
			t.Fatal("REQUIRE_ROUTING_TESTS=yes but ROUTING_TEST_URL is not set")
		}
		t.Skip("ROUTING_TEST_URL not set")
	}
	c, err := routing.New(routing.Config{BaseURL: url, Profile: "scooter", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	g := hexgrid.Bengaluru()
	cells, err := Build(context.Background(), c, g, gate, 1500)
	if err != nil {
		t.Fatal(err)
	}
	reachable, within80 := 0, 0
	for _, x := range cells {
		if !x.Reachable {
			continue
		}
		reachable++
		if x.RoadSeconds <= 80 {
			within80++
		}
		// Never faster than the scooter's 40 km/h cap, nor shorter than the straight line (less
		// the snap onto the road at each end, at most a cell's reach).
		if x.RoadMetres > 200 && x.RoadSeconds < x.RoadMetres/(40/3.6)*0.97 {
			t.Errorf("%v: %.0f m in %.0f s, faster than the cap", x.Cell, x.RoadMetres, x.RoadSeconds)
		}
		if x.RoadMetres < x.CentreMetres-2*g.Edge() {
			t.Errorf("%v: %.0f m by road, %.0f m straight", x.Cell, x.RoadMetres, x.CentreMetres)
		}
	}
	if reachable < len(cells)*8/10 {
		t.Fatalf("%d of %d cells reach the gate; want most of a city grid", reachable, len(cells))
	}
	// The sweet spot (80 s by road) can only hold cells a scooter could reach at its 40 km/h cap in
	// 80 s, so no cell further than 889 m in a straight line; and it is not empty.
	for _, x := range cells {
		if x.Reachable && x.RoadSeconds <= 80 && x.CentreMetres > 80*40/3.6+g.Edge() {
			t.Errorf("%v is %.0f m away but %.0f s by road", x.Cell, x.CentreMetres, x.RoadSeconds)
		}
	}
	if within80 == 0 {
		t.Fatal("no cell within 80 s of the gate")
	}
	// Measured, for the record (w2): how many cells fall within each road-time budget.
	for _, budget := range []float64{60, 80, 120, 180, 300} {
		n := 0
		for _, x := range cells {
			if x.Reachable && x.RoadSeconds <= budget {
				n++
			}
		}
		t.Logf("within %3.0f s by road: %3d cells (%.3f km²)", budget, n, float64(n)*3*math.Sqrt(3)/2*g.Edge()*g.Edge()/1e6)
	}
	t.Logf("%d cells, %d reachable", len(cells), reachable)
}

func TestStoreRefusesAnErasedOrMovedGate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	g := hexgrid.Bengaluru()
	cells, _ := Build(ctx, &fakeRouter{}, g, gate, 350)
	try := func(from hexgrid.Point) error {
		return database.InTx(ctx, f.pool, func(tx pgx.Tx) error {
			_, err := Store(ctx, tx, f.gh, f.roadData, g, 350, from, cells)
			return err
		})
	}
	// Built from somewhere else than the gate version now says.
	if err := try(hexgrid.Point{Lat: gate.Lat + 0.001, Lng: gate.Lng}); !errors.Is(err, ErrGateChanged) {
		t.Fatalf("moved: %v", err)
	}
	// Erased while the build ran (what geo.RedactGates does to gate_history).
	if _, err := f.pool.Exec(ctx, `UPDATE gate_history SET label = '(removed)', entry_notes = NULL, landmark = NULL,
		lat = round(lat::numeric, 2)::double precision, lng = round(lng::numeric, 2)::double precision, redacted_at = now()
		WHERE id = $1`, f.gh); err != nil {
		t.Fatal(err)
	}
	if err := try(gate); !errors.Is(err, ErrGateChanged) {
		t.Fatalf("erased: %v", err)
	}
	// And the table refuses it outright, whatever the code does.
	_, err := f.pool.Exec(ctx, `INSERT INTO gate_cell_builds (gate_history_id, road_data_version_id, grid, max_radius_m, cells)
		VALUES ($1, $2, 'blr76', 350, 1)`, f.gh, f.roadData)
	refusedAppendOnly(t, err, "a build for an erased gate version")
	var n int
	if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM gate_cell_builds").Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d builds stored, %v", n, err)
	}
}

func TestCellsMustFitTheirBuild(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cells, _ := Build(ctx, &fakeRouter{}, hexgrid.Bengaluru(), gate, 350)
	id := f.store(t, f.roadData, cells)
	_, err := f.pool.Exec(ctx, "INSERT INTO gate_cells (build_id, cell, centre_metres) VALUES ($1, 'mys76:1:2', 10)", id)
	refusedAppendOnly(t, err, "a cell from another grid")
	_, err = f.pool.Exec(ctx, `INSERT INTO gate_cell_builds (gate_history_id, road_data_version_id, grid, max_radius_m, cells, redacted_at)
		VALUES ($1, $2, 'blr76', 350, 1, now())`, f.gh, f.roadData)
	refusedAppendOnly(t, err, "a build that starts redacted")
	if err := database.InTx(ctx, f.pool, func(tx pgx.Tx) error { return Redact(ctx, tx, f.customer) }); err != nil {
		t.Fatal(err)
	}
	_, err = f.pool.Exec(ctx, "INSERT INTO gate_cells (build_id, cell, centre_metres) VALUES ($1, 'blr76:99:99', 10)", id)
	refusedAppendOnly(t, err, "a cell added to an erased build")
}
