package orders

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

// begin is started, with a hook that runs before the order is placed, such as a zone setting.
func begin(t *testing.T, setup func(*riders)) *trip {
	t.Helper()
	x := withRiders(newFixture(t))
	if setup != nil {
		setup(x)
	}
	srv := httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		x.svc.MountRider(r, identity.RequireActor(x.sessions.Signer, identity.TypeRider))
		x.svc.MountTracking(r, identity.RequireActor(x.sessions.Signer, identity.TypeRider))
	}))
	t.Cleanup(srv.Close)
	x.srv = srv
	start := x.clock
	id := x.placed(x.chai, 2)
	x.pay(id, "paid")
	_, tok := x.signIn(x.scooter)
	x.dispatch(start.Add(6 * time.Second))
	o := x.offer(tok)
	x.clock = start.Add(10 * time.Second)
	tr := &trip{riders: x, tok: tok, order: id, offer: o["id"].(string), fencing: o["fencing_token"].(string)}
	if code, out := x.call("POST", "/offers/"+tr.offer+"/accept", tok, tr.token()); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	tr.ready(200)
	return tr
}

func creditOf(x *fixture) func(*riders) {
	return func(r *riders) {
		r.exec("INSERT INTO zone_settings (zone_id, version, remedy_credit_paise, reason, created_by) VALUES ($1, 1, 2000, 'test', 'test')", r.zone)
	}
}

func (tr *trip) remedies() int {
	return tr.count("SELECT count(*) FROM remedies WHERE order_id = $1", tr.order)
}

// runTimer starts the timer and returns when it started.
func (tr *trip) runTimer() time.Time {
	tr.t.Helper()
	tr.at(inZone, 20*time.Second)
	tr.at(inZone, 3*time.Second)
	if tr.state() != "TIMER_RUNNING" {
		tr.t.Fatalf("the timer did not start: %s", tr.state())
	}
	return tr.clock
}

func TestAMissedTimerIsCredited(t *testing.T) {
	tr := begin(t, creditOf(nil))
	begun := tr.runTimer()
	// The rider keeps pinging, so the miss is known.
	for tr.clock.Before(begun.Add(90 * time.Second)) {
		tr.at(inZone, 10*time.Second)
	}
	tr.dispatch(begun.Add(89 * time.Second))
	if tr.remedies() != 0 {
		t.Error("credited before the timer ended")
	}
	tr.dispatch(tr.clock) // Marks the miss.
	seq := tr.customerSeq(tr.order)
	tr.dispatch(tr.clock) // Owes the credit.
	tr.dispatch(tr.clock)
	c := tr.customer()
	remedy, _ := c["remedy"].(map[string]any)
	if remedy == nil || remedy["type"] != "credit" || remedy["amount"].(map[string]any)["amount_minor"] != float64(2000) {
		t.Fatalf("%v", c)
	}
	check(t, remedy, "Remedy")
	if tr.remedies() != 1 || tr.customerSeq(tr.order) != seq+1 {
		t.Error("one credit, and a seq the customer sees")
	}
	// The order carries on to arrival and handover, with the credit still shown.
	tr.at(atGate, 10*time.Second)
	tr.at(atGateToo, 3*time.Second)
	tr.handOver(tr.otp(), 2, tr.clock)
	if c := tr.customer(); c["customer_stage"] != "handed_over" || c["remedy"] == nil || c["payment_status"] != "paid" {
		t.Errorf("%v", c)
	}
}

func TestACreditWithNoAmountSet(t *testing.T) {
	tr := begin(t, nil)
	begun := tr.runTimer()
	tr.at(inZone, 85*time.Second)
	tr.at(inZone, 10*time.Second)
	tr.dispatch(begun.Add(96 * time.Second)) // Marks the miss.
	tr.dispatch(begun.Add(97 * time.Second)) // Owes the credit.
	remedy, _ := tr.customer()["remedy"].(map[string]any)
	if remedy == nil || remedy["type"] != "credit" || remedy["amount"] != nil {
		t.Errorf("%v", remedy)
	}
}

func TestNoCreditWhileTheSignalIsLost(t *testing.T) {
	tr := begin(t, creditOf(nil))
	begun := tr.runTimer()
	tr.dispatch(begun.Add(95 * time.Second))
	tr.dispatch(begun.Add(96 * time.Second))
	tr.dispatch(begun.Add(5 * time.Minute))
	if tr.count("SELECT count(*) FROM orders WHERE id = $1 AND timer_missed", tr.order) != 1 || tr.remedies() != 0 {
		t.Error("with no signal the miss is marked, but the credit waits")
	}
	tr.dispatch(begun.Add(90*time.Second + 10*time.Minute + time.Second))
	if tr.remedies() != 1 {
		t.Error("after the limit the credit goes ahead")
	}
}

func TestTheSignalReturningReleasesTheCredit(t *testing.T) {
	tr := begin(t, creditOf(nil))
	begun := tr.runTimer()
	tr.dispatch(begun.Add(95 * time.Second))
	tr.clock = begun.Add(120 * time.Second)
	tr.pingAt(inZone, 0, 5, tr.clock)
	tr.dispatch(tr.clock)
	if tr.remedies() != 1 {
		t.Error("the signal is back and shows the miss")
	}
}

func TestALateArrivalWaitsForReview(t *testing.T) {
	tr := begin(t, creditOf(nil))
	begun := tr.runTimer()
	tr.dispatch(begun.Add(95 * time.Second))
	// A queued ping arrives late, from inside the timer, at the gate.
	tr.clock = begun.Add(150 * time.Second)
	tr.pingAt(atGate, 0, 5, begun.Add(60*time.Second))
	if tr.count("SELECT count(*) FROM arrival_reviews WHERE order_id = $1 AND status = 'open'", tr.order) != 1 {
		t.Fatal("no review was opened")
	}
	tr.pingAt(inZone, 0, 5, tr.clock) // The signal is back too.
	tr.dispatch(tr.clock)
	if tr.remedies() != 0 {
		t.Error("the credit must wait for the review")
	}
	tr.dispatch(tr.clock.Add(reviewLimit + time.Second))
	if tr.remedies() != 1 || tr.count("SELECT count(*) FROM arrival_reviews WHERE status = 'timed_out'") != 1 {
		t.Error("an undecided review lets the credit go ahead after 10 minutes")
	}
	// A late ping from outside the timer opens nothing.
	tr.pingAt(atGate, 0, 5, begun.Add(-5*time.Second))
	if tr.count("SELECT count(*) FROM arrival_reviews WHERE order_id = $1", tr.order) != 1 {
		t.Error("a ping from before the timer opened a review")
	}
}

func TestADecidedReviewMeansNoCredit(t *testing.T) {
	tr := begin(t, creditOf(nil))
	begun := tr.runTimer()
	tr.clock = begun.Add(150 * time.Second)
	tr.pingAt(atGate, 0, 5, begun.Add(60*time.Second))
	tr.exec("UPDATE arrival_reviews SET status = 'arrived_in_time', decided_at = now()")
	tr.pingAt(inZone, 0, 5, tr.clock)
	tr.dispatch(tr.clock)
	tr.dispatch(tr.clock.Add(time.Hour))
	if tr.remedies() != 0 {
		t.Error("a rider found to be in time owes no credit")
	}
}

func TestMissingFoodIsRefunded(t *testing.T) {
	tr := begin(t, nil)
	tr.at(atGate, 20*time.Second)
	tr.at(atGateToo, 3*time.Second)
	if code, out := tr.handOver(tr.otp(), 1, tr.clock); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	c := tr.customer()
	remedy, _ := c["remedy"].(map[string]any)
	if remedy == nil || remedy["type"] != "refund" || remedy["amount"].(map[string]any)["amount_minor"] != float64(4900) {
		t.Errorf("%v", c)
	}
	if c["payment_status"] != "paid" {
		t.Errorf("a part refund is not a refunded payment: %v", c["payment_status"])
	}
	if _, r := tr.send("GET", "/orders/"+tr.order+"/refund", tr.fixture.token, ""); r["refund"].(map[string]any)["reason_code"] != "missing_food" {
		t.Errorf("%v", r)
	}
	// A full handover owes nothing.
	full := begin(t, nil)
	full.at(atGate, 20*time.Second)
	full.at(atGateToo, 3*time.Second)
	full.handOver(full.otp(), 2, full.clock)
	if full.remedies() != 0 || full.count("SELECT count(*) FROM refunds") != 0 {
		t.Error("a full handover was refunded")
	}
}

// toNoScooter takes a paid order to no_scooter: its only rider declines.
func toNoScooter(t *testing.T) *riders {
	x := withRiders(newFixture(t))
	start := x.clock
	id := x.placed(x.chai, 2)
	x.pay(id, "paid")
	_, tok := x.signIn(x.scooter)
	x.dispatch(start.Add(6 * time.Second))
	o := x.offer(tok)
	x.clock = start.Add(8 * time.Second)
	x.call("POST", "/offers/"+o["id"].(string)+"/decline", tok, `{"fencing_token":"`+o["fencing_token"].(string)+`"}`)
	x.dispatch(x.clock)
	if x.count("SELECT count(*) FROM orders WHERE state = 'NO_SCOOTER'") != 1 {
		t.Fatal("not at no scooter")
	}
	return x
}

func TestALongerETA(t *testing.T) {
	x := toNoScooter(t)
	id := x.id("SELECT id::text FROM orders")
	offer := x.id("INSERT INTO eta_offers (order_id, scooter_id, eta_s, expires_at) VALUES ($1, $2, 600, $3) RETURNING id::text", id, x.scooter, x.clock.Add(time.Minute))
	_, out := x.send("GET", "/orders/"+id, x.token, "")
	eta, _ := orderOf(out)["eta_offer"].(map[string]any)
	if eta == nil || eta["id"] != offer || eta["eta_s"] != float64(600) {
		t.Fatalf("%v", out)
	}
	check(t, eta, "EtaOffer")
	if code, out := x.send("POST", "/orders/"+id+"/accept-eta", x.token, `{"eta_offer_id":"00000000-0000-0000-0000-000000000000"}`); code != 409 || out["code"] != "eta_offer_expired" {
		t.Errorf("%d %v", code, out)
	}
	seq := x.customerSeq(id)
	code, out := x.send("POST", "/orders/"+id+"/accept-eta", x.token, `{"eta_offer_id":"`+offer+`"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	o := orderOf(out)
	deadline, _ := time.Parse(time.RFC3339Nano, o["wait_deadline"].(string))
	if o["customer_stage"] != "looking_for_rider" || o["eta_offer"] != nil || deadline.Sub(x.clock) < 599*time.Second || x.customerSeq(id) != seq+1 {
		t.Errorf("%v", o)
	}
	if x.held() != 2 {
		t.Errorf("%d held: the stock must be held again", x.held())
	}
	// The search starts again, so the rider who declined can be offered it again.
	x.dispatch(x.clock.Add(time.Second))
	if x.count("SELECT count(*) FROM offers WHERE order_id = $1 AND state = 'open'", id) != 1 {
		t.Error("the order was not offered again after the longer ETA")
	}
	if code, again := x.send("POST", "/orders/"+id+"/accept-eta", x.token, `{"eta_offer_id":"`+offer+`"}`); code != 200 || orderOf(again)["customer_stage"] != "looking_for_rider" {
		t.Errorf("accepting twice: %d %v", code, again)
	}
}

func TestAnExpiredETAIsRefused(t *testing.T) {
	x := toNoScooter(t)
	id := x.id("SELECT id::text FROM orders")
	offer := x.id("INSERT INTO eta_offers (order_id, scooter_id, eta_s, expires_at) VALUES ($1, $2, 600, $3) RETURNING id::text", id, x.scooter, x.clock.Add(-time.Second))
	if code, out := x.send("POST", "/orders/"+id+"/accept-eta", x.token, `{"eta_offer_id":"`+offer+`"}`); code != 409 || out["code"] != "eta_offer_expired" {
		t.Errorf("%d %v", code, out)
	}
	// Stock gone from the scooter also ends the offer.
	fresh := x.id("INSERT INTO eta_offers (order_id, scooter_id, eta_s, expires_at) VALUES ($1, $2, 600, $3) RETURNING id::text", id, x.scooter, x.clock.Add(time.Minute))
	x.exec("UPDATE scooter_stock SET reserved = on_hand")
	if code, out := x.send("POST", "/orders/"+id+"/accept-eta", x.token, `{"eta_offer_id":"`+fresh+`"}`); code != 409 || out["code"] != "eta_offer_expired" {
		t.Errorf("%d %v", code, out)
	}
	_, other := x.customer(true)
	if code, _ := x.send("POST", "/orders/"+id+"/accept-eta", other, `{"eta_offer_id":"`+fresh+`"}`); code != 404 {
		t.Errorf("another customer: %d", code)
	}
}

func TestTheRiderStreamSendsOrderChanges(t *testing.T) {
	l := withStreams(t)
	l.pay(l.order, "paid")
	l.dispatch(l.clock.Add(6 * time.Second))
	o := l.riders.offer(l.tok)
	l.offer, l.fencing = o["id"].(string), o["fencing_token"].(string)
	l.clock = l.clock.Add(10 * time.Second)
	l.call("POST", "/offers/"+l.offer+"/accept", l.tok, l.token())
	s := open(t, l.rideSrv, "/riders/me/stream", l.tok)
	s.next("auth_ok")
	s.next("order_state")
	l.ready(200)
	f, _ := s.next("order_state")
	if f == nil || f["data"].(map[string]any)["state"] != "PACKED" {
		t.Fatalf("Ready sent no order_state: %v", f)
	}
	l.at(atGate, 20*time.Second)
	l.at(atGateToo, 3*time.Second)
	for i := 0; i < 5; i++ {
		f, _ = s.next("order_state")
		if f == nil || f["data"].(map[string]any)["state"] == "ARRIVED" {
			break
		}
	}
	if f == nil || f["data"].(map[string]any)["state"] != "ARRIVED" {
		t.Errorf("arrival sent no order_state: %v", f)
	}
	_ = fmt.Sprint()
}

func TestAFlushOfOldPingsIsNotASignal(t *testing.T) {
	tr := begin(t, creditOf(nil))
	begun := tr.runTimer()
	tr.dispatch(begun.Add(95 * time.Second))
	// The phone comes back and flushes its queue in order: first a ping from on the way.
	tr.clock = begun.Add(150 * time.Second)
	tr.pingAt(far, 0, 5, begun.Add(30*time.Second))
	tr.dispatch(tr.clock)
	if tr.remedies() != 0 {
		t.Fatal("a flushed ping is not a live signal, so the credit must wait")
	}
	// Then the one from the gate, inside the timer: a review opens and holds the credit.
	tr.pingAt(atGate, 0, 5, begun.Add(60*time.Second))
	tr.dispatch(tr.clock)
	if tr.remedies() != 0 || tr.count("SELECT count(*) FROM arrival_reviews WHERE status = 'open'") != 1 {
		t.Error("the review must hold the credit")
	}
}

func TestAUsedETAIsRefused(t *testing.T) {
	x := toNoScooter(t)
	id := x.id("SELECT id::text FROM orders")
	offer := x.id("INSERT INTO eta_offers (order_id, scooter_id, eta_s, expires_at) VALUES ($1, $2, 600, $3) RETURNING id::text", id, x.scooter, x.clock.Add(time.Minute))
	if code, out := x.send("POST", "/orders/"+id+"/accept-eta", x.token, `{"eta_offer_id":"`+offer+`"}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	// The search fails again: back at no scooter.
	x.exec("UPDATE orders SET state = 'NO_SCOOTER' WHERE id = $1", id)
	if code, out := x.send("POST", "/orders/"+id+"/accept-eta", x.token, `{"eta_offer_id":"`+offer+`"}`); code != 409 || out["code"] != "eta_offer_expired" {
		t.Errorf("a used offer was accepted again: %d %v", code, out)
	}
}

func TestAnEndedOrderGetsNoCredit(t *testing.T) {
	tr := begin(t, creditOf(nil))
	tr.runTimer()
	tr.exec("UPDATE orders SET state = 'FAILED', ended_reason = 'failed', timer_missed = true WHERE id = $1", tr.order)
	tr.dispatch(tr.clock.Add(time.Hour))
	if tr.remedies() != 0 {
		t.Error("a failed order is refunded in full, not credited as well")
	}
}

func TestARefundShowsBeforeACredit(t *testing.T) {
	tr := begin(t, creditOf(nil))
	tr.exec("INSERT INTO remedies (order_id, type, reason, amount_paise) VALUES ($1, 'refund', 'missing_food', 4900)", tr.order)
	tr.exec("INSERT INTO remedies (order_id, type, reason, amount_paise, created_at) VALUES ($1, 'credit', 'timer_missed', 2000, now() + interval '1 minute')", tr.order)
	if r, _ := tr.customer()["remedy"].(map[string]any); r == nil || r["type"] != "refund" {
		t.Errorf("%v", r)
	}
}
