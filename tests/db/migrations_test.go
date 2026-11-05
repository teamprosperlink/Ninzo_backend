// Package db tests the migrations on a real Postgres. Each test uses its own new
// database, which it drops afterwards. It refuses to run anywhere but this machine, so it
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

	"github.com/teamprosperlink/Ninzo_backend/internal/dbtarget"
	"github.com/teamprosperlink/Ninzo_backend/migrations"
)

const (
	dataMapPath = "../../docs/engineering/endpoint_data_map.md"

	checkViolation     = "23514"
	uniqueViolation    = "23505"
	foreignKeyMismatch = "23503"
	appendOnlyRefused  = "23001" // restrict_violation, raised by forbid_change.

	actorA = "11111111-1111-1111-1111-111111111111"
	actorB = "22222222-2222-2222-2222-222222222222"
)

var supabaseAPIRoles = []string{"anon", "authenticated", "service_role"}

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
	target, err := dbtarget.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.CheckLocal(); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal("TEST_DATABASE_URL is not a valid URL")
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

func mustExec(t *testing.T, db *sql.DB, stmt string) {
	t.Helper()
	if _, err := db.Exec(stmt); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

// refused checks the exact reason, so a typo or a missing column cannot pass as a refusal.
func refused(t *testing.T, db *sql.DB, name, stmt, code, constraint string) {
	t.Helper()
	_, err := db.Exec(stmt)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Errorf("%s must be refused, got %v", name, err)
		return
	}
	if pgErr.Code != code || pgErr.ConstraintName != constraint {
		t.Errorf("%s: want %s on %q, got %s on %q: %s", name, code, constraint, pgErr.Code, pgErr.ConstraintName, pgErr.Message)
	}
}

// tempRoles creates roles and removes them after the test. Roles belong to the whole
// server, so they must not be left behind.
func tempRoles(t *testing.T, db *sql.DB, roles ...string) {
	t.Helper()
	for _, role := range roles {
		mustExec(t, db, "CREATE ROLE "+role+" NOLOGIN")
		t.Cleanup(func() {
			for _, stmt := range []string{"DROP OWNED BY " + role, "DROP ROLE " + role} {
				if _, err := db.Exec(stmt); err != nil {
					t.Errorf("%s: %v", stmt, err)
				}
			}
		})
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
	if err := rows.Err(); err != nil {
		t.Fatal(err)
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

func TestGoingDownKeepsAPostGISThatWasThereFirst(t *testing.T) {
	db, p := freshDB(t)
	mustExec(t, db, "CREATE SCHEMA extensions")
	mustExec(t, db, "CREATE EXTENSION postgis SCHEMA extensions")
	up(t, p)
	if _, err := p.DownTo(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM pg_extension WHERE extname = 'postgis'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Error("going down removed PostGIS")
	}
}

func TestMigrationRefusesPostGISInTheWrongSchema(t *testing.T) {
	db, p := freshDB(t)
	mustExec(t, db, "CREATE EXTENSION postgis")
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
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var policies int
	if err := db.QueryRow(`SELECT count(*) FROM pg_policies WHERE schemaname = 'public'`).Scan(&policies); err != nil {
		t.Fatal(err)
	}
	if policies != 0 {
		t.Errorf("%d policies exist, but no table should have any", policies)
	}
}

// TestRowLevelSecurityHidesRowsEvenWithAGrant proves the second lock works alone.
func TestRowLevelSecurityHidesRowsEvenWithAGrant(t *testing.T) {
	db, p := freshDB(t)
	up(t, p)
	tempRoles(t, db, "ninzo_test_reader")
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, stmt := range []string{
		"INSERT INTO customers (phone) VALUES ('+919876543210')",
		"GRANT SELECT ON customers TO ninzo_test_reader",
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	var owner int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM customers").Scan(&owner); err != nil || owner != 1 {
		t.Fatalf("the owner must see the row: %d, %v", owner, err)
	}
	if _, err := conn.ExecContext(ctx, "SET ROLE ninzo_test_reader"); err != nil {
		t.Fatal(err)
	}
	var seen int
	err = conn.QueryRowContext(ctx, "SELECT count(*) FROM customers").Scan(&seen)
	if _, resetErr := conn.ExecContext(ctx, "RESET ROLE"); resetErr != nil {
		t.Fatal(resetErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if seen != 0 {
		t.Errorf("another role saw %d rows", seen)
	}
}

// emulateSupabaseGrants sets the default grants Supabase gives its API roles, so
// migrations run as they would there.
func emulateSupabaseGrants(t *testing.T, db *sql.DB) {
	t.Helper()
	tempRoles(t, db, supabaseAPIRoles...)
	for _, role := range supabaseAPIRoles {
		for _, kind := range []string{"TABLES", "SEQUENCES", "ROUTINES"} {
			mustExec(t, db, fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON %s TO %s", kind, role))
		}
	}
}

func apiRolesHaveNoAccess(t *testing.T, db *sql.DB, tables []string) {
	t.Helper()
	for _, role := range supabaseAPIRoles {
		for _, table := range tables {
			var has bool
			err := db.QueryRow(`SELECT has_table_privilege($1, $2, 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')`, role, table).Scan(&has)
			if err != nil {
				t.Fatal(err)
			}
			if has {
				t.Errorf("%s has access to %s", role, table)
			}
		}
	}
}

func TestSupabaseAPIRolesGetNothing(t *testing.T) {
	db, p := freshDB(t)
	emulateSupabaseGrants(t, db)
	up(t, p)

	tables := append(publicTables(t, db), "goose_db_version")
	apiRolesHaveNoAccess(t, db, tables)

	for _, role := range supabaseAPIRoles {
		var has bool
		if err := db.QueryRow(`SELECT has_sequence_privilege($1, 'audit_events_id_seq', 'USAGE,SELECT,UPDATE')`, role).Scan(&has); err != nil {
			t.Fatal(err)
		}
		if has {
			t.Errorf("%s can use a sequence", role)
		}
		if err := db.QueryRow(`SELECT has_function_privilege($1, 'forbid_change()', 'EXECUTE')`, role).Scan(&has); err != nil {
			t.Fatal(err)
		}
		if has {
			t.Errorf("%s can call forbid_change", role)
		}
	}

	// A table from a later migration must get nothing either.
	mustExec(t, db, "CREATE TABLE later_table (id integer)")
	apiRolesHaveNoAccess(t, db, []string{"later_table"})
}

func TestDatabaseRefusesBadData(t *testing.T) {
	db, p := freshDB(t)
	up(t, p)
	hash := strings.Repeat("a", 64)
	mustExec(t, db, "INSERT INTO customers (phone) VALUES ('+919876543210')")
	mustExec(t, db, "INSERT INTO riders (id, phone, name) VALUES ('"+actorA+"', '+919876543211', 'Ravi')")
	mustExec(t, db, "INSERT INTO devices (actor_type, actor_id, platform, push_token) VALUES ('rider', '"+actorA+"', 'android', 'tok1')")
	mustExec(t, db, "INSERT INTO refresh_tokens (actor_type, actor_id, chain_id, token_hash, expires_at) VALUES ('rider', '"+actorA+"', '"+actorA+"', '"+hash+"', now())")

	idem := "INSERT INTO idempotency_keys (actor_id, method, path, key, request_hash, status, response, locked_until) VALUES ('" + actorA + "', 'POST', '/x', "
	cases := []struct{ name, stmt, code, constraint string }{
		{"a bad phone number", "INSERT INTO customers (phone) VALUES ('98765')", checkViolation, "customers_phone_check"},
		{"a duplicate customer phone", "INSERT INTO customers (phone) VALUES ('+919876543210')", uniqueViolation, "customers_phone_key"},
		{"a duplicate rider phone", "INSERT INTO riders (phone, name) VALUES ('+919876543211', 'Asha')", uniqueViolation, "riders_phone_key"},
		{"an empty rider name", "INSERT INTO riders (phone, name) VALUES ('+919876543212', '')", checkViolation, "riders_name_check"},
		{"a plain sign-in code", "INSERT INTO otp_requests (actor_type, phone, code_hash, expires_at) VALUES ('customer', '+919876543210', '123456', now())", checkViolation, "otp_requests_code_hash_check"},
		{"a plain refresh token", "INSERT INTO refresh_tokens (actor_type, actor_id, chain_id, token_hash, expires_at) VALUES ('rider', '" + actorA + "', '" + actorA + "', 'short', now())", checkViolation, "refresh_tokens_token_hash_check"},
		{"a duplicate refresh token", "INSERT INTO refresh_tokens (actor_type, actor_id, chain_id, token_hash, expires_at) VALUES ('rider', '" + actorA + "', '" + actorA + "', '" + hash + "', now())", uniqueViolation, "refresh_tokens_token_hash_key"},
		{"negative OTP attempts", "INSERT INTO otp_requests (actor_type, phone, code_hash, attempts, expires_at) VALUES ('customer', '+919876543210', '" + hash + "', -1, now())", checkViolation, "otp_requests_attempts_check"},
		{"an unknown actor type", "INSERT INTO devices (actor_type, actor_id, platform, push_token) VALUES ('admin', '" + actorA + "', 'android', 'tok2')", checkViolation, "devices_actor_type_check"},
		{"an unknown platform", "INSERT INTO devices (actor_type, actor_id, platform, push_token) VALUES ('rider', '" + actorA + "', 'web', 'tok3')", checkViolation, "devices_platform_check"},
		{"a duplicate push token", "INSERT INTO devices (actor_type, actor_id, platform, push_token) VALUES ('rider', '" + actorA + "', 'ios', 'tok1')", uniqueViolation, "devices_push_token_key"},
		{"an unknown verification status", "INSERT INTO rider_verifications (rider_id, status, method) VALUES ('" + actorA + "', 'maybe', 'digilocker')", checkViolation, "rider_verifications_status_check"},
		{"a verification for no rider", "INSERT INTO rider_verifications (rider_id, status, method) VALUES ('" + actorB + "', 'pending', 'digilocker')", foreignKeyMismatch, "rider_verifications_rider_id_fkey"},
		{"a short idempotency key", idem + "'short', 'h', NULL, NULL, now())", checkViolation, "idempotency_keys_key_check"},
		{"a stored non-2xx reply", idem + "'long-enough-key', 'h', 409, '{}', NULL)", checkViolation, "idempotency_keys_status_check"},
		{"a status with no reply", idem + "'long-enough-key', 'h', 200, NULL, NULL)", checkViolation, "idempotency_keys_reply_complete"},
		{"a running request holding its key forever", idem + "'long-enough-key', 'h', NULL, NULL, NULL)", checkViolation, "idempotency_keys_lock_expires"},
	}
	for _, c := range cases {
		refused(t, db, c.name, c.stmt, c.code, c.constraint)
	}
}

func TestRefreshTokenChainRules(t *testing.T) {
	db, p := freshDB(t)
	up(t, p)
	insert := func(id, actor, chain string, n int, extra, extraValues string) string {
		return fmt.Sprintf("INSERT INTO refresh_tokens (id, actor_type, actor_id, chain_id, token_hash, expires_at%s) VALUES ('%s', 'rider', '%s', '%s', '%s', now()%s)",
			extra, id, actor, chain, strings.Repeat(fmt.Sprint(n), 64), extraValues)
	}
	id := func(n int) string { return fmt.Sprintf("%08d-0000-0000-0000-000000000000", n) }
	mustExec(t, db, insert(id(1), actorA, actorA, 1, "", ""))
	mustExec(t, db, insert(id(2), actorA, actorA, 2, ", used_at, replaced_by", ", now(), '"+id(1)+"'"))

	refused(t, db, "a successor on an unused token",
		insert(id(3), actorA, actorA, 3, ", replaced_by", ", '"+id(1)+"'"),
		checkViolation, "refresh_tokens_used_when_replaced")
	refused(t, db, "two tokens with one successor",
		insert(id(4), actorA, actorA, 4, ", used_at, replaced_by", ", now(), '"+id(1)+"'"),
		uniqueViolation, "refresh_tokens_one_predecessor_idx")
	mustExec(t, db, insert(id(7), actorA, actorA, 7, "", "")) // No predecessor yet, so only the chain rule can refuse.
	refused(t, db, "a successor from another chain",
		insert(id(5), actorA, actorB, 5, ", used_at, replaced_by", ", now(), '"+id(7)+"'"),
		foreignKeyMismatch, "refresh_tokens_successor_same_chain")
	refused(t, db, "a successor from another user",
		insert(id(6), actorB, actorA, 6, ", used_at, replaced_by", ", now(), '"+id(7)+"'"),
		foreignKeyMismatch, "refresh_tokens_successor_same_chain")

	// Deleting a successor clears only the link, not who the token belongs to.
	mustExec(t, db, "DELETE FROM refresh_tokens WHERE id = '"+id(1)+"'")
	var actor string
	var replaced sql.NullString
	if err := db.QueryRow("SELECT actor_id, replaced_by FROM refresh_tokens WHERE id = '"+id(2)+"'").Scan(&actor, &replaced); err != nil {
		t.Fatal(err)
	}
	if actor != actorA || replaced.Valid {
		t.Errorf("after deleting the successor: actor %s, replaced_by %v", actor, replaced)
	}
}

func TestOneIdempotencyKeyPerAction(t *testing.T) {
	db, p := freshDB(t)
	up(t, p)
	stmt := func(actor, path string) string {
		return fmt.Sprintf("INSERT INTO idempotency_keys (actor_id, method, path, key, request_hash, locked_until) VALUES ('%s', 'POST', '%s', 'long-enough-key', 'h', now())", actor, path)
	}
	mustExec(t, db, stmt(actorA, "/orders"))
	refused(t, db, "the same key on the same path", stmt(actorA, "/orders"), uniqueViolation, "idempotency_keys_pkey")
	mustExec(t, db, stmt(actorA, "/gates"))
	mustExec(t, db, stmt(actorB, "/orders"))
}

func TestOneOpenTaskOfAKind(t *testing.T) {
	db, p := freshDB(t)
	up(t, p)
	const index = "scheduled_tasks_one_open_idx"
	for name, insert := range map[string]string{
		"a task for a record":   "INSERT INTO scheduled_tasks (kind, entity_id, run_at) VALUES ('offer_expires', '" + actorA + "', now())",
		"a task with no record": "INSERT INTO scheduled_tasks (kind, run_at) VALUES ('create_ping_partitions', now())",
	} {
		mustExec(t, db, insert)
		refused(t, db, "a second open copy of "+name, insert, uniqueViolation, index)
		mustExec(t, db, "UPDATE scheduled_tasks SET done_at = now() WHERE done_at IS NULL")
		mustExec(t, db, insert)
		mustExec(t, db, "UPDATE scheduled_tasks SET failed_at = now() WHERE done_at IS NULL")
		mustExec(t, db, insert)
		refused(t, db, "another copy of "+name, insert, uniqueViolation, index)
	}
}

func TestAuditEventsAreAppendOnly(t *testing.T) {
	db, p := freshDB(t)
	up(t, p)
	mustExec(t, db, "INSERT INTO audit_events (actor_type, action, entity_type, entity_id) VALUES ('rider', 'test', 'order', 'x')")
	for _, stmt := range []string{"UPDATE audit_events SET action = 'changed'", "DELETE FROM audit_events", "TRUNCATE audit_events"} {
		refused(t, db, stmt, stmt, appendOnlyRefused, "")
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM audit_events").Scan(&count); err != nil || count != 1 {
		t.Errorf("rows: %d, %v", count, err)
	}
}
