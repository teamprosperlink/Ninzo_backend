package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/teamprosperlink/Ninzo_backend/internal/console"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
)

func env(values map[string]string) func(string) string {
	return func(k string) string { return values[k] }
}

func TestAddingTheFirstAdminAndResetting(t *testing.T) {
	pool := dbtest.Fresh(t)
	getenv := env(map[string]string{"DATABASE_URL": pool.Config().ConnString()})
	for _, args := range [][]string{nil, {"-local"}, {"-local", "add-admin"}, {"-local", "add-admin", "-login", "asha.k"}, {"-local", "reset", "-login", "asha.k"},
		{"-staging", "add-admin", "-login", "asha.k", "-name", "Asha"}} {
		if err := run(context.Background(), args, getenv, &bytes.Buffer{}); err == nil {
			t.Errorf("%v ran", args)
		}
	}
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-local", "add-admin", "-login", "asha.k", "-name", "Asha K"}, getenv, &out); err != nil {
		t.Fatal(err)
	}
	_, temp, found := strings.Cut(strings.TrimSpace(out.String()), "shown once: ")
	if !found || len(temp) != 20 {
		t.Fatalf("%q", out.String())
	}
	var hash, role string
	var must bool
	if err := pool.QueryRow(context.Background(), "SELECT password_hash, role, must_change_password FROM console_users WHERE login_id = 'asha.k'").Scan(&hash, &role, &must); err != nil {
		t.Fatal(err)
	}
	if !console.CheckPassword(hash, temp) || role != "admin" || !must {
		t.Error("the first admin must have the printed temporary password and change it")
	}
	if err := run(context.Background(), []string{"-local", "add-admin", "-login", "asha.k", "-name", "Again"}, getenv, &bytes.Buffer{}); err == nil {
		t.Error("the same login twice")
	}
	pool.Exec(context.Background(), "UPDATE console_users SET locked_until = now() + interval '1 hour'")
	out.Reset()
	if err := run(context.Background(), []string{"-local", "reset", "-login", "asha.k", "-reason", "locked out"}, getenv, &out); err != nil {
		t.Fatal(err)
	}
	var locked bool
	pool.QueryRow(context.Background(), "SELECT locked_until IS NOT NULL FROM console_users WHERE login_id = 'asha.k'").Scan(&locked)
	if locked || !strings.Contains(out.String(), "shown once") {
		t.Error("a reset must clear the lock and print a new password")
	}
	if err := run(context.Background(), []string{"-remote", "reset", "-login", "asha.k", "-reason", "x y z"}, getenv, &bytes.Buffer{}); err == nil {
		t.Error("-remote ran against local Postgres")
	}
}
