package shifts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The rider app's operations (issue 159): devices, breaks, the shift list and summary, and the hand
// count of stock.

// sendKey is send with a chosen Idempotency-Key, to test a replay.
func (f *fixture) sendKey(method, path, token, key, body string) (int, map[string]any, http.Header) {
	f.t.Helper()
	req, _ := http.NewRequest(method, f.srv.URL+"/v1"+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, resp.Header
}

func (f *fixture) get(path, token string) (int, map[string]any) {
	f.t.Helper()
	req, _ := http.NewRequest("GET", f.srv.URL+"/v1"+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (f *fixture) device(rider string) string {
	f.t.Helper()
	f.keys++
	return f.id("INSERT INTO devices (actor_type, actor_id, platform, push_token) VALUES ('rider', $1, 'android', $2) RETURNING id::text", rider, fmt.Sprintf("token-%d", f.keys))
}

// breakLimit gives the zone its own longest break, as a new settings version.
func (f *fixture) breakLimit(seconds int) {
	f.t.Helper()
	f.exec("INSERT INTO zone_settings (zone_id, version, max_break_s, reason, created_by) VALUES ($1, (SELECT coalesce(max(version), 0) + 1 FROM zone_settings WHERE zone_id = $1), $2, 'test', 'test')", f.zone, seconds)
}

func (f *fixture) seq(rider string) int {
	return f.count("SELECT coalesce((SELECT seq FROM rider_state WHERE rider_id = $1), 0)", rider)
}

func shiftOf(out map[string]any) map[string]any { return out["shift"].(map[string]any) }

// order makes an order for the rider in the given state, with an offer in the given state.
func (f *fixture) order(rider, scooter, orderState string) string {
	f.t.Helper()
	f.keys++
	person := f.id("INSERT INTO people (phone) VALUES ($1) RETURNING id::text", fmt.Sprintf("+9197000007%03d", f.keys))
	customer := f.id("INSERT INTO customers (person_id) VALUES ($1) RETURNING id::text", person)
	gate := f.id("INSERT INTO gates (customer_id, zone_id, label, lat, lng) VALUES ($1, $2, 'Gate', 12.93, 77.62) RETURNING id::text", customer, f.zone)
	history := f.id("INSERT INTO gate_history (gate_id, version, label, lat, lng, verified, reason) VALUES ($1, 1, 'Gate', 12.93, 77.62, false, 'test') RETURNING id::text", gate)
	return f.id(`INSERT INTO orders (customer_id, idempotency_key, gate_id, gate_history_id, zone_id, scooter_id, global_settings, state, total_paise, hold_until, paid_at, rider_id)
		VALUES ($1, $2, $3, $4, $5, $6, (SELECT id FROM zone_settings WHERE zone_id IS NULL ORDER BY version DESC LIMIT 1), $7, 9900, now(), now(), $8)
		RETURNING id::text`, customer, fmt.Sprintf("key-%05d", f.keys), gate, history, f.zone, scooter, orderState, rider)
}

func (f *fixture) offerFor(order, rider, scooter, state, expires string) {
	f.t.Helper()
	f.keys++
	f.exec(`INSERT INTO offers (order_id, rider_id, scooter_id, fencing_token, expires_at, state, rider_accepted_at, released_at, ended_reason, final_state)
		VALUES ($1, $2, $3, $4, now() + $5::interval, $6::text, CASE WHEN $6::text = 'accepted' THEN now() END,
			CASE WHEN $6::text IN ('declined', 'expired') THEN now() END, CASE WHEN $6::text IN ('declined', 'expired') THEN 'declined' END,
			CASE WHEN $6::text IN ('declined', 'expired') THEN 'OFFERED' END)`,
		order, rider, scooter, fmt.Sprintf("fence-%d", f.keys), expires, state)
}

func TestStartShiftChecksTheDevice(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	rider, token := f.rider()
	other, _ := f.rider()
	mine, theirs := f.device(rider), f.device(other)
	start := func(extra string) (int, map[string]any) {
		return f.send("POST", "/riders/me/shifts/start", token, `{"node_id":"`+f.node+`"`+extra+`}`)
	}
	for name, extra := range map[string]string{
		"a device nobody registered":  `,"device_id":"` + "00000000-0000-0000-0000-000000000001" + `"`,
		"another rider's device":      `,"device_id":"` + theirs + `"`,
		"take_over with no device_id": `,"take_over":true`,
	} {
		code, out := start(extra)
		want := "unknown_device"
		if strings.HasPrefix(name, "take_over") {
			want = "invalid_request"
		}
		if code != 400 || out["code"] != want {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if code, out := start(`,"device_id":"not-an-id"`); code != 400 || out["code"] != "invalid_request" {
		t.Errorf("a bad id: %d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM shifts WHERE rider_id = $1", rider); n != 0 {
		t.Fatalf("a refused start left %d shifts", n)
	}
	code, out := start(`,"device_id":"` + mine + `"`)
	if code != 200 || shiftOf(out)["device_id"] != mine {
		t.Fatalf("%d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM shifts WHERE device_id = $1", mine); n != 1 {
		t.Error("the shift does not keep its device")
	}
	// A start with no device leaves it null.
	f.send("POST", "/riders/me/shifts/end", token, "")
	code, out = start("")
	if code != 200 || shiftOf(out)["device_id"] != nil {
		t.Errorf("a start without a device: %d %v", code, out)
	}
}

func TestTakeOverMovesTheOpenShiftToTheNewPhone(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	rider, token := f.rider()
	one, two := f.device(rider), f.device(rider)
	body := func(device string, takeOver bool) string {
		return fmt.Sprintf(`{"node_id":"%s","device_id":"%s","take_over":%t}`, f.node, device, takeOver)
	}
	code, first := f.send("POST", "/riders/me/shifts/start", token, body(one, false))
	if code != 200 {
		t.Fatalf("%d %v", code, first)
	}
	if code, out := f.send("POST", "/riders/me/shifts/start", token, body(two, false)); code != 409 || out["code"] != "shift_already_open" {
		t.Errorf("without take_over: %d %v", code, out)
	}
	before := f.seq(rider)
	code, out := f.send("POST", "/riders/me/shifts/start", token, body(two, true))
	if code != 200 || shiftOf(out)["device_id"] != two || shiftOf(out)["id"] != shiftOf(first)["id"] {
		t.Fatalf("take over: %d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM shifts WHERE rider_id = $1", rider); n != 1 {
		t.Errorf("%d shifts after a take over", n)
	}
	if f.seq(rider) != before+1 {
		t.Error("a take over did not raise the rider seq")
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'shift.taken_over'"); n != 1 {
		t.Errorf("%d audit rows", n)
	}
	// The same phone again changes nothing.
	again := f.seq(rider)
	if code, out := f.send("POST", "/riders/me/shifts/start", token, body(two, true)); code != 200 || shiftOf(out)["device_id"] != two {
		t.Errorf("the same phone: %d %v", code, out)
	}
	if f.seq(rider) != again {
		t.Error("taking over from the same phone raised the seq")
	}
	// With no shift open, take_over simply starts one.
	f.send("POST", "/riders/me/shifts/end", token, "")
	if code, out := f.send("POST", "/riders/me/shifts/start", token, body(one, true)); code != 200 || shiftOf(out)["device_id"] != one {
		t.Errorf("take_over with no shift: %d %v", code, out)
	}
	// A device another person's token names is refused even when taking over.
	other, otherToken := f.rider()
	f.scooter("SC-2")
	if code, out := f.send("POST", "/riders/me/shifts/start", otherToken, body(one, true)); code != 400 || out["code"] != "unknown_device" {
		t.Errorf("another rider taking over with this rider's phone: %d %v (%s)", code, out, other)
	}
}

func TestABreakStartsAndEnds(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	rider, token := f.rider()
	if code, out := f.send("POST", "/riders/me/breaks/start", token, ""); code != 409 || out["code"] != "no_open_shift" {
		t.Errorf("a break with no shift: %d %v", code, out)
	}
	if code, out := f.send("POST", "/riders/me/breaks/end", token, ""); code != 409 || out["code"] != "no_open_shift" {
		t.Errorf("ending a break with no shift: %d %v", code, out)
	}
	f.breakLimit(600)
	f.start(token)
	before := f.seq(rider)
	code, out := f.send("POST", "/riders/me/breaks/start", token, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "ShiftResponse")
	sh := shiftOf(out)
	check(t, sh, "Shift")
	started, _ := time.Parse(time.RFC3339Nano, sh["break_started_at"].(string))
	ends, _ := time.Parse(time.RFC3339Nano, sh["break_ends_at"].(string))
	if ends.Sub(started) != 600*time.Second {
		t.Errorf("the break lasts %v, want the console's 600 seconds", ends.Sub(started))
	}
	if f.seq(rider) != before+1 {
		t.Error("starting a break did not raise the rider seq")
	}
	if n := f.count("SELECT count(*) FROM shift_breaks WHERE ended_at IS NULL"); n != 1 {
		t.Errorf("%d open break rows", n)
	}
	if code, out := f.send("POST", "/riders/me/breaks/start", token, ""); code != 409 || out["code"] != "on_break" {
		t.Errorf("a second break: %d %v", code, out)
	}
	// getRiderState-style reads show the break through the shift too.
	if n := f.count("SELECT count(*) FROM shifts WHERE rider_id = $1 AND break_started_at IS NOT NULL AND break_ends_at IS NOT NULL", rider); n != 1 {
		t.Error("the shift does not show the break")
	}
	code, out = f.send("POST", "/riders/me/breaks/end", token, "")
	if code != 200 || shiftOf(out)["break_started_at"] != nil || shiftOf(out)["break_ends_at"] != nil {
		t.Fatalf("%d %v", code, out)
	}
	if f.seq(rider) != before+2 {
		t.Error("ending a break did not raise the rider seq")
	}
	if n := f.count("SELECT count(*) FROM shift_breaks WHERE ended_by = 'rider' AND ended_at IS NOT NULL"); n != 1 {
		t.Error("the break row was not closed by the rider")
	}
	// Ending when no break runs returns the shift unchanged and raises nothing.
	again := f.seq(rider)
	if code, out := f.send("POST", "/riders/me/breaks/end", token, ""); code != 200 || shiftOf(out)["break_started_at"] != nil {
		t.Errorf("ending with no break: %d %v", code, out)
	}
	if f.seq(rider) != again {
		t.Error("ending with no break raised the seq")
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action IN ('shift.break_started', 'shift.break_ended')"); n != 2 {
		t.Errorf("%d break audit rows, want 2", n)
	}
	// The next break uses the console's value as it is then.
	f.breakLimit(1200)
	_, out = f.send("POST", "/riders/me/breaks/start", token, "")
	started, _ = time.Parse(time.RFC3339Nano, shiftOf(out)["break_started_at"].(string))
	ends, _ = time.Parse(time.RFC3339Nano, shiftOf(out)["break_ends_at"].(string))
	if ends.Sub(started) != 1200*time.Second {
		t.Errorf("the second break lasts %v", ends.Sub(started))
	}
}

func TestTheLongestBreakDefaultsToThirtyMinutes(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	_, token := f.rider()
	f.start(token)
	_, out := f.send("POST", "/riders/me/breaks/start", token, "")
	started, _ := time.Parse(time.RFC3339Nano, shiftOf(out)["break_started_at"].(string))
	ends, _ := time.Parse(time.RFC3339Nano, shiftOf(out)["break_ends_at"].(string))
	if ends.Sub(started) != 1800*time.Second {
		t.Errorf("the default break is %v, want 30 minutes", ends.Sub(started))
	}
}

func TestABreakIsRefusedWithAnOrderOrAnOfferWaiting(t *testing.T) {
	f := newFixture(t)
	scooter := f.scooter("SC-1")
	rider, token := f.rider()
	f.start(token)

	order := f.order(rider, scooter, "OFFERED")
	f.offerFor(order, rider, scooter, "open", "1 minute")
	if code, out := f.send("POST", "/riders/me/breaks/start", token, ""); code != 409 || out["code"] != "offer_open" {
		t.Errorf("with an offer waiting: %d %v", code, out)
	}
	// An offer past its time cannot be answered, so it does not stop a break.
	f.exec("UPDATE offers SET expires_at = now() - interval '1 minute'")
	if code, out := f.send("POST", "/riders/me/breaks/start", token, ""); code != 200 {
		t.Errorf("with a lapsed offer: %d %v", code, out)
	}
	f.send("POST", "/riders/me/breaks/end", token, "")

	f.exec("UPDATE offers SET state = 'accepted', rider_accepted_at = now(), expires_at = now() + interval '1 minute'")
	if code, out := f.send("POST", "/riders/me/breaks/start", token, ""); code != 409 || out["code"] != "order_in_progress" {
		t.Errorf("with a live order: %d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM shifts WHERE break_started_at IS NOT NULL"); n != 0 {
		t.Error("a refused break was recorded")
	}

	// A finished offer does not count.
	f.exec("UPDATE offers SET released_at = now(), final_state = 'HANDED_OVER'")
	if code, out := f.send("POST", "/riders/me/breaks/start", token, ""); code != 200 {
		t.Errorf("with the order handed over: %d %v", code, out)
	}
}

func TestAReplayedBreakStartActsOnce(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	rider, token := f.rider()
	f.start(token)
	code, first, _ := f.sendKey("POST", "/riders/me/breaks/start", token, "break-key-0001", "")
	code2, second, header := f.sendKey("POST", "/riders/me/breaks/start", token, "break-key-0001", "")
	if code != 200 || code2 != 200 || shiftOf(first)["break_started_at"] != shiftOf(second)["break_started_at"] {
		t.Fatalf("%d %v / %d %v", code, first, code2, second)
	}
	if header.Get("Idempotency-Replayed") == "" {
		t.Errorf("the replay is not marked: %v", header)
	}
	if n := f.count("SELECT count(*) FROM shift_breaks"); n != 1 {
		t.Errorf("%d break rows after a replay", n)
	}
	if n := f.seq(rider); n != 2 {
		t.Errorf("seq %d after a replay, want 2 (start of shift, one break)", n)
	}
	// The same key with a body change is not allowed to run twice either: end then replay start.
	f.send("POST", "/riders/me/breaks/end", token, "")
	if code, _, _ := f.sendKey("POST", "/riders/me/breaks/start", token, "break-key-0001", ""); code != 200 {
		t.Errorf("replay after the break ended: %d", code)
	}
	if n := f.count("SELECT count(*) FROM shift_breaks"); n != 1 {
		t.Errorf("the replay started another break: %d rows", n)
	}
}

func TestABreakEndsOnItsOwnAtItsTime(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	rider, token := f.rider()
	f.breakLimit(300)
	f.start(token)
	f.send("POST", "/riders/me/breaks/start", token, "")
	svc := &Service{Pool: f.s.Pool, Log: quiet}
	ctx := context.Background()
	if n, err := svc.EndDueBreaks(ctx, time.Now().Add(4*time.Minute)); err != nil || n != 0 {
		t.Fatalf("before its time: %d %v", n, err)
	}
	if n := f.count("SELECT count(*) FROM shifts WHERE break_started_at IS NOT NULL"); n != 1 {
		t.Fatal("the break ended early")
	}
	before := f.seq(rider)
	if n, err := svc.EndDueBreaks(ctx, time.Now().Add(6*time.Minute)); err != nil || n != 1 {
		t.Fatalf("at its time: %d %v", n, err)
	}
	if n := f.count("SELECT count(*) FROM shifts WHERE break_started_at IS NULL AND break_ends_at IS NULL"); n != 1 {
		t.Error("the shift still shows a break")
	}
	if n := f.count("SELECT count(*) FROM shift_breaks WHERE ended_by = 'timeout' AND ended_at = started_at + interval '300 seconds'"); n != 1 {
		t.Error("the break row was not closed at its time by the timeout")
	}
	if f.seq(rider) != before+1 {
		t.Error("the automatic end did not raise the rider seq")
	}
	if n := f.count("SELECT count(*) FROM rider_notifications WHERE rider_id = $1 AND kind = 'shift'", rider); n != 1 {
		t.Errorf("%d notifications for the end of the break", n)
	}
	if n := f.count("SELECT count(*) FROM outbox_events WHERE topic = 'push.rider'"); n != 1 {
		t.Errorf("%d rider pushes", n)
	}
	if n, err := svc.EndDueBreaks(ctx, time.Now().Add(time.Hour)); err != nil || n != 0 {
		t.Errorf("a second pass: %d %v", n, err)
	}
	// A break that ended is not counted past its time in the shift list.
	code, out := f.get("/riders/me/shifts", token)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if got := out["shifts"].([]any)[0].(map[string]any)["break_s"]; got != float64(300) {
		t.Errorf("break_s %v, want 300", got)
	}
}

func TestEndingTheShiftEndsABreak(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	_, token := f.rider()
	f.start(token)
	f.send("POST", "/riders/me/breaks/start", token, "")
	code, out := f.send("POST", "/riders/me/shifts/end", token, "")
	if code != 200 || shiftOf(out)["break_started_at"] != nil || shiftOf(out)["ended_at"] == nil {
		t.Fatalf("%d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM shift_breaks WHERE ended_by = 'shift_end' AND ended_at IS NOT NULL"); n != 1 {
		t.Error("the break was not ended by the end of the shift")
	}
	if n := f.count("SELECT count(*) FROM shifts WHERE break_started_at IS NOT NULL"); n != 0 {
		t.Error("an ended shift still shows a break")
	}
	// The next shift starts with no break.
	_, out = f.start(token)
	if shiftOf(out)["break_started_at"] != nil {
		t.Errorf("a new shift starts on a break: %v", out)
	}
}

func TestBreakRoutesNeedASession(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/riders/me/breaks/start", "/riders/me/breaks/end", "/riders/me/stock/count"} {
		if code, _ := f.send("POST", path, "", "{}"); code != 401 {
			t.Errorf("%s with no session: %d", path, code)
		}
	}
	for _, path := range []string{"/riders/me/shifts", "/riders/me/shifts/" + "00000000-0000-0000-0000-000000000001/summary"} {
		if code, _ := f.get(path, ""); code != 401 {
			t.Errorf("%s with no session: %d", path, code)
		}
	}
	rider, token := f.rider()
	f.exec("UPDATE riders SET status = 'suspended' WHERE id = $1", rider)
	if code, _ := f.send("POST", "/riders/me/breaks/start", token, ""); code != 403 {
		t.Errorf("a suspended rider's break: %d", code)
	}
}

// shiftAt inserts a finished shift that began ago before now and lasted length.
func (f *fixture) shiftAt(rider, scooter string, ago, length time.Duration) string {
	f.t.Helper()
	return f.id(`INSERT INTO shifts (rider_id, node_id, scooter_id, started_at, ended_at)
		VALUES ($1, $2, $3, now() - make_interval(secs => $4), now() - make_interval(secs => $5)) RETURNING id::text`,
		rider, f.node, scooter, ago.Seconds(), (ago - length).Seconds())
}

func TestShiftListPagesNewestFirst(t *testing.T) {
	f := newFixture(t)
	scooter := f.scooter("SC-1")
	rider, token := f.rider()
	other, _ := f.rider()
	var ids []string
	for i := 5; i >= 1; i-- { // Oldest first, so the newest is the last.
		ids = append(ids, f.shiftAt(rider, scooter, time.Duration(i*24)*time.Hour, 2*time.Hour))
	}
	f.shiftAt(other, scooter, 3*time.Hour, time.Hour)
	// A break of 30 minutes in the newest shift, and an order handed over in it.
	newest := ids[len(ids)-1]
	f.exec(`INSERT INTO shift_breaks (shift_id, started_at, ended_at, ended_by)
		SELECT id, started_at + interval '10 minutes', started_at + interval '40 minutes', 'rider' FROM shifts WHERE id = $1`, newest)
	order := f.order(rider, scooter, "COMPLETED")
	f.exec("UPDATE orders SET handed_over_at = (SELECT started_at + interval '1 hour' FROM shifts WHERE id = $2), rider_accepted_at = (SELECT started_at + interval '50 minutes' FROM shifts WHERE id = $2) WHERE id = $1", order, newest)

	seen := []string{}
	cursor := ""
	for page := 0; page < 5; page++ {
		path := "/riders/me/shifts?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		code, out := f.get(path, token)
		if code != 200 {
			t.Fatalf("%d %v", code, out)
		}
		check(t, out, "ShiftList")
		for _, raw := range out["shifts"].([]any) {
			e := raw.(map[string]any)
			check(t, e, "ShiftEntry")
			seen = append(seen, e["id"].(string))
			if e["id"] == newest {
				if e["break_s"] != float64(1800) || e["online_s"] != float64(2*3600-1800) || e["orders_handed_over"] != float64(1) {
					t.Errorf("the newest shift: %v", e)
				}
			} else if e["online_s"] != float64(7200) || e["break_s"] != float64(0) || e["orders_handed_over"] != float64(0) {
				t.Errorf("a shift with no break: %v", e)
			}
		}
		if out["next_cursor"] == nil {
			break
		}
		cursor = out["next_cursor"].(string)
	}
	if len(seen) != 5 {
		t.Fatalf("saw %d shifts, want 5 (the other rider's must not show): %v", len(seen), seen)
	}
	for i, id := range seen {
		if id != ids[len(ids)-1-i] {
			t.Errorf("position %d is %s, want newest first", i, id)
		}
	}
}

func TestShiftListFiltersByLocalDayAndRefusesBadInput(t *testing.T) {
	f := newFixture(t)
	scooter := f.scooter("SC-1")
	_, token := f.rider()
	rider := f.id("SELECT id::text FROM riders LIMIT 1")
	// 2025-03-09 23:30 UTC is 2025-03-10 05:00 in the zone's local time.
	f.exec(`INSERT INTO shifts (rider_id, node_id, scooter_id, started_at, ended_at) VALUES
		($1, $2, $3, '2025-03-09 23:30:00+00', '2025-03-10 02:00:00+00'),
		($1, $2, $3, '2025-03-09 17:00:00+00', '2025-03-09 18:00:00+00')`, rider, f.node, scooter)
	list := func(query string) int {
		code, out := f.get("/riders/me/shifts?"+query, token)
		if code != 200 {
			t.Fatalf("%s: %d %v", query, code, out)
		}
		return len(out["shifts"].([]any))
	}
	if n := list("from=2025-03-10&to=2025-03-10"); n != 1 {
		t.Errorf("the local day 10 Nov: %d shifts, want 1 (started 05:00 local)", n)
	}
	if n := list("from=2025-03-09&to=2025-03-09"); n != 1 {
		t.Errorf("the local day 9 Nov: %d shifts, want 1 (started 22:30 local)", n)
	}
	if n := list("from=2025-03-09&to=2025-03-10"); n != 2 {
		t.Errorf("both days: %d", n)
	}
	if n := list("from=2025-03-11"); n != 0 {
		t.Errorf("after: %d", n)
	}
	if n := list("to=2025-03-08"); n != 0 {
		t.Errorf("before: %d", n)
	}
	for _, q := range []string{"from=2025-03-11&to=2025-03-10", "from=yesterday", "to=2026-13-40", "limit=0", "limit=101", "limit=x", "cursor=nonsense"} {
		if code, out := f.get("/riders/me/shifts?"+q, token); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", q, code, out)
		}
	}
}

func TestAnOpenShiftIsCountedUpToNow(t *testing.T) {
	f := newFixture(t)
	scooter := f.scooter("SC-1")
	rider, token := f.rider()
	f.exec(`INSERT INTO shifts (rider_id, node_id, scooter_id, started_at) VALUES ($1, $2, $3, now() - interval '3 hours')`, rider, f.node, scooter)
	f.exec(`UPDATE scooters SET current_rider_id = $1 WHERE id = $2`, rider, scooter)
	f.exec(`INSERT INTO shift_breaks (shift_id, started_at, ended_at, ended_by) SELECT id, now() - interval '2 hours', now() - interval '90 minutes', 'rider' FROM shifts`)
	f.exec(`INSERT INTO shift_breaks (shift_id, started_at) SELECT id, now() - interval '10 minutes' FROM shifts`)
	f.exec(`UPDATE shifts SET break_started_at = now() - interval '10 minutes', break_ends_at = now() + interval '20 minutes'`)
	code, out := f.get("/riders/me/shifts", token)
	e := out["shifts"].([]any)[0].(map[string]any)
	if code != 200 || e["ended_at"] != nil {
		t.Fatalf("%d %v", code, out)
	}
	// 3 hours, less 30 minutes and the 10 minutes of the break still running (allow a second of slack).
	if b, o := e["break_s"].(float64), e["online_s"].(float64); b < 2400 || b > 2410 || o < 3*3600-2410 || o > 3*3600-2400 {
		t.Errorf("break_s %v online_s %v", b, o)
	}
}

func TestShiftSummaryOfAnEndedShift(t *testing.T) {
	f := newFixture(t)
	scooter := f.scooter("SC-1")
	rider, token := f.rider()
	other, otherToken := f.rider()
	shift := f.shiftAt(rider, scooter, 10*time.Hour, 4*time.Hour)
	f.exec(`INSERT INTO shift_breaks (shift_id, started_at, ended_at, ended_by) SELECT id, started_at + interval '1 hour', started_at + interval '1 hour 20 minutes', 'rider' FROM shifts WHERE id = $1`, shift)
	price := f.id("INSERT INTO price_history (item_id, price_paise, reason) VALUES ($1, 4900, 'test') RETURNING id::text", f.item)
	tea := func(qty, handed int, minutes, accepted int) {
		o := f.order(rider, scooter, "COMPLETED")
		f.exec(`UPDATE orders SET handed_over_at = now() - interval '9 hours' + make_interval(mins => $2), rider_accepted_at = now() - interval '9 hours' + make_interval(mins => $2 - $3) WHERE id = $1`, o, minutes, accepted)
		f.exec(`INSERT INTO order_items (order_id, item_id, price_history_id, name, qty, unit_paise, qty_handed) VALUES ($1, $2, $3, 'Chai', $4, 4900, $5)`, o, f.item, price, qty, handed)
	}
	tea(2, 2, 30, 4) // 4 minutes from accept to handover
	tea(3, 2, 50, 6) // 6 minutes; one unit was not handed over
	// An order handed over after the shift ended belongs to no shift of this one.
	late := f.order(rider, scooter, "COMPLETED")
	f.exec(`UPDATE orders SET handed_over_at = now(), rider_accepted_at = now() - interval '1 minute' WHERE id = $1`, late)
	f.exec(`INSERT INTO order_items (order_id, item_id, price_history_id, name, qty, unit_paise, qty_handed) VALUES ($1, $2, $3, 'Chai', 1, 4900, 1)`, late, f.item, price)
	// Pay: two orders in the shift, one outside it.
	for _, at := range []string{"9 hours", "8 hours 30 minutes", "1 hour"} {
		f.exec(`INSERT INTO earnings (rider_id, kind, order_id, amount_paise, day, earned_at)
			SELECT $1, 'order', (SELECT id FROM orders WHERE rider_id = $1 AND id NOT IN (SELECT order_id FROM earnings WHERE order_id IS NOT NULL) ORDER BY created_at LIMIT 1),
				4000, current_date, now() - $2::interval`, rider, at)
	}
	f.exec("UPDATE shifts SET returned_items = $2::jsonb WHERE id = $1", shift, fmt.Sprintf(`[{"item_id":"%s","name":"Chai","qty":3}]`, f.item))

	code, out := f.get("/riders/me/shifts/"+shift+"/summary", token)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "ShiftSummary")
	if out["break_s"] != float64(1200) || out["online_s"] != float64(4*3600-1200) || out["orders_handed_over"] != float64(2) {
		t.Errorf("%v", out)
	}
	if out["avg_delivery_s"] != float64(300) {
		t.Errorf("avg_delivery_s %v, want 300 (4 and 6 minutes)", out["avg_delivery_s"])
	}
	if got := out["earnings"].(map[string]any); got["amount_minor"] != float64(8000) || got["currency"] != "INR" {
		t.Errorf("earnings %v, want only the two inside the shift", got)
	}
	sold := out["items_sold"].([]any)
	if len(sold) != 1 || sold[0].(map[string]any)["qty"] != float64(4) || sold[0].(map[string]any)["item_id"] != f.item {
		t.Errorf("items_sold %v, want what was handed (2 + 2), not what was ordered", sold)
	}
	back := out["items_returned"].([]any)
	if len(back) != 1 || back[0].(map[string]any)["qty"] != float64(3) || back[0].(map[string]any)["name"] != "Chai" {
		t.Errorf("items_returned %v", back)
	}
	// Another rider's shift is not found, and so is an id that is no shift.
	if code, out := f.get("/riders/me/shifts/"+shift+"/summary", otherToken); code != 404 || out["code"] != "not_found" {
		t.Errorf("another rider's shift: %d %v (%s)", code, out, other)
	}
	for _, id := range []string{"00000000-0000-0000-0000-000000000001", "nonsense"} {
		if code, _ := f.get("/riders/me/shifts/"+id+"/summary", token); code != 404 {
			t.Errorf("%s: %d", id, code)
		}
	}
}

func TestShiftSummaryWithNothingDoneAndOfAnOpenShift(t *testing.T) {
	f := newFixture(t)
	scooter := f.scooter("SC-1")
	lot := f.lot("LOT-1", 4, time.Hour)
	_, token := f.rider()
	_, out := f.start(token)
	shift := shiftOf(out)["id"].(string)
	f.exec("INSERT INTO scooter_stock (scooter_id, lot_id, on_hand) VALUES ($1, $2, 4)", scooter, lot)
	code, sum := f.get("/riders/me/shifts/"+shift+"/summary", token)
	if code != 200 {
		t.Fatalf("%d %v", code, sum)
	}
	check(t, sum, "ShiftSummary")
	if sum["ended_at"] != nil || sum["avg_delivery_s"] != nil || sum["orders_handed_over"] != float64(0) ||
		len(sum["items_sold"].([]any)) != 0 || len(sum["items_returned"].([]any)) != 0 || sum["earnings"].(map[string]any)["amount_minor"] != float64(0) {
		t.Errorf("an open shift: %v", sum)
	}
	// Ending the shift keeps what stood on the scooter, and the summary shows it.
	f.send("POST", "/riders/me/shifts/end", token, "")
	_, sum = f.get("/riders/me/shifts/"+shift+"/summary", token)
	back := sum["items_returned"].([]any)
	if sum["ended_at"] == nil || len(back) != 1 || back[0].(map[string]any)["qty"] != float64(4) || back[0].(map[string]any)["item_id"] != f.item {
		t.Errorf("an ended shift: %v", sum)
	}
	// Stock that changes afterwards does not rewrite it.
	f.exec("UPDATE scooter_stock SET on_hand = 0")
	_, sum = f.get("/riders/me/shifts/"+shift+"/summary", token)
	if back := sum["items_returned"].([]any); len(back) != 1 || back[0].(map[string]any)["qty"] != float64(4) {
		t.Errorf("the summary changed with later stock: %v", sum)
	}
}

func TestAHandCountComparesWithTheScooter(t *testing.T) {
	f := newFixture(t)
	scooter := f.scooter("SC-1")
	lot := f.lot("LOT-1", 5, time.Hour)
	rider, token := f.rider()
	count := func(body string) (int, map[string]any) { return f.send("POST", "/riders/me/stock/count", token, body) }
	if code, out := count(`{"lines":[{"item_id":"` + f.item + `","qty":1}],"reason":"scanner broke"}`); code != 409 || out["code"] != "no_open_shift" {
		t.Errorf("with no shift: %d %v", code, out)
	}
	f.start(token)
	f.exec("INSERT INTO scooter_stock (scooter_id, lot_id, on_hand, reserved) VALUES ($1, $2, 5, 1)", scooter, lot)
	second := f.id("INSERT INTO menu_items (name, price_paise, status) VALUES ('Samosa', 3000, 'active') RETURNING id::text")
	audit := f.count("SELECT count(*) FROM audit_events")
	code, out := count(`{"lines":[{"item_id":"` + f.item + `","qty":4},{"item_id":"` + second + `","qty":0}],"reason":"scanner broke"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "StockCountResponse")
	lines := out["counted"].([]any)
	first, none := lines[0].(map[string]any), lines[1].(map[string]any)
	if first["expected"] != float64(5) || first["counted"] != float64(4) || none["expected"] != float64(0) || none["counted"] != float64(0) || out["mismatches"] != float64(1) {
		t.Errorf("%v", out)
	}
	// An item not on the scooter has expected 0, so counting some is a mismatch.
	if _, out := count(`{"lines":[{"item_id":"` + second + `","qty":2}],"reason":"scanner broke"}`); out["mismatches"] != float64(1) {
		t.Errorf("counting an item that is not on the scooter: %v", out)
	}
	// Only the lines sent are compared: counting everything right is no mismatch.
	if _, out := count(`{"lines":[{"item_id":"` + f.item + `","qty":5}],"reason":"recount"}`); out["mismatches"] != float64(0) || len(out["counted"].([]any)) != 1 {
		t.Errorf("a right count: %v", out)
	}
	if n := f.count("SELECT count(*) FROM stock_counts WHERE rider_id = $1 AND scooter_id = $2", rider, scooter); n != 3 {
		t.Errorf("%d counts stored", n)
	}
	if n := f.count("SELECT count(*) FROM stock_count_lines"); n != 4 {
		t.Errorf("%d count lines stored", n)
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'stock.counted'"); n != 3 || f.count("SELECT count(*) FROM audit_events") != audit+3 {
		t.Errorf("%d audit rows for the counts", n)
	}
	// It records, and does not change the stock.
	if n := f.count("SELECT on_hand FROM scooter_stock WHERE scooter_id = $1", scooter); n != 5 {
		t.Errorf("a count changed the stock to %d", n)
	}
	if n := f.count("SELECT count(*) FROM stock_ledger WHERE reason <> 'scan'"); n != 0 {
		t.Error("a count wrote to the ledger")
	}
}

func TestAHandCountRefusesBadInput(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	_, token := f.rider()
	f.start(token)
	item := `{"item_id":"` + f.item + `","qty":1}`
	for name, body := range map[string]string{
		"no lines":            `{"lines":[],"reason":"x"}`,
		"no reason":           `{"lines":[` + item + `]}`,
		"a blank reason":      `{"lines":[` + item + `],"reason":"   "}`,
		"a long reason":       `{"lines":[` + item + `],"reason":"` + strings.Repeat("a", 201) + `"}`,
		"the same item twice": `{"lines":[` + item + `,` + item + `],"reason":"x"}`,
		"a negative qty":      `{"lines":[{"item_id":"` + f.item + `","qty":-1}],"reason":"x"}`,
		"a missing qty":       `{"lines":[{"item_id":"` + f.item + `"}],"reason":"x"}`,
		"a bad item id":       `{"lines":[{"item_id":"nope","qty":1}],"reason":"x"}`,
		"an unknown item":     `{"lines":[{"item_id":"00000000-0000-0000-0000-000000000009","qty":1}],"reason":"x"}`,
		"not json":            `[`,
	} {
		if code, out := f.send("POST", "/riders/me/stock/count", token, body); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if n := f.count("SELECT count(*) FROM stock_counts"); n != 0 {
		t.Errorf("a refused count left %d rows", n)
	}
}

func TestAHandCountReplayActsOnce(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	_, token := f.rider()
	f.start(token)
	body := `{"lines":[{"item_id":"` + f.item + `","qty":1}],"reason":"scanner broke"}`
	_, first, _ := f.sendKey("POST", "/riders/me/stock/count", token, "count-key-0001", body)
	_, second, header := f.sendKey("POST", "/riders/me/stock/count", token, "count-key-0001", body)
	if first["mismatches"] != second["mismatches"] || header.Get("Idempotency-Replayed") == "" {
		t.Errorf("%v / %v / %v", first, second, header)
	}
	if n := f.count("SELECT count(*) FROM stock_counts"); n != 1 {
		t.Errorf("%d counts after a replay", n)
	}
}
