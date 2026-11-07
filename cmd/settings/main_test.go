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
	for _, args := range [][]string{
		nil, {"show"}, {"-local"}, {"-local", "delete"}, {"-staging", "show"},
		{"-local", "set"}, {"-local", "set", "-reason", "x", "-by", "y", "nope=1"}, {"-local", "show", "a", "b"},
	} {
		if err := run(context.Background(), args, env(map[string]string{"DATABASE_URL": "postgres://u:p@localhost/db"}), &bytes.Buffer{}); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
}

func TestEachModeRefusesTheOtherTarget(t *testing.T) {
	remote := "postgres://postgres.chyejfgsipcsdxnginei:secret-pass@aws-0-ap-south-1.pooler.supabase.com:5432/postgres"
	err := run(context.Background(), []string{"-local", "show"}, env(map[string]string{"DATABASE_URL": remote}), &bytes.Buffer{})
	if err == nil {
		t.Error("-local would have touched Supabase")
	} else if strings.Contains(err.Error(), "secret-pass") {
		t.Error("the error shows the password")
	}
	local := "postgres://ninzo:ninzo_local@localhost:5432/ninzo?sslmode=disable"
	if err := run(context.Background(), []string{"-remote", "show"}, env(map[string]string{"DATABASE_URL": local}), &bytes.Buffer{}); err == nil {
		t.Error("-remote accepted a local database")
	}
}

func TestShowAndSetLocally(t *testing.T) {
	pool := dbtest.Fresh(t)
	url := pool.Config().ConnString()
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-local", "show"}, env(map[string]string{"DATABASE_URL": url}), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "timer_length_s") || !strings.Contains(out.String(), "global version 1") {
		t.Errorf("%s", out.String())
	}
	out.Reset()
	args := []string{"-local", "set", "-reason", "a reason with spaces", "-by", "tester", "timer_length_s=100"}
	if err := run(context.Background(), args, env(map[string]string{"DATABASE_URL": url}), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "stored version 2") || !strings.Contains(out.String(), "100 seconds") {
		t.Errorf("%s", out.String())
	}
	var reason string
	if err := pool.QueryRow(context.Background(), "SELECT reason FROM audit_events WHERE action = 'settings.changed'").Scan(&reason); err != nil || reason != "tester: a reason with spaces" {
		t.Errorf("%q %v", reason, err)
	}
	if err := run(context.Background(), []string{"-local", "set", "-reason", "bad", "-by", "tester", "arrival_perimeter_m=500"}, env(map[string]string{"DATABASE_URL": url}), &bytes.Buffer{}); err == nil {
		t.Error("a contradiction was stored")
	}
}
