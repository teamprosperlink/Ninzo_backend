package orders

import (
	"context"
	"strings"
	"testing"
	"time"
)

func (f *fixture) placed(lines ...any) string {
	f.t.Helper()
	code, out := f.order(lines...)
	if code != 201 {
		f.t.Fatalf("%d %v", code, out)
	}
	return out["order"].(map[string]any)["id"].(string)
}

func (f *fixture) pay(id, result string) {
	f.t.Helper()
	if code, out := f.send("POST", "/test/payments/"+id, f.token, `{"result":"`+result+`"}`); code != 200 {
		f.t.Fatalf("%d %v", code, out)
	}
}

func (f *fixture) held() int {
	return f.count("SELECT coalesce(sum(reserved), 0) FROM scooter_stock")
}

func orderOf(out map[string]any) map[string]any {
	o, _ := out["order"].(map[string]any)
	return o
}

func TestCancellingBeforePayment(t *testing.T) {
	f := newFixture(t)
	id := f.placed(f.chai, 2)
	code, out := f.send("POST", "/orders/"+id+"/cancel", f.token, `{}`)
	o := orderOf(out)
	if code != 200 || o["customer_stage"] != "ended" || o["ended_reason"] != "cancelled" || o["payment_status"] != "not_paid" || o["cancellable"] != false {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "OrderResponse")
	if f.held() != 0 || f.count("SELECT count(*) FROM reservations WHERE status = 'held'") != 0 {
		t.Error("the stock was not given back")
	}
	if n := f.count("SELECT coalesce(sum(delta_reserved), 0) FROM stock_ledger"); n != 0 {
		t.Errorf("the ledger does not balance: %d", n)
	}
	if f.count("SELECT count(*) FROM refunds") != 0 {
		t.Error("nothing was paid, so nothing is refunded")
	}
	// Cancelling again, as after a lost reply, succeeds and changes nothing.
	seq := o["seq"]
	if code, out := f.send("POST", "/orders/"+id+"/cancel", f.token, `{}`); code != 200 || orderOf(out)["seq"] != seq {
		t.Errorf("%d %v", code, out)
	}
	// A payment that lands after the cancel goes back.
	f.pay(id, "paid")
	if code, out := f.send("GET", "/orders/"+id+"/refund", f.token, ""); code != 200 || out["refund"].(map[string]any)["reason_code"] != "customer_cancelled" {
		t.Errorf("%d %v", code, out)
	}
	for _, body := range []string{"", `{"reason":"` + strings.Repeat("a", 201) + `"}`} {
		if code, _ := f.send("POST", "/orders/"+id+"/cancel", f.token, body); code != 400 {
			t.Errorf("%q: %d", body, code)
		}
	}
}

func TestTheCancelWindow(t *testing.T) {
	f := newFixture(t)
	start := f.clock
	inside := f.placed(f.chai, 1)
	f.pay(inside, "paid")
	f.clock = start.Add(4 * time.Second)
	code, out := f.send("POST", "/orders/"+inside+"/cancel", f.token, `{"reason":"changed my mind"}`)
	if o := orderOf(out); code != 200 || o["customer_stage"] != "ended" || o["payment_status"] != "refunded" {
		t.Fatalf("inside the window: %d %v", code, out)
	}
	code, out = f.send("GET", "/orders/"+inside+"/refund", f.token, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "RefundResponse")
	r := out["refund"].(map[string]any)
	check(t, r, "Refund")
	if r["reason_code"] != "customer_cancelled" || r["status"] != "processed" || r["amount"].(map[string]any)["amount_minor"] != float64(4900) {
		t.Errorf("%v", r)
	}

	f.clock = start
	late := f.placed(f.samosa, 1)
	f.pay(late, "paid")
	f.clock = start.Add(6 * time.Second)
	if code, out := f.send("POST", "/orders/"+late+"/cancel", f.token, `{}`); code != 409 || out["code"] != "cancel_not_allowed" {
		t.Errorf("after the window: %d %v", code, out)
	}
	if code, _ := f.send("GET", "/orders/"+late+"/refund", f.token, ""); code != 404 {
		t.Errorf("no refund yet: %d", code)
	}
	f.clock = start.Add(301 * time.Second)
	code, out = f.send("POST", "/orders/"+late+"/cancel", f.token, `{}`)
	if code != 200 || orderOf(out)["customer_stage"] != "ended" {
		t.Fatalf("after the wait deadline: %d %v", code, out)
	}
	if _, out := f.send("GET", "/orders/"+late+"/refund", f.token, ""); out["refund"].(map[string]any)["reason_code"] != "wait_deadline_passed" {
		t.Errorf("%v", out)
	}
	if f.held() != 0 {
		t.Error("a cancelled paid order kept its stock")
	}
	// An ended order other than a cancel cannot be cancelled.
	f.clock = start
	gone := f.placed(f.samosa, 1)
	f.exec("UPDATE orders SET state = 'TIMED_OUT', ended_reason = 'timed_out' WHERE id = $1", gone)
	if code, out := f.send("POST", "/orders/"+gone+"/cancel", f.token, `{}`); code != 409 || out["code"] != "cancel_not_allowed" {
		t.Errorf("%d %v", code, out)
	}
	_, other := f.customer(true)
	for _, path := range []string{"/orders/" + late + "/refund", "/orders/nope/refund"} {
		if code, _ := f.send("GET", path, other, ""); code != 404 {
			t.Errorf("%s: %d", path, code)
		}
	}
	if code, _ := f.send("POST", "/orders/"+gone+"/cancel", other, `{}`); code != 404 {
		t.Errorf("cancelling another customer's order: %d", code)
	}
}

func TestRetryingPayment(t *testing.T) {
	f := newFixture(t)
	id := f.placed(f.chai, 1)
	if code, out := f.send("POST", "/orders/"+id+"/payment/retry", f.token, ""); code != 409 || out["code"] != "payment_retry_not_allowed" {
		t.Errorf("before a failure: %d %v", code, out)
	}
	_, out := f.send("GET", "/orders/"+id, f.token, "")
	first := orderOf(out)["payment_request"].(map[string]any)["razorpay_order_id"]
	f.pay(id, "failed")
	code, out := f.send("POST", "/orders/"+id+"/payment/retry", f.token, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "CreateOrderResult")
	if orderOf(out)["customer_stage"] != "payment" || out["payment"].(map[string]any)["razorpay_order_id"] == first {
		t.Errorf("%v", out)
	}
	if f.held() != 1 {
		t.Error("a retry must keep the stock held")
	}
	f.pay(id, "paid")
	if _, out := f.send("GET", "/orders/"+id, f.token, ""); orderOf(out)["customer_stage"] != "looking_for_rider" {
		t.Errorf("%v", out)
	}
	// After the hold, a failed payment cannot be tried again.
	late := f.placed(f.samosa, 1)
	f.pay(late, "failed")
	f.clock = f.clock.Add(601 * time.Second)
	if code, out := f.send("POST", "/orders/"+late+"/payment/retry", f.token, ""); code != 409 || out["code"] != "sold_out" {
		t.Errorf("after the hold: %d %v", code, out)
	}
	_, other := f.customer(true)
	if code, _ := f.send("POST", "/orders/"+late+"/payment/retry", other, ""); code != 404 {
		t.Errorf("another customer: %d", code)
	}
}

func TestChangingTheGate(t *testing.T) {
	f := newFixture(t)
	id := f.placed(f.chai, 4)
	office := f.id("INSERT INTO gates (customer_id, zone_id, label, lat, lng, verified) VALUES ($1, $2, 'Office', 12.934, 77.626, true) RETURNING id::text", f.cust, f.zone)
	f.exec("INSERT INTO gate_history (gate_id, version, label, lat, lng, verified, reason) VALUES ($1, 1, 'Office', 12.934, 77.626, true, 'test')", office)
	code, out := f.send("PATCH", "/orders/"+id+"/gate", f.token, `{"gate_id":"`+office+`"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "CreateOrderResult")
	if g := orderOf(out)["gate"].(map[string]any); g["id"] != office || g["label"] != "Office" || out["payment"] == nil {
		t.Errorf("%v", out)
	}
	if f.held() != 4 || f.count("SELECT count(*) FROM reservations WHERE status = 'held'") != 2 {
		t.Errorf("%d held after the change", f.held())
	}
	// Nothing can reach a gate far away, so nothing changes.
	far := f.id("INSERT INTO gates (customer_id, zone_id, label, lat, lng, verified) VALUES ($1, $2, 'Far', 13.2, 77.9, true) RETURNING id::text", f.cust, f.zone)
	f.exec("INSERT INTO gate_history (gate_id, version, label, lat, lng, verified, reason) VALUES ($1, 1, 'Far', 13.2, 77.9, true, 'test')", far)
	if code, out := f.send("PATCH", "/orders/"+id+"/gate", f.token, `{"gate_id":"`+far+`"}`); code != 409 || out["code"] != "no_offer" {
		t.Errorf("%d %v", code, out)
	}
	if f.held() != 4 || f.count("SELECT count(*) FROM orders WHERE gate_id = $1", office) != 1 {
		t.Error("a refused change altered the order")
	}
	f.exec("UPDATE gates SET verified = false WHERE id = $1", f.gate)
	if code, out := f.send("PATCH", "/orders/"+id+"/gate", f.token, `{"gate_id":"`+f.gate+`"}`); code != 409 || out["code"] != "gate_not_verified" {
		t.Errorf("%d %v", code, out)
	}
	other, otherToken := f.customer(true)
	theirs := f.id("INSERT INTO gates (customer_id, zone_id, label, lat, lng, verified) VALUES ($1, $2, 'Theirs', 12.934, 77.626, true) RETURNING id::text", other, f.zone)
	if code, _ := f.send("PATCH", "/orders/"+id+"/gate", f.token, `{"gate_id":"`+theirs+`"}`); code != 404 {
		t.Errorf("another customer's gate: %d", code)
	}
	if code, _ := f.send("PATCH", "/orders/"+id+"/gate", otherToken, `{"gate_id":"`+theirs+`"}`); code != 404 {
		t.Errorf("another customer's order: %d", code)
	}
	if code, _ := f.send("PATCH", "/orders/"+id+"/gate", f.token, `{"gate_id":"x"}`); code != 400 {
		t.Errorf("bad input: %d", code)
	}
	f.pay(id, "paid")
	if code, out := f.send("PATCH", "/orders/"+id+"/gate", f.token, `{"gate_id":"`+office+`"}`); code != 409 || out["code"] != "gate_change_not_allowed" {
		t.Errorf("after payment: %d %v", code, out)
	}
}

func TestTheHoldEnds(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	start := f.clock
	unpaid := f.placed(f.chai, 2)
	failed := f.placed(f.samosa, 1)
	f.pay(failed, "failed")
	paid := f.placed(f.chai, 1)
	f.pay(paid, "paid")
	if n, err := f.svc.Tick(ctx, start.Add(time.Second)); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
	if n, err := f.svc.Tick(ctx, start.Add(601*time.Second)); err != nil || n != 4 {
		t.Fatalf("two holds ended and the paid order's clocks moved twice: %d %v", n, err)
	}
	for _, id := range []string{unpaid, failed} {
		_, out := f.send("GET", "/orders/"+id, f.token, "")
		if o := orderOf(out); o["customer_stage"] != "ended" || o["ended_reason"] != "timed_out" || o["payment_status"] != "not_paid" {
			t.Errorf("%v", o)
		}
	}
	if f.held() != 1 {
		t.Errorf("%d held, only the paid order's unit should be", f.held())
	}
	if n, _ := f.svc.Tick(ctx, start.Add(700*time.Second)); n != 0 {
		t.Errorf("a second pass changed %d", n)
	}
	// A payment that lands after the hold ended goes back.
	f.exec("INSERT INTO payments (order_id, provider, provider_order_id, amount_paise) VALUES ($1, 'test', 'test_late_1', 3900)", failed)
	f.pay(failed, "paid")
	if f.count("SELECT count(*) FROM refunds WHERE order_id = $1 AND reason_code = 'payment_after_hold'", failed) != 1 {
		t.Error("a late payment was kept")
	}
}

func TestSeqMovesWhenCancellableChanges(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	start := f.clock
	id := f.placed(f.chai, 1)
	f.pay(id, "paid")
	seq := func() int { return f.count("SELECT seq FROM orders WHERE id = $1", id) }
	before := seq()
	if _, err := f.svc.Tick(ctx, start.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	if seq() != before+1 {
		t.Errorf("the window closed: seq %d, was %d", seq(), before)
	}
	_, _ = f.svc.Tick(ctx, start.Add(7*time.Second))
	if seq() != before+1 {
		t.Error("seq moved twice for one change")
	}
	_, _ = f.svc.Tick(ctx, start.Add(301*time.Second))
	if seq() != before+2 {
		t.Errorf("the wait deadline passed: seq %d", seq())
	}
}

func TestACancelAndTheHoldEndingAtOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.placed(f.chai, 3)
	f.clock = f.clock.Add(601 * time.Second)
	done := make(chan int, 1)
	go func() {
		code, _ := f.send("POST", "/orders/"+id+"/cancel", f.token, `{}`)
		done <- code
	}()
	if _, err := f.svc.Tick(ctx, f.clock); err != nil {
		t.Fatal(err)
	}
	code := <-done
	if code != 200 && code != 409 {
		t.Errorf("%d", code)
	}
	if f.held() != 0 || f.count("SELECT coalesce(sum(delta_reserved), 0) FROM stock_ledger") != 0 {
		t.Error("the stock was given back twice, or not at all")
	}
	if n := f.count("SELECT count(*) FROM order_events WHERE order_id = $1 AND to_state IN ('CANCELLED', 'TIMED_OUT')", id); n != 1 {
		t.Errorf("%d endings", n)
	}
}

func TestThePassSkipsAnOrderInUse(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.placed(f.chai, 1)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// A customer request holds the order and ends it.
	if _, err := tx.Exec(ctx, "UPDATE orders SET state = 'CANCELLED', ended_reason = 'cancelled' WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		n, _ := f.svc.Tick(ctx, f.clock.Add(601*time.Second))
		done <- n
	}()
	select {
	case n := <-done:
		if n != 0 {
			t.Errorf("the pass changed %d orders in use", n)
		}
	case <-time.After(3 * time.Second):
		t.Error("the pass waited on an order in use")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.count("SELECT count(*) FROM orders WHERE id = $1 AND state = 'CANCELLED'", id); n != 1 {
		t.Error("the pass overwrote the customer's change")
	}
}

func TestTheExactBoundaries(t *testing.T) {
	f := newFixture(t)
	f.clock = f.clock.Truncate(time.Microsecond) // The database keeps microseconds.
	start := f.clock
	id := f.placed(f.chai, 1)
	f.pay(id, "paid")
	var cancelUntil, waitDeadline time.Time
	if err := f.pool.QueryRow(context.Background(), "SELECT cancel_until, wait_deadline FROM orders WHERE id = $1", id).Scan(&cancelUntil, &waitDeadline); err != nil {
		t.Fatal(err)
	}
	if !cancelUntil.Equal(start.Add(5 * time.Second)) {
		t.Errorf("cancel_until %v, paid at %v", cancelUntil, start)
	}
	f.clock = cancelUntil.Add(-time.Microsecond)
	if _, out := f.send("GET", "/orders/"+id, f.token, ""); orderOf(out)["cancellable"] != true {
		t.Error("just inside the window")
	}
	f.clock = cancelUntil
	if code, out := f.send("POST", "/orders/"+id+"/cancel", f.token, `{}`); code != 409 {
		t.Errorf("at cancel_until itself the window has closed: %d %v", code, out)
	}
	f.clock = waitDeadline.Add(-time.Microsecond)
	if code, _ := f.send("POST", "/orders/"+id+"/cancel", f.token, `{}`); code != 409 {
		t.Errorf("just before the wait deadline: %d", code)
	}
	f.clock = waitDeadline
	if code, out := f.send("POST", "/orders/"+id+"/cancel", f.token, `{}`); code != 200 {
		t.Errorf("at the wait deadline itself: %d %v", code, out)
	}
}

func TestOneBadOrderDoesNotBlockThePass(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bad := f.placed(f.chai, 1)
	good := f.placed(f.samosa, 1)
	f.exec("UPDATE orders SET hold_until = hold_until - interval '1 second' WHERE id = $1", bad)
	// Break the bad order's stock row, so giving it back fails.
	f.exec("UPDATE scooter_stock SET reserved = 0 WHERE lot_id IN (SELECT lot_id FROM reservations WHERE order_id = $1)", bad)
	n, err := f.svc.Tick(ctx, f.clock.Add(601*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || f.count("SELECT count(*) FROM orders WHERE id = $1 AND state = 'TIMED_OUT'", good) != 1 {
		t.Errorf("%d changed; the good order must still end", n)
	}
	if f.count("SELECT count(*) FROM orders WHERE id = $1 AND state = 'PAYMENT_PENDING'", bad) != 1 {
		t.Error("the bad order should be left for a person to look at")
	}
}

func TestAPanicInThePassIsCaught(t *testing.T) {
	f := newFixture(t)
	f.svc.now = func() time.Time { panic("boom") }
	f.svc.safeTick(context.Background())
}
