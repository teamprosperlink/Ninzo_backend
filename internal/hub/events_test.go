package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func ev(id, kind, payload string) string {
	return `{"event_id":"` + id + `","type":"` + kind + `","device_time":"2026-11-11T10:00:00Z","payload":` + payload + `}`
}

func (q *queue) batch(token string, events ...string) (int, map[string]any) {
	return q.call("POST", "/hub/events", token, `{"events":[`+strings.Join(events, ",")+`]}`)
}

func idsOf(out map[string]any) ([]string, map[string]map[string]any) {
	acc := []string{}
	for _, id := range out["accepted_ids"].([]any) {
		acc = append(acc, id.(string))
	}
	rej := map[string]map[string]any{}
	for _, raw := range out["rejected"].([]any) {
		r := raw.(map[string]any)
		rej[r["id"].(string)] = r
	}
	return acc, rej
}

func TestAFullPackSyncedInOrder(t *testing.T) {
	q := newQueue(t)
	q.lot("CHAI-E1", q.chai, 8, 6*time.Hour)
	q.lot("SAM-E1", q.samosa, 4, 6*time.Hour)
	r := `"request_id":"` + q.request + `"`
	events := []string{
		ev("evt-accept-01", "accept", `{`+r+`}`),
		ev("evt-scan-0001", "scan", `{`+r+`,"lot_code":"CHAI-E1","scanned_at":"2026-11-11T10:00:01Z"}`),
		ev("evt-scan-0002", "scan", `{`+r+`,"lot_code":"SAM-E1","scanned_at":"2026-11-11T10:00:02Z"}`),
		ev("evt-ready-001", "ready", `{`+r+`,"lot_numbers":["CHAI-E1","SAM-E1"]}`),
	}
	code, out := q.batch(q.staffTok, events...)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "EventBatchResult")
	if acc, rej := idsOf(out); len(acc) != 4 || len(rej) != 0 {
		t.Fatalf("%v", out)
	}
	if q.state() != "ready" {
		t.Errorf("%s", q.state())
	}
	// Sent again: the same answer, and nothing changes twice.
	seq := q.count("SELECT seq::int FROM restock_requests WHERE id = $1", q.request)
	if _, again := q.batch(q.staffTok, events...); len(again["accepted_ids"].([]any)) != 4 {
		t.Errorf("%v", again)
	}
	if q.count("SELECT seq::int FROM restock_requests WHERE id = $1", q.request) != seq || q.count("SELECT count(*) FROM hub_events") != 4 {
		t.Error("a repeated event changed something")
	}
}

func TestOneRejectedEventNeverBlocksTheRest(t *testing.T) {
	q := newQueue(t)
	q.lot("CHAI-E2", q.chai, 8, 6*time.Hour)
	r := `"request_id":"` + q.request + `"`
	other := q.id("INSERT INTO hubs (zone_id, name, lat, lng) VALUES ($1, 'Hub B', 12.94, 77.63) RETURNING id::text", q.zone)
	elsewhere := q.id("INSERT INTO restock_requests (rider_id, scooter_id, hub_id, source, state) VALUES ($1, (SELECT id FROM scooters WHERE code = 'SC-7'), $2, 'manual', 'cancelled') RETURNING id::text", q.rider, other)
	code, out := q.batch(q.staffTok,
		ev("evt-accept-02", "accept", `{`+r+`}`),
		ev("evt-badlot-01", "scan", `{`+r+`,"lot_code":"NOPE","scanned_at":"2026-11-11T10:00:01Z"}`),
		ev("evt-mystery-1", "teleport", `{`+r+`}`),
		`{"event_id":"bad","type":"scan","device_time":"2026-11-11T10:00:00Z","payload":{}}`,
		ev("evt-payload-1", "scan", `"oops"`),
		ev("evt-away-0001", "accept", `{"request_id":"`+elsewhere+`"}`),
		ev("evt-quaran-01", "quarantine", `{"lot_id":"`+q.id("SELECT id::text FROM lots WHERE code = 'CHAI-E2'")+`","reason":"damaged"}`),
		ev("evt-scan-0003", "scan", `{`+r+`,"lot_code":"CHAI-E2","scanned_at":"2026-11-11T10:00:02Z"}`),
	)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	acc, rej := idsOf(out)
	if strings.Join(acc, ",") != "evt-accept-02,evt-scan-0003" {
		t.Errorf("accepted: %v", acc)
	}
	for id, want := range map[string]string{"evt-badlot-01": "wrong_lot", "evt-mystery-1": "unknown_event_type", "bad": "invalid_event",
		"evt-payload-1": "invalid_event", "evt-away-0001": "not_found", "evt-quaran-01": "forbidden"} {
		if rej[id]["code"] != want || rej[id]["retryable"] != false {
			t.Errorf("%s: %v", id, rej[id])
		}
	}
	if q.state() != "packing" {
		t.Errorf("%s", q.state())
	}
}

func TestADirectCallAndItsQueuedCopyCountOnce(t *testing.T) {
	q := newQueue(t)
	q.lot("CHAI-E3", q.chai, 8, 6*time.Hour)
	q.accept()
	// The direct scan used the event_id as its Idempotency-Key, as the contract asks.
	req := `{"lot_code":"CHAI-E3","scanned_at":"2026-11-11T10:00:01Z"}`
	code, _ := q.callKey("POST", "/hub/restock/"+q.request+"/scan", q.staffTok, req, "evt-direct-001")
	if code != 200 {
		t.Fatal(code)
	}
	seq := q.count("SELECT seq::int FROM restock_requests WHERE id = $1", q.request)
	_, out := q.batch(q.staffTok, ev("evt-direct-001", "unscan", `{"request_id":"`+q.request+`","lot_code":"CHAI-E3"}`))
	if acc, _ := idsOf(out); len(acc) != 1 || q.count("SELECT seq::int FROM restock_requests WHERE id = $1", q.request) != seq {
		t.Errorf("the queued copy of a direct call must not apply again: %v", out)
	}
}

func TestABlockedStaffMembersBatchIsRefused(t *testing.T) {
	q := newQueue(t)
	q.exec("UPDATE hub_staff SET status = 'suspended' WHERE id = $1", q.staff)
	if code, out := q.batch(q.staffTok, ev("evt-blocked-1", "accept", `{"request_id":"`+q.request+`"}`)); code != 403 {
		t.Errorf("%d %v", code, out)
	}
	if code, _ := q.call("POST", "/hub/events", q.leadTok, `{"events":[]}`); code != 400 {
		t.Errorf("no events: %d", code)
	}
	var list []string
	for i := 0; i < 201; i++ {
		list = append(list, ev(fmt.Sprintf("evt-many-%04d", i), "accept", `{}`))
	}
	if code, _ := q.batch(q.leadTok, list...); code != 400 {
		t.Errorf("201 events: %d", code)
	}
}

// callKey is call with a chosen Idempotency-Key.
func (q *queue) callKey(method, path, token, body, key string) (int, map[string]any) {
	q.t.Helper()
	req, _ := http.NewRequest(method, q.srv.URL+"/v1"+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		q.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestQueuedEventsAreCheckedLikeTheirEndpoints(t *testing.T) {
	q := newQueue(t)
	q.lot("CHAI-E9", q.chai, 8, 6*time.Hour)
	q.accept()
	r := `"request_id":"` + q.request + `"`
	_, out := q.batch(q.staffTok,
		ev("evt-noscanat1", "scan", `{`+r+`,"lot_code":"CHAI-E9"}`),
		ev("evt-badscanat", "scan", `{`+r+`,"lot_code":"CHAI-E9","scanned_at":"yesterday"}`),
		ev("evt-emptylot1", "ready", `{`+r+`,"lot_numbers":[""]}`),
	)
	_, rej := idsOf(out)
	for _, id := range []string{"evt-noscanat1", "evt-badscanat", "evt-emptylot1"} {
		if rej[id]["code"] != "invalid_event" {
			t.Errorf("%s: %v", id, rej[id])
		}
	}
	if q.state() != "hub_accepted" {
		t.Errorf("a bad event changed the request: %s", q.state())
	}
}
