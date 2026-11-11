package restock

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// cancelRestock (issue 159).

func (f *fixture) riderSeq() int {
	return f.count("SELECT coalesce((SELECT seq FROM rider_state WHERE rider_id = $1), 0)::int", f.rider)
}

func (f *fixture) hubSeq() int {
	return f.count("SELECT coalesce((SELECT seq FROM hub_state WHERE hub_id = $1), 0)::int", f.hub)
}

func TestARiderCancelsAConfirmedRestock(t *testing.T) {
	f := newFixture(t)
	f.withRequests()
	f.hubLot(f.hub, "HUB-C-1", f.chai, 10, time.Hour)
	id := f.request(f.chai, 4)
	riderBefore, hubBefore := f.riderSeq(), f.hubSeq()
	code, out := f.post("/restock/"+id+"/cancel", f.token, `{"reason":"changed my mind"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "RestockResponse")
	if out["request"].(map[string]any)["state"] != "cancelled" || f.state(id) != "cancelled" {
		t.Errorf("%v", out)
	}
	if n := f.count("SELECT count(*) FROM restock_requests WHERE id = $1 AND reason = 'changed my mind'", id); n != 1 {
		t.Error("the reason was not kept")
	}
	if f.riderSeq() != riderBefore+1 {
		t.Error("the rider seq did not rise")
	}
	if f.hubSeq() <= hubBefore {
		t.Error("the hub did not see the request go")
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'restock.cancelled' AND entity_id = $1", id); n != 1 {
		t.Errorf("%d audit rows", n)
	}
	if n := f.count("SELECT count(*) FROM outbox_events WHERE topic = 'push.rider'"); n != 1 {
		t.Errorf("%d rider pushes", n)
	}
	if n := f.count("SELECT count(*) FROM rider_notifications WHERE rider_id = $1 AND kind = 'restock' AND title = 'Restock cancelled'", f.rider); n != 1 {
		t.Errorf("%d notifications", n)
	}
	// The scooter is free to ask again.
	if n := f.count("SELECT count(*) FROM restock_requests WHERE scooter_id = $1 AND state IN ('confirmed', 'hub_accepted', 'packing', 'ready', 'picked_up', 'partial')", f.scooter); n != 0 {
		t.Error("the scooter still has an open request")
	}
	// A request already cancelled returns the same request again, and changes nothing more.
	riderAfter := f.riderSeq()
	code, again := f.post("/restock/"+id+"/cancel", f.token, `{}`)
	if code != 200 || again["request"].(map[string]any)["id"] != id || again["request"].(map[string]any)["state"] != "cancelled" {
		t.Errorf("%d %v", code, again)
	}
	if f.riderSeq() != riderAfter || f.count("SELECT count(*) FROM audit_events WHERE action = 'restock.cancelled'") != 1 {
		t.Error("cancelling twice changed something")
	}
}

func TestCancellingAReleasesWhatTheRequestHeld(t *testing.T) {
	f := newFixture(t)
	f.withRequests()
	lot := f.hubLot(f.hub, "HUB-C-2", f.chai, 10, time.Hour)
	id := f.request(f.chai, 4)
	f.exec("INSERT INTO restock_lots (request_id, lot_id) VALUES ($1, $2)", id, lot)
	if code, out := f.post("/restock/"+id+"/cancel", f.token, ""); code != 200 {
		t.Fatalf("no body: %d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM restock_lots WHERE request_id = $1 AND released_at IS NOT NULL", id); n != 1 {
		t.Error("the lot was not put back")
	}
}

func TestCancellingASuggestedRequestThatWasConfirmedTellsTheHub(t *testing.T) {
	f := newFixture(t)
	f.withRequests()
	f.hubLot(f.hub, "HUB-C-3", f.chai, 10, time.Hour)
	id := f.request(f.chai, 4)
	f.exec("UPDATE restock_requests SET source = 'suggested' WHERE id = $1", id)
	before := f.hubSeq()
	if code, out := f.post("/restock/"+id+"/cancel", f.token, `{}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if f.hubSeq() <= before {
		t.Error("the hub was not told that a confirmed suggestion was cancelled")
	}
}

func TestOnlyBeforeTheHubAcceptsCanARequestBeCancelled(t *testing.T) {
	f := newFixture(t)
	f.withRequests()
	f.hubLot(f.hub, "HUB-C-4", f.chai, 10, time.Hour)
	id := f.request(f.chai, 4)
	for _, state := range []string{"hub_accepted", "packing", "ready", "picked_up", "partial", "received", "closed", "expired"} {
		f.exec("UPDATE restock_requests SET state = $2 WHERE id = $1", id, state)
		riderBefore := f.riderSeq()
		code, out := f.post("/restock/"+id+"/cancel", f.token, `{}`)
		if code != 409 || out["code"] != "restock_not_cancellable" {
			t.Errorf("%s: %d %v", state, code, out)
		}
		if f.state(id) != state || f.riderSeq() != riderBefore {
			t.Errorf("%s: a refused cancel changed something", state)
		}
	}
	// A suggestion is declined, not cancelled.
	f.exec("UPDATE restock_requests SET state = 'suggested', expires_at = now() + interval '10 minutes' WHERE id = $1", id)
	if code, out := f.post("/restock/"+id+"/cancel", f.token, `{}`); code != 409 || out["code"] != "restock_not_cancellable" {
		t.Errorf("a suggestion: %d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'restock.cancelled'"); n != 0 {
		t.Error("a refusal was audited as a cancel")
	}
}

func TestCancellingRestockRefusals(t *testing.T) {
	f := newFixture(t)
	f.withRequests()
	id := f.request(f.chai, 4)
	for _, bad := range []string{"nonsense", "00000000-0000-0000-0000-000000000001"} {
		if code, out := f.post("/restock/"+bad+"/cancel", f.token, `{}`); code != 404 || out["code"] != "not_found" {
			t.Errorf("%s: %d %v", bad, code, out)
		}
	}
	// Another rider's request is not found.
	_, other := f.signIn("", "")
	if code, out := f.post("/restock/"+id+"/cancel", other, `{}`); code != 404 {
		t.Errorf("another rider: %d %v", code, out)
	}
	if f.state(id) != "confirmed" {
		t.Error("another rider cancelled the request")
	}
	if code, out := f.post("/restock/"+id+"/cancel", f.token, `{"reason":"`+strings.Repeat("a", 201)+`"}`); code != 400 || out["code"] != "invalid_request" {
		t.Errorf("a long reason: %d %v", code, out)
	}
	if code, out := f.post("/restock/"+id+"/cancel", f.token, `[`); code != 400 || out["code"] != "invalid_request" {
		t.Errorf("not json: %d %v", code, out)
	}
	if code, _ := f.post("/restock/"+id+"/cancel", "", `{}`); code != 401 {
		t.Errorf("no session: %d", code)
	}
	f.exec("UPDATE riders SET status = 'suspended' WHERE id = $1", f.rider)
	if code, out := f.post("/restock/"+id+"/cancel", f.token, `{}`); code != 403 || out["code"] != "account_blocked" {
		t.Errorf("a suspended rider: %d %v", code, out)
	}
	if f.state(id) != "confirmed" {
		t.Error("a refused request changed the state")
	}
}

func TestCancellingARestockReplayActsOnce(t *testing.T) {
	f := newFixture(t)
	f.withRequests()
	id := f.request(f.chai, 4)
	send := func() (int, string, http.Header) {
		req, _ := http.NewRequest("POST", f.srv.URL+"/v1/restock/"+id+"/cancel", strings.NewReader(`{"reason":"x"}`))
		req.Header.Set("Authorization", "Bearer "+f.token)
		req.Header.Set("Idempotency-Key", "restock-cancel-0001")
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
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'restock.cancelled'"); n != 1 {
		t.Errorf("%d audit rows after a replay", n)
	}
}

// Every push.rider the restock jobs queue also writes a notification.
func TestRestockPushesAreRecordedAsNotifications(t *testing.T) {
	f := newFixture(t)
	f.hubLot(f.hub, "HUB-N-1", f.chai, 10, time.Hour)
	id := f.request(f.chai, 4)
	f.packed(time.Now())
	pushes := f.count("SELECT count(*) FROM outbox_events WHERE topic = 'push.rider'")
	notes := f.count("SELECT count(*) FROM rider_notifications WHERE rider_id = $1 AND kind = 'restock'", f.rider)
	if pushes == 0 || pushes != notes {
		t.Errorf("%d pushes but %d notifications (request %s, state %s)", pushes, notes, id, f.state(id))
	}
	if n := f.count("SELECT count(*) FROM rider_notifications WHERE title = 'Restock ready'"); n != 1 {
		t.Errorf("%d ready notifications", n)
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
