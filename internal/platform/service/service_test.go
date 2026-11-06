package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/dbtarget"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/config"
)

const testSigningKey = config.LocalKeyPrefix + "key-for-tests-long-enough"

func TestRunRefusesBadSettingsBeforeConnecting(t *testing.T) {
	err := Run(context.Background(), Options{Name: "test", Getenv: func(k string) string {
		if k == "DATABASE_URL" {
			return "postgres://postgres." + dbtarget.NinzoRef + ":S3CRET@nonexistent.pooler.supabase.com:5432/postgres?sslmode=require"
		}
		if k == "APP_ENV" {
			return "local"
		}
		if k == "JWT_SIGNING_KEY" {
			return testSigningKey
		}
		if k == "OTP_TEST_MODE" {
			return "true"
		}
		if k == "OTP_TEST_CODE" {
			return "123456"
		}
		return ""
	}})
	if err == nil || !strings.Contains(err.Error(), "local Postgres only") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "S3CRET") {
		t.Errorf("the error shows the password: %v", err)
	}
}

func TestRunStopsIfAValidationFails(t *testing.T) {
	refused := errors.New("not this time")
	err := Run(context.Background(), Options{
		Name: "test", Validate: func(config.Config) error { return refused },
		Getenv: func(k string) string {
			return map[string]string{"APP_ENV": "local", "DATABASE_URL": "postgres://u:p@127.0.0.1:1/d?sslmode=disable", "JWT_SIGNING_KEY": testSigningKey}[k]
		},
	})
	if !errors.Is(err, refused) {
		t.Errorf("got %v", err)
	}
}

func TestRunGivesUpIfTheDatabaseStaysDown(t *testing.T) {
	var logs bytes.Buffer
	err := Run(context.Background(), Options{
		Name: "test", LogOutput: &logs, StartupTimeout: time.Second,
		Getenv: func(k string) string {
			return map[string]string{"APP_ENV": "local", "DATABASE_URL": "postgres://ninzo:S3CRET@127.0.0.1:1/ninzo?sslmode=disable", "JWT_SIGNING_KEY": testSigningKey, "OTP_TEST_MODE": "true", "OTP_TEST_CODE": "123456"}[k]
		},
	})
	if err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "S3CRET") || strings.Contains(logs.String(), "S3CRET") {
		t.Errorf("the password leaked: %v, %s", err, logs.String())
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestRunEndToEnd starts a whole API on local Postgres, and stops it.
func TestRunEndToEnd(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_DB_TESTS") == "yes" {
			t.Fatal("TEST_DATABASE_URL is required here")
		}
		t.Skip("TEST_DATABASE_URL is not set. Run: task test:db")
	}
	port := freePort(t)
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Name: "test-api", LogOutput: &logs,
			Getenv: func(k string) string {
				return map[string]string{"APP_ENV": "local", "DATABASE_URL": url, "HTTP_PORT": fmt.Sprint(port), "DB_MAX_CONNS": "2", "JWT_SIGNING_KEY": testSigningKey, "OTP_TEST_MODE": "true", "OTP_TEST_CODE": "123456"}[k]
			},
			Mount: func(r chi.Router, d Deps) {
				r.Get("/now", func(w http.ResponseWriter, req *http.Request) {
					var now time.Time
					if err := d.DB.QueryRow(req.Context(), "SELECT now()").Scan(&now); err != nil {
						http.Error(w, "database", 500)
						return
					}
					w.WriteHeader(204)
				})
			},
		})
	}()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	status := func(path string) int {
		resp, err := http.Get(base + path)
		if err != nil {
			return 0
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	// Stop at once if Run ended early, such as when another program took the free port.
	for i := 0; i < 100 && status("/readyz") != 200; i++ {
		select {
		case err := <-done:
			t.Fatalf("the service stopped early: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if got := status("/readyz"); got != 200 {
		t.Fatalf("readyz: %d", got)
	}
	if got := status("/healthz"); got != 200 {
		t.Errorf("healthz: %d", got)
	}
	if got := status("/v1/now"); got != 204 {
		t.Errorf("a mounted endpoint that uses the database: %d", got)
	}
	if got := status("/v1/none"); got != 404 {
		t.Errorf("an unknown endpoint: %d", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a clean stop returned %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the service did not stop")
	}
	out := logs.String()
	if !strings.Contains(out, `"service":"test-api"`) || !strings.Contains(out, "shutting down") {
		t.Errorf("logs: %s", out)
	}
	if strings.Contains(out, "ninzo_local") {
		t.Errorf("the logs hold the password: %s", out)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !json.Valid([]byte(line)) {
			t.Errorf("a log line is not JSON: %s", line)
		}
	}
}

func TestConnectChecksTheLoginOutsideLocal(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_DB_TESTS") == "yes" {
			t.Fatal("TEST_DATABASE_URL is required here")
		}
		t.Skip("TEST_DATABASE_URL is not set. Run: task test:db")
	}
	ctx := context.Background()
	pool, err := connect(ctx, config.Config{Env: config.EnvLocal, DatabaseURL: config.Secret(url), DBMaxConns: 1})
	if err != nil {
		t.Fatalf("local: %v", err)
	}
	pool.Close()
	// The local login is not ninzo_service, so these must refuse it.
	for _, env := range []string{config.EnvStaging, config.EnvProduction} {
		_, err := connect(ctx, config.Config{Env: env, DatabaseURL: config.Secret(url), DBMaxConns: 1})
		if err == nil || !strings.Contains(err.Error(), "services must use") {
			t.Errorf("%s: got %v", env, err)
		}
	}
}
