package profile

import (
	"testing"
	"time"
)

// Issue 158: the rider shows when the account was made, and when it was approved.
func TestARiderShowsCreationAndApprovalTimes(t *testing.T) {
	f := newFixture(t)
	id, token := f.newRider("+919800000158")
	read := func() map[string]any {
		t.Helper()
		code, out := f.send(f.rider, "GET", "/riders/me", token, "")
		if code != 200 {
			t.Fatalf("%d %v", code, out)
		}
		r := out["rider"].(map[string]any)
		checkFields(t, r, "rider", "Rider")
		return r
	}
	r := read()
	created, err := time.Parse(time.RFC3339Nano, r["created_at"].(string))
	if err != nil || time.Since(created) > time.Minute {
		t.Errorf("created_at %v %v", r["created_at"], err)
	}
	if v, ok := r["approved_at"]; !ok || v != nil {
		t.Errorf("before approval approved_at must be null: %v", r)
	}
	// Another rider's approval and an unrelated event do not count.
	f.exec(`INSERT INTO audit_events (actor_type, action, entity_type, entity_id, created_at)
		VALUES ('system', 'rider.approved', 'rider', gen_random_uuid()::text, now()),
		       ('system', 'rider.suspend', 'rider', $1, now())`, id)
	if r = read(); r["approved_at"] != nil {
		t.Errorf("only this rider's approval counts: %v", r)
	}
	// The console writes rider.approve, the command line rider.approved. The newest counts.
	f.exec(`INSERT INTO audit_events (actor_type, action, entity_type, entity_id, created_at)
		VALUES ('system', 'rider.approved', 'rider', $1, '2026-11-01T10:00:00Z'),
		       ('console', 'rider.approve', 'rider', $1, '2026-11-05T12:30:00Z')`, id)
	r = read()
	approved, err := time.Parse(time.RFC3339Nano, r["approved_at"].(string))
	if err != nil || !approved.Equal(time.Date(2026, 11, 5, 12, 30, 0, 0, time.UTC)) {
		t.Errorf("approved_at %v %v", r["approved_at"], err)
	}
}
