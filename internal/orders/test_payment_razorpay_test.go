package orders

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
)

// razorpayNamed stands in for Razorpay as a Payments provider: its orders look like Razorpay's,
// and nothing is sent anywhere.
type razorpayNamed struct{ n *atomic.Int64 }

func (razorpayNamed) Name() string  { return "razorpay" }
func (razorpayNamed) KeyID() string { return "rzp_test_fake" }
func (p razorpayNamed) NewOrder(context.Context, int) (string, error) {
	return fmt.Sprintf("order_FAKE%06d", p.n.Add(1)), nil
}

// On staging the test payment sits next to Razorpay (issue 263). An order paid through it becomes a
// test payment, so its refund is processed at once and never waits for Razorpay.
func TestTheTestPaymentNextToRazorpayMakesATestPayment(t *testing.T) {
	f := newFixture(t)
	f.svc.Payments = razorpayNamed{n: &atomic.Int64{}}
	id := f.placed(f.chai, 1)
	if p := f.id("SELECT provider FROM payments WHERE order_id = $1", id); p != "razorpay" {
		t.Fatalf("the order was not made with the Razorpay stand-in: %s", p)
	}
	f.pay(id, "paid")
	if p := f.id("SELECT provider FROM payments WHERE order_id = $1", id); p != "test" {
		t.Errorf("a payment settled through the test route is still %s", p)
	}
	if s := f.id("SELECT state FROM orders WHERE id = $1", id); s != "PAYMENT_SECURED" {
		t.Errorf("order state %s", s)
	}
	// Inside the cancel window the customer gets a full refund: done at once, as for any test payment.
	if code, out := f.send("POST", "/orders/"+id+"/cancel", f.token, `{}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM refunds WHERE order_id = $1 AND status = 'processed'", id); n != 1 {
		t.Errorf("%d processed refunds, want 1 (a pending one would wait for Razorpay forever)", n)
	}
}
