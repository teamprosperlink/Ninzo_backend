package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
)

func TestTestersAddRidersAndScootersOnceAndNeverTouchARealPerson(t *testing.T) {
	pool := dbtest.Fresh(t)
	ctx := context.Background()
	url := pool.Config().ConnString()
	do := func(what string) error {
		return run(ctx, []string{"-local", what}, env(map[string]string{"DATABASE_URL": url}), &bytes.Buffer{})
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := do("testers"); err == nil {
		t.Error("testers ran with no test zones")
	}
	for _, what := range []string{"zones", "menu", "fleet", "rider", "testers", "testers"} {
		if err := do(what); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	// Four scooters at each test zone's node: fleet's two and the two extra, each once.
	for _, z := range testZones {
		if n := count(`SELECT count(*) FROM scooters s JOIN zones z ON z.id = s.zone_id JOIN staging_nodes n ON n.id = s.node_id
			WHERE z.name = $1 AND s.is_test AND n.zone_id = z.id`, z.name); n != 2+extraScooters {
			t.Errorf("%s has %d scooters, want %d", z.name, n, 2+extraScooters)
		}
	}
	// Every extra rider is active, verified, in the first test zone, and approved once in the audit log.
	for _, tr := range extraRiders {
		if n := count(`SELECT count(*) FROM riders r JOIN people p ON p.id = r.person_id
			WHERE p.phone = $1 AND r.status = 'active' AND r.name = $2
				AND r.zone_id = (SELECT id FROM zones WHERE is_test AND area IS NOT NULL ORDER BY name LIMIT 1)
				AND EXISTS (SELECT 1 FROM rider_verifications v WHERE v.rider_id = r.id AND v.status = 'verified')`, tr.phone, tr.name); n != 1 {
			t.Errorf("%s: %d ready riders, want 1", tr.phone, n)
		}
	}
	if n := count("SELECT count(*) FROM audit_events WHERE action = 'rider.approved' AND reason = 'test data'"); n != 1+len(extraRiders) {
		t.Errorf("%d approvals in the audit log, want %d", n, 1+len(extraRiders))
	}
	if n := count("SELECT count(*) FROM riders"); n != 1+len(extraRiders) {
		t.Errorf("%d riders, want %d", n, 1+len(extraRiders))
	}

	// A real customer with one of the numbers is never made a rider.
	if _, err := pool.Exec(ctx, "INSERT INTO people (phone) VALUES ('+919800000084')"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO customers (person_id) SELECT id FROM people WHERE phone = '+919800000084'"); err != nil {
		t.Fatal(err)
	}
	saved := extraRiders
	t.Cleanup(func() { extraRiders = saved })
	extraRiders = append(append([]struct{ phone, name string }{}, saved...), struct{ phone, name string }{"+919800000084", "Test Rider 5"})
	if err := do("testers"); err == nil {
		t.Error("test data made a real customer a rider")
	}
	if n := count("SELECT count(*) FROM riders r JOIN people p ON p.id = r.person_id WHERE p.phone = '+919800000084'"); n != 0 {
		t.Error("the real customer became a rider")
	}
}
