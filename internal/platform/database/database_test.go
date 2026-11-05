package database

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/dbtarget"
)

// localURL refuses anything but this machine, so these tests can never touch Supabase.
func localURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_DB_TESTS") == "yes" {
			t.Fatal("TEST_DATABASE_URL is required here")
		}
		t.Skip("TEST_DATABASE_URL is not set. Run: task test:db")
	}
	target, err := dbtarget.Parse(url)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.CheckLocal(); err != nil {
		t.Fatal(err)
	}
	return url
}

func open(t *testing.T, maxConns int32) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := Open(ctx, localURL(t), maxConns)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestOpenConnects(t *testing.T) {
	pool := open(t, 3)
	if err := Ping(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	if got := pool.Config().MaxConns; got != 3 {
		t.Errorf("max connections: %d", got)
	}
}

func TestOpenGivesUpWithoutShowingThePassword(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	// Nothing listens on this port.
	_, err := Open(ctx, "postgres://ninzo:S3CRET@127.0.0.1:1/ninzo?sslmode=disable", 1)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "S3CRET") {
		t.Errorf("the error shows the password: %v", err)
	}
}

func TestOpenRefusesABadURL(t *testing.T) {
	_, err := Open(context.Background(), "://S3CRET", 1)
	if err == nil || strings.Contains(err.Error(), "S3CRET") {
		t.Errorf("got %v", err)
	}
}

// exec runs admin SQL for a test role, and removes the role afterwards.
func tempRole(t *testing.T, pool *pgxpool.Pool, role, options string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "CREATE ROLE "+role+" NOLOGIN "+options); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, "DROP ROLE "+role); err != nil {
			t.Errorf("drop role: %v", err)
		}
	})
}

func TestVerifyRole(t *testing.T) {
	ctx := context.Background()
	admin := open(t, 2)
	tempRole(t, admin, "ninzo_test_good", "BYPASSRLS")
	tempRole(t, admin, "ninzo_test_norls", "")
	tempRole(t, admin, "ninzo_test_strong", "BYPASSRLS CREATEDB")

	// One connection, so SET ROLE applies to the check that follows.
	cfg := admin.Config().Copy()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, c := range []struct{ role, want, problem string }{
		{"ninzo_test_good", "ninzo_test_good", ""},
		{"ninzo_test_good", "ninzo_service", "services must use"},
		{"ninzo_test_norls", "ninzo_test_norls", "cannot bypass"},
		{"ninzo_test_strong", "ninzo_test_strong", "more power"},
	} {
		if _, err := pool.Exec(ctx, "SET ROLE "+c.role); err != nil {
			t.Fatal(err)
		}
		err := VerifyRole(ctx, pool, c.want)
		if _, resetErr := pool.Exec(ctx, "RESET ROLE"); resetErr != nil {
			t.Fatal(resetErr)
		}
		switch {
		case c.problem == "" && err != nil:
			t.Errorf("%s: %v", c.role, err)
		case c.problem != "" && (err == nil || !strings.Contains(err.Error(), c.problem)):
			t.Errorf("%s as %s: want %q, got %v", c.role, c.want, c.problem, err)
		}
	}
}
