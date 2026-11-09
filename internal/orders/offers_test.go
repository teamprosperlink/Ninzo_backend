package orders

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/shifts"
)

type riders struct {
	*fixture
	srv *httptest.Server
}

func withRiders(f *fixture) *riders {
	srv := httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		f.svc.MountRider(r, identity.RequireActor(f.sessions.Signer, identity.TypeRider))
		(&shifts.Service{Pool: f.pool, Log: quiet}).MountRider(r, identity.RequireActor(f.sessions.Signer, identity.TypeRider))
	}))
	f.t.Cleanup(srv.Close)
	return &riders{f, srv}
}

// token signs in the rider on a scooter's shift.
func (x *riders) signIn(scooter string) (string, string) {
	x.t.Helper()
	rider := x.id("SELECT rider_id::text FROM shifts WHERE scooter_id = $1 AND ended_at IS NULL", scooter)
	var pair identity.Pair
	if err := database.InTx(context.Background(), x.pool, func(tx pgx.Tx) error {
		var err error
		pair, _, err = x.sessions.Start(context.Background(), tx, identity.TypeRider, rider)
		return err
	}); err != nil {
		x.t.Fatal(err)
	}
	return rider, pair.AccessToken
}

func (x *riders) call(method, path, token, body string) (int, map[string]any) {
	x.t.Helper()
	x.keys++
	req, _ := http.NewRequest(method, x.srv.URL+"/v1"+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", fmt.Sprintf("rider-key-%05d", x.keys))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		x.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (x *riders) dispatch(at time.Time) int {
	x.t.Helper()
	n, err := x.svc.Dispatch(context.Background(), at)
	if err != nil {
		x.t.Fatal(err)
	}
	return n
}

func (x *riders) offer(token string) map[string]any {
	x.t.Helper()
	code, out := x.call("GET", "/riders/me/state", token, "")
	if code != 200 {
		x.t.Fatalf("%d %v", code, out)
	}
	offers := out["offers"].([]any)
	if len(offers) != 1 {
		x.t.Fatalf("%d offers: %v", len(offers), out)
	}
	return offers[0].(map[string]any)
}

func riderRequired(t *testing.T, schema string) []string {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/rider/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Required []string `yaml:"required"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Components.Schemas[schema].Required
}

func riderCheck(t *testing.T, got map[string]any, schema string) {
	t.Helper()
	fields := riderRequired(t, schema)
	if len(fields) == 0 {
		t.Fatalf("no required fields for %s", schema)
	}
	for _, field := range fields {
		if _, ok := got[field]; !ok {
			t.Errorf("%s has no %s", schema, field)
		}
	}
}

func (f *fixture) customerSeq(id string) int {
	return f.count("SELECT seq::int FROM orders WHERE id = $1", id)
}

func TestAnOfferGoesOutAfterTheWindow(t *testing.T) {
	x := withRiders(newFixture(t))
	start := x.clock
	id := x.placed(x.chai, 2)
	x.pay(id, "paid")
	_, tok := x.signIn(x.scooter)
	if n := x.dispatch(start.Add(4 * time.Second)); n != 0 {
		t.Errorf("an offer went out inside the cancel window: %d", n)
	}
	seq := x.customerSeq(id)
	if n := x.dispatch(start.Add(6 * time.Second)); n != 1 {
		t.Fatalf("%d", n)
	}
	if x.customerSeq(id) != seq {
		t.Error("an offer is hidden from the customer, so seq must not move")
	}
	if _, out := x.send("GET", "/orders/"+id, x.token, ""); orderOf(out)["customer_stage"] != "looking_for_rider" || orderOf(out)["rider"] != nil {
		t.Errorf("%v", out)
	}
	code, state := x.call("GET", "/riders/me/state", tok, "")
	if code != 200 {
		t.Fatalf("%d %v", code, state)
	}
	riderCheck(t, state, "RiderState")
	o := x.offer(tok)
	riderCheck(t, o, "Offer")
	for _, field := range []string{"id", "label", "lat", "lng", "entry_notes"} {
		if _, ok := o["gate"].(map[string]any)[field]; !ok {
			t.Errorf("the offer's gate has no %s", field)
		}
	}
	if o["state"] != "open" || o["order_id"] != nil || len(o["items"].([]any)) != 1 || o["gate"].(map[string]any)["label"] != "Home" {
		t.Errorf("%v", o)
	}
	code, list := x.call("GET", "/riders/me/offers", tok, "")
	if code != 200 || len(list["offers"].([]any)) != 1 {
		t.Errorf("%d %v", code, list)
	}
	riderCheck(t, list, "OfferList")
	// A second pass offers nothing more.
	if n := x.dispatch(start.Add(7 * time.Second)); n != 0 {
		t.Errorf("%d", n)
	}
}

func TestAcceptingReadyAndTheMinimumWait(t *testing.T) {
	x := withRiders(newFixture(t))
	start := x.clock
	id := x.placed(x.chai, 1)
	x.pay(id, "paid")
	x.exec("UPDATE orders SET min_wait_until = $2 WHERE id = $1", id, start.Add(60*time.Second))
	rider, tok := x.signIn(x.scooter)
	x.dispatch(start.Add(6 * time.Second))
	o := x.offer(tok)
	offer, token := o["id"].(string), o["fencing_token"].(string)
	if code, out := x.call("POST", "/offers/"+offer+"/accept", tok, `{"fencing_token":"ft_wrong"}`); code != 409 || out["code"] != "stale_fencing_token" {
		t.Errorf("%d %v", code, out)
	}
	second := x.scooterOnShift("SC-2")
	_, otherTok := x.signIn(second)
	if code, _ := x.call("POST", "/offers/"+offer+"/accept", otherTok, `{"fencing_token":"`+token+`"}`); code != 404 {
		t.Errorf("another rider's offer: %d", code)
	}
	x.clock = start.Add(10 * time.Second)
	seq := x.customerSeq(id)
	code, out := x.call("POST", "/offers/"+offer+"/accept", tok, `{"fencing_token":"`+token+`"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	riderCheck(t, out, "OrderResponse")
	ro := orderOf(out)
	riderCheck(t, ro, "RiderOrder")
	if ro["state"] != "RIDER_ACCEPTED" || ro["timer_label"] != "1.5 mins" || ro["rider_accepted_at"] == nil || ro["ended_reason"] != nil {
		t.Errorf("%v", ro)
	}
	if x.count("SELECT count(*) FROM orders WHERE id = $1 AND rider_id = $2 AND timer_length_s = 90 AND zone_radius_m = 350 AND arrival_perimeter_m = 100", id, rider) != 1 {
		t.Error("the timer values were not fixed on the order")
	}
	if x.customerSeq(id) != seq {
		t.Error("before the minimum wait, accepting is hidden")
	}
	if _, c := x.send("GET", "/orders/"+id, x.token, ""); orderOf(c)["customer_stage"] != "looking_for_rider" || orderOf(c)["rider"] != nil {
		t.Errorf("the rider was shown during the minimum wait: %v", c)
	}
	// Accepting again has no second effect.
	if code, again := x.call("POST", "/offers/"+offer+"/accept", tok, `{"fencing_token":"`+token+`"}`); code != 200 || orderOf(again)["state"] != "RIDER_ACCEPTED" {
		t.Errorf("%d %v", code, again)
	}
	if code, out := x.call("POST", "/offers/"+offer+"/ready", tok, `{"fencing_token":"`+token+`"}`); code != 200 || orderOf(out)["state"] != "PACKED" {
		t.Errorf("%d %v", code, out)
	}
	if code, out := x.call("POST", "/offers/"+offer+"/ready", tok, `{"fencing_token":"`+token+`"}`); code != 200 || orderOf(out)["state"] != "PACKED" {
		t.Errorf("ready twice: %d %v", code, out)
	}
	// The minimum wait passes: the customer now sees the rider, and seq moves once.
	x.clock = start.Add(61 * time.Second)
	_, c := x.send("GET", "/orders/"+id, x.token, "")
	if orderOf(c)["customer_stage"] != "rider_accepted" || orderOf(c)["rider"] == nil || orderOf(c)["cancellable"] != false {
		t.Errorf("%v", c)
	}
	if code, out := x.send("POST", "/orders/"+id+"/cancel", x.token, `{}`); code != 409 || out["code"] != "cancel_not_allowed" {
		t.Errorf("a cancel at rider_accepted before the wait deadline: %d %v", code, out)
	}
	x.dispatch(x.clock)
	x.dispatch(x.clock)
	if x.customerSeq(id) != seq+1 {
		t.Errorf("seq %d, was %d", x.customerSeq(id), seq)
	}
	if code, out := x.call("GET", "/orders/"+id, tok, ""); code != 200 || orderOf(out)["state"] != "PACKED" {
		t.Errorf("%d %v", code, out)
	}
	if code, out := x.call("POST", "/riders/me/shifts/end", tok, ""); code != 409 || out["code"] != "order_in_progress" {
		t.Errorf("ending a shift with an accepted order: %d %v", code, out)
	}
	// After the wait deadline the customer may leave with a refund. The rider is told.
	x.clock = start.Add(301 * time.Second)
	if code, out := x.send("POST", "/orders/"+id+"/cancel", x.token, `{}`); code != 200 || orderOf(out)["rider"] == nil {
		t.Fatalf("%d %v", code, out)
	}
	if _, r := x.send("GET", "/orders/"+id+"/refund", x.token, ""); r["refund"].(map[string]any)["reason_code"] != "wait_deadline_passed" {
		t.Errorf("%v", r)
	}
	code, out = x.call("GET", "/orders/"+id, tok, "")
	if ro := orderOf(out); code != 200 || ro["ended_reason"] != "cancelled" || ro["state"] != "CANCELLED" {
		t.Errorf("%d %v", code, out)
	}
	if code, out := x.call("POST", "/offers/"+offer+"/ready", tok, `{"fencing_token":"`+token+`"}`); code != 409 || out["code"] != "order_cancelled" {
		t.Errorf("%d %v", code, out)
	}
	if x.held() != 0 {
		t.Error("the stock was not given back")
	}
	if code, st := x.call("GET", "/riders/me/state", tok, ""); code != 200 || st["current_order"] != nil {
		t.Errorf("the rider still holds a cancelled order: %v", st)
	}
}

func TestADeclineGoesToTheNextScooter(t *testing.T) {
	x := withRiders(newFixture(t))
	second := x.scooterOnShift("SC-2")
	x.load(second, "L-CHAI-B", x.chai, 3)
	start := x.clock
	id := x.placed(x.chai, 2)
	x.pay(id, "paid")
	_, tokA := x.signIn(x.scooter)
	_, tokB := x.signIn(second)
	// The order's own scooter, the one holding the stock, goes first, whichever it is.
	firstScooter, nextScooter := x.scooter, second
	if x.count("SELECT count(*) FROM orders WHERE id = $1 AND scooter_id = $2", id, second) == 1 {
		tokA, tokB = tokB, tokA
		firstScooter, nextScooter = second, x.scooter
	}
	x.dispatch(start.Add(6 * time.Second))
	if x.count("SELECT count(*) FROM offers WHERE scooter_id = $1", firstScooter) != 1 {
		t.Fatal("the first offer did not go to the scooter holding the stock")
	}
	o := x.offer(tokA)
	x.clock = start.Add(8 * time.Second)
	code, out := x.call("POST", "/offers/"+o["id"].(string)+"/decline", tokA, `{"fencing_token":"`+o["fencing_token"].(string)+`","reason":"flat tyre"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	riderCheck(t, out, "Done")
	x.dispatch(x.clock)
	b := x.offer(tokB)
	if x.count("SELECT coalesce(sum(reserved), 0) FROM scooter_stock WHERE scooter_id = $1", nextScooter) != 2 || x.count("SELECT coalesce(sum(reserved), 0) FROM scooter_stock WHERE scooter_id = $1", firstScooter) != 0 {
		t.Error("the hold did not move to the second scooter")
	}
	if n := x.count("SELECT coalesce(sum(delta_reserved), 0) FROM stock_ledger"); n != 2 {
		t.Errorf("the ledger says %d held", n)
	}
	if _, list := x.call("GET", "/riders/me/offers", tokA, ""); len(list["offers"].([]any)) != 1 || list["offers"].([]any)[0].(map[string]any)["state"] != "declined" {
		t.Errorf("%v", list)
	}
	seq := x.customerSeq(id)
	x.call("POST", "/offers/"+b["id"].(string)+"/decline", tokB, `{"fencing_token":"`+b["fencing_token"].(string)+`"}`)
	x.dispatch(x.clock)
	_, c := x.send("GET", "/orders/"+id, x.token, "")
	if o := orderOf(c); o["customer_stage"] != "no_scooter" || o["cancellable"] != true || x.customerSeq(id) != seq+1 {
		t.Fatalf("%v", c)
	}
	if x.held() != 0 {
		t.Error("an order at no scooter still holds stock")
	}
	// Nobody chooses in time: it ends, and the money goes back.
	x.dispatch(x.clock.Add(301 * time.Second))
	_, c = x.send("GET", "/orders/"+id, x.token, "")
	if o := orderOf(c); o["customer_stage"] != "ended" || o["ended_reason"] != "timed_out" || o["payment_status"] != "refunded" {
		t.Errorf("%v", c)
	}
	if _, r := x.send("GET", "/orders/"+id+"/refund", x.token, ""); r["refund"].(map[string]any)["reason_code"] != "no_rider_found" {
		t.Errorf("%v", r)
	}
}

func TestAnUnansweredOfferExpires(t *testing.T) {
	x := withRiders(newFixture(t))
	start := x.clock
	id := x.placed(x.chai, 1)
	x.pay(id, "paid")
	_, tok := x.signIn(x.scooter)
	x.dispatch(start.Add(6 * time.Second))
	o := x.offer(tok)
	x.clock = start.Add(37 * time.Second)
	if code, out := x.call("POST", "/offers/"+o["id"].(string)+"/accept", tok, `{"fencing_token":"`+o["fencing_token"].(string)+`"}`); code != 409 || out["code"] != "offer_expired" {
		t.Errorf("accepting after the offer's time: %d %v", code, out)
	}
	// The only scooter has had its chance, so the order moves to no scooter.
	if n := x.dispatch(start.Add(37 * time.Second)); n != 2 {
		t.Errorf("%d", n)
	}
	if x.count("SELECT count(*) FROM orders WHERE id = $1 AND state = 'NO_SCOOTER'", id) != 1 {
		t.Error("not at no scooter")
	}
	if _, list := x.call("GET", "/riders/me/offers", tok, ""); list["offers"].([]any)[0].(map[string]any)["state"] != "expired" {
		t.Errorf("%v", list)
	}
}

func TestOneOrderPerRider(t *testing.T) {
	x := withRiders(newFixture(t))
	start := x.clock
	first := x.placed(x.chai, 1)
	x.pay(first, "paid")
	x.clock = start.Add(time.Second) // Paid later, so offered second.
	second := x.placed(x.samosa, 1)
	x.pay(second, "paid")
	_, tok := x.signIn(x.scooter)
	x.dispatch(start.Add(6 * time.Second))
	if n := x.count("SELECT count(*) FROM offers"); n != 1 {
		t.Fatalf("%d offers for one rider", n)
	}
	if x.count("SELECT count(*) FROM orders WHERE id = $1 AND state = 'PAYMENT_SECURED'", second) != 1 {
		t.Error("the second order should wait for the busy rider")
	}
	o := x.offer(tok)
	x.call("POST", "/offers/"+o["id"].(string)+"/accept", tok, `{"fencing_token":"`+o["fencing_token"].(string)+`"}`)
	x.dispatch(start.Add(10 * time.Second))
	if x.count("SELECT count(*) FROM orders WHERE id = $1 AND state = 'PAYMENT_SECURED'", second) != 1 {
		t.Error("the second order should still wait")
	}
	// Once the first order ends, the rider is free for the second.
	x.clock = start.Add(301 * time.Second)
	x.send("POST", "/orders/"+first+"/cancel", x.token, `{}`)
	x.dispatch(x.clock)
	if x.count("SELECT count(*) FROM offers WHERE order_id = $1 AND state = 'open'", second) != 1 {
		t.Error("the free rider did not get the waiting order")
	}
}

func TestThePackingLimit(t *testing.T) {
	x := withRiders(newFixture(t))
	x.exec(`INSERT INTO zone_settings (zone_id, version, packing_limit_s, reason, created_by) VALUES ($1, 1, 60, 'test', 'test')`, x.zone)
	start := x.clock
	id := x.placed(x.chai, 1)
	x.pay(id, "paid")
	_, tok := x.signIn(x.scooter)
	x.dispatch(start.Add(6 * time.Second))
	o := x.offer(tok)
	x.clock = start.Add(10 * time.Second)
	x.call("POST", "/offers/"+o["id"].(string)+"/accept", tok, `{"fencing_token":"`+o["fencing_token"].(string)+`"}`)
	x.dispatch(start.Add(69 * time.Second))
	if x.count("SELECT count(*) FROM orders WHERE id = $1 AND state = 'RIDER_ACCEPTED'", id) != 1 {
		t.Error("packed before the limit")
	}
	x.dispatch(start.Add(71 * time.Second))
	if x.count("SELECT count(*) FROM orders WHERE id = $1 AND state = 'PACKED'", id) != 1 {
		t.Error("not packed at the limit")
	}
}

func TestTheRiderSeesOnlyTheirOwn(t *testing.T) {
	x := withRiders(newFixture(t))
	start := x.clock
	id := x.placed(x.chai, 1)
	x.pay(id, "paid")
	_, tok := x.signIn(x.scooter)
	x.dispatch(start.Add(6 * time.Second))
	second := x.scooterOnShift("SC-2")
	_, other := x.signIn(second)
	if code, _ := x.call("GET", "/orders/"+id, other, ""); code != 404 {
		t.Errorf("another rider's order: %d", code)
	}
	if code, _ := x.call("GET", "/orders/"+id, tok, ""); code != 404 {
		t.Errorf("an order not yet accepted shows through offers only: %d", code)
	}
	if code, _ := x.call("POST", "/offers/nope/accept", tok, `{"fencing_token":"x"}`); code != 404 {
		t.Errorf("%d", code)
	}
	if code, _ := x.call("POST", "/offers/"+id+"/accept", tok, `{}`); code != 400 {
		t.Errorf("no token: %d", code)
	}
	// The customer cancels after the wait deadline while the offer is open: the rider sees it cancelled.
	x.clock = start.Add(301 * time.Second)
	x.send("POST", "/orders/"+id+"/cancel", x.token, `{}`)
	if _, list := x.call("GET", "/riders/me/offers", tok, ""); list["offers"].([]any)[0].(map[string]any)["state"] != "cancelled" {
		t.Errorf("%v", list)
	}
	x.exec("UPDATE people SET status = 'blocked' WHERE id = (SELECT person_id FROM riders WHERE id = (SELECT rider_id FROM shifts WHERE scooter_id = $1))", x.scooter)
	if code, out := x.call("GET", "/riders/me/state", tok, ""); code != 403 || out["code"] != "account_blocked" {
		t.Errorf("%d %v", code, out)
	}
}

func TestEndingAShiftGivesBackAnOpenOffer(t *testing.T) {
	x := withRiders(newFixture(t))
	start := x.clock
	id := x.placed(x.chai, 1)
	x.pay(id, "paid")
	_, tok := x.signIn(x.scooter)
	x.dispatch(start.Add(6 * time.Second))
	if code, out := x.call("POST", "/riders/me/shifts/end", tok, ""); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if x.count("SELECT count(*) FROM orders WHERE id = $1 AND state = 'PAYMENT_SECURED'", id) != 1 {
		t.Error("the order did not go back to be offered again")
	}
	if x.count("SELECT count(*) FROM offers WHERE state = 'expired' AND ended_reason = 'reassigned' AND released_at IS NOT NULL") != 1 {
		t.Error("the offer was not given back")
	}
	// No rider is left, and the order's scooter has nobody on it: no scooter.
	x.dispatch(start.Add(8 * time.Second))
	if x.count("SELECT count(*) FROM orders WHERE id = $1 AND state = 'NO_SCOOTER'", id) != 1 {
		t.Error("with no rider left, the order should move to no scooter")
	}
}

func TestNoWaitingForARiderWhoCannotCome(t *testing.T) {
	x := withRiders(newFixture(t))
	start := x.clock
	first := x.placed(x.chai, 1)
	x.pay(first, "paid")
	x.clock = start.Add(time.Second) // Paid later, so offered second.
	second := x.placed(x.samosa, 1)
	x.pay(second, "paid")
	_, tok := x.signIn(x.scooter)
	x.dispatch(start.Add(6 * time.Second))
	o := x.offer(tok)
	x.clock = start.Add(10 * time.Second)
	if code, out := x.call("POST", "/offers/"+o["id"].(string)+"/accept", tok, `{"fencing_token":"`+o["fencing_token"].(string)+`"}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	// The only rider holds the first order, so the second waits, but only until its wait deadline.
	x.dispatch(start.Add(200 * time.Second))
	if x.count("SELECT count(*) FROM orders WHERE id = $1 AND state = 'PAYMENT_SECURED'", second) != 1 {
		t.Error("before the deadline, the order should wait")
	}
	x.dispatch(start.Add(301 * time.Second))
	if x.count("SELECT count(*) FROM orders WHERE id = $1 AND state = 'NO_SCOOTER'", second) != 1 {
		t.Error("past the wait deadline, the order must stop waiting")
	}
}

func TestNoWaitingForASuspendedRider(t *testing.T) {
	x := withRiders(newFixture(t))
	start := x.clock
	first := x.placed(x.chai, 1)
	x.pay(first, "paid")
	x.clock = start.Add(time.Second)
	second := x.placed(x.samosa, 1)
	x.pay(second, "paid")
	_, tok := x.signIn(x.scooter)
	x.dispatch(start.Add(6 * time.Second))
	o := x.offer(tok)
	x.clock = start.Add(10 * time.Second)
	x.call("POST", "/offers/"+o["id"].(string)+"/accept", tok, `{"fencing_token":"`+o["fencing_token"].(string)+`"}`)
	// The busy rider is suspended: the second order must not wait for them.
	x.exec("UPDATE riders SET status = 'suspended' WHERE id = (SELECT rider_id FROM shifts WHERE scooter_id = $1)", x.scooter)
	x.dispatch(start.Add(20 * time.Second))
	if x.count("SELECT count(*) FROM orders WHERE id = $1 AND state = 'NO_SCOOTER'", second) != 1 {
		t.Error("an order must not wait for a suspended rider")
	}
}

func TestAcceptingNeedsTheOffersScooter(t *testing.T) {
	x := withRiders(newFixture(t))
	start := x.clock
	id := x.placed(x.chai, 1)
	x.pay(id, "paid")
	_, tok := x.signIn(x.scooter)
	x.dispatch(start.Add(6 * time.Second))
	o := x.offer(tok)
	// The rider's shift moves to another scooter while the offer is open.
	other := x.id("INSERT INTO scooters (code, zone_id, node_id) VALUES ('SC-OTHER', $1, $2) RETURNING id::text", x.zone, x.node)
	x.exec("UPDATE shifts SET scooter_id = $1 WHERE scooter_id = $2", other, x.scooter)
	if code, out := x.call("POST", "/offers/"+o["id"].(string)+"/accept", tok, `{"fencing_token":"`+o["fencing_token"].(string)+`"}`); code != 409 || out["code"] != "offer_expired" {
		t.Errorf("%d %v", code, out)
	}
	_ = id
}
