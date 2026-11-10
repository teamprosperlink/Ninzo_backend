package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
)

func TestTestStaffAreAddedOnceAndNeverTouchARealPerson(t *testing.T) {
	pool := dbtest.Fresh(t)
	url := pool.Config().ConnString()
	do := func(what string) error {
		return run(context.Background(), []string{"-local", what}, env(map[string]string{"DATABASE_URL": url}), &bytes.Buffer{})
	}
	if err := do("staff"); err == nil {
		t.Error("staff ran with no test hub")
	}
	for _, what := range []string{"zones", "menu", "hubs", "staff", "staff"} {
		if err := do(what); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	var leads, staff int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE role = 'hub_lead'), count(*) FILTER (WHERE role = 'hub_staff')
		FROM hub_staff s JOIN hubs h ON h.id = s.hub_id WHERE s.is_test AND h.is_test`).Scan(&leads, &staff); err != nil {
		t.Fatal(err)
	}
	if leads != 1 || staff != 1 {
		t.Errorf("%d leads and %d staff, want one of each", leads, staff)
	}
	// A real customer with a test number is never made hub staff.
	pool.Exec(context.Background(), "DELETE FROM hub_staff")
	pool.Exec(context.Background(), "INSERT INTO customers (person_id) SELECT id FROM people WHERE phone = '+919800000091'")
	if err := do("staff"); err == nil {
		t.Error("test data made a real customer hub staff")
	}
}
