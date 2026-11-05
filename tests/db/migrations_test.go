// Package db tests the migrations on a real Postgres. Each test uses its own new
// database, which it drops afterwards. It refuses to run anywhere but localhost, so it
// can never touch Supabase.
package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/teamprosperlink/Ninzo_backend/migrations"
)

const dataMapPath = "../../docs/engineering/endpoint_data_map.md"

// plannedTables reads the tables the data map says this schema must have.
func plannedTables(t *testing.T, versions ...string) []string {
	t.Helper()
	raw, err := os.ReadFile(dataMapPath)
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("(?m)^\\| `[a-z]+` \\| `([a-z_]+)` \\|.*\\| (v[0-9.]+) \\|\\s*$")
	want := map[string]bool{}
	for _, v := range versions {
		want[v] = true
	}
	var tables []string
	for _, m := range row.FindAllStringSubmatch(string(raw), -1) {
		if want[m[2]] {
			tables = append(tables, m[1])
		}
	}
	sort.Strings(tables)
	if len(tables) == 0 {
		t.Fatal("no planned tables found, so this test would prove nothing")
	}
	return tables
}

// freshDB makes a new empty database on the local server, and drops it after the test.
func freshDB(t *testing.T) (*sql.DB, *goose.Provider) {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		if os.Getenv("REQUIRE_DB_TESTS") == "yes" {
			t.Fatal("TEST_DATABASE_URL is required here")
		}
		t.Skip("TEST_DATABASE_URL is not set. Run: task test:db")
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal("TEST_DATABASE_URL is not a valid URL")
	}
	if h := u.Hostname(); h != "localhost" && h != "127.0.0.1" {
		t.Fatalf("refusing to run database tests against %s", h)
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
	u.Path = "/" + name
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		c, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		if _, err := admin.ExecContext(c, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database: %v", err)
		}
	})
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	return db, provider
}

func up(t *testing.T, p *goose.Provider) {
	t.Helper()
	if _, err := p.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func publicTables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT tablename FROM pg_tables WHERE schemaname = 'public' AND tablename <> 'goose_db_version' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func TestSchemaMatchesTheDataMap(t *testing.T) {
	db, p := freshDB(t)
	up(t, p)
	got := strings.Join(publicTables(t, db), ",")
	want := strings.Join(plannedTables(t, "v0.2.0"), ",")
	if got != want {
		t.Errorf("tables differ from the data map.\n got: %s\nwant: %s", got, want)
	}
}

func TestDownAndUpAgain(t *testing.T) {
	db, p := freshDB(t)
	up(t, p)
	before := publicTables(t, db)
	if _, err := p.DownTo(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if left := publicTables(t, db); len(left) != 0 {
		t.Errorf("tables left after going down: %v", left)
	}
	up(t, p)
	if after := publicTables(t, db); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Errorf("up again differs: %v", after)
	}
}

func TestPostGISIsInTheExtensionsSchema(t *testing.T) {
	db, p := freshDB(t)
	up(t, p)
	var schema string
	err := db.QueryRow(`SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = 'postgis'`).Scan(&schema)
	if err != nil {
		t.Fatal(err)
	}
	if schema != "extensions" {
		t.Errorf("PostGIS is in %s", schema)
	}
}

func TestMigrationRefusesPostGISInTheWrongSchema(t *testing.T) {
	db, p := freshDB(t)
	if _, err := db.Exec("CREATE EXTENSION postgis"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Up(context.Background()); err == nil || !strings.Contains(err.Error(), "must be in extensions") {
		t.Errorf("expected a refusal, got %v", err)
	}
}

func TestEveryTableHasRowLevelSecurityWithNoPolicies(t *testing.T) {
	db, p := freshDB(t)
	up(t, p)
	rows, err := db.Query(`SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND NOT c.relrowsecurity`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		t.Errorf("%s has no Row Level Security", name)
	}
	var policies int
	if err := db.QueryRow(`SELECT count(*) FROM pg_policies WHERE schemaname = 'public'`).Scan(&policies); err != nil {
		t.Fatal(err)
	}
	if policies != 0 {
		t.Errorf("%d policies exist, but no table should have any", policies)
	}
}

// TestOtherRolesCannotReadRows acts like the Supabase Data API's anon role.
func TestOtherRolesCannotReadRows(t *testing.T) {
	db, p := freshDB(t)
	up(t, p)
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	role := "anon_" + strings.ReplaceAll(t.Name(), "Test", "")[:8]
	role = strings.ToLower(role)
	for _, stmt := range []string{
		"INSERT INTO customers (phone) VALUES ('+919876543210')",
		"DO $$ BEGIN CREATE ROLE " + role + " NOLOGIN; EXCEPTION WHEN duplicate_object THEN NULL; END $$",
		"GRANT SELECT ON customers TO " + role,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	var owner int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM customers").Scan(&owner); err != nil || owner != 1 {
		t.Fatalf("the owner must see the row: %d, %v", owner, err)
	}
	if _, err := conn.ExecContext(ctx, "SET ROLE "+role); err != nil {
		t.Fatal(err)
	}
	var seen int
	err = conn.QueryRowContext(ctx, "SELECT count(*) FROM customers").Scan(&seen)
	_, _ = conn.ExecContext(ctx, "RESET ROLE")
	if err != nil {
		t.Fatal(err)
	}
	if seen != 0 {
		t.Errorf("another role saw %d rows", seen)
	}
}

func TestDatabaseRefusesBadData(t *testing.T) {
	db, p := freshDB(t)
	up(t, p)
	id := "11111111-1111-1111-1111-111111111111"
	hash := strings.Repeat("a", 64)
	mustExec := func(stmt string) {
		t.Helper()
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	mustExec("INSERT INTO customers (phone) VALUES ('+919876543210')")
	mustExec("INSERT INTO riders (id, phone, name) VALUES ('" + id + "', '+919876543211', 'Ravi')")
	mustExec("INSERT INTO devices (actor_type, actor_id, platform, push_token) VALUES ('rider', '" + id + "', 'android', 'tok1')")
	mustExec("INSERT INTO refresh_tokens (actor_type, actor_id, chain_id, token_hash, expires_at) VALUES ('rider', '" + id + "', '" + id + "', '" + hash + "', now())")

	cases := []struct{ name, stmt string }{
		{"a bad phone number", "INSERT INTO customers (phone) VALUES ('98765')"},
		{"a duplicate customer phone", "INSERT INTO customers (phone) VALUES ('+919876543210')"},
		{"a duplicate rider phone", "INSERT INTO riders (phone, name) VALUES ('+919876543211', 'Asha')"},
		{"an empty rider name", "INSERT INTO riders (phone, name) VALUES ('+919876543212', '')"},
		{"a plain sign-in code", "INSERT INTO otp_requests (actor_type, phone, code_hash, expires_at) VALUES ('customer', '+919876543210', '123456', now())"},
		{"a plain refresh token", "INSERT INTO refresh_tokens (actor_type, actor_id, chain_id, token_hash, expires_at) VALUES ('rider', '" + id + "', '" + id + "', 'short', now())"},
		{"a duplicate refresh token", "INSERT INTO refresh_tokens (actor_type, actor_id, chain_id, token_hash, expires_at) VALUES ('rider', '" + id + "', '" + id + "', '" + hash + "', now())"},
		{"negative OTP attempts", "INSERT INTO otp_requests (actor_type, phone, code_hash, attempts, expires_at) VALUES ('customer', '+919876543210', '" + hash + "', -1, now())"},
		{"an unknown actor type", "INSERT INTO devices (actor_type, actor_id, platform, push_token) VALUES ('admin', '" + id + "', 'android', 'tok2')"},
		{"an unknown platform", "INSERT INTO devices (actor_type, actor_id, platform, push_token) VALUES ('rider', '" + id + "', 'web', 'tok3')"},
		{"a duplicate push token", "INSERT INTO devices (actor_type, actor_id, platform, push_token) VALUES ('rider', '" + id + "', 'ios', 'tok1')"},
		{"an unknown verification status", "INSERT INTO rider_verifications (rider_id, status, method) VALUES ('" + id + "', 'maybe', 'digilocker')"},
		{"a verification for no rider", "INSERT INTO rider_verifications (rider_id, status, method) VALUES ('22222222-2222-2222-2222-222222222222', 'pending', 'digilocker')"},
		{"a short idempotency key", "INSERT INTO idempotency_keys (actor_id, method, path, key, request_hash) VALUES ('" + id + "', 'POST', '/x', 'short', 'h')"},
		{"a stored non-2xx reply", "INSERT INTO idempotency_keys (actor_id, method, path, key, request_hash, status, response) VALUES ('" + id + "', 'POST', '/x', 'long-enough-key', 'h', 409, '{}')"},
		{"a status with no reply", "INSERT INTO idempotency_keys (actor_id, method, path, key, request_hash, status) VALUES ('" + id + "', 'POST', '/x', 'long-enough-key', 'h', 200)"},
	}
	for _, c := range cases {
		var pgErr *pgconn.PgError
		if _, err := db.Exec(c.stmt); !errors.As(err, &pgErr) {
			t.Errorf("%s must be refused, got %v", c.name, err)
		}
	}
	mustExec("DELETE FROM riders WHERE id = '" + id + "'")
}

func TestOneIdempotencyKeyPerAction(t *testing.T) {
	db, p := freshDB(t)
	up(t, p)
	stmt := func(actor, path string) string {
		return fmt.Sprintf("INSERT INTO idempotency_keys (actor_id, method, path, key, request_hash) VALUES ('%s', 'POST', '%s', 'long-enough-key', 'h')", actor, path)
	}
	a, b := "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	if _, err := db.Exec(stmt(a, "/orders")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(stmt(a, "/orders")); err == nil {
		t.Error("the same key on the same path must be refused")
	}
	if _, err := db.Exec(stmt(a, "/gates")); err != nil {
		t.Errorf("another path may use the key: %v", err)
	}
	if _, err := db.Exec(stmt(b, "/orders")); err != nil {
		t.Errorf("another user may use the key: %v", err)
	}
}

func TestOneOpenTaskOfAKind(t *testing.T) {
	db, p := freshDB(t)
	up(t, p)
	id := "11111111-1111-1111-1111-111111111111"
	insert := "INSERT INTO scheduled_tasks (kind, entity_id, run_at) VALUES ('offer_expires', '" + id + "', now())"
	if _, err := db.Exec(insert); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(insert); err == nil {
		t.Error("a second open task of the same kind must be refused")
	}
	if _, err := db.Exec("UPDATE scheduled_tasks SET done_at = now()"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(insert); err != nil {
		t.Errorf("a new task is fine once the last is done: %v", err)
	}
}

func TestAuditEventsAreAppendOnly(t *testing.T) {
	db, p := freshDB(t)
	up(t, p)
	insert := "INSERT INTO audit_events (actor_type, action, entity_type, entity_id) VALUES ('rider', 'test', 'order', 'x')"
	if _, err := db.Exec(insert); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{"UPDATE audit_events SET action = 'changed'", "DELETE FROM audit_events", "TRUNCATE audit_events"} {
		if _, err := db.Exec(stmt); err == nil {
			t.Errorf("%s must be refused", stmt)
		}
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM audit_events").Scan(&count); err != nil || count != 1 {
		t.Errorf("rows: %d, %v", count, err)
	}
}
