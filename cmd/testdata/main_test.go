package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
)

func env(values map[string]string) func(string) string {
	return func(k string) string { return values[k] }
}

func TestBadArgumentsTouchNothing(t *testing.T) {
	for _, args := range [][]string{nil, {"zones"}, {"-local"}, {"-local", "menu"}, {"-staging", "zones"}} {
		if err := run(context.Background(), args, env(map[string]string{"DATABASE_URL": "postgres://u:p@localhost/db"}), &bytes.Buffer{}); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
	remote := "postgres://postgres.chyejfgsipcsdxnginei:secret-pass@aws-0-ap-south-1.pooler.supabase.com:5432/postgres"
	if err := run(context.Background(), []string{"-local", "zones"}, env(map[string]string{"DATABASE_URL": remote}), &bytes.Buffer{}); err == nil {
		t.Error("-local would have touched Supabase")
	}
}

func TestZonesAreAddedOnceAndRealOnesAreLeftAlone(t *testing.T) {
	pool := dbtest.Fresh(t)
	url := pool.Config().ConnString()
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-local", "zones"}, env(map[string]string{"DATABASE_URL": url}), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "added") != len(testZones) {
		t.Errorf("%s", out.String())
	}
	out.Reset()
	if err := run(context.Background(), []string{"-local", "zones"}, env(map[string]string{"DATABASE_URL": url}), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "kept") != len(testZones) {
		t.Errorf("running again must change nothing: %s", out.String())
	}
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM zones WHERE is_test AND status = 'active' AND area IS NOT NULL").Scan(&n); err != nil || n != len(testZones) {
		t.Errorf("%d %v", n, err)
	}
	if _, err := pool.Exec(context.Background(), "UPDATE zones SET status = 'paused', area = NULL WHERE name = $1", testZones[0].name); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run(context.Background(), []string{"-local", "zones"}, env(map[string]string{"DATABASE_URL": url}), &out); err != nil || !strings.Contains(out.String(), "restored") {
		t.Errorf("a broken test zone was not restored: %s %v", out.String(), err)
	}
	if _, err := pool.Exec(context.Background(), "UPDATE zones SET is_test = false WHERE name = $1", testZones[0].name); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"-local", "zones"}, env(map[string]string{"DATABASE_URL": url}), &bytes.Buffer{}); err == nil {
		t.Error("test data touched a real zone")
	}
}

func TestTheTestMenuIsAddedOnce(t *testing.T) {
	pool := dbtest.Fresh(t)
	url := pool.Config().ConnString()
	var out bytes.Buffer
	for i := 0; i < 2; i++ {
		out.Reset()
		if err := run(context.Background(), []string{"-local", "menu"}, env(map[string]string{"DATABASE_URL": url}), &out); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Count(out.String(), "kept") != len(testItems) {
		t.Errorf("running again must change nothing: %s", out.String())
	}
	var items, prices int
	if err := pool.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM menu_items WHERE is_test AND status = 'active'), (SELECT count(*) FROM price_history WHERE reason = 'test data')").Scan(&items, &prices); err != nil || items != len(testItems) || prices != len(testItems) {
		t.Errorf("%d items, %d prices, %v", items, prices, err)
	}
}

func TestTheTestFleetIsAddedOnce(t *testing.T) {
	pool := dbtest.Fresh(t)
	url := pool.Config().ConnString()
	if err := run(context.Background(), []string{"-local", "fleet"}, env(map[string]string{"DATABASE_URL": url}), &bytes.Buffer{}); err == nil {
		t.Error("fleet ran with no zones")
	}
	for _, what := range []string{"zones", "menu", "fleet", "fleet"} {
		if err := run(context.Background(), []string{"-local", what}, env(map[string]string{"DATABASE_URL": url}), &bytes.Buffer{}); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	var nodes, scooters, lots int
	if err := pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM staging_nodes), (SELECT count(*) FROM scooters), (SELECT count(*) FROM lots)`).Scan(&nodes, &scooters, &lots); err != nil {
		t.Fatal(err)
	}
	if nodes != len(testZones) || scooters != 2*len(testZones) || lots != 10*len(testItems) {
		t.Errorf("%d nodes, %d scooters, %d lots", nodes, scooters, lots)
	}
}

func TestTheTestRiderIsAddedOnce(t *testing.T) {
	pool := dbtest.Fresh(t)
	url := pool.Config().ConnString()
	for _, what := range []string{"zones", "rider", "rider"} {
		if err := run(context.Background(), []string{"-local", what}, env(map[string]string{"DATABASE_URL": url}), &bytes.Buffer{}); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM riders r JOIN people p ON p.id = r.person_id
		WHERE p.phone = $1 AND r.status = 'active' AND r.zone_id IS NOT NULL
			AND EXISTS (SELECT 1 FROM rider_verifications v WHERE v.rider_id = r.id AND v.status = 'verified')`, testRiderPhone).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d %v", n, err)
	}
}

func TestTestHubsAreAddedOnce(t *testing.T) {
	pool := dbtest.Fresh(t)
	url := pool.Config().ConnString()
	if err := run(context.Background(), []string{"-local", "hubs"}, env(map[string]string{"DATABASE_URL": url}), &bytes.Buffer{}); err == nil {
		t.Error("hubs ran with no zones")
	}
	for _, what := range []string{"zones", "menu", "hubs", "hubs"} {
		if err := run(context.Background(), []string{"-local", what}, env(map[string]string{"DATABASE_URL": url}), &bytes.Buffer{}); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	var hubs, lots int
	if err := pool.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM hubs WHERE is_test), (SELECT count(*) FROM lots WHERE hub_id IS NOT NULL)").Scan(&hubs, &lots); err != nil {
		t.Fatal(err)
	}
	var zones, items int
	if err := pool.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM zones WHERE is_test), (SELECT count(*) FROM menu_items WHERE is_test)").Scan(&zones, &items); err != nil {
		t.Fatal(err)
	}
	if hubs != zones || lots != zones*items*5 {
		t.Errorf("%d hubs, %d hub lots, for %d zones and %d items", hubs, lots, zones, items)
	}
}
