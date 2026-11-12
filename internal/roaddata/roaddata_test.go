package roaddata

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
)

// A real build.json from tools/osrm/build.sh (#183), Bengaluru.
const realBuild = `{
  "name": "bengaluru",
  "source": "https://download.geofabrik.de/asia/india/southern-zone-latest.osm.pbf",
  "source_sha256": "81f355bdfb7925a5aae68552bfb53dd558891bddbcd88e07a29765567bdf0693",
  "clip_box": "77.35,12.75,77.90,13.25",
  "osm_extract": "bengaluru-clip.osm.pbf",
  "osm_extract_sha256": "03e3a95d89675158f70d53adcefd0ab7b50b714a9459afce82f3314712f7bdc5",
  "osm_timestamp": "2026-11-11T21:22:47Z",
  "profile": "scooter",
  "profile_version": "scooter-2",
  "profile_sha256": "33c759078421405fa5778a8126955caabf5c0e0e6952cdd4584f05bcc88b2404",
  "osrm_image": "ghcr.io/project-osrm/osrm-backend:v6.0.0@sha256:729461bcc9ae9e6aafa92c0f93db9b060a32e85d5e72092c01ae4a4a9f1eb564",
  "osmium": "1.15.0-1",
  "algorithm": "mld",
  "built_at": "2026-11-12T18:18:57Z",
  "build_seconds": 31
}`

func edited(t *testing.T, change func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(realBuild), &m); err != nil {
		t.Fatal(err)
	}
	change(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestARealBuildParses(t *testing.T) {
	v, err := Parse([]byte(realBuild))
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != "bengaluru" || v.ProfileVersion != "scooter-2" || v.Algorithm != "mld" ||
		!v.OSMTimestamp.Equal(time.Date(2026, 11, 11, 21, 22, 47, 0, time.UTC)) {
		t.Fatalf("%+v", v)
	}
}

func TestBadBuildsAreRefused(t *testing.T) {
	cases := map[string]struct {
		change func(m map[string]any)
		want   string
	}{
		"empty timestamp (the old build.sh)": {func(m map[string]any) { m["osm_timestamp"] = "" }, "osm_timestamp"},
		"no timestamp":                       {func(m map[string]any) { delete(m, "osm_timestamp") }, "osm_timestamp"},
		"short sha":                          {func(m map[string]any) { m["profile_sha256"] = "abc" }, "profile_sha256"},
		"upper-case sha":                     {func(m map[string]any) { m["source_sha256"] = strings.ToUpper(m["source_sha256"].(string)) }, "source_sha256"},
		"no source":                          {func(m map[string]any) { delete(m, "source") }, "source"},
		"box upside down":                    {func(m map[string]any) { m["clip_box"] = "77.90,12.75,77.35,13.25" }, "clip_box"},
		"box out of range":                   {func(m map[string]any) { m["clip_box"] = "77.35,12.75,277.90,13.25" }, "clip_box"},
		"box of three":                       {func(m map[string]any) { m["clip_box"] = "77.35,12.75,77.90" }, "clip_box"},
		"box with a plus":                    {func(m map[string]any) { m["clip_box"] = "+77.35,12.75,77.90,13.25" }, "clip_box"},
		"box in exponent form":               {func(m map[string]any) { m["clip_box"] = "7.735e1,12.75,77.90,13.25" }, "clip_box"},
		"bad name":                           {func(m map[string]any) { m["name"] = "Bengaluru City" }, "name"},
		"unknown algorithm":                  {func(m map[string]any) { m["algorithm"] = "dijkstra" }, "algorithm"},
		"no image":                           {func(m map[string]any) { m["osrm_image"] = "" }, "osrm_image"},
		"map newer than the build":           {func(m map[string]any) { m["osm_timestamp"] = "2026-11-13T00:00:00Z" }, "newer"},
		"several things":                     {func(m map[string]any) { delete(m, "profile"); m["algorithm"] = "x" }, "algorithm, profile"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(edited(t, c.change))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error naming %q", err, c.want)
			}
		})
	}
	for name, raw := range map[string]string{
		"not json":    "built bengaluru in 30s",
		"two values":  realBuild + realBuild,
		"wrong types": `{"name": 3}`,
		"empty":       ``,
	} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func register(t *testing.T, ctx context.Context, pool interface {
	Begin(context.Context) (pgx.Tx, error)
}, v Version) (string, bool) {
	t.Helper()
	var id string
	var created bool
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	id, created, err = Register(ctx, tx, v)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return id, created
}

func TestTheSameBuildRegistersOnce(t *testing.T) {
	pool := dbtest.Fresh(t)
	ctx := context.Background()
	v, err := Parse([]byte(realBuild))
	if err != nil {
		t.Fatal(err)
	}
	id1, created := register(t, ctx, pool, v)
	if !created || id1 == "" {
		t.Fatalf("first: %q %v", id1, created)
	}
	// The same build again, even with a later build time or another name: the same id, nothing changed.
	again := v
	again.BuiltAt = v.BuiltAt.Add(time.Hour)
	again.Name = "bengaluru-again"
	id2, created := register(t, ctx, pool, again)
	if created || id2 != id1 {
		t.Fatalf("second: %q %v, want %q and not created", id2, created, id1)
	}
	var name string
	var built time.Time
	if err := pool.QueryRow(ctx, "SELECT name, built_at FROM road_data_versions WHERE id = $1", id1).Scan(&name, &built); err != nil {
		t.Fatal(err)
	}
	if name != "bengaluru" || !built.Equal(v.BuiltAt) {
		t.Fatalf("the stored row changed: %s %v", name, built)
	}
	// A new profile is a new build.
	newProfile := v
	newProfile.ProfileVersion = "scooter-3"
	newProfile.ProfileSHA256 = strings.Repeat("a", 64)
	id3, created := register(t, ctx, pool, newProfile)
	if !created || id3 == id1 {
		t.Fatalf("new profile: %q %v", id3, created)
	}
	rows, err := List(ctx, pool)
	if err != nil || len(rows) != 2 || rows[0].ID != id3 {
		t.Fatalf("list %+v %v", rows, err)
	}
}

func TestTwoRegistersAtOnceStoreOneRow(t *testing.T) {
	pool := dbtest.Fresh(t)
	ctx := context.Background()
	v, _ := Parse([]byte(realBuild))
	ids := make([]string, 4)
	errs := make([]error, 4)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = database.InTx(ctx, pool, func(tx pgx.Tx) error {
				var err error
				ids[i], _, err = Register(ctx, tx, v)
				return err
			})
		}()
	}
	wg.Wait()
	for i, id := range ids {
		if errs[i] != nil || id == "" || id != ids[0] {
			t.Fatalf("ids %v, errors %v: want one id and no errors", ids, errs)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM road_data_versions").Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d rows, %v", n, err)
	}
}

// insertRaw stores a build straight through SQL, past Check, with one column changed.
func insertRaw(ctx context.Context, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, v Version, column string, value any) error {
	cols := []string{"name", "source", "source_sha256", "clip_box", "osm_extract_sha256", "osm_timestamp",
		"profile", "profile_version", "profile_sha256", "osrm_image", "algorithm", "built_at"}
	vals := []any{v.Name, v.Source, v.SourceSHA256, v.ClipBox, v.OSMExtractSHA256, v.OSMTimestamp,
		v.Profile, v.ProfileVersion, v.ProfileSHA256, v.OSRMImage, v.Algorithm, v.BuiltAt}
	for i, c := range cols {
		if c == column {
			vals[i] = value
		}
	}
	marks := make([]string, len(cols))
	for i := range marks {
		marks[i] = "$" + strconv.Itoa(i+1)
	}
	_, err := pool.Exec(ctx, "INSERT INTO road_data_versions ("+strings.Join(cols, ", ")+") VALUES ("+strings.Join(marks, ", ")+")", vals...)
	return err
}

func TestTheDatabaseRefusesWhatCheckRefuses(t *testing.T) {
	pool := dbtest.Fresh(t)
	ctx := context.Background()
	v, _ := Parse([]byte(realBuild))
	// Straight to SQL, past Check: the table's own rules must hold too.
	for _, c := range []struct {
		column string
		value  any
	}{
		{"name", "Has Spaces"}, {"name", ""},
		{"source", ""}, {"source", strings.Repeat("s", 501)},
		{"source_sha256", "nope"}, {"osm_extract_sha256", strings.ToUpper(v.OSMExtractSHA256)}, {"profile_sha256", strings.Repeat("a", 63)},
		{"clip_box", "77.35,12.75,77.90"}, {"clip_box", "+77.35,12.75,77.90,13.25"}, {"clip_box", "7.735e1,12.75,77.90,13.25"},
		{"osm_timestamp", nil}, {"built_at", nil},
		{"profile", "Scooter"}, {"profile_version", "has space"},
		{"osrm_image", ""}, {"osrm_image", strings.Repeat("i", 301)},
		{"algorithm", "dijkstra"},
	} {
		err := insertRaw(ctx, pool, v, c.column, c.value)
		var pgErr *pgconn.PgError
		// 23514 is a CHECK, 23502 a NOT NULL: the table's own rule refused it, not some other error.
		if !errors.As(err, &pgErr) || (pgErr.Code != "23514" && pgErr.Code != "23502") {
			t.Errorf("%s = %v: want a CHECK or NOT NULL refusal, got %v", c.column, c.value, err)
		}
	}
	// The real build itself is fine.
	if err := insertRaw(ctx, pool, v, "", nil); err != nil {
		t.Fatalf("the real build was refused: %v", err)
	}
}

func TestAStoredBuildCannotChange(t *testing.T) {
	pool := dbtest.Fresh(t)
	ctx := context.Background()
	v, _ := Parse([]byte(realBuild))
	id, _ := register(t, ctx, pool, v)
	for name, sql := range map[string]string{
		"update":   "UPDATE road_data_versions SET profile_version = 'scooter-9' WHERE id = '" + id + "'",
		"delete":   "DELETE FROM road_data_versions WHERE id = '" + id + "'",
		"truncate": "TRUNCATE road_data_versions",
	} {
		if _, err := pool.Exec(ctx, sql); err == nil {
			t.Errorf("%s: allowed", name)
		}
	}
	var pv string
	if err := pool.QueryRow(ctx, "SELECT profile_version FROM road_data_versions WHERE id = $1", id).Scan(&pv); err != nil || pv != "scooter-2" {
		t.Fatalf("%q %v", pv, err)
	}
}
