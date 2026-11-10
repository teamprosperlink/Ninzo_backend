package restock

import (
	"context"
	"testing"
	"time"
)

// The real jobs draft suggestions and expire cassettes, but never act for hub staff.
func TestTheJobsNeverPlayTheHub(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.hubLot(f.other, "HUB-J-1", f.chai, 10, time.Hour)
	id := f.request(f.chai, 1)
	f.svc.Tick(ctx, time.Now().Add(10*time.Minute)) // Inside restock_accept_s, which TestARequestNoHubAcceptsExpires covers.
	if f.state(id) != "confirmed" {
		t.Errorf("the jobs acted for the hub: %s", f.state(id))
	}
	f.exec("UPDATE restock_requests SET state = 'ready', arrive_by = now() - interval '1 hour' WHERE id = $1", id)
	f.svc.Tick(ctx, time.Now())
	if f.state(id) != "expired" {
		t.Errorf("an uncollected cassette must still expire: %s", f.state(id))
	}
}

func TestARequestNoHubAcceptsExpires(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.request(f.chai, 1)
	f.svc.Tick(ctx, time.Now().Add(10*time.Minute))
	if f.state(id) != "confirmed" {
		t.Fatalf("expired before the limit: %s", f.state(id))
	}
	before := f.count("SELECT coalesce((SELECT seq::int FROM rider_state WHERE rider_id = $1), 0)", f.rider)
	f.svc.Tick(ctx, time.Now().Add(16*time.Minute))
	if f.state(id) != "expired" {
		t.Errorf("a request nobody accepted must expire after 15 minutes: %s", f.state(id))
	}
	if f.count("SELECT coalesce((SELECT seq::int FROM rider_state WHERE rider_id = $1), 0)", f.rider) <= before {
		t.Error("the rider was not told")
	}
	// The scooter can ask again.
	if again := f.request(f.chai, 1); f.state(again) != "confirmed" {
		t.Error("the scooter is still blocked")
	}
}

func TestASuggestionPrefersAStaffedHub(t *testing.T) {
	f := newFixture(t)
	person := f.id("INSERT INTO people (phone) VALUES ('+919800000177') RETURNING id::text")
	f.exec("INSERT INTO hub_staff (person_id, hub_id, role) VALUES ($1, $2, 'hub_lead')", person, f.hub) // Hub A, the farther one.
	f.hubLot(f.hub, "HUB-ST-1", f.chai, 10, time.Hour)
	f.hubLot(f.other, "HUB-ST-2", f.chai, 10, time.Hour)
	f.svc.Suggest(context.Background(), time.Now())
	if f.id("SELECT hub_id::text FROM restock_requests WHERE state = 'suggested'") != f.hub {
		t.Error("a suggestion must go to a hub with staff before a nearer one without")
	}
}
