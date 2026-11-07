package database

import (
	"context"
	"io"
	"net"
	neturl "net/url"
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

func TestOpenStopsAtOnceOnAPermanentRefusal(t *testing.T) {
	good := localURL(t)
	for name, swap := range map[string][2]string{
		"an unknown database": {"/ninzo?", "/ninzo_no_such_database?"},
		"a wrong password":    {":ninzo_local@", ":S3CRET@"},
	} {
		t.Run(name, func(t *testing.T) {
			url := strings.Replace(good, swap[0], swap[1], 1)
			if url == good {
				t.Fatal("the URL did not change, so this test would prove nothing")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			start := time.Now()
			_, err := Open(ctx, url, 1)
			if err == nil || !strings.Contains(err.Error(), "refused the connection") {
				t.Fatalf("got %v", err)
			}
			if took := time.Since(start); took > 15*time.Second {
				t.Errorf("it kept retrying for %s", took)
			}
			if strings.Contains(err.Error(), "S3CRET") {
				t.Errorf("the error shows the password: %v", err)
			}
		})
	}
}

// tempRole makes a test role, and removes the role afterwards.
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

// slowProxy holds each new connection for a while before passing it on, like a far-away database.
func slowProxy(t *testing.T, target string, delay time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer client.Close()
				time.Sleep(delay)
				server, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer server.Close()
				go func() { _, _ = io.Copy(server, client) }()
				_, _ = io.Copy(client, server)
			}()
		}
	}()
	return ln.Addr().String()
}

func TestAFarAwayDatabaseStillConnects(t *testing.T) {
	u, err := neturl.Parse(localURL(t))
	if err != nil {
		t.Fatal(err)
	}
	u.Host = slowProxy(t, u.Host, 3*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := Open(ctx, u.String(), 2)
	if err != nil {
		t.Fatalf("a connection that takes 3 seconds must not stop the API starting: %v", err)
	}
	pool.Close()
}

func TestAPingOutlastsANewConnection(t *testing.T) {
	if PingTimeout <= connectTimeout {
		t.Errorf("ping %s, connect %s: a ping could never finish making a connection", PingTimeout, connectTimeout)
	}
}
