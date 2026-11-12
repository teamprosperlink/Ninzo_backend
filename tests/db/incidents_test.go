package db

import (
	"context"
	"slices"
	"testing"
)

// TestMigration0044FillsTheQueueFromRowsThatAlreadyExist is what staging does: the database
// holds sources from before the migration, and applying it must open one incident for each that
// still needs a person, and none for the rest.
func TestMigration0044FillsTheQueueFromRowsThatAlreadyExist(t *testing.T) {
	db, p := freshDB(t)
	ctx := context.Background()
	if _, err := p.UpTo(ctx, 43); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "INSERT INTO people (id, phone) VALUES ('"+personA+"', '+919800000001')")
	mustExec(t, db, "INSERT INTO customers (person_id) VALUES ('"+personA+"')")
	mustExec(t, db, `INSERT INTO support_tickets (customer_id, type, status) SELECT id, 'other', 'open' FROM customers`)
	mustExec(t, db, `INSERT INTO support_tickets (customer_id, type, status) SELECT id, 'other', 'resolved' FROM customers`)
	mustExec(t, db, `INSERT INTO problem_reports (type, source, outcome) VALUES ('breakdown', 'rider', 'none'), ('signal_lost', 'system', 'support_alerted')`)
	mustExec(t, db, `INSERT INTO outbox_events (topic, payload, delivered_at) VALUES
		('support.alert', '{"reason":"handover_wrong_otp","order_id":"not-an-id"}', NULL),
		('support.alert', '{"reason":"handover_wrong_otp"}', now()),
		('support.alert', '[]', NULL),
		('push.rider', '{"kind":"offer"}', NULL)`)
	up(t, p)

	rows, err := db.Query("SELECT kind, status, order_id IS NULL FROM incidents ORDER BY kind, opened_at")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var kind, status string
		var noOrder bool
		if err := rows.Scan(&kind, &status, &noOrder); err != nil {
			t.Fatal(err)
		}
		if status != "open" || !noOrder {
			t.Errorf("%s: %s, no order %v", kind, status, noOrder)
		}
		got = append(got, kind)
	}
	want := []string{"problem_report", "signal_lost", "support_alert", "support_alert", "ticket"}
	if !slices.Equal(got, want) {
		t.Errorf("incidents after the migration: %v want %v", got, want)
	}
	// And the source tables work as before afterwards.
	mustExec(t, db, `INSERT INTO problem_reports (type, source, outcome) VALUES ('accident', 'rider', 'none')`)
	var n int
	if err := db.QueryRow("SELECT count(*) FROM incidents").Scan(&n); err != nil || n != 6 {
		t.Errorf("a new report opens an incident: %d %v", n, err)
	}
	// An incident is one of three statuses, and an assignee goes with the assigned status.
	refused(t, db, "assigned with nobody", "UPDATE incidents SET status = 'assigned'", checkViolation, "incidents_assigned_together")
	refused(t, db, "closed without a time", "UPDATE incidents SET status = 'closed'", checkViolation, "incidents_closed_together")
}

// TestMigration0044SettlesWhatIsOld: before 0044 nothing marked reports or alerts handled, so the
// migration settles those over a day old with no order (and those of ended orders) instead of
// flooding the queue (#148 review).
func TestMigration0044SettlesWhatIsOld(t *testing.T) {
	db, p := freshDB(t)
	ctx := context.Background()
	if _, err := p.UpTo(ctx, 43); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO problem_reports (type, source, outcome, created_at) VALUES ('breakdown', 'rider', 'none', now() - interval '2 days'),
		('signal_lost', 'system', 'support_alerted', now() - interval '2 days'), ('accident', 'rider', 'none', now())`)
	mustExec(t, db, `INSERT INTO outbox_events (topic, payload, created_at) VALUES ('support.alert', '{"reason":"handover_wrong_otp"}', now() - interval '2 days'),
		('support.alert', '{"reason":"handover_wrong_otp"}', now())`)
	up(t, p)
	var incidents, openReports, openAlerts int
	if err := db.QueryRow(`SELECT (SELECT count(*) FROM incidents), (SELECT count(*) FROM problem_reports WHERE handled_at IS NULL),
		(SELECT count(*) FROM outbox_events WHERE topic = 'support.alert' AND delivered_at IS NULL)`).Scan(&incidents, &openReports, &openAlerts); err != nil {
		t.Fatal(err)
	}
	if incidents != 2 || openReports != 1 || openAlerts != 1 {
		t.Errorf("%d incidents, %d open reports, %d open alerts; want 2, 1 and 1 (only today's)", incidents, openReports, openAlerts)
	}
}
