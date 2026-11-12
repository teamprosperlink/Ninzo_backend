package orders

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
)

// A refund the console makes (issue 147) is read by the customer's own getRefund and order views
// with no special case: the same table, the same statuses.
func TestASupportRefundShowsInTheCustomersViews(t *testing.T) {
	f := newFixture(t)
	id := f.placed(f.chai, 1) // 4900 paise
	f.pay(id, "paid")
	var staff string
	if err := f.pool.QueryRow(context.Background(), "INSERT INTO console_users (login_id, name, role, password_hash) VALUES ('desk', 'Desk', 'support', 'x') RETURNING id::text").Scan(&staff); err != nil {
		t.Fatal(err)
	}
	var payment string
	if err := f.pool.QueryRow(context.Background(), "SELECT id::text FROM payments WHERE order_id = $1", id).Scan(&payment); err != nil {
		t.Fatal(err)
	}
	record := func(provider string, amount int) string {
		var refund string
		err := database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
			var err error
			refund, err = RecordSupportRefund(context.Background(), tx, id, payment, provider, amount, staff, "customer rang to say it was cold")
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return refund
	}

	seq := func() int { return f.count("SELECT seq::int FROM orders WHERE id = $1", id) }
	before := seq()
	record("test", 1000)
	if seq() != before+1 {
		t.Error("the customer's order view must refresh")
	}
	code, out := f.send("GET", "/orders/"+id+"/refund", f.token, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "RefundResponse")
	r := out["refund"].(map[string]any)
	check(t, r, "Refund")
	if r["reason_code"] != "support_refund_1" || r["status"] != "processed" || r["method"] != "original_payment_method" || r["amount"].(map[string]any)["amount_minor"] != float64(1000) {
		t.Errorf("%v", r)
	}
	if _, has := r["note"]; has {
		t.Error("what staff typed is not part of the customer's refund")
	}
	// Part of the payment is back: the order still reads as paid. All of it: refunded.
	if _, out := f.send("GET", "/orders/"+id, f.token, ""); orderOf(out)["payment_status"] != "paid" {
		t.Errorf("%v", orderOf(out)["payment_status"])
	}
	record("test", 3900)
	if f.count("SELECT count(DISTINCT reason_code) FROM refunds WHERE order_id = $1", id) != 2 {
		t.Error("each support refund has its own code")
	}
	_, out = f.send("GET", "/orders/"+id+"/refund", f.token, "")
	if r := out["refund"].(map[string]any); r["reason_code"] != "support_refund_2" || r["amount"].(map[string]any)["amount_minor"] != float64(4900) {
		t.Errorf("the newest refund names the reason, the amount is the total (1000 + 3900): %v", r)
	}
	if _, out := f.send("GET", "/orders/"+id, f.token, ""); orderOf(out)["payment_status"] != "refunded" {
		t.Errorf("%v", orderOf(out)["payment_status"])
	}
	// A real provider's refund waits until the provider confirms it.
	other := f.placed(f.chai, 1)
	f.pay(other, "paid")
	f.exec("UPDATE payments SET provider = 'razorpay' WHERE order_id = $1", other)
	if err := f.pool.QueryRow(context.Background(), "SELECT id::text FROM payments WHERE order_id = $1", other).Scan(&payment); err != nil {
		t.Fatal(err)
	}
	id = other
	record("razorpay", 100)
	if f.count("SELECT count(*) FROM refunds WHERE order_id = $1 AND status = 'pending'", other) != 1 {
		t.Error("a razorpay refund starts pending")
	}
	// More than the payment is refused by the table itself.
	err := database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		_, err := RecordSupportRefund(context.Background(), tx, other, payment, "razorpay", 5000, staff, "too much for this one")
		return err
	})
	if err == nil {
		t.Error("a refund over the payment was recorded")
	}
}

// consoleRefund records a refund the console made, as the console does (issue 147), on the order's
// paid payment.
func (f *fixture) consoleRefund(id string, amount int) {
	f.t.Helper()
	var staff, payment, provider string
	ctx := context.Background()
	if err := f.pool.QueryRow(ctx, `INSERT INTO console_users (login_id, name, role, password_hash) VALUES ('desk-' || substr(gen_random_uuid()::text, 1, 8), 'Desk', 'support', 'x') RETURNING id::text`).Scan(&staff); err != nil {
		f.t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, "SELECT id::text, provider FROM payments WHERE order_id = $1 AND status = 'paid'", id).Scan(&payment, &provider); err != nil {
		f.t.Fatal(err)
	}
	err := database.InTx(ctx, f.pool, func(tx pgx.Tx) error {
		_, err := RecordSupportRefund(ctx, tx, id, payment, provider, amount, staff, "customer rang to say it was cold")
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

// refunded is everything that went back on an order (failed refunds not counted), and paid is what
// the customer paid. The first must never pass the second.
func (f *fixture) refunded(id string) (back, paid int) {
	f.t.Helper()
	return f.count("SELECT coalesce(sum(amount_paise), 0)::int FROM refunds WHERE order_id = $1 AND status <> 'failed'", id),
		f.count("SELECT amount_paise FROM payments WHERE order_id = $1 AND status IN ('paid', 'refunded')", id)
}

func (f *fixture) wantRefunded(id string, back int) {
	f.t.Helper()
	got, paid := f.refunded(id)
	if got != back || got > paid {
		f.t.Errorf("refunded %d of %d paid, want %d", got, paid, back)
	}
}

// The customer cancels inside the window after the console refunded part: the rest goes back.
func TestACancelAfterASupportRefundRefundsWhatIsLeft(t *testing.T) {
	f := newFixture(t)
	id := f.placed(f.chai, 1) // 4900
	f.pay(id, "paid")
	f.consoleRefund(id, 1500)
	code, out := f.send("POST", "/orders/"+id+"/cancel", f.token, `{}`)
	if code != 200 || orderOf(out)["ended_reason"] != "cancelled" {
		t.Fatalf("%d %v", code, out)
	}
	f.wantRefunded(id, 4900)
	// The customer sees one refund: the newest one's reason and status, the total amount.
	_, out = f.send("GET", "/orders/"+id+"/refund", f.token, "")
	check(t, out, "RefundResponse")
	if r := out["refund"].(map[string]any); r["reason_code"] != "customer_cancelled" || r["amount"].(map[string]any)["amount_minor"] != float64(4900) {
		t.Errorf("%v", r)
	}
	// A cancel after the console refunded all of it succeeds too, with nothing more to give back.
	all := f.placed(f.chai, 1)
	f.pay(all, "paid")
	f.consoleRefund(all, 4900)
	if code, out := f.send("POST", "/orders/"+all+"/cancel", f.token, `{}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	f.wantRefunded(all, 4900)
}

// A failed order (a rider's accident after the customer saw the rider) refunds what is left.
func TestAFailedOrderAfterASupportRefundRefundsWhatIsLeft(t *testing.T) {
	tr, _ := accepted(t, false) // 2 x 4900
	tr.consoleRefund(tr.order, 5000)
	if code, out := tr.report("accident", ""); code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	if c := tr.customer(); c["customer_stage"] != "ended" || c["ended_reason"] != "failed" {
		t.Fatalf("%v", c)
	}
	tr.wantRefunded(tr.order, 9800)
}

// Missing food at handover, after the console refunded most of the payment: the refund and the
// remedy are capped at what is left, and the handover succeeds.
func TestMissingFoodAfterASupportRefundIsCapped(t *testing.T) {
	tr := begin(t, nil) // 2 x 4900, one handed over
	tr.consoleRefund(tr.order, 6000)
	tr.at(atGate, 20*time.Second)
	tr.at(atGateToo, 3*time.Second)
	if code, out := tr.handOver(tr.otp(), 1, tr.clock); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	tr.wantRefunded(tr.order, 9800)
	if n := tr.count("SELECT amount_paise FROM remedies WHERE order_id = $1 AND type = 'refund'", tr.order); n != 3800 {
		t.Errorf("the owed refund is %d, want 3800", n)
	}
	// Nothing left at all: the handover succeeds and owes no refund.
	none := begin(t, nil)
	none.consoleRefund(none.order, 9800)
	none.at(atGate, 20*time.Second)
	none.at(atGateToo, 3*time.Second)
	if code, out := none.handOver(none.otp(), 1, none.clock); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	none.wantRefunded(none.order, 9800)
	if none.count("SELECT count(*) FROM remedies WHERE order_id = $1 AND type = 'refund'", none.order) != 0 {
		t.Error("a refund of nothing was owed")
	}
}

// The NO_SCOOTER timeout sweep must not retry forever after a console refund.
func TestTheNoScooterTimeoutAfterASupportRefundRefundsWhatIsLeft(t *testing.T) {
	x := withRiders(newFixture(t))
	second := x.scooterOnShift("SC-2")
	x.load(second, "L-CHAI-B", x.chai, 3)
	start := x.clock
	id := x.placed(x.chai, 2) // 9800
	x.pay(id, "paid")
	_, tokA := x.signIn(x.scooter)
	_, tokB := x.signIn(second)
	if x.count("SELECT count(*) FROM orders WHERE id = $1 AND scooter_id = $2", id, second) == 1 {
		tokA, tokB = tokB, tokA
	}
	x.dispatch(start.Add(6 * time.Second))
	x.clock = start.Add(8 * time.Second)
	for _, tok := range []string{tokA, tokB} {
		o := x.offer(tok)
		x.call("POST", "/offers/"+o["id"].(string)+"/decline", tok, `{"fencing_token":"`+o["fencing_token"].(string)+`"}`)
		x.dispatch(x.clock)
	}
	if _, c := x.send("GET", "/orders/"+id, x.token, ""); orderOf(c)["customer_stage"] != "no_scooter" {
		t.Fatalf("%v", c)
	}
	x.consoleRefund(id, 7000)
	x.dispatch(x.clock.Add(301 * time.Second))
	_, c := x.send("GET", "/orders/"+id, x.token, "")
	if o := orderOf(c); o["customer_stage"] != "ended" || o["ended_reason"] != "timed_out" {
		t.Fatalf("the sweep did not end the order: %v", c)
	}
	x.wantRefunded(id, 9800)
}
