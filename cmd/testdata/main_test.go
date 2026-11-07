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
