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
	ok := []string{"-local", "approve", "-phone", "+919800000001", "-zone", "z", "-reason", "checked", "-by", "owner"}
	for _, args := range [][]string{
		nil, {"-local"}, {"-local", "remove"}, ok[:4], {"-local", "approve", "-phone", "98000", "-zone", "z", "-reason", "checked", "-by", "o"},
		{"-local", "approve", "-phone", "+919800000001", "-zone", "z", "-reason", "x", "-by", "o"}, append(append([]string{}, ok...), "extra"),
	} {
		if err := run(context.Background(), args, env(map[string]string{"DATABASE_URL": "postgres://u:p@localhost/db"}), &bytes.Buffer{}); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
	remote := "postgres://postgres.chyejfgsipcsdxnginei:secret-pass@aws-0-ap-south-1.pooler.supabase.com:5432/postgres"
	if err := run(context.Background(), ok, env(map[string]string{"DATABASE_URL": remote}), &bytes.Buffer{}); err == nil || strings.Contains(err.Error(), "secret-pass") {
		t.Errorf("-local touched Supabase, or showed the password: %v", err)
	}
}

func TestApprovingARider(t *testing.T) {
	pool := dbtest.Fresh(t)
	ctx := context.Background()
	url := pool.Config().ConnString()
	var zone, person, rider string
	if err := pool.QueryRow(ctx, "INSERT INTO zones (name, city) VALUES ('Z', 'Bengaluru') RETURNING id::text").Scan(&zone); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO people (phone) VALUES ('+919800000001') RETURNING id::text").Scan(&person); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO riders (person_id) VALUES ($1) RETURNING id::text", person).Scan(&rider); err != nil {
		t.Fatal(err)
	}
	args := []string{"-local", "approve", "-phone", "+919800000001", "-zone", zone, "-reason", "documents checked", "-by", "owner"}
	if err := run(ctx, args, env(map[string]string{"DATABASE_URL": url}), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var status, z, v string
	if err := pool.QueryRow(ctx, `SELECT r.status, r.zone_id::text, (SELECT status FROM rider_verifications WHERE rider_id = r.id)
		FROM riders r WHERE r.id = $1`, rider).Scan(&status, &z, &v); err != nil || status != "active" || z != zone || v != "verified" {
		t.Errorf("%s %s %s %v", status, z, v, err)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE action = 'rider.approved' AND reason = 'owner: documents checked'").Scan(&n); err != nil || n != 1 {
		t.Errorf("%d %v", n, err)
	}
	for name, a := range map[string][]string{
		"unknown phone": {"-local", "approve", "-phone", "+919800000002", "-zone", zone, "-reason", "checked", "-by", "owner"},
		"unknown zone":  {"-local", "approve", "-phone", "+919800000001", "-zone", "00000000-0000-0000-0000-000000000000", "-reason", "checked", "-by", "owner"},
	} {
		if err := run(ctx, a, env(map[string]string{"DATABASE_URL": url}), &bytes.Buffer{}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := pool.Exec(ctx, "UPDATE riders SET status = 'suspended'"); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, args, env(map[string]string{"DATABASE_URL": url}), &bytes.Buffer{}); err == nil {
		t.Error("approving undid a suspension")
	}
}
