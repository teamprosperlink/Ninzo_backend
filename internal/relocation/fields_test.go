package relocation

import (
	"context"
	"testing"
	"time"
)

// Issue 158: a suggestion keeps the demand count behind it, as orders_waiting.
func TestASuggestionKeepsHowManyOrdersAreWaiting(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first, _ := f.rider(f.b)
	f.rider(f.b)
	f.rider(f.a)
	f.rider(f.a)
	f.orders(3, 12.9425, 77.6365, 10*time.Minute)
	now := time.Now()
	if n, err := f.svc.Check(ctx, now); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	id := f.suggestion(first)
	if got := f.count("SELECT orders_waiting FROM relocations WHERE id = $1", id); got != 3 {
		t.Errorf("stored %d, want 3", got)
	}
	cur, err := Current(ctx, f.pool, first, now)
	if err != nil || cur == nil {
		t.Fatalf("%v %v", cur, err)
	}
	if m := cur.(map[string]any); m["orders_waiting"] != 3 {
		t.Errorf("%v", m)
	}
	// A suggestion made before the count was kept shows null.
	f.exec("UPDATE relocations SET orders_waiting = NULL WHERE id = $1", id)
	cur, _ = Current(ctx, f.pool, first, now)
	if m := cur.(map[string]any); m["orders_waiting"] != nil {
		t.Errorf("an older suggestion: %v", m)
	}
}
