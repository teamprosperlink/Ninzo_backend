package orders

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func stamp3339(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func locationEvent(id string, p [2]float64, at time.Time) string {
	return fmt.Sprintf(`{"event_id":"%s","type":"location","device_time":"%s","payload":{"lat":%f,"lng":%f,"speed_mps":0,"accuracy_m":5}}`, id, stamp3339(at), p[0], p[1])
}

func (tr *trip) handoverEvent(id, otp string, qty int, at time.Time) string {
	return fmt.Sprintf(`{"event_id":"%s","type":"handover","device_time":"%s","payload":{"order_id":"%s","otp":"%s","quantities":[{"item_id":"%s","qty":%d}]}}`,
		id, stamp3339(at), tr.order, otp, tr.chai, qty)
}

func (tr *trip) batch(events ...string) (int, map[string]any) {
	tr.t.Helper()
	return tr.call("POST", "/riders/me/events", tr.tok, `{"events":[`+strings.Join(events, ",")+`]}`)
}

func ids(out map[string]any) ([]string, map[string]map[string]any) {
	acc := []string{}
	for _, v := range out["accepted_ids"].([]any) {
		acc = append(acc, v.(string))
	}
	rej := map[string]map[string]any{}
	for _, v := range out["rejected"].([]any) {
		r := v.(map[string]any)
		rej[r["id"].(string)] = r
	}
	return acc, rej
}

func TestAQueueFlushedInOrder(t *testing.T) {
	tr := begin(t, nil)
	// Offline from the zone to the gate, then the handover, all sent at once when the signal returns.
	start := tr.clock
	tr.clock = start.Add(90 * time.Second) // Every queued ping is now more than 30 s old.
	otp := tr.otp()
	code, out := tr.batch(
		locationEvent("evt-loc-0001", atGate, start.Add(40*time.Second)),
		locationEvent("evt-loc-0002", atGateToo, start.Add(43*time.Second)),
		tr.handoverEvent("evt-hand-0001", otp, 2, start.Add(50*time.Second)),
	)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	riderCheck(t, out, "EventBatchResult")
	acc, rej := ids(out)
	if len(acc) != 3 || len(rej) != 0 {
		t.Fatalf("%v", out)
	}
	if tr.state() != "HANDED_OVER" || tr.held() != 0 {
		t.Errorf("the queued handover did not apply: %s", tr.state())
	}
	// The late pings are evidence only, so arrival came from the handover itself.
	if tr.count("SELECT count(*) FROM location_pings WHERE order_id = $1 AND NOT applied", tr.order) != 2 {
		t.Error("late queued pings must be kept but not used")
	}
	// Sending the same events again changes nothing and answers the same.
	code, again := tr.batch(tr.handoverEvent("evt-hand-0001", otp, 2, start.Add(50*time.Second)))
	if acc, _ := ids(again); code != 200 || len(acc) != 1 {
		t.Errorf("%d %v", code, again)
	}
	if tr.count("SELECT count(*) FROM rider_events") != 3 {
		t.Error("each final result is kept once")
	}
}

func TestOneBadEventNeverBlocksTheQueue(t *testing.T) {
	tr := begin(t, nil)
	tr.at(atGate, 20*time.Second)
	tr.at(atGateToo, 3*time.Second)
	wrong := "0000"
	if tr.otp() == wrong {
		wrong = "1111"
	}
	code, out := tr.batch(
		`{"event_id":"bad","type":"location","device_time":"x","payload":{}}`,
		`{"event_id":"evt-bad-payload","type":"location","device_time":"`+stamp3339(tr.clock)+`","payload":{"lat":200}}`,
		`{"event_id":"evt-mystery-1","type":"teleport","device_time":"`+stamp3339(tr.clock)+`","payload":{}}`,
		`{"event_id":"evt-restock-1","type":"restock_confirm","device_time":"`+stamp3339(tr.clock)+`","payload":{}}`,
		tr.handoverEvent("evt-wrong-code", wrong, 2, tr.clock),
		tr.handoverEvent("evt-right-code", tr.otp(), 2, tr.clock),
	)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	acc, rej := ids(out)
	for id, want := range map[string]string{"bad": "invalid_event", "evt-bad-payload": "invalid_event", "evt-mystery-1": "unknown_event_type", "evt-restock-1": "invalid_event", "evt-wrong-code": "wrong_otp"} {
		if rej[id]["code"] != want {
			t.Errorf("%s: %v", id, rej[id])
		}
	}
	if rej["evt-wrong-code"]["retryable"] != false {
		t.Error("retryable must say whether the event may succeed later")
	}
	if len(acc) != 1 || acc[0] != "evt-right-code" || tr.state() != "HANDED_OVER" {
		t.Errorf("the good event after bad ones must apply: %v %s", acc, tr.state())
	}
	if tr.count("SELECT otp_attempts FROM orders WHERE id = $1", tr.order) != 1 {
		t.Error("a queued wrong code still counts as an attempt")
	}
	// A final rejection is kept. A retryable one is checked in TestARetryableRejectionIsNotKept.
	if tr.count("SELECT count(*) FROM rider_events WHERE event_id = 'evt-wrong-code'") != 1 {
		t.Error("only final results are kept")
	}
	for _, body := range []string{`{}`, `{"events":[]}`, `nope`} {
		if code, _ := tr.call("POST", "/riders/me/events", tr.tok, body); code != 400 {
			t.Errorf("%s: %d", body, code)
		}
	}
}

func TestAQueuedHandoverAfterACancel(t *testing.T) {
	tr := begin(t, nil)
	otp := tr.otp()
	tr.clock = tr.clock.Add(301 * time.Second)
	tr.send("POST", "/orders/"+tr.order+"/cancel", tr.fixture.token, `{}`)
	_, out := tr.batch(tr.handoverEvent("evt-late-hand", otp, 2, tr.clock.Add(-10*time.Second)))
	if _, rej := ids(out); rej["evt-late-hand"]["code"] != "order_cancelled" {
		t.Errorf("%v", out)
	}
}

func TestADirectRequestAndItsQueuedCopyCountOnce(t *testing.T) {
	tr := begin(t, nil)
	// The rider reports a problem directly, with the event_id as its Idempotency-Key ...
	code, out := tr.callKey("POST", "/riders/me/problems", tr.tok, `{"type":"gate_closed"}`, "evt-problem-001")
	if code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	// ... and the queued copy arrives later.
	_, batch := tr.batch(`{"event_id":"evt-problem-001","type":"problem","device_time":"` + stamp3339(tr.clock) + `","payload":{"problem_type":"gate_closed"}}`)
	if acc, _ := ids(batch); len(acc) != 1 {
		t.Errorf("%v", batch)
	}
	if tr.count("SELECT count(*) FROM problem_reports") != 1 {
		t.Error("the queued copy of a direct request applied again")
	}
}

func TestQueuedProblemsAndScans(t *testing.T) {
	tr := begin(t, nil)
	tr.exec("INSERT INTO lots (code, item_id, qty, expires_at) VALUES ('L-NEW', $1, 3, now() + interval '1 day')", tr.samosa)
	_, out := tr.batch(
		`{"event_id":"evt-prob-0001","type":"problem","device_time":"`+stamp3339(tr.clock)+`","payload":{"problem_type":"unsafe_route","order_id":"`+tr.order+`","note":"flooded road"}}`,
		`{"event_id":"evt-scan-0001","type":"stock_scan","device_time":"`+stamp3339(tr.clock)+`","payload":{"lot_code":"L-NEW"}}`,
		`{"event_id":"evt-scan-0002","type":"stock_scan","device_time":"`+stamp3339(tr.clock)+`","payload":{"lot_code":"L-NOPE"}}`,
	)
	acc, rej := ids(out)
	if len(acc) != 2 || rej["evt-scan-0002"]["code"] != "bad_scan" {
		t.Errorf("%v", out)
	}
	if tr.count("SELECT count(*) FROM problem_reports WHERE note = 'flooded road'") != 1 || tr.count("SELECT count(*) FROM scooter_stock ss JOIN lots l ON l.id = ss.lot_id WHERE l.code = 'L-NEW'") != 1 {
		t.Error("the queued problem or scan did not apply")
	}
}

// callKey is call with a chosen Idempotency-Key, as a direct request sharing an event_id sends.
func (x *riders) callKey(method, path, token, body, key string) (int, map[string]any) {
	x.t.Helper()
	req, _ := http.NewRequest(method, x.srv.URL+"/v1"+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		x.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestQueuedPingsOnTimeCount(t *testing.T) {
	tr := begin(t, nil)
	tr.clock = tr.clock.Add(10 * time.Minute) // Well away from the real clock, so device times decide.
	tr.at(far, 0)
	tr.clock = tr.clock.Add(40 * time.Second)
	_, out := tr.batch(
		locationEvent("evt-ontime-01", atGate, tr.clock.Add(-4*time.Second)),
		locationEvent("evt-ontime-02", atGateToo, tr.clock.Add(-2*time.Second)),
	)
	if acc, _ := ids(out); len(acc) != 2 || tr.state() != "ARRIVED" {
		t.Errorf("queued pings sent on time must count: %s %v", tr.state(), out)
	}
}

func TestAQueuedHandoverIsJudgedByItsOwnTime(t *testing.T) {
	tr := begin(t, nil)
	tr.clock = tr.clock.Add(10 * time.Minute)
	tr.at(far, 0) // Fresh GPS: the rider is away from the gate now.
	// Entered a minute ago, at the gate, then the rider rode on: accepted, and flagged.
	_, old := tr.batch(tr.handoverEvent("evt-old-handover", tr.otp(), 2, tr.clock.Add(-time.Minute)))
	if acc, _ := ids(old); len(acc) != 1 {
		t.Fatalf("an older queued handover must be accepted: %v", old)
	}
	if tr.count("SELECT count(*) FROM orders WHERE id = $1 AND audit_flag = 'late_handover_away_from_gate'", tr.order) != 1 {
		t.Error("and flagged for audit")
	}
}

func TestACurrentQueuedHandoverFarFromTheGateIsRefused(t *testing.T) {
	tr := begin(t, nil)
	tr.clock = tr.clock.Add(10 * time.Minute)
	tr.at(far, 0)
	_, out := tr.batch(tr.handoverEvent("evt-now-handover", tr.otp(), 2, tr.clock))
	if _, rej := ids(out); rej["evt-now-handover"]["code"] != "rider_not_near" {
		t.Errorf("%v", out)
	}
}

func TestAWrongCodeSentDirectlyCountsOnce(t *testing.T) {
	tr := begin(t, nil)
	tr.at(atGate, 20*time.Second)
	tr.at(atGateToo, 3*time.Second)
	wrong := "0000"
	if tr.otp() == wrong {
		wrong = "1111"
	}
	body := fmt.Sprintf(`{"otp":"%s","quantities":[{"item_id":"%s","qty":2}],"client_time":"%s"}`, wrong, tr.chai, stamp3339(tr.clock))
	if code, out := tr.callKey("POST", "/orders/"+tr.order+"/handover", tr.tok, body, "evt-direct-wrong"); code != 409 || out["code"] != "wrong_otp" {
		t.Fatalf("%d %v", code, out)
	}
	// The reply was lost, so the queue sends the same entry.
	_, out := tr.batch(tr.handoverEvent("evt-direct-wrong", wrong, 2, tr.clock))
	if _, rej := ids(out); rej["evt-direct-wrong"]["code"] != "wrong_otp" {
		t.Errorf("%v", out)
	}
	if tr.count("SELECT otp_attempts FROM orders WHERE id = $1", tr.order) != 1 {
		t.Error("one wrong entry used up two attempts")
	}
}

func TestAnUnknownTypeIsNotStoredAsSent(t *testing.T) {
	tr := begin(t, nil)
	tr.batch(`{"event_id":"evt-weird-type","type":"` + strings.Repeat("x", 300) + `","device_time":"` + stamp3339(tr.clock) + `","payload":{}}`)
	if tr.count("SELECT count(*) FROM rider_events WHERE event_id = 'evt-weird-type' AND type = 'unknown'") != 1 {
		t.Error("an unknown type must be stored as unknown")
	}
}

func TestARetryableRejectionIsNotKept(t *testing.T) {
	tr := begin(t, nil)
	tr.exec("INSERT INTO lots (code, item_id, qty, expires_at) VALUES ('L-LATER', $1, 3, now() + interval '1 day')", tr.samosa)
	tr.exec("UPDATE shifts SET ended_at = now()") // With no shift, a scan may work once one starts.
	_, out := tr.batch(`{"event_id":"evt-scan-noshift","type":"stock_scan","device_time":"` + stamp3339(tr.clock) + `","payload":{"lot_code":"L-LATER"}}`)
	if _, rej := ids(out); rej["evt-scan-noshift"]["code"] != "no_open_shift" || rej["evt-scan-noshift"]["retryable"] != true {
		t.Fatalf("%v", out)
	}
	if tr.count("SELECT count(*) FROM rider_events WHERE event_id = 'evt-scan-noshift'") != 0 {
		t.Error("a retryable rejection was kept, so it would never be checked again")
	}
}

func TestQueuedRestockConfirmAndDecline(t *testing.T) {
	tr := begin(t, nil)
	hub := tr.id("INSERT INTO hubs (zone_id, name, lat, lng) VALUES ($1, 'Hub', 12.935, 77.625) RETURNING id::text", tr.zone)
	tr.exec("INSERT INTO lots (code, item_id, qty, expires_at, hub_id) VALUES ('HUB-L1', $1, 10, now() + interval '1 day', $2)", tr.samosa, hub)
	rider := tr.id("SELECT rider_id::text FROM shifts WHERE scooter_id = $1 AND ended_at IS NULL", tr.scooter)
	sug := func() string {
		id := tr.id("INSERT INTO restock_requests (rider_id, scooter_id, hub_id, source, state, expires_at) VALUES ($1, $2, $3, 'suggested', 'suggested', now() + interval '10 minutes') RETURNING id::text", rider, tr.scooter, hub)
		tr.exec("INSERT INTO restock_lines (request_id, item_id, qty) VALUES ($1, $2, 5)", id, tr.samosa)
		return id
	}
	first := sug()
	_, out := tr.batch(`{"event_id":"evt-rs-decline","type":"restock_decline","device_time":"` + stamp3339(tr.clock) + `","payload":{"request_id":"` + first + `","reason":"later"}}`)
	if acc, _ := ids(out); len(acc) != 1 || tr.id("SELECT state FROM restock_requests WHERE id = $1", first) != "cancelled" {
		t.Errorf("%v", out)
	}
	second := sug()
	_, out = tr.batch(`{"event_id":"evt-rs-confirm","type":"restock_confirm","device_time":"` + stamp3339(tr.clock) + `","payload":{"request_id":"` + second + `"}}`)
	if acc, _ := ids(out); len(acc) != 1 || tr.id("SELECT state FROM restock_requests WHERE id = $1", second) != "confirmed" {
		t.Errorf("%v", out)
	}
	_, out = tr.batch(`{"event_id":"evt-rs-gone","type":"restock_confirm","device_time":"` + stamp3339(tr.clock) + `","payload":{"request_id":"` + first + `"}}`)
	if _, rej := ids(out); rej["evt-rs-gone"]["code"] != "suggestion_expired" {
		t.Errorf("%v", out)
	}
}

func TestAHandoverRecordsThePay(t *testing.T) {
	tr := begin(t, nil)
	start := tr.clock
	tr.clock = start.Add(90 * time.Second)
	otp := tr.otp()
	if _, out := tr.batch(tr.handoverEvent("evt-pay-0001", otp, 2, start.Add(50*time.Second))); tr.state() != "HANDED_OVER" {
		t.Fatalf("%v", out)
	}
	if tr.count("SELECT count(*) FROM earnings WHERE order_id = $1 AND kind = 'order' AND amount_paise = 4000", tr.order) != 1 {
		t.Error("the handover did not record the pay")
	}
	tr.batch(tr.handoverEvent("evt-pay-0001", otp, 2, start.Add(50*time.Second)))
	if tr.count("SELECT count(*) FROM earnings") != 1 {
		t.Error("a repeat paid twice")
	}
}
