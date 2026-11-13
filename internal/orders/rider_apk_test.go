package orders

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/teamprosperlink/Ninzo_backend/internal/contracttest"
)

// Issue 235, part B1: every offer and order a rider reads carries what the customer paid and how it
// was paid, and the open shift carries what the rider app reads of it.

const riderApkContract = "rider.openapi.yaml"

// noCustomerData fails the test if a reply holds a phone number, a phone key or the customer's name.
func noCustomerData(t *testing.T, where string, reply map[string]any) {
	t.Helper()
	raw, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, bad := range []string{`"phone`, "+91", `"customer`, `"address`} {
		if strings.Contains(text, bad) {
			t.Errorf("%s shows customer data (%s): %s", where, bad, text)
		}
	}
}

func TestEveryOfferAndOrderTheRiderReadsShowsTheTotalAndPrepaid(t *testing.T) {
	tr := started(t, false)
	want := tr.count("SELECT total_paise FROM orders WHERE id = $1", tr.order)
	if want <= 0 {
		t.Fatalf("the order has no total: %d", want)
	}
	isTotal := func(where string, v map[string]any) {
		t.Helper()
		total, _ := v["total"].(map[string]any)
		if total == nil || total["amount_minor"] != float64(want) || total["currency"] != "INR" || v["payment_method"] != "prepaid" {
			t.Errorf("%s: total %v, payment_method %v, want %d prepaid", where, v["total"], v["payment_method"], want)
		}
		noCustomerData(t, where, v)
	}
	// The order was accepted by started(). getRiderState shows it as the current order, getRiderOrder alone.
	_, state := tr.call("GET", "/riders/me/state", tr.tok, "")
	contracttest.Check(t, riderApkContract, "RiderState", state)
	isTotal("getRiderState current_order", state["current_order"].(map[string]any))
	code, out := tr.call("GET", "/orders/"+tr.order, tr.tok, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	contracttest.Check(t, riderApkContract, "OrderResponse", out)
	isTotal("getRiderOrder", out["order"].(map[string]any))
	// markReady.
	ready := tr.ready(200)
	contracttest.Check(t, riderApkContract, "OrderResponse", ready)
	isTotal("markReady", ready["order"].(map[string]any))
	// The handover reply, with the rider at the gate.
	tr.at(atGate, 40*time.Second)
	tr.at(atGateToo, 3*time.Second)
	code, out = tr.handOver(tr.otp(), 2, tr.clock)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	contracttest.Check(t, riderApkContract, "OrderResponse", out)
	isTotal("handOver", out["order"].(map[string]any))
}

func TestAnOpenOfferShowsTheTotalInStateAndInTheList(t *testing.T) {
	x := withRiders(newFixture(t))
	start := x.clock
	id := x.placed(x.chai, 2)
	x.pay(id, "paid")
	_, tok := x.signIn(x.scooter)
	x.dispatch(start.Add(6 * time.Second))
	want := x.count("SELECT total_paise FROM orders WHERE id = $1", id)
	offer := x.offer(tok)
	code, out := x.call("GET", "/riders/me/offers", tok, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	contracttest.Check(t, riderApkContract, "OfferList", out)
	listed := out["offers"].([]any)
	if len(listed) != 1 {
		t.Fatalf("%v", out)
	}
	for where, o := range map[string]map[string]any{"getRiderState offer": offer, "listOffers": listed[0].(map[string]any)} {
		contracttest.Check(t, riderApkContract, "Offer", o)
		total, _ := o["total"].(map[string]any)
		if total == nil || total["amount_minor"] != float64(want) || total["currency"] != "INR" || o["payment_method"] != "prepaid" {
			t.Errorf("%s: %v", where, o)
		}
		noCustomerData(t, where, o)
	}
	// Accepting returns the order with the same two fields.
	fencing := offer["fencing_token"].(string)
	x.clock = start.Add(10 * time.Second)
	code, out = x.call("POST", "/offers/"+offer["id"].(string)+"/accept", tok, `{"fencing_token":"`+fencing+`"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	contracttest.Check(t, riderApkContract, "OrderResponse", out)
	if o := out["order"].(map[string]any); o["payment_method"] != "prepaid" || o["total"].(map[string]any)["amount_minor"] != float64(want) {
		t.Errorf("acceptOffer: %v", o)
	}
}

func TestTheRiderStreamCarriesTheTotalAndPrepaid(t *testing.T) {
	l := withStreams(t)
	s := open(t, l.rideSrv, "/riders/me/stream", l.tok)
	if f, code := s.next("auth_ok"); f == nil {
		t.Fatalf("%d", code)
	}
	l.pay(l.order, "paid")
	l.dispatch(l.clock.Add(6 * time.Second))
	want := l.count("SELECT total_paise FROM orders WHERE id = $1", l.order)
	f, _ := s.next("offer")
	if f == nil {
		t.Fatal("no offer on the stream")
	}
	o := f["data"].(map[string]any)
	contracttest.Check(t, riderApkContract, "Offer", o)
	if o["payment_method"] != "prepaid" || o["total"].(map[string]any)["amount_minor"] != float64(want) {
		t.Errorf("stream offer: %v", o)
	}
	l.offer, l.fencing = o["id"].(string), o["fencing_token"].(string)
	l.clock = l.clock.Add(10 * time.Second)
	l.call("POST", "/offers/"+l.offer+"/accept", l.tok, l.token())
	f, _ = s.next("order_state")
	if f == nil {
		t.Fatal("no order_state on the stream")
	}
	order := f["data"].(map[string]any)
	contracttest.Check(t, riderApkContract, "RiderOrder", order)
	if order["payment_method"] != "prepaid" || order["total"].(map[string]any)["amount_minor"] != float64(want) {
		t.Errorf("stream order: %v", order)
	}
	noCustomerData(t, "stream order", order)
}

func TestTheRiderStateShowsTheShiftFieldsTheRiderAppReads(t *testing.T) {
	x := withRiders(newFixture(t))
	_, tok := x.signIn(x.scooter)
	shiftOf := func() map[string]any {
		_, st := x.call("GET", "/riders/me/state", tok, "")
		contracttest.Check(t, riderApkContract, "RiderState", st)
		return st["shift"].(map[string]any)
	}
	sh := shiftOf()
	for _, field := range []string{"online_at", "planned_end_at", "break"} {
		if v, ok := sh[field]; !ok || v != nil {
			t.Errorf("%s must be sent as null: %v", field, sh)
		}
	}
	x.exec(`UPDATE shifts SET online_at = '2026-11-13T04:00:00Z', planned_end_at = '2026-11-13T12:30:00Z',
		break_started_at = '2026-11-13T05:00:00Z', break_ends_at = '2026-11-13T05:15:00Z' WHERE ended_at IS NULL`)
	sh = shiftOf()
	b, _ := sh["break"].(map[string]any)
	if sh["online_at"] != "2026-11-13T04:00:00Z" || sh["planned_end_at"] != "2026-11-13T12:30:00Z" ||
		b == nil || b["started_at"] != sh["break_started_at"] || b["ends_at"] != sh["break_ends_at"] || b["ends_at"] != "2026-11-13T05:15:00Z" {
		t.Errorf("%v", sh)
	}
}
