package main

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/teamprosperlink/Ninzo_backend/internal/dbtarget"
)

// These stop before connecting, so no database is needed. The hosts do not exist.
func TestRunRefusesTheWrongTarget(t *testing.T) {
	const pooler = "nonexistent.pooler.supabase.com:5432/postgres?sslmode=require"
	ninzo := "postgres://postgres." + dbtarget.NinzoRef + ":p@" + pooler
	other := "postgres://postgres.fvyyfghhccivwwvywvuu:p@" + pooler
	local := "postgres://u:p@localhost:5432/d?sslmode=disable"
	cases := []struct {
		name, url string
		args      []string
		want      string
	}{
		{"no flag", local, []string{"up"}, "usage"},
		{"unknown flag", local, []string{"-staging", "up"}, "usage"},
		{"unknown command", local, []string{"-local", "drop"}, "usage"},
		{"no URL", "", []string{"-local", "up"}, "not set"},
		{"Supabase on -local", ninzo, []string{"-local", "up"}, "local Postgres only"},
		{"localhost on -remote", local, []string{"-remote", "up"}, "refusing localhost"},
		{"the other project", other, []string{"-remote", "up"}, "refusing project"},
		{"the other project, status", other, []string{"-remote", "status"}, "refusing project"},
		{"down on Supabase", ninzo, []string{"-remote", "down"}, "refusing to go down"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", c.url)
			t.Setenv("MIGRATE_ALLOW_DOWN", "")
			err := run(c.args)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

func TestStatusChangesNothingOnAnEmptyDatabase(t *testing.T) {
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		if os.Getenv("REQUIRE_DB_TESTS") == "yes" {
			t.Fatal("TEST_DATABASE_URL is required here")
		}
		t.Skip("TEST_DATABASE_URL is not set. Run: task test:db")
	}
	target, err := dbtarget.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.CheckLocal(); err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	const name = "ninzo_test_status"
	if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop: %v", err)
		}
	}()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name

	t.Setenv("DATABASE_URL", u.String())
	if err := run([]string{"-local", "status"}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var tables int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Errorf("status created %d tables", tables)
	}
}
