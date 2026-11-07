package geo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
)

func ptr(v int) *int { return &v }

func newZone(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), "INSERT INTO zones (name, city, is_test) VALUES ($1, 'Bengaluru', true) RETURNING id::text", name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func change(t *testing.T, pool *pgxpool.Pool, zoneID string, changes ...Change) (int, error) {
	t.Helper()
	var v int
	err := database.InTx(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		v, err = ChangeSettings(context.Background(), tx, zoneID, changes, "a test change", "test")
		return err
	})
	return v, err
}

func load(t *testing.T, pool *pgxpool.Pool, zoneID string) Effective {
	t.Helper()
	e, err := LoadSettings(context.Background(), pool, zoneID)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestTheStartingValues(t *testing.T) {
	pool := dbtest.Fresh(t)
	e := load(t, pool, "")
	for name, want := range map[string]int{
		"timer_length_s": 90, "zone_radius_m": 350, "arrival_perimeter_m": 100, "max_wait_before_timer_s": 300,
		"cancel_window_s": 5, "signal_release_s": 30, "reassign_after_s": 120, "sweet_spot_budget_s": 80,
	} {
		if got, ok := e.Get(name); !ok || got != want {
			t.Errorf("%s: %d, %v", name, got, ok)
		}
	}
	if _, ok := e.Get("packing_limit_s"); ok {
		t.Error("the packing limit is not decided, so it must be unset")
	}
	if e.GlobalVersion != 1 || e.ZoneVersion != 0 {
		t.Errorf("%+v", e)
	}
	if err := e.check(); err != nil {
		t.Errorf("the starting values contradict each other: %v", err)
	}
}

func TestEverySettingHasAColumnAndARange(t *testing.T) {
	pool := dbtest.Fresh(t)
	var defs string
	if err := pool.QueryRow(context.Background(), `SELECT string_agg(pg_get_constraintdef(oid), ' ') FROM pg_constraint
		WHERE conrelid = 'zone_settings'::regclass AND contype = 'c'`).Scan(&defs); err != nil {
		t.Fatal(err)
	}
	for _, s := range Settings {
		lo, hi := fmt.Sprintf("(%s >= %d)", s.Name, s.Min), fmt.Sprintf("(%s <= %d)", s.Name, s.Max)
		if !strings.Contains(defs, lo) || !strings.Contains(defs, hi) {
			t.Errorf("%s: the database must allow exactly %d to %d, like the code", s.Name, s.Min, s.Max)
		}
	}
}

func TestAZoneOverridesOnlyWhatItSets(t *testing.T) {
	pool := dbtest.Fresh(t)
	zone := newZone(t, pool, "Koramangala test")
	if _, err := change(t, pool, zone, Change{"timer_length_s", ptr(120)}); err != nil {
		t.Fatal(err)
	}
	e := load(t, pool, zone)
	if v, _ := e.Get("timer_length_s"); v != 120 {
		t.Errorf("the zone's own value: %d", v)
	}
	if v, _ := e.Get("zone_radius_m"); v != 350 {
		t.Errorf("the global value: %d", v)
	}
	if g := load(t, pool, ""); func() int { v, _ := g.Get("timer_length_s"); return v }() != 90 {
		t.Error("a zone change altered the global value")
	}
	// Clearing an override falls back to the global value.
	if _, err := change(t, pool, zone, Change{Name: "timer_length_s"}); err != nil {
		t.Fatal(err)
	}
	if v, _ := load(t, pool, zone).Get("timer_length_s"); v != 90 {
		t.Errorf("after clearing: %d", v)
	}
}

func TestAChangeIsANewVersionAndOldOnesStay(t *testing.T) {
	pool := dbtest.Fresh(t)
	v, err := change(t, pool, "", Change{"timer_length_s", ptr(100)})
	if err != nil || v != 2 {
		t.Fatalf("%d %v", v, err)
	}
	var old int
	if err := pool.QueryRow(context.Background(), "SELECT timer_length_s FROM zone_settings WHERE zone_id IS NULL AND version = 1").Scan(&old); err != nil || old != 90 {
		t.Errorf("version 1 changed: %d %v", old, err)
	}
	if _, err := pool.Exec(context.Background(), "UPDATE zone_settings SET timer_length_s = 60"); err == nil {
		t.Error("a stored version could be edited, so an order's values could change under it")
	}
	if _, err := pool.Exec(context.Background(), "DELETE FROM zone_settings"); err == nil {
		t.Error("a stored version could be deleted")
	}
}

func TestEveryChangeIsAudited(t *testing.T) {
	pool := dbtest.Fresh(t)
	if _, err := change(t, pool, "", Change{"sweet_spot_budget_s", ptr(70)}); err != nil {
		t.Fatal(err)
	}
	var before, after, reason string
	if err := pool.QueryRow(context.Background(), "SELECT before::text, after::text, reason FROM audit_events WHERE action = 'settings.changed'").Scan(&before, &after, &reason); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(before, "80") || !strings.Contains(after, "70") || !strings.Contains(reason, "test") {
		t.Errorf("%s %s %s", before, after, reason)
	}
}

func TestBadChangesStoreNothing(t *testing.T) {
	pool := dbtest.Fresh(t)
	zone := newZone(t, pool, "Indiranagar test")
	for name, c := range map[string]struct {
		zone    string
		changes []Change
	}{
		"perimeter outside the zone":     {"", []Change{{"arrival_perimeter_m", ptr(400)}}},
		"a zone's perimeter too wide":    {zone, []Change{{"arrival_perimeter_m", ptr(350)}}},
		"reassign before release":        {"", []Change{{"reassign_after_s", ptr(30)}}},
		"search narrower than the zone":  {"", []Change{{"max_search_radius_m", ptr(300)}}},
		"clearing a needed global value": {"", []Change{{Name: "timer_length_s"}}},
		"a zone that does not exist":     {"00000000-0000-0000-0000-000000000000", []Change{{"timer_length_s", ptr(100)}}},
	} {
		if _, err := change(t, pool, c.zone, c.changes...); !errors.Is(err, ErrInvalidSetting) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A global change that breaks one zone is refused too.
	if _, err := change(t, pool, zone, Change{"arrival_perimeter_m", ptr(200)}); err != nil {
		t.Fatal(err)
	}
	if _, err := change(t, pool, "", Change{"zone_radius_m", ptr(150)}); !errors.Is(err, ErrInvalidSetting) {
		t.Errorf("a global change broke a zone: %v", err)
	}
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM zone_settings").Scan(&n); err != nil || n != 2 {
		t.Errorf("%d rows: a refused change was stored", n)
	}
}

func TestParsingChanges(t *testing.T) {
	got, err := ParseChanges([]string{"timer_length_s=120", "packing_limit_s="})
	if err != nil || len(got) != 2 || *got[0].Value != 120 || got[1].Value != nil {
		t.Errorf("%+v %v", got, err)
	}
	for _, args := range [][]string{
		nil, {"nope=1"}, {"timer_length_s"}, {"timer_length_s=abc"}, {"timer_length_s=29"}, {"timer_length_s=601"},
		{"timer_length_s=90", "timer_length_s=91"}, {"timer_length_s=9.5"},
	} {
		if _, err := ParseChanges(args); !errors.Is(err, ErrInvalidSetting) {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestChangesNeedAReasonAndAName(t *testing.T) {
	pool := dbtest.Fresh(t)
	for _, c := range [][2]string{{"", "owner"}, {"ok", "owner"}, {"a real reason", " "}} {
		err := database.InTx(context.Background(), pool, func(tx pgx.Tx) error {
			_, err := ChangeSettings(context.Background(), tx, "", []Change{{"timer_length_s", ptr(100)}}, c[0], c[1])
			return err
		})
		if !errors.Is(err, ErrInvalidSetting) {
			t.Errorf("%q by %q: %v", c[0], c[1], err)
		}
	}
}

func TestDescribeShowsEverySetting(t *testing.T) {
	pool := dbtest.Fresh(t)
	out := load(t, pool, "").Describe()
	for _, s := range Settings {
		if !strings.Contains(out, s.Name) {
			t.Errorf("%s is missing", s.Name)
		}
	}
	if !strings.Contains(out, "not set") {
		t.Error("unset values must say so")
	}
}
