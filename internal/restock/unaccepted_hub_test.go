package restock

import (
	"context"
	"testing"
	"time"
)

// A confirmed request that began as a suggestion was on the hub's queue, so when no hub accepts it
// and it expires on its own, the hub's seq rises and the hub app drops it (#192).
func TestAnExpiredConfirmedSuggestionRaisesTheHubSeq(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.request(f.chai, 1)
	f.exec("UPDATE restock_requests SET source = 'suggested' WHERE id = $1", id)
	hub := f.id("SELECT hub_id::text FROM restock_requests WHERE id = $1", id)
	seq := func() int {
		return f.count("SELECT coalesce((SELECT seq FROM hub_state WHERE hub_id = $1), 0)::int", hub)
	}
	before := seq()
	f.svc.Tick(ctx, time.Now().Add(16*time.Minute))
	if f.state(id) != "expired" {
		t.Fatalf("not expired: %s", f.state(id))
	}
	if seq() <= before {
		t.Error("the hub's seq did not rise, so the hub app keeps showing the expired request")
	}
}

// A request the rider asked for raises the hub's seq once on expiry, as before (#192).
func TestAnExpiredManualRequestRaisesTheHubSeqOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.request(f.chai, 1)
	hub := f.id("SELECT hub_id::text FROM restock_requests WHERE id = $1", id)
	before := f.count("SELECT coalesce((SELECT seq FROM hub_state WHERE hub_id = $1), 0)::int", hub)
	f.svc.Tick(ctx, time.Now().Add(16*time.Minute))
	if f.state(id) != "expired" {
		t.Fatalf("not expired: %s", f.state(id))
	}
	if after := f.count("SELECT coalesce((SELECT seq FROM hub_state WHERE hub_id = $1), 0)::int", hub); after != before+1 {
		t.Errorf("hub seq %d -> %d, want one step", before, after)
	}
}
