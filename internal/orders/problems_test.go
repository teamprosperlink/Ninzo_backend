package orders

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

// accepted gives an order accepted by the first scooter's rider, with a second scooter holding
// the same stock and its own rider, so a rematch has somewhere to go. hidden keeps the minimum
// wait running, so the customer has not seen the rider.
func accepted(t *testing.T, hidden bool) (*trip, string) {
	t.Helper()
	x := withRiders(newFixture(t))
	second := x.scooterOnShift("SC-2")
	x.load(second, "L-CHAI-B", x.chai, 5)
	srv := httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		x.svc.MountRider(r, identity.RequireActor(x.sessions.Signer, identity.TypeRider))
		x.svc.MountTracking(r, identity.RequireActor(x.sessions.Signer, identity.TypeRider))
	}))
	t.Cleanup(srv.Close)
	x.srv = srv
	start := x.clock
	id := x.placed(x.chai, 2)
	x.pay(id, "paid")
	if hidden {
		x.exec("UPDATE orders SET min_wait_until = $2 WHERE id = $1", id, start.Add(time.Hour))
	}
	own := x.id("SELECT scooter_id::text FROM orders WHERE id = $1", id)
	other := second
	if own == second {
		other = x.scooter
	}
	_, tok := x.signIn(own)
	x.dispatch(start.Add(6 * time.Second))
	o := x.offer(tok)
	x.clock = start.Add(10 * time.Second)
	tr := &trip{riders: x, tok: tok, order: id, offer: o["id"].(string), fencing: o["fencing_token"].(string)}
	if code, out := x.call("POST", "/offers/"+tr.offer+"/accept", tok, tr.token()); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	return tr, other
}

func (tr *trip) report(kind, order string) (int, map[string]any) {
	body := `{"type":"` + kind + `","note":"chain came off"`
	if order != "" {
		body += `,"order_id":"` + order + `"`
	}
	return tr.call("POST", "/riders/me/problems", tr.tok, body+"}")
}

func TestABreakdownBeforeTheRiderIsShownRematchesSilently(t *testing.T) {
	tr, other := accepted(t, true)
	seq := tr.customerSeq(tr.order)
	code, out := tr.report("breakdown", tr.order)
	if code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	riderCheck(t, out, "ProblemResponse")
	if tr.state() != "PAYMENT_SECURED" || tr.customerSeq(tr.order) != seq {
		t.Errorf("a hidden rematch must not show: %s, seq %d was %d", tr.state(), tr.customerSeq(tr.order), seq)
	}
	if c := tr.customer(); c["customer_stage"] != "looking_for_rider" || c["rider"] != nil {
		t.Errorf("%v", c)
	}
	if tr.count("SELECT count(*) FROM scooters WHERE status = 'maintenance'") != 1 {
		t.Error("the broken scooter must leave service")
	}
	if _, ro := tr.call("GET", "/orders/"+tr.order, tr.tok, ""); orderOf(ro)["ended_reason"] != "reassigned" {
		t.Errorf("the rider sees their own ending: %v", ro)
	}
	// The next pass moves the hold to the working scooter and offers it to its rider.
	tr.dispatch(tr.clock)
	if tr.count("SELECT count(*) FROM offers WHERE order_id = $1 AND state = 'open' AND scooter_id = $2", tr.order, other) != 1 {
		t.Error("the order was not offered to the working scooter")
	}
	if tr.count("SELECT coalesce(sum(reserved), 0)::int FROM scooter_stock WHERE scooter_id = $1", other) != 2 {
		t.Error("the hold did not move")
	}
	if tr.count("SELECT count(*) FROM problem_reports WHERE outcome = 'rematched' AND note = 'chain came off'") != 1 {
		t.Error("the report was not kept")
	}
}

func TestABreakdownAfterTheRiderIsShownFailsAndRefunds(t *testing.T) {
	tr, _ := accepted(t, false)
	if c := tr.customer(); c["customer_stage"] != "rider_accepted" {
		t.Fatalf("%v", c)
	}
	if code, out := tr.report("accident", ""); code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	c := tr.customer()
	if c["customer_stage"] != "ended" || c["ended_reason"] != "failed" || c["payment_status"] != "refunded" {
		t.Fatalf("%v", c)
	}
	if _, r := tr.send("GET", "/orders/"+tr.order+"/refund", tr.fixture.token, ""); r["refund"].(map[string]any)["reason_code"] != "order_failed" {
		t.Errorf("%v", r)
	}
	if tr.held() != 0 {
		t.Error("the stock was not given back")
	}
}

func TestOtherProblemsAreOnlyRecorded(t *testing.T) {
	tr, _ := accepted(t, false)
	for _, kind := range []string{"gate_closed", "customer_absent", "unsafe_route"} {
		if code, out := tr.report(kind, tr.order); code != 201 {
			t.Errorf("%s: %d %v", kind, code, out)
		}
	}
	if tr.state() != "RIDER_ACCEPTED" || tr.count("SELECT count(*) FROM problem_reports WHERE outcome = 'recorded'") != 3 {
		t.Error("these must not change the order")
	}
	// A problem on an order that is not the rider's is refused, and bad input is a 400.
	x := tr.riders
	_, other := x.signIn(x.scooterOnShift("SC-3"))
	if code, out := x.call("POST", "/riders/me/problems", other, `{"type":"gate_closed","order_id":"`+tr.order+`"}`); code != 409 || out["code"] != "order_not_yours" {
		t.Errorf("%d %v", code, out)
	}
	for _, body := range []string{`{}`, `{"type":"bored"}`, `{"type":"gate_closed","order_id":"x"}`} {
		if code, _ := x.call("POST", "/riders/me/problems", other, body); code != 400 {
			t.Errorf("%s: %d", body, code)
		}
	}
}

func TestNoPingForTwoMinutesReassigns(t *testing.T) {
	tr, other := accepted(t, true)
	tr.at(far, 20*time.Second)
	last := tr.clock
	tr.dispatch(last.Add(119 * time.Second))
	if tr.state() != "RIDER_ACCEPTED" {
		t.Fatal("reassigned too early")
	}
	tr.dispatch(last.Add(121 * time.Second)) // Reassigns.
	tr.dispatch(last.Add(122 * time.Second)) // Offers it to the next rider.
	if tr.count("SELECT count(*) FROM offers WHERE order_id = $1 AND state = 'open' AND scooter_id = $2", tr.order, other) != 1 {
		t.Errorf("not reassigned to the other scooter: %s", tr.state())
	}
	if tr.count("SELECT count(*) FROM problem_reports WHERE type = 'signal_lost' AND source = 'system'") != 1 {
		t.Error("support must see why")
	}
}

func TestAReassignAfterTheRiderWasShownMovesSeq(t *testing.T) {
	tr, _ := accepted(t, false)
	tr.at(far, 20*time.Second)
	seq := tr.customerSeq(tr.order)
	tr.dispatch(tr.clock.Add(3 * time.Minute))
	if c := tr.customer(); c["customer_stage"] != "looking_for_rider" || c["rider"] != nil || tr.customerSeq(tr.order) <= seq {
		t.Errorf("the customer must see the change: %v", c)
	}
}

func TestNoReassignOnceTheTimerRuns(t *testing.T) {
	tr, _ := accepted(t, false)
	tr.ready(200)
	tr.at(inZone, 20*time.Second)
	tr.at(inZone, 3*time.Second)
	tr.dispatch(tr.clock.Add(3 * time.Minute))
	tr.dispatch(tr.clock.Add(4 * time.Minute))
	if tr.state() != "TIMER_RUNNING" {
		t.Errorf("a rider near the gate must not lose the order: %s", tr.state())
	}
	if tr.count("SELECT count(*) FROM problem_reports WHERE type = 'signal_lost'") != 1 {
		t.Error("support must be alerted once")
	}
}

func TestABrokenScooterGetsNoOffers(t *testing.T) {
	tr, _ := accepted(t, true)
	tr.report("breakdown", "")
	broken := tr.id("SELECT id::text FROM scooters WHERE status = 'maintenance'")
	tr.dispatch(tr.clock)
	if tr.count("SELECT count(*) FROM offers WHERE scooter_id = $1 AND state = 'open'", broken) != 0 {
		t.Error("a scooter in maintenance was offered an order")
	}
}

func TestARiderWhoNeverPingedIsNotReassigned(t *testing.T) {
	tr, _ := accepted(t, true)
	tr.dispatch(tr.clock.Add(3 * time.Minute))
	if tr.state() != "RIDER_ACCEPTED" {
		t.Errorf("the rule counts from the last on-time ping: %s", tr.state())
	}
}

func TestNoReassignOnceTheCustomerHasTheCode(t *testing.T) {
	tr, _ := accepted(t, false)
	tr.at(far, 20*time.Second)
	tr.exec("UPDATE orders SET otp_released_at = now() WHERE id = $1", tr.order)
	tr.dispatch(tr.clock.Add(3 * time.Minute))
	if tr.state() != "RIDER_ACCEPTED" {
		t.Errorf("the customer has the code, so the rider keeps the order: %s", tr.state())
	}
}

func TestAScooterBrokenBeforeTheOfferGetsNone(t *testing.T) {
	x := withRiders(newFixture(t))
	start := x.clock
	id := x.placed(x.chai, 1)
	x.pay(id, "paid")
	_, tok := x.signIn(x.scooter)
	// The rider reports a breakdown before any offer goes out: the order's own scooter.
	if code, out := x.call("POST", "/riders/me/problems", tok, `{"type":"breakdown"}`); code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	x.dispatch(start.Add(6 * time.Second))
	if x.count("SELECT count(*) FROM offers WHERE order_id = $1", id) != 0 {
		t.Error("a scooter in maintenance was offered the order")
	}
}

func TestAnEndedOrderIsNeverTakenBack(t *testing.T) {
	tr, _ := accepted(t, false)
	tr.clock = tr.clock.Add(301 * time.Second)
	if code, out := tr.send("POST", "/orders/"+tr.order+"/cancel", tr.fixture.token, `{}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	// Whatever arrives late, a rematch or a failure leaves the cancelled order as it is.
	ctx := context.Background()
	for _, act := range []func(pgx.Tx) error{
		func(tx pgx.Tx) error { return rematch(ctx, tx, tr.order, "late report", tr.clock) },
		func(tx pgx.Tx) error { return failOrder(ctx, tx, tr.order, "late report") },
	} {
		if err := database.InTx(ctx, tr.pool, act); err != nil {
			t.Fatal(err)
		}
	}
	if tr.state() != "CANCELLED" || tr.count("SELECT count(*) FROM refunds WHERE order_id = $1", tr.order) != 1 {
		t.Errorf("an ended order changed: %s", tr.state())
	}
	if code, _ := tr.report("breakdown", tr.order); code != 201 || tr.state() != "CANCELLED" {
		t.Errorf("a report after a cancel changed the order: %s", tr.state())
	}
}

func TestTheNewRidersPingsCountAfterARematch(t *testing.T) {
	tr, other := accepted(t, true)
	tr.at(far, 20*time.Second)
	tr.report("breakdown", tr.order)
	tr.dispatch(tr.clock)
	_, tok := tr.signIn(other)
	o := tr.riders.offer(tok)
	tr.clock = tr.clock.Add(3 * time.Second)
	if code, out := tr.call("POST", "/offers/"+o["id"].(string)+"/accept", tok, `{"fencing_token":"`+o["fencing_token"].(string)+`"}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	// One second later, the new rider pings from the gate, a kilometre from the old rider.
	tr.clock = tr.clock.Add(time.Second)
	tr.tok = tok
	tr.pingAt(atGate, 0, 5, tr.clock)
	if n := tr.count("SELECT count(*) FROM location_pings WHERE order_id = $1 AND applied AND note IS NULL", tr.order); n != 2 {
		t.Errorf("the new rider's ping was judged against the old rider's: %d applied", n)
	}
}

func TestEachRiderGetsTheirOwnSignalAlert(t *testing.T) {
	tr, _ := accepted(t, false)
	tr.exec(`INSERT INTO problem_reports (rider_id, order_id, type, source, outcome)
		SELECT r.id, $1, 'signal_lost', 'system', 'support_alerted' FROM riders r WHERE r.id <> (SELECT rider_id FROM orders WHERE id = $1) LIMIT 1`, tr.order)
	tr.ready(200)
	tr.at(inZone, 20*time.Second)
	tr.at(inZone, 3*time.Second)
	tr.dispatch(tr.clock.Add(3 * time.Minute))
	if tr.count("SELECT count(*) FROM problem_reports WHERE type = 'signal_lost' AND order_id = $1", tr.order) != 2 {
		t.Error("an alert about an earlier rider hid this rider's lost signal")
	}
}

func TestALostSignalRaisesOneSupportAlert(t *testing.T) {
	tr, _ := accepted(t, false)
	tr.ready(200)
	tr.at(inZone, 20*time.Second)
	tr.at(inZone, 3*time.Second)
	tr.dispatch(tr.clock.Add(3 * time.Minute))
	tr.dispatch(tr.clock.Add(4 * time.Minute))
	if tr.count("SELECT count(*) FROM outbox_events WHERE topic = 'support.alert' AND payload->>'reason' = 'signal_lost' AND payload->>'order_id' = $1", tr.order) != 1 {
		t.Error("a lost signal must raise exactly one support alert, naming the order")
	}
}
