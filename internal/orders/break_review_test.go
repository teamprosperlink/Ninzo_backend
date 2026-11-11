package orders

import (
	"context"
	"testing"
	"time"
)

// Tests from the #159 independent review.

// The matcher holds the shift row while it decides on a rider, so a break cannot start in the
// middle and leave a rider on a break with an offer.
func TestABreakWaitsForAMatcherLookingAtTheRider(t *testing.T) {
	x := withRiders(newFixture(t))
	_, tok := x.signIn(x.scooter)
	ctx := context.Background()
	tx, err := x.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, free, err := freeRider(ctx, tx, x.scooter); err != nil || !free {
		t.Fatalf("the rider is not free: %v %v", free, err)
	}
	type result struct {
		code int
		out  map[string]any
	}
	done := make(chan result, 1)
	go func() {
		code, out := x.call("POST", "/riders/me/breaks/start", tok, "")
		done <- result{code, out}
	}()
	select {
	case r := <-done:
		t.Fatalf("the break started while the matcher held the rider: %d %v", r.code, r.out)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.code != 200 {
			t.Errorf("once the matcher let go: %d %v", r.code, r.out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the break never started")
	}
}

// When the timer is running the customer is shown the rider even before the minimum wait has
// passed, so waiting at the gate raises their seq along with the push.
func TestWaitingWhileTheTimerRunsRaisesTheCustomerSeq(t *testing.T) {
	tr := started(t, true)
	tr.at(inZone, 20*time.Second)
	tr.at(inZone, 3*time.Second)
	tr.clock = tr.clock.Add(95 * time.Second)
	tr.dispatch(tr.clock)
	if tr.state() != "TIMER_RUNNING" {
		t.Fatalf("%s", tr.state())
	}
	tr.exec("UPDATE orders SET min_wait_until = $2 WHERE id = $1", tr.order, tr.clock.Add(time.Hour))
	seq := tr.customerSeq(tr.order)
	tr.waiting(200)
	if tr.customerPushes() != 1 {
		t.Errorf("%d pushes, want 1", tr.customerPushes())
	}
	if tr.customerSeq(tr.order) != seq+1 {
		t.Error("the customer was pushed but their seq did not rise")
	}
	if c := tr.customer(); c["rider_waiting_since"] == nil {
		t.Errorf("%v", c)
	}
}
