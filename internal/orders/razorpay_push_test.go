package orders

import (
	"context"
	"testing"
)

// refundPushes counts the "refund processed" pushes queued for an order's customer.
func (p *payFixture) refundPushes(order string) int {
	return p.count(`SELECT count(*) FROM outbox_events e JOIN orders o ON o.id::text = e.payload->>'order_id'
		WHERE e.topic = 'push.customer' AND e.payload->>'stage' = 'refund_processed' AND o.id::text = $1
			AND e.payload->>'customer_id' = o.customer_id::text AND e.payload->>'refund_id' IS NOT NULL`, order)
}

func TestARefundSettledByWebhookTellsTheCustomerOnce(t *testing.T) {
	p := newPayFixture(t)
	order := p.placed(p.chai, 1)
	rzOrder, amount := p.providerOrder(order)
	p.deliver("evt_1", "", paymentEvent("payment.captured", "pay_A", rzOrder, amount))
	p.send("POST", "/orders/"+order+"/cancel", p.token, `{}`)
	refundID := p.id("SELECT id::text FROM refunds WHERE order_id = $1", order)
	if n, err := p.svc.SendRefunds(context.Background(), p.rz, p.clock); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if p.refundPushes(order) != 0 {
		t.Fatal("told before the money went back")
	}
	p.deliver("evt_2", "", refundEvent("refund.processed", "rfnd_T1", refundID))
	if p.refundPushes(order) != 1 {
		t.Fatal("the customer was not told the refund went back")
	}
	p.deliver("evt_3", "", refundEvent("refund.processed", "rfnd_T1", refundID)) // a second event for the same refund
	if p.refundPushes(order) != 1 {
		t.Fatal("told twice")
	}
}

func TestARefundProcessedAtOnceTellsTheCustomerOnce(t *testing.T) {
	p := newPayFixture(t)
	order := p.placed(p.chai, 1)
	rzOrder, amount := p.providerOrder(order)
	p.deliver("evt_1", "", paymentEvent("payment.captured", "pay_A", rzOrder, amount))
	p.send("POST", "/orders/"+order+"/cancel", p.token, `{}`)
	refundID := p.id("SELECT id::text FROM refunds WHERE order_id = $1", order)
	// Razorpay already has it, processed: the sender settles it.
	p.z.refunds = map[string][]rzRefund{"pay_A": {{ID: "rfnd_OLD", Amount: amount, Status: "processed", Notes: rzNotes{"refund_id": refundID}}}}
	if n, err := p.svc.SendRefunds(context.Background(), p.rz, p.clock); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if p.refundPushes(order) != 1 {
		t.Fatal("the customer was not told")
	}
	p.deliver("evt_2", "", refundEvent("refund.processed", "rfnd_OLD", refundID)) // the webhook arrives after
	if p.refundPushes(order) != 1 {
		t.Fatal("told twice")
	}
}

func TestAFailedRefundTellsTheCustomerNothing(t *testing.T) {
	p := newPayFixture(t)
	order := p.placed(p.chai, 1)
	rzOrder, amount := p.providerOrder(order)
	p.deliver("evt_1", "", paymentEvent("payment.captured", "pay_A", rzOrder, amount))
	p.send("POST", "/orders/"+order+"/cancel", p.token, `{}`)
	refundID := p.id("SELECT id::text FROM refunds WHERE order_id = $1", order)
	p.svc.SendRefunds(context.Background(), p.rz, p.clock)
	p.deliver("evt_2", "", refundEvent("refund.failed", "rfnd_T1", refundID))
	if p.refundPushes(order) != 0 {
		t.Fatal("a failed refund was reported as processed")
	}
	if p.alerts("refund_failed") != 1 {
		t.Fatal("support was not alerted")
	}
}
