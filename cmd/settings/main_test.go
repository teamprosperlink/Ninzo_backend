package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
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
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_DB_TESTS") == "yes" {
			t.Fatal("TEST_DATABASE_URL is required here")
		}
		t.Skip("TEST_DATABASE_URL is not set")
	}
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-local", "show"}, env(map[string]string{"DATABASE_URL": url}), &out); err != nil {
		t.Skipf("the local database has no migrations applied: %v", err)
	}
	if !strings.Contains(out.String(), "timer_length_s") {
		t.Errorf("%s", out.String())
	}
}
