// Package dbtest gives tests a throwaway, fully migrated database on local Postgres. It refuses
// anything but this machine, so it can never touch Supabase.
package dbtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // The database/sql driver used to run migrations.
	"github.com/pressly/goose/v3"

	"github.com/teamprosperlink/Ninzo_backend/internal/dbtarget"
	"github.com/teamprosperlink/Ninzo_backend/migrations"
)

// Fresh returns a pool on a new migrated database, signed in as ninzo_service, so tests prove
// the code works with the powers a real service has. It skips when no database is configured.
func Fresh(t *testing.T) *pgxpool.Pool {
	t.Helper()
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
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name := "ninzo_test_" + hex.EncodeToString(suffix)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		if _, err := admin.ExecContext(c, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database: %v", err)
		}
		// Fails harmlessly if another local database, such as the real one, still uses it.
		_, _ = admin.ExecContext(c, "DROP ROLE IF EXISTS ninzo_service")
	})
	u.Path = "/" + name

	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}

	cfg, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 8
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE ninzo_service")
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var who string
	if err := pool.QueryRow(ctx, "SELECT current_user").Scan(&who); err != nil || who != dbtarget.ServiceRole {
		t.Fatalf("tests must run as the service role, got %q, %v", who, err)
	}
	return pool
}
