package orders

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The rider app's operations that live with orders (issue 159): waiting at the gate, the shift as
// getRiderState shows it, and a rider on a break getting no offer.

// arrived takes a packed order through the zone to the perimeter, so it is ARRIVED.
func (tr *trip) arrived() {
	tr.t.Helper()
	tr.at(far, 20*time.Second)
	tr.at(inZone, 30*time.Second)
	tr.at(inZone, 3*time.Second)
	tr.at(atGate, 10*time.Second)
	tr.at(atGateToo, 3*time.Second)
	if tr.state() != "ARRIVED" {
		tr.t.Fatalf("not arrived: %s", tr.state())
	}
}

func (tr *trip) waiting(want int) map[string]any {
	tr.t.Helper()
	code, out := tr.call("POST", "/orders/"+tr.order+"/waiting", tr.tok, "")
	if code != want {
		tr.t.Fatalf("waiting: %d %v", code, out)
	}
	return out
}

func (tr *trip) riderSeqNow() int {
	return tr.count("SELECT coalesce((SELECT seq FROM rider_state WHERE rider_id = (SELECT rider_id FROM offers WHERE id = $1)), 0)::int", tr.offer)
}

func (tr *trip) customerPushes() int {
	return tr.count("SELECT count(*) FROM outbox_events WHERE topic = 'push.customer' AND payload->>'kind' = 'rider_waiting'")
}

func TestTheRiderSaysTheyAreWaitingAtTheGate(t *testing.T) {
	tr := started(t, true)
	if out := tr.waiting(409); out["code"] != "not_arrived" {
		t.Errorf("before arrival: %v", out)
	}
	if _, ro := tr.call("GET", "/orders/"+tr.order, tr.tok, ""); orderOf(ro)["waiting_since"] != nil {
		t.Errorf("waiting_since before it was said: %v", ro)
	}
	tr.arrived()
	if c := tr.customer(); c["rider_waiting_since"] != nil {
		t.Fatalf("the customer sees waiting before the rider said so: %v", c)
	}
	customerBefore, riderBefore := tr.customerSeq(tr.order), tr.riderSeqNow()
	events := tr.count("SELECT count(*) FROM order_events WHERE order_id = $1", tr.order)

	out := tr.waiting(200)
	order := orderOf(out)
	riderCheck(t, order, "RiderOrder")
	since, _ := order["waiting_since"].(string)
	if since == "" || order["state"] != "ARRIVED" {
		t.Fatalf("%v", order)
	}
	if tr.customerSeq(tr.order) != customerBefore+1 {
		t.Error("the customer's order seq did not rise")
	}
	if tr.riderSeqNow() != riderBefore+1 {
		t.Error("the rider seq did not rise")
	}
	if tr.customerPushes() != 1 {
		t.Errorf("%d pushes to the customer, want 1", tr.customerPushes())
	}
	if n := tr.count("SELECT count(*) FROM outbox_events WHERE topic = 'push.customer' AND payload->>'kind' = 'rider_waiting' AND payload::text ~* '(phone|lat|lng|address)'"); n != 0 {
		t.Error("the push to the customer carries personal data")
	}
	if tr.count("SELECT count(*) FROM order_events WHERE order_id = $1", tr.order) != events+1 {
		t.Error("the order's history has no record of it")
	}
	if tr.count("SELECT count(*) FROM audit_events WHERE action = 'order.rider_waiting' AND entity_id = $1", tr.order) != 1 {
		t.Error("no audit record")
	}
	c := tr.customer()
	if c["rider_waiting_since"] != since {
		t.Errorf("the customer's order shows %v, want %v", c["rider_waiting_since"], since)
	}
	if _, ro := tr.call("GET", "/orders/"+tr.order, tr.tok, ""); orderOf(ro)["waiting_since"] != since {
		t.Errorf("getRiderOrder: %v", ro)
	}

	// Saying it again keeps the first time and changes nothing.
	tr.clock = tr.clock.Add(20 * time.Second)
	again := orderOf(tr.waiting(200))
	if again["waiting_since"] != since {
		t.Errorf("the time moved: %v vs %v", again["waiting_since"], since)
	}
	if tr.customerSeq(tr.order) != customerBefore+1 || tr.riderSeqNow() != riderBefore+1 || tr.customerPushes() != 1 ||
		tr.count("SELECT count(*) FROM order_events WHERE order_id = $1", tr.order) != events+1 {
		t.Error("a repeat changed something")
	}

	// A handed-over order cannot be marked, and keeps the time the rider said.
	if code, out := tr.handOver(tr.otp(), 2, tr.clock); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if out := tr.waiting(409); out["code"] != "handover_not_allowed" {
		t.Errorf("after the handover: %v", out)
	}
	if _, ro := tr.call("GET", "/orders/"+tr.order, tr.tok, ""); orderOf(ro)["waiting_since"] != since {
		t.Errorf("the time was lost at the handover: %v", ro)
	}
}

func TestWaitingAtTheGateReplayActsOnce(t *testing.T) {
	tr := started(t, true)
	tr.arrived()
	send := func() (int, string, http.Header) {
		req, _ := http.NewRequest("POST", tr.riders.srv.URL+"/v1/orders/"+tr.order+"/waiting", strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer "+tr.tok)
		req.Header.Set("Idempotency-Key", "waiting-key-0001")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw), resp.Header
	}
	code, first, _ := send()
	code2, second, header := send()
	if code != 200 || code2 != 200 || serverTimeOf(first) != serverTimeOf(second) || serverTimeOf(first) == "" || header.Get("Idempotency-Replayed") != "true" {
		t.Errorf("%d %s / %d %s / %v", code, first, code2, second, header)
	}
	if tr.customerPushes() != 1 {
		t.Errorf("%d pushes after a replay", tr.customerPushes())
	}
}

func TestWaitingAtTheGateRefusals(t *testing.T) {
	// Not this rider's order, or no order at all.
	tr := started(t, true)
	tr.arrived()
	for _, id := range []string{"nonsense", "00000000-0000-0000-0000-000000000001"} {
		if code, out := tr.call("POST", "/orders/"+id+"/waiting", tr.tok, ""); code != 404 || out["code"] != "not_found" {
			t.Errorf("%s: %d %v", id, code, out)
		}
	}
	if code, out := tr.call("POST", "/orders/"+tr.order+"/waiting", "", ""); code != 401 {
		t.Errorf("no session: %d %v", code, out)
	}
	// The customer's token is not a rider's.
	if code, _ := tr.call("POST", "/orders/"+tr.order+"/waiting", tr.fixture.token, ""); code != 401 && code != 403 {
		t.Errorf("a customer: %d", code)
	}

	// The customer cancelled.
	cancelled := started(t, true)
	cancelled.clock = cancelled.clock.Add(301 * time.Second)
	if code, out := cancelled.send("POST", "/orders/"+cancelled.order+"/cancel", cancelled.fixture.token, `{}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if out := cancelled.waiting(409); out["code"] != "order_cancelled" {
		t.Errorf("after a cancel: %v", out)
	}

	// The order went to another rider.
	moved := started(t, true)
	moved.exec("UPDATE offers SET released_at = now(), ended_reason = 'reassigned', final_state = 'PACKED' WHERE id = $1", moved.offer)
	if out := moved.waiting(409); out["code"] != "order_reassigned" {
		t.Errorf("after a reassignment: %v", out)
	}
}

func TestWaitingAfterTheCodeWasReleasedByALostSignal(t *testing.T) {
	tr := started(t, true)
	tr.at(inZone, 20*time.Second)
	tr.at(inZone, 3*time.Second)
	begun := tr.clock
	tr.clock = begun.Add(95 * time.Second)
	tr.dispatch(tr.clock)
	if tr.state() != "TIMER_RUNNING" || tr.count("SELECT count(*) FROM orders WHERE id = $1 AND otp_released_at IS NOT NULL", tr.order) != 1 {
		t.Fatalf("the code was not released: %s", tr.state())
	}
	if out := tr.waiting(200); orderOf(out)["waiting_since"] == nil {
		t.Errorf("%v", out)
	}
}

func TestWaitingDuringTheMinimumWaitShowsNothingToTheCustomer(t *testing.T) {
	tr := started(t, true)
	tr.exec("UPDATE orders SET state = 'ARRIVED', arrived_at = $2, min_wait_until = $3 WHERE id = $1", tr.order, tr.clock, tr.clock.Add(time.Hour))
	seq := tr.customerSeq(tr.order)
	if orderOf(tr.waiting(200))["waiting_since"] == nil {
		t.Fatal("the rider's view has no time")
	}
	if tr.customerSeq(tr.order) != seq || tr.customerPushes() != 0 {
		t.Error("the customer was told while the minimum wait still hides the rider")
	}
	if c := tr.customer(); c["rider_waiting_since"] != nil || c["customer_stage"] != "looking_for_rider" {
		t.Errorf("%v", c)
	}
}

func TestTheRiderStateShowsTheBreakAndTheDevice(t *testing.T) {
	x := withRiders(newFixture(t))
	_, tok := x.signIn(x.scooter)
	shift := func() map[string]any {
		_, st := x.call("GET", "/riders/me/state", tok, "")
		return st["shift"].(map[string]any)
	}
	if sh := shift(); sh["break_started_at"] != nil || sh["break_ends_at"] != nil || sh["device_id"] != nil {
		t.Fatalf("%v", sh)
	}
	rider := x.id("SELECT rider_id::text FROM shifts WHERE scooter_id = $1 AND ended_at IS NULL", x.scooter)
	device := x.id("INSERT INTO devices (actor_type, actor_id, platform, push_token) VALUES ('rider', $1, 'ios', 'state-token') RETURNING id::text", rider)
	x.exec("UPDATE shifts SET device_id = $1, break_started_at = now(), break_ends_at = now() + interval '30 minutes' WHERE ended_at IS NULL", device)
	sh := shift()
	riderCheck(t, sh, "Shift")
	if sh["device_id"] != device || sh["break_started_at"] == nil || sh["break_ends_at"] == nil {
		t.Errorf("%v", sh)
	}
}

func TestARiderOnABreakGetsNoOffer(t *testing.T) {
	x := withRiders(newFixture(t))
	start := x.clock
	first := x.placed(x.chai, 1)
	x.pay(first, "paid")
	rider, _ := x.signIn(x.scooter)
	x.exec("UPDATE shifts SET break_started_at = now(), break_ends_at = now() + interval '30 minutes' WHERE rider_id = $1 AND ended_at IS NULL", rider)
	x.dispatch(start.Add(6 * time.Second))
	if n := x.count("SELECT count(*) FROM offers"); n != 0 {
		t.Fatalf("%d offers went to a rider on a break", n)
	}
	if n := x.count("SELECT count(*) FROM rider_notifications"); n != 0 {
		t.Errorf("%d notifications for a rider who got nothing", n)
	}

	// Once the break is over, the next order reaches them, and they are told.
	x.exec("UPDATE shifts SET break_started_at = NULL, break_ends_at = NULL WHERE rider_id = $1 AND ended_at IS NULL", rider)
	x.clock = start.Add(time.Second)
	second := x.placed(x.chai, 1)
	x.pay(second, "paid")
	x.dispatch(start.Add(12 * time.Second))
	if n := x.count("SELECT count(*) FROM offers WHERE rider_id = $1", rider); n != 1 {
		t.Fatalf("%d offers after the break", n)
	}
	if n := x.count("SELECT count(*) FROM rider_notifications WHERE rider_id = $1 AND kind = 'offer' AND order_id = $2", rider, second); n != 1 {
		t.Errorf("%d offer notifications", n)
	}
	if n := x.count("SELECT count(*) FROM rider_notifications WHERE rider_id = $1 AND (title || body) ~* '(\\+91|phone|address)'", rider); n != 0 {
		t.Error("a notification carries personal data")
	}
}

// serverTimeOf reads server_time from a JSON reply; a replay carries the first reply's time.
func serverTimeOf(raw string) string {
	var v struct {
		ServerTime string `json:"server_time"`
	}
	_ = json.Unmarshal([]byte(raw), &v)
	return v.ServerTime
}
