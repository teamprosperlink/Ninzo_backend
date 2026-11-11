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

// Points around the test gate at 12.935, 77.625. The zone is 350 m and the perimeter 100 m.
var (
	far       = [2]float64{12.945, 77.625}   // About 1.1 km away.
	inZone    = [2]float64{12.9375, 77.625}  // About 280 m.
	atGate    = [2]float64{12.9355, 77.625}  // About 55 m.
	atGateToo = [2]float64{12.93545, 77.625} // About 50 m.
)

type trip struct {
	*riders
	tok, order, offer, fencing string
}

// started gives an order accepted and packed by the first scooter's rider, with tracking mounted.
func started(t *testing.T, pack bool) *trip {
	x := withRiders(newFixture(t))
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
	if pack {
		tr.ready(200)
	}
	return tr
}

func (tr *trip) token() string { return `{"fencing_token":"` + tr.fencing + `"}` }

func (tr *trip) ready(want int) map[string]any {
	tr.t.Helper()
	code, out := tr.call("POST", "/offers/"+tr.offer+"/ready", tr.tok, tr.token())
	if code != want {
		tr.t.Fatalf("ready: %d %v", code, out)
	}
	return out
}

// at moves the clock on and pings from a point, with the device time now.
func (tr *trip) at(p [2]float64, after time.Duration) map[string]any {
	tr.t.Helper()
	tr.clock = tr.clock.Add(after)
	return tr.pingAt(p, 0, 5, tr.clock)
}

func (tr *trip) pingAt(p [2]float64, speed, accuracy float64, device time.Time) map[string]any {
	tr.t.Helper()
	code, out := tr.call("POST", "/riders/me/location", tr.tok, fmt.Sprintf(`{"lat":%f,"lng":%f,"speed_mps":%f,"accuracy_m":%f,"device_time":"%s"}`,
		p[0], p[1], speed, accuracy, device.UTC().Format(time.RFC3339Nano)))
	if code != 202 {
		tr.t.Fatalf("ping: %d %v", code, out)
	}
	return out
}

func (tr *trip) state() string {
	var s string
	if err := tr.pool.QueryRow(context.Background(), "SELECT state FROM orders WHERE id = $1", tr.order).Scan(&s); err != nil {
		tr.t.Fatal(err)
	}
	return s
}

func (tr *trip) customer() map[string]any {
	tr.t.Helper()
	_, out := tr.send("GET", "/orders/"+tr.order, tr.fixture.token, "")
	return orderOf(out)
}

func (tr *trip) otp() string {
	return tr.id("SELECT handover_otp FROM orders WHERE id = $1", tr.order)
}

func (tr *trip) handOver(otp string, qty int, client time.Time) (int, map[string]any) {
	return tr.call("POST", "/orders/"+tr.order+"/handover", tr.tok, fmt.Sprintf(`{"otp":"%s","quantities":[{"item_id":"%s","qty":%d}],"client_time":"%s"}`,
		otp, tr.chai, qty, client.UTC().Format(time.RFC3339Nano)))
}

func TestTheTimerRunsFromTheZoneToThePerimeter(t *testing.T) {
	tr := started(t, true)
	out := tr.at(far, 20*time.Second)
	riderCheck(t, out, "LocationAccepted")
	if out["next_ping_s"] != float64(3) {
		t.Errorf("a packed rider pings fast: %v", out)
	}
	tr.at(inZone, 30*time.Second)
	if tr.state() != "PACKED" {
		t.Error("one ping inside the zone must not start the timer")
	}
	seq := tr.customerSeq(tr.order)
	tr.at(inZone, 3*time.Second)
	c := tr.customer()
	timer := c["timer"].(map[string]any)
	if tr.state() != "TIMER_RUNNING" || c["customer_stage"] != "timer_running" || timer["timer_started_at"] == nil || timer["timer_duration_s"] != float64(90) {
		t.Fatalf("%v", c)
	}
	if tr.customerSeq(tr.order) != seq+1 {
		t.Error("the timer starting is a change the customer sees")
	}
	if c["handover_otp"] != nil {
		t.Error("the code was shown before arrival")
	}
	tr.at(atGate, 10*time.Second)
	if tr.state() != "TIMER_RUNNING" {
		t.Error("one ping inside the perimeter must not stop the timer")
	}
	tr.at(atGateToo, 3*time.Second)
	c = tr.customer()
	timer = c["timer"].(map[string]any)
	if tr.state() != "ARRIVED" || c["customer_stage"] != "arrived" || timer["timer_stopped_at"] == nil || timer["timer_missed"] != false {
		t.Fatalf("%v", c)
	}
	if otp, _ := c["handover_otp"].(string); len(otp) != 4 {
		t.Errorf("the customer has no code at arrival: %v", c["handover_otp"])
	}
	if _, ro := tr.call("GET", "/orders/"+tr.order, tr.tok, ""); orderOf(ro)["otp_available"] != true {
		t.Errorf("the rider should know the customer has the code: %v", ro)
	}
	// A wrong code, then the right one.
	wrong := "0000"
	if tr.otp() == wrong {
		wrong = "1111"
	}
	if code, out := tr.handOver(wrong, 2, tr.clock); code != 409 || out["code"] != "wrong_otp" {
		t.Errorf("%d %v", code, out)
	}
	code, out := tr.handOver(tr.otp(), 2, tr.clock)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if ro := orderOf(out); ro["state"] != "HANDED_OVER" || ro["handed_over_at"] == nil || ro["ended_reason"] != nil {
		t.Errorf("%v", ro)
	}
	if c := tr.customer(); c["customer_stage"] != "handed_over" || c["handover_otp"] != nil || c["rider"] == nil {
		t.Errorf("%v", c)
	}
	if tr.held() != 0 || tr.count("SELECT sum(on_hand)::int FROM scooter_stock ss JOIN lots l ON l.id = ss.lot_id WHERE l.item_id = $1", tr.chai) != 3 {
		t.Error("two chai must leave the scooter for good")
	}
	if n := tr.count("SELECT coalesce(sum(delta_on_hand), 0)::int FROM stock_ledger WHERE reason = 'handover'"); n != -2 {
		t.Errorf("the ledger says %d handed over", n)
	}
	if code, again := tr.handOver(tr.id("SELECT '1234'"), 2, tr.clock); code != 200 || orderOf(again)["state"] != "HANDED_OVER" {
		t.Errorf("a repeat after success: %d %v", code, again)
	}
	if tr.count("SELECT count(*) FROM orders WHERE id = $1 AND handover_otp IS NULL", tr.order) != 1 {
		t.Error("the code must be cleared once handed over")
	}
	if _, st := tr.call("GET", "/riders/me/state", tr.tok, ""); st["current_order"] != nil {
		t.Errorf("the rider is still holding a handed-over order: %v", st)
	}
	if tr.count("SELECT count(*) FROM location_pings WHERE order_id = $1 AND applied", tr.order) != 5 {
		t.Error("every applied ping must be kept")
	}
}

func TestNoTimerInsideThePerimeter(t *testing.T) {
	tr := started(t, true)
	tr.at(atGate, 20*time.Second)
	tr.at(atGateToo, 3*time.Second)
	if tr.state() != "ARRIVED" || tr.customer()["timer"].(map[string]any)["timer_started_at"] != nil {
		t.Errorf("a rider already at the gate has no timer: %s %v", tr.state(), tr.customer())
	}
}

func TestPingsBeforePackingStartNothing(t *testing.T) {
	tr := started(t, false)
	tr.at(inZone, 20*time.Second)
	tr.at(inZone, 3*time.Second)
	if tr.state() != "RIDER_ACCEPTED" {
		t.Error("the timer started before the rider packed")
	}
	tr.ready(200)
	tr.at(inZone, 3*time.Second)
	if tr.state() != "TIMER_RUNNING" {
		t.Errorf("packed and inside the zone: %s", tr.state())
	}
}

func TestAMissedTimer(t *testing.T) {
	tr := started(t, true)
	tr.at(inZone, 20*time.Second)
	tr.at(inZone, 3*time.Second)
	begun := tr.clock
	seq := tr.customerSeq(tr.order)
	tr.dispatch(begun.Add(89 * time.Second))
	if tr.count("SELECT count(*) FROM orders WHERE id = $1 AND timer_missed", tr.order) != 0 {
		t.Error("missed before its time")
	}
	tr.dispatch(begun.Add(91 * time.Second))
	after := tr.customerSeq(tr.order)
	tr.dispatch(begun.Add(92 * time.Second))
	if c := tr.customer(); c["timer"].(map[string]any)["timer_missed"] != true || after <= seq || tr.customerSeq(tr.order) != after {
		t.Errorf("seq %d then %d, was %d: %v", after, tr.customerSeq(tr.order), seq, c)
	}
	tr.clock = begun.Add(100 * time.Second)
	tr.at(atGate, 0)
	tr.at(atGateToo, 3*time.Second)
	if c := tr.customer(); c["customer_stage"] != "arrived" || c["timer"].(map[string]any)["timer_missed"] != true {
		t.Errorf("%v", c)
	}
}

func TestBadPingsAreKeptButNotUsed(t *testing.T) {
	tr := started(t, true)
	tr.at(inZone, 20*time.Second)
	// Late, inaccurate, repeated and jumping pings change nothing.
	tr.pingAt(inZone, 0, 5, tr.clock.Add(-40*time.Second))
	tr.pingAt(inZone, 0, 500, tr.clock.Add(time.Second))
	tr.pingAt(inZone, 0, 5, tr.clock)
	tr.clock = tr.clock.Add(2 * time.Second)
	tr.pingAt([2]float64{13.5, 77.625}, 0, 5, tr.clock)
	if tr.state() != "PACKED" {
		t.Errorf("a bad ping counted: %s", tr.state())
	}
	if n := tr.count("SELECT count(*) FROM location_pings WHERE order_id = $1 AND NOT applied", tr.order); n != 4 {
		t.Errorf("%d pings kept as evidence only", n)
	}
	tr.at(inZone, 3*time.Second)
	if tr.state() != "TIMER_RUNNING" {
		t.Error("a good ping after bad ones should still count")
	}
	if _, err := tr.pool.Exec(context.Background(), "UPDATE location_pings SET lat = 0"); err == nil {
		t.Error("pings must never change")
	}
}

func TestTheMinimumWaitHoldsTheTimer(t *testing.T) {
	tr := started(t, true)
	tr.exec("UPDATE orders SET min_wait_until = $2 WHERE id = $1", tr.order, tr.clock.Add(120*time.Second))
	tr.at(inZone, 20*time.Second)
	tr.at(inZone, 3*time.Second)
	if tr.state() != "PACKED" {
		t.Error("the timer started during the minimum wait")
	}
	tr.at(inZone, 100*time.Second)
	if tr.state() != "TIMER_RUNNING" {
		t.Errorf("after the wait: %s", tr.state())
	}
}

func TestReadyIsRefusedWhileMoving(t *testing.T) {
	tr := started(t, false)
	tr.clock = tr.clock.Add(time.Second)
	tr.pingAt(far, 8, 5, tr.clock)
	if out := tr.ready(409); out["code"] != "rider_moving" {
		t.Errorf("%v", out)
	}
	tr.clock = tr.clock.Add(15 * time.Second)
	tr.ready(200)
}

func TestHandoverRules(t *testing.T) {
	tr := started(t, false)
	if code, out := tr.handOver(tr.otp(), 2, tr.clock); code != 409 || out["code"] != "handover_not_allowed" {
		t.Errorf("before packing: %d %v", code, out)
	}
	tr.ready(200)
	_, other := tr.signIn(tr.scooterOnShift("SC-2"))
	if code, _ := tr.call("POST", "/orders/"+tr.order+"/handover", other, `{"otp":"1234","quantities":[{"item_id":"`+tr.chai+`","qty":1}],"client_time":"2026-01-01T00:00:00Z"}`); code != 404 {
		t.Errorf("another rider: %d", code)
	}
	for _, body := range []string{`{}`, `{"otp":"12","quantities":[],"client_time":"x"}`,
		`{"otp":"1234","quantities":[{"item_id":"` + tr.samosa + `","qty":1}],"client_time":"2026-01-01T00:00:00Z"}`,
		`{"otp":"` + tr.otp() + `","quantities":[{"item_id":"` + tr.chai + `","qty":3}],"client_time":"2026-01-01T00:00:00Z"}`} {
		if code, out := tr.call("POST", "/orders/"+tr.order+"/handover", tr.tok, body); code != 400 && !(code == 409 && out["code"] == "wrong_otp") {
			t.Errorf("%s: %d %v", body, code, out)
		}
	}
	// Fresh GPS far from the gate refuses a current handover.
	tr.at(far, 20*time.Second)
	if code, out := tr.handOver(tr.otp(), 2, tr.clock); code != 409 || out["code"] != "rider_not_near" {
		t.Errorf("%d %v", code, out)
	}
	// A partial handover: one of two chai.
	tr.at(atGate, 40*time.Second)
	tr.at(atGateToo, 3*time.Second)
	if code, out := tr.handOver(tr.otp(), 1, tr.clock); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if n := tr.count("SELECT coalesce(qty_handed, -1) FROM order_items WHERE order_id = $1", tr.order); n != 1 {
		t.Errorf("the partial handover was not recorded: %d", n)
	}
	if tr.held() != 0 {
		t.Errorf("%d still held after the handover", tr.held())
	}
	if n := tr.count("SELECT sum(on_hand)::int FROM scooter_stock ss JOIN lots l ON l.id = ss.lot_id WHERE l.item_id = $1", tr.chai); n != 4 {
		t.Errorf("only the handed unit leaves the scooter: %d left", n)
	}
}

func TestTooManyWrongCodes(t *testing.T) {
	tr := started(t, true)
	tr.at(atGate, 20*time.Second)
	tr.at(atGateToo, 3*time.Second)
	wrong := "0000"
	if tr.otp() == wrong {
		wrong = "1111"
	}
	for i := 0; i < maxOTPAttempts; i++ {
		if code, out := tr.handOver(wrong, 2, tr.clock); code != 409 || out["code"] != "wrong_otp" {
			t.Fatalf("%d %v", code, out)
		}
	}
	if code, out := tr.handOver(tr.otp(), 2, tr.clock); code != 409 || out["code"] != "otp_attempts_exceeded" {
		t.Errorf("%d %v", code, out)
	}
}

func TestAHandoverWithNoSignal(t *testing.T) {
	tr := started(t, true)
	// No ping at all: a valid code still hands over, records arrival from the handover, and flags it.
	tr.clock = tr.clock.Add(60 * time.Second)
	if code, out := tr.handOver(tr.otp(), 2, tr.clock); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if tr.count("SELECT count(*) FROM orders WHERE id = $1 AND arrival_source = 'handover' AND audit_flag = 'arrival_from_handover'", tr.order) != 1 {
		t.Error("a handover with no GPS must be flagged")
	}
}

func TestTheCodeIsReleasedWhenTheSignalIsLost(t *testing.T) {
	tr := started(t, true)
	tr.at(inZone, 20*time.Second)
	tr.at(inZone, 3*time.Second)
	begun := tr.clock
	// No ping after the timer ran out: the customer gets the code.
	tr.dispatch(begun.Add(80 * time.Second))
	if tr.customer()["handover_otp"] != nil {
		t.Error("released before the timer ended")
	}
	tr.clock = begun.Add(95 * time.Second)
	tr.dispatch(tr.clock)
	if otp, _ := tr.customer()["handover_otp"].(string); len(otp) != 4 {
		t.Errorf("not released: %v", tr.customer())
	}
	if _, ro := tr.call("GET", "/orders/"+tr.order, tr.tok, ""); orderOf(ro)["otp_available"] != true {
		t.Errorf("%v", ro)
	}
}

func TestPingsWithNoOrder(t *testing.T) {
	tr := started(t, true)
	_, other := tr.signIn(tr.scooterOnShift("SC-2"))
	code, out := tr.call("POST", "/riders/me/location", other, fmt.Sprintf(`{"lat":1,"lng":2,"speed_mps":0,"accuracy_m":5,"device_time":"%s"}`, tr.clock.UTC().Format(time.RFC3339)))
	if code != 202 || out["next_ping_s"] != float64(15) {
		t.Errorf("%d %v", code, out)
	}
	if tr.count("SELECT count(*) FROM location_pings WHERE order_id IS NULL") != 1 {
		t.Error("a ping with no order is still kept")
	}
	for _, body := range []string{`{}`, `{"lat":91,"lng":2,"speed_mps":0,"accuracy_m":5,"device_time":"2026-01-01T00:00:00Z"}`, `{"lat":1,"lng":2,"speed_mps":-1,"accuracy_m":5,"device_time":"2026-01-01T00:00:00Z"}`} {
		if code, _ := tr.call("POST", "/riders/me/location", other, body); code != 400 {
			t.Errorf("%s: %d", body, code)
		}
	}
}

func TestALateFlushNeverStartsATimer(t *testing.T) {
	tr := started(t, true)
	// Two pings inside the zone, both queued a minute ago: evidence only.
	tr.clock = tr.clock.Add(90 * time.Second)
	tr.pingAt(inZone, 0, 5, tr.clock.Add(-61*time.Second))
	tr.pingAt(inZone, 0, 5, tr.clock.Add(-60*time.Second))
	if tr.state() != "PACKED" {
		t.Errorf("a flush of old pings started the timer: %s", tr.state())
	}
}

func TestPingsMustBeInARow(t *testing.T) {
	tr := started(t, true)
	tr.at(inZone, 20*time.Second)
	tr.at(far, 30*time.Second)
	tr.at(inZone, 30*time.Second)
	if tr.state() != "PACKED" {
		t.Errorf("two pings inside the zone with one outside between them: %s", tr.state())
	}
}

func TestTheCodeStaysHiddenWhileTheRiderPings(t *testing.T) {
	tr := started(t, true)
	tr.at(inZone, 20*time.Second)
	tr.at(inZone, 3*time.Second)
	// The timer runs out, but the rider keeps pinging: GPS works, so the code waits for arrival.
	for i := 0; i < 10; i++ {
		tr.at(inZone, 10*time.Second)
	}
	tr.dispatch(tr.clock)
	if tr.customer()["handover_otp"] != nil {
		t.Error("the code was released while the rider's GPS was working")
	}
}

func TestAPingFromTheFutureIsEvidenceOnly(t *testing.T) {
	tr := started(t, true)
	tr.at(inZone, 20*time.Second)
	tr.pingAt(inZone, 0, 5, tr.clock.Add(2*time.Minute))
	if tr.state() != "PACKED" {
		t.Error("a ping dated in the future counted")
	}
	// True pings after it still count.
	tr.at(inZone, 3*time.Second)
	if tr.state() != "TIMER_RUNNING" {
		t.Errorf("a ping from the future blocked true pings: %s", tr.state())
	}
}

func TestPoorPingsStillKeepTheCodeHidden(t *testing.T) {
	tr := started(t, true)
	tr.at(inZone, 20*time.Second)
	tr.at(inZone, 3*time.Second)
	for i := 0; i < 12; i++ {
		tr.clock = tr.clock.Add(10 * time.Second)
		tr.pingAt(inZone, 0, 500, tr.clock)
	}
	tr.dispatch(tr.clock)
	if tr.customer()["handover_otp"] != nil {
		t.Error("the code was released while the rider's phone was still pinging")
	}
}

func TestAPoorFixDoesNotRefuseAHandover(t *testing.T) {
	tr := started(t, true)
	tr.at(atGate, 20*time.Second)
	tr.at(atGateToo, 3*time.Second)
	tr.clock = tr.clock.Add(time.Second)
	tr.pingAt(far, 0, 500, tr.clock)
	if code, out := tr.handOver(tr.otp(), 2, tr.clock); code != 200 {
		t.Errorf("an inaccurate fix refused an honest rider: %d %v", code, out)
	}
}

func TestAHandoverAfterACancel(t *testing.T) {
	tr := started(t, true)
	otp := tr.otp()
	tr.clock = tr.clock.Add(301 * time.Second)
	if code, out := tr.send("POST", "/orders/"+tr.order+"/cancel", tr.fixture.token, `{}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if code, out := tr.handOver(otp, 2, tr.clock); code != 409 || out["code"] != "order_cancelled" {
		t.Errorf("%d %v", code, out)
	}
}

func TestAHandoverOnceTheCodeWasReleasedBeforePacking(t *testing.T) {
	tr := started(t, false)
	tr.clock = tr.clock.Add(301 * time.Second)
	tr.dispatch(tr.clock)
	if tr.customer()["handover_otp"] == nil {
		t.Fatal("the code was not released after the wait deadline with no ping")
	}
	if code, out := tr.handOver(tr.otp(), 2, tr.clock); code != 200 {
		t.Errorf("%d %v", code, out)
	}
}

func TestArrivingExactlyAtTheEndIsMissed(t *testing.T) {
	tr := started(t, true)
	tr.at(inZone, 20*time.Second)
	tr.at(inZone, 3*time.Second)
	begun := tr.clock
	tr.at(atGate, 87*time.Second)
	tr.clock = begun.Add(90 * time.Second)
	tr.pingAt(atGateToo, 0, 5, tr.clock)
	if tr.count("SELECT count(*) FROM orders WHERE id = $1 AND timer_missed", tr.order) != 1 {
		t.Error("arriving at the timer's end must count as missed, as the pass does")
	}
}

// An arrival review decided arrived_in_time stops the timer at the rider's evidence, while the
// order still runs. Neither the miss pass nor a later arrival may call it missed again.
func TestATimerStoppedByAReviewStaysInTime(t *testing.T) {
	tr := started(t, true)
	tr.at(inZone, 20*time.Second)
	tr.at(inZone, 3*time.Second)
	begun := tr.clock
	tr.exec("UPDATE orders SET timer_stopped_at = timer_started_at + interval '60 seconds' WHERE id = $1", tr.order) // As decideArrivalReview does.
	tr.dispatch(begun.Add(95 * time.Second))
	tr.dispatch(begun.Add(96 * time.Second))
	if tr.count("SELECT count(*) FROM orders WHERE id = $1 AND timer_missed", tr.order) != 0 {
		t.Error("the miss pass undid a stopped timer")
	}
	tr.clock = begun.Add(120 * time.Second)
	tr.at(atGate, 0)
	tr.at(atGateToo, 3*time.Second)
	if c := tr.customer(); c["customer_stage"] != "arrived" || c["timer"].(map[string]any)["timer_missed"] != false {
		t.Errorf("arriving after the length must not undo a timer stopped in time: %v", c)
	}
}

// Masked calls are a stand-in until a telephony provider (#151): each side may ask about its own
// order only, and is told the call is not available.
func TestMaskedCallsAreNotAvailableYet(t *testing.T) {
	tr := started(t, true)
	if code, out := tr.call("POST", "/orders/"+tr.order+"/call", tr.tok, ""); code != 409 || out["code"] != "call_not_available" {
		t.Errorf("the rider's own order: %d %v", code, out)
	}
	if code, _ := tr.call("POST", "/orders/00000000-0000-0000-0000-000000000009/call", tr.tok, ""); code != 404 {
		t.Errorf("an order the rider never accepted: %d", code)
	}
	if code, out := tr.send("POST", "/orders/"+tr.order+"/call", tr.fixture.token, ""); code != 409 || out["code"] != "call_not_available" {
		t.Errorf("the customer's own order: %d %v", code, out)
	}
	if code, _ := tr.send("POST", "/orders/00000000-0000-0000-0000-000000000009/call", tr.fixture.token, ""); code != 404 {
		t.Errorf("an order that is not the customer's: %d", code)
	}
}

// The route is a straight line until the routing engine (#156), with no time shown.
func TestTheRouteIsAStraightLineForNow(t *testing.T) {
	tr := started(t, true)
	tr.at(far, 20*time.Second)
	code, out := tr.call("GET", "/orders/"+tr.order+"/route", tr.tok, "")
	if code != 200 || out["eta_source"] != "placeholder" || out["eta_s"] != nil {
		t.Fatalf("%d %v", code, out)
	}
	var gLat, gLng float64
	if err := tr.pool.QueryRow(context.Background(), "SELECT h.lat, h.lng FROM orders o JOIN gate_history h ON h.id = o.gate_history_id WHERE o.id = $1", tr.order).Scan(&gLat, &gLng); err != nil {
		t.Fatal(err)
	}
	if want := encodePolyline([][2]float64{{far[0], far[1]}, {gLat, gLng}}); out["polyline"] != want {
		t.Errorf("from the last ping to the gate: %v, want %s", out["polyline"], want)
	}
	if code, _ := tr.call("GET", "/orders/00000000-0000-0000-0000-000000000009/route", tr.tok, ""); code != 404 {
		t.Errorf("an order the rider never accepted: %d", code)
	}
	_, otherTok := tr.signIn(tr.scooterOnShift("SC-ROUTE-2"))
	if code, _ := tr.call("GET", "/orders/"+tr.order+"/route", otherTok, ""); code != 404 {
		t.Errorf("another rider's order: %d", code)
	}
}

func TestPolylineEncoding(t *testing.T) {
	// Google's own example: (38.5, -120.2), (40.7, -120.95), (43.252, -126.453).
	if got := encodePolyline([][2]float64{{38.5, -120.2}, {40.7, -120.95}, {43.252, -126.453}}); got != "_p~iF~ps|U_ulLnnqC_mqNvxq`@" {
		t.Errorf("%s", got)
	}
}
