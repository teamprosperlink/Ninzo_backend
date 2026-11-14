package orders

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/teamprosperlink/Ninzo_backend/internal/contracttest"
)

// Issue 235, part B3: checkGate compares and answers. It never decides arrival, never changes the order or
// the timer, and takes no Idempotency-Key. Every test goes through the real HTTP handlers and checks the
// reply against the bundled rider contract.

// gateCheck sends the call the way the rider app does: a bearer token and no Idempotency-Key.
func (x *riders) gateCheck(token, order, body string) (int, map[string]any) {
	x.t.Helper()
	req, _ := http.NewRequest("POST", x.srv.URL+"/v1/orders/"+order+"/gate-check", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		x.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// everything is all that a gate check could change, in one string: the order, its timer and arrival, its
// events, the rider's seq, the pings, the audit log, the outbox and the saved Idempotency-Keys.
func (x *riders) everything(rider string) string {
	x.t.Helper()
	var s string
	err := x.pool.QueryRow(x.t.Context(), `SELECT
		(SELECT coalesce(string_agg(concat_ws(',', id, state, seq, timer_started_at, timer_stopped_at, arrived_at, rider_waiting_since, timer_missed, arrival_source, handed_over_at, updated_at), ';' ORDER BY id), '') FROM orders) || '|' ||
		(SELECT count(*) FROM order_events) || '|' || (SELECT count(*) FROM audit_events) || '|' || (SELECT count(*) FROM outbox_events) || '|' ||
		(SELECT count(*) FROM location_pings) || '|' || (SELECT count(*) FROM idempotency_keys) || '|' ||
		(SELECT coalesce(string_agg(concat_ws(',', id, state, released_at), ';' ORDER BY id), '') FROM offers) || '|' ||
		coalesce((SELECT seq FROM rider_state WHERE rider_id = $1), 0)`, rider).Scan(&s)
	if err != nil {
		x.t.Fatal(err)
	}
	return s
}

func TestCheckGateComparesAScannedQRAndChangesNothing(t *testing.T) {
	tr := started(t, true)
	rider := tr.id("SELECT rider_id::text FROM offers WHERE id = $1", tr.offer)
	// The rider is in the zone, with the timer running and the perimeter close: everything a gate check
	// must not be able to disturb is in motion.
	tr.at(inZone, 20*time.Second)
	tr.at(atGate, 20*time.Second)
	before := tr.everything(rider)
	timerBefore := tr.count("SELECT count(*) FROM orders WHERE id = $1 AND timer_started_at IS NOT NULL", tr.order)
	gate := tr.id("SELECT gate_id::text FROM orders WHERE id = $1", tr.order)
	otherGate := "00000000-0000-4000-8000-0000000000aa"
	// checked and matched for each: a typed code has nothing to check (matched, not checked); a scanned
	// QR is checked against the gate the order was placed to.
	type answer struct {
		body             string
		checked, matched bool
	}
	for name, tc := range map[string]answer{
		"a typed code":               {`{"method":"code","value":"GATE-4821"}`, false, true},
		"one character":              {`{"method":"code","value":"x"}`, false, true},
		"64 characters":              {`{"method":"code","value":"` + strings.Repeat("9", 64) + `"}`, false, true},
		"unicode":                    {`{"method":"code","value":"गेट-४८२१"}`, false, true},
		"the handover code":          {`{"method":"code","value":"` + tr.otp() + `"}`, false, true},
		"a code that is a QR":        {`{"method":"code","value":"NZG|` + otherGate + `"}`, false, true},
		"this gate's QR":             {`{"method":"qr","value":"NZG|` + gate + `"}`, true, true},
		"this gate's QR in capitals": {`{"method":"qr","value":"NZG|` + strings.ToUpper(gate) + `"}`, true, true},
		"another gate's QR":          {`{"method":"qr","value":"NZG|` + otherGate + `"}`, true, false},
		"a QR with no gate":          {`{"method":"qr","value":"NZG|"}`, true, false},
		"a QR with a bad gate id":    {`{"method":"qr","value":"NZG|not-an-id"}`, true, false},
		"a QR with the wrong tag":    {`{"method":"qr","value":"XYZ|` + gate + `"}`, true, false},
		"the bare gate id":           {`{"method":"qr","value":"` + gate + `"}`, true, false},
		"a QR with more after it":    {`{"method":"qr","value":"NZG|` + gate + `|x"}`, true, false},
		"a web address":              {`{"method":"qr","value":"https://example.invalid/g/4821"}`, true, false},
	} {
		code, out := tr.gateCheck(tr.tok, tr.order, tc.body)
		if code != 200 {
			t.Errorf("%s: %d %v", name, code, out)
			continue
		}
		contracttest.Check(t, riderApkContract, "GateCheck", out)
		if out["checked"] != tc.checked || out["matched"] != tc.matched {
			t.Errorf("%s: %v, want checked %v matched %v", name, out, tc.checked, tc.matched)
		}
		noCustomerData(t, name, out)
		if len(out) != 3 {
			t.Errorf("%s: the reply holds more than checked, matched and server_time: %v", name, out)
		}
	}
	if now := tr.everything(rider); now != before {
		t.Errorf("a gate check changed something:\n%s\n%s", before, now)
	}
	if n := tr.count("SELECT count(*) FROM orders WHERE id = $1 AND timer_started_at IS NOT NULL", tr.order); n != timerBefore {
		t.Error("the timer changed")
	}
	if n := tr.count("SELECT count(*) FROM orders WHERE id = $1 AND arrived_at IS NOT NULL", tr.order); n != 0 {
		t.Error("a gate check marked the rider as arrived")
	}
	// The order still ends the way it would have: the handover works after a gate check.
	if code, out := tr.handOver(tr.otp(), 2, tr.clock); code != 200 {
		t.Errorf("the handover after a gate check: %d %v", code, out)
	}
}

func TestCheckGateIsOnlyForAnOrderThisRiderHoldsAndThatIsNotFinished(t *testing.T) {
	tr := started(t, true)
	body := `{"method":"code","value":"GATE-4821"}`
	rider := tr.id("SELECT rider_id::text FROM offers WHERE id = $1", tr.offer)
	// Another rider on another scooter.
	second := tr.scooterOnShift("SC-2")
	_, other := tr.signIn(second)
	before := tr.everything(rider)
	missing := "00000000-0000-4000-8000-000000000001"
	for name, tc := range map[string]struct{ token, order string }{
		"another rider's order":      {other, tr.order},
		"an order that is not there": {tr.tok, missing},
		"an id that is not an id":    {tr.tok, "not-an-id"},
	} {
		code, out := tr.gateCheck(tc.token, tc.order, body)
		if code != 404 || out["code"] != "not_found" {
			t.Errorf("%s: %d %v", name, code, out)
		}
		noCustomerData(t, name, out)
	}
	// Another rider's order answers exactly as a missing one does.
	_, a := tr.gateCheck(other, tr.order, body)
	_, b := tr.gateCheck(other, missing, body)
	delete(a, "server_time")
	delete(b, "server_time")
	delete(a, "request_id")
	delete(b, "request_id")
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if string(aj) != string(bj) {
		t.Errorf("another rider's order is told apart from a missing one:\n%s\n%s", aj, bj)
	}
	// An offer this rider has not accepted, or one that ended, is not theirs to check.
	tr.exec("UPDATE offers SET released_at = now() WHERE id = $1", tr.offer)
	if code, _ := tr.gateCheck(tr.tok, tr.order, body); code != 404 {
		t.Errorf("an accepted offer that was released (the order went elsewhere): %d", code)
	}
	tr.exec("UPDATE offers SET released_at = NULL WHERE id = $1", tr.offer)
	if code, _ := tr.gateCheck(tr.tok, tr.order, body); code != 200 {
		t.Fatalf("the accepted order again: %d", code)
	}
	// Once the order is finished, in any way, it is 404.
	for _, state := range []string{"HANDED_OVER", "COMPLETED"} {
		tr.exec("UPDATE orders SET state = $2 WHERE id = $1", tr.order, state)
		if code, out := tr.gateCheck(tr.tok, tr.order, body); code != 404 {
			t.Errorf("an order %s: %d %v", state, code, out)
		}
	}
	for _, state := range []string{"CANCELLED", "TIMED_OUT", "FAILED"} {
		tr.exec("UPDATE orders SET state = $2, ended_reason = $3 WHERE id = $1", tr.order, state, map[string]string{"CANCELLED": "cancelled", "TIMED_OUT": "timed_out", "FAILED": "failed"}[state])
		if code, out := tr.gateCheck(tr.tok, tr.order, body); code != 404 {
			t.Errorf("an order %s: %d %v", state, code, out)
		}
	}
	if before == "" {
		t.Fatal("no snapshot")
	}
}

func TestCheckGateRefusesBadBodiesAndAnUnknownCaller(t *testing.T) {
	tr := started(t, true)
	rider := tr.id("SELECT rider_id::text FROM offers WHERE id = $1", tr.offer)
	before := tr.everything(rider)
	for name, body := range map[string]string{
		"nothing": ``, "an empty object": `{}`, "no value": `{"method":"code"}`, "no method": `{"value":"x"}`, "an unknown method": `{"method":"nfc","value":"x"}`,
		"an empty value": `{"method":"code","value":""}`, "65 characters": `{"method":"code","value":"` + strings.Repeat("9", 65) + `"}`,
		"a number": `{"method":"code","value":4821}`, "null": `{"method":null,"value":null}`, "not json": `x`, "a list": `[]`,
	} {
		if code, out := tr.gateCheck(tr.tok, tr.order, body); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if code, _ := tr.gateCheck("", tr.order, `{"method":"code","value":"x"}`); code != 401 {
		t.Errorf("no token: %d", code)
	}
	if now := tr.everything(rider); now != before {
		t.Errorf("a refused call changed something:\n%s\n%s", before, now)
	}
	tr.exec("UPDATE people SET status = 'blocked' WHERE id = (SELECT person_id FROM riders WHERE id = $1)", rider)
	if code, out := tr.gateCheck(tr.tok, tr.order, `{"method":"code","value":"x"}`); code != 403 || out["code"] != "account_blocked" {
		t.Errorf("a blocked rider: %d %v", code, out)
	}
}
