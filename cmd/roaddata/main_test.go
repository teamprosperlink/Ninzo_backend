package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func noFile(string) ([]byte, error) { return nil, errors.New("no file read expected") }

const build = `{"name":"test-zones","source":"tools/osrm/testdata/test-zones.osm.pbf",
"source_sha256":"ec0b26bb9ef1ba6e626813baefbcc2ce996f2fc2a1676cb98255af815a822aa7","clip_box":"77.600,12.915,77.660,12.995",
"osm_extract_sha256":"ec0b26bb9ef1ba6e626813baefbcc2ce996f2fc2a1676cb98255af815a822aa7","osm_timestamp":"2026-11-11T21:22:47Z",
"profile":"scooter","profile_version":"scooter-2","profile_sha256":"33c759078421405fa5778a8126955caabf5c0e0e6952cdd4584f05bcc88b2404",
"osrm_image":"ghcr.io/project-osrm/osrm-backend:v6.0.0","algorithm":"mld","built_at":"2026-11-12T18:22:31Z"}`

func TestBadArgumentsTouchNothing(t *testing.T) {
	for _, args := range [][]string{nil, {"-local"}, {"register"}, {"-local", "register"}, {"-local", "list", "x"}, {"-staging", "list"}, {"-local", "delete"}} {
		if err := run(context.Background(), args, env(nil), noFile, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestABadFileIsRefusedBeforeAnyDatabase(t *testing.T) {
	bad := func(string) ([]byte, error) { return []byte(`{"name":"x"}`), nil }
	// No DATABASE_URL at all: the file check must come first.
	err := run(context.Background(), []string{"-local", "register", "build.json"}, env(nil), bad, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "build.json") {
		t.Fatalf("got %v", err)
	}
	huge := func(string) ([]byte, error) { return bytes.Repeat([]byte(" "), maxBuildJSON+1), nil }
	if err := run(context.Background(), []string{"-local", "register", "x"}, env(nil), huge, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("got %v", err)
	}
}

func TestEachModeRefusesTheOtherTarget(t *testing.T) {
	ok := func(string) ([]byte, error) { return []byte(build), nil }
	remote := "postgres://postgres.chyejfgsipcsdxnginei:secret-pass@aws-0-ap-south-1.pooler.supabase.com:5432/postgres"
	err := run(context.Background(), []string{"-local", "register", "b.json"}, env(map[string]string{"DATABASE_URL": remote}), ok, &bytes.Buffer{})
	if err == nil {
		t.Error("-local would have touched Supabase")
	} else if strings.Contains(err.Error(), "secret-pass") {
		t.Error("the error shows the password")
	}
	local := "postgres://ninzo:ninzo_local@localhost:5432/ninzo?sslmode=disable"
	if err := run(context.Background(), []string{"-remote", "list"}, env(map[string]string{"DATABASE_URL": local}), noFile, &bytes.Buffer{}); err == nil {
		t.Error("-remote accepted a local database")
	}
}

func TestRegisterAndListLocally(t *testing.T) {
	pool := dbtest.Fresh(t)
	url := pool.Config().ConnString()
	path := filepath.Join(t.TempDir(), "build.json")
	if err := os.WriteFile(path, []byte(build), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-local", "list"}, env(map[string]string{"DATABASE_URL": url}), os.ReadFile, &out); err != nil || !strings.Contains(out.String(), "no road data registered") {
		t.Fatalf("%s %v", out.String(), err)
	}
	out.Reset()
	if err := run(context.Background(), []string{"-local", "register", path}, env(map[string]string{"DATABASE_URL": url}), os.ReadFile, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "registered test-zones (scooter-2, mld, map of 2026-11-11T21:22:47Z)") {
		t.Fatalf("%s", out.String())
	}
	out.Reset()
	if err := run(context.Background(), []string{"-local", "register", path}, env(map[string]string{"DATABASE_URL": url}), os.ReadFile, &out); err != nil || !strings.Contains(out.String(), "already registered") {
		t.Fatalf("%s %v", out.String(), err)
	}
	out.Reset()
	if err := run(context.Background(), []string{"-local", "list"}, env(map[string]string{"DATABASE_URL": url}), os.ReadFile, &out); err != nil || strings.Count(out.String(), "test-zones") != 1 {
		t.Fatalf("%s %v", out.String(), err)
	}
}
