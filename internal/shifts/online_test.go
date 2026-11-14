package shifts

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/teamprosperlink/Ninzo_backend/internal/contracttest"
	"github.com/teamprosperlink/Ninzo_backend/internal/riderapp"
)

// Issue 235, part B2: goOnline and setPlannedEnd record on the open shift and do nothing else. Every test
// goes through the real HTTP handlers and checks the reply against the bundled rider contract.

func TestGoOnlineRecordsTheFirstTimeOnlyAndNothingElse(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	rider, token := f.rider()
	if code, out := f.send("POST", "/riders/me/shifts/online", token, ""); code != 409 || out["code"] != "no_open_shift" {
		t.Fatalf("with no shift: %d %v", code, out)
	}
	if code, out := f.start(token); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	seq := f.count("SELECT seq FROM rider_state WHERE rider_id = $1", rider)
	code, out := f.send("POST", "/riders/me/shifts/online", token, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	contracttest.Check(t, riderContract, "ShiftResponse", out)
	first := shiftOf(out)["online_at"]
	if first == nil || time.Since(at(t, first)) > time.Minute {
		t.Fatalf("online_at %v", first)
	}
	var stored time.Time
	if err := f.s.Pool.QueryRow(t.Context(), "SELECT online_at FROM shifts WHERE rider_id = $1", rider).Scan(&stored); err != nil || !stored.Equal(at(t, first)) {
		t.Errorf("stored %v, replied %v, %v", stored, first, err)
	}
	if n := f.count("SELECT seq FROM rider_state WHERE rider_id = $1", rider); n != seq+1 {
		t.Errorf("seq %d, want %d: going online is something the app can see", n, seq+1)
	}
	// Going online again keeps the first time, changes nothing and writes no audit row.
	time.Sleep(20 * time.Millisecond)
	code, out = f.send("POST", "/riders/me/shifts/online", token, "")
	if code != 200 || shiftOf(out)["online_at"] != first {
		t.Errorf("a repeat: %d %v, first was %v", code, out, first)
	}
	contracttest.Check(t, riderContract, "ShiftResponse", out)
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'shift.online'"); n != 1 {
		t.Errorf("%d audit rows, want 1", n)
	}
	if n := f.count("SELECT seq FROM rider_state WHERE rider_id = $1", rider); n != seq+1 {
		t.Errorf("a repeat raised seq to %d", n)
	}
	// It records only: the shift stays open, on no break, and no offer, order or event came from it.
	sh := shiftOf(out)
	if sh["ended_at"] != nil || sh["break"] != nil || sh["planned_end_at"] != nil {
		t.Errorf("the shift: %v", sh)
	}
	for _, table := range []string{"offers", "orders", "outbox_events", "scheduled_tasks", "shift_breaks"} {
		if n := f.count("SELECT count(*) FROM " + table); n != 0 {
			t.Errorf("%s has %d rows after going online", table, n)
		}
	}
	// The same key replies the same.
	_, a, _ := f.sendKey("POST", "/riders/me/shifts/online", token, "online-key-0001", "")
	_, b, _ := f.sendKey("POST", "/riders/me/shifts/online", token, "online-key-0001", "")
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if string(aj) != string(bj) {
		t.Errorf("replay differs:\n%s\n%s", aj, bj)
	}
	// A suspended rider cannot, and the answer after the shift ends is no_open_shift again.
	f.send("POST", "/riders/me/shifts/end", token, "")
	if code, out := f.send("POST", "/riders/me/shifts/online", token, ""); code != 409 || out["code"] != "no_open_shift" {
		t.Errorf("after the shift ended: %d %v", code, out)
	}
	f.exec("UPDATE riders SET status = 'suspended' WHERE id = $1", rider)
	if code, out := f.send("POST", "/riders/me/shifts/online", token, ""); code != 403 || out["code"] != "account_blocked" {
		t.Errorf("a suspended rider: %d %v", code, out)
	}
	if code, _ := f.send("POST", "/riders/me/shifts/online", "", ""); code != 401 {
		t.Errorf("no token: %d", code)
	}
}

func TestSetPlannedEndIsRecordedWithinSixteenHoursAndEndsNothing(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	rider, token := f.rider()
	set := func(at time.Time) (int, map[string]any) {
		return f.send("POST", "/riders/me/shifts/planned-end", token, fmt.Sprintf(`{"planned_end_at":%q}`, at.UTC().Format(time.RFC3339)))
	}
	if code, out := set(time.Now().Add(time.Hour)); code != 409 || out["code"] != "no_open_shift" {
		t.Fatalf("with no shift: %d %v", code, out)
	}
	if code, out := f.start(token); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	// Refused: bad bodies, the past, and past 16 hours from the shift's start.
	for name, body := range map[string]string{
		"missing": `{}`, "null": `{"planned_end_at":null}`, "a number": `{"planned_end_at":1}`, "not a time": `{"planned_end_at":"soon"}`,
		"a date only": `{"planned_end_at":"2099-01-01"}`, "not json": `x`, "a list": `{"planned_end_at":["2099-01-01T00:00:00Z"]}`,
		"two objects": `{"planned_end_at":"2099-01-01T00:00:00Z"}{}`,
	} {
		if code, out := f.send("POST", "/riders/me/shifts/planned-end", token, body); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	for name, when := range map[string]time.Time{"the past": time.Now().Add(-time.Minute), "now": time.Now(), "17 hours on": time.Now().Add(17 * time.Hour)} {
		if code, out := set(when); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if n := f.count("SELECT count(*) FROM shifts WHERE planned_end_at IS NOT NULL"); n != 0 {
		t.Fatalf("a refused call recorded %d planned ends", n)
	}
	seq := f.count("SELECT seq FROM rider_state WHERE rider_id = $1", rider)
	want := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
	code, out := set(want)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	contracttest.Check(t, riderContract, "ShiftResponse", out)
	if got := at(t, shiftOf(out)["planned_end_at"]); !got.Equal(want) {
		t.Errorf("planned_end_at %v, want %v", got, want)
	}
	var stored time.Time
	if err := f.s.Pool.QueryRow(t.Context(), "SELECT planned_end_at FROM shifts WHERE rider_id = $1", rider).Scan(&stored); err != nil || !stored.Equal(want) {
		t.Errorf("stored %v, %v", stored, err)
	}
	if n := f.count("SELECT seq FROM rider_state WHERE rider_id = $1", rider); n != seq+1 {
		t.Errorf("seq %d, want %d", n, seq+1)
	}
	// The same time again changes nothing, and a new time (Add time) replaces it with one audit row more.
	if code, out := set(want); code != 200 || !at(t, shiftOf(out)["planned_end_at"]).Equal(want) {
		t.Errorf("the same time again: %d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'shift.planned_end_set'"); n != 1 {
		t.Errorf("%d audit rows after the same time twice, want 1", n)
	}
	later := want.Add(time.Hour)
	if code, out := set(later); code != 200 || !at(t, shiftOf(out)["planned_end_at"]).Equal(later) {
		t.Errorf("Add time: %d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'shift.planned_end_set'"); n != 2 {
		t.Errorf("%d audit rows, want 2", n)
	}
	// The 16 hours count from the shift's start, not from now.
	f.exec("UPDATE shifts SET started_at = now() - interval '10 hours' WHERE rider_id = $1", rider)
	if code, out := set(time.Now().Add(7 * time.Hour)); code != 400 {
		t.Errorf("17 hours after the start: %d %v", code, out)
	}
	if code, out := set(time.Now().Add(5 * time.Hour)); code != 200 {
		t.Errorf("15 hours after the start: %d %v", code, out)
	}
	// It records only: the shift is still open, on no break, and nothing was sent or scheduled.
	for _, check := range []string{"SELECT count(*) FROM shifts WHERE ended_at IS NOT NULL", "SELECT count(*) FROM shift_breaks", "SELECT count(*) FROM outbox_events",
		"SELECT count(*) FROM scheduled_tasks", "SELECT count(*) FROM rider_notifications"} {
		if n := f.count(check); n != 0 {
			t.Errorf("%s = %d after setting a planned end", check, n)
		}
	}
	// A planned end in the past (the time has come) ends nothing either: set it by hand and read the state.
	f.exec("UPDATE shifts SET planned_end_at = now() - interval '1 minute' WHERE rider_id = $1", rider)
	open, found, err := OpenShift(t.Context(), f.s.Pool, rider)
	if err != nil || !found || open["ended_at"] != nil {
		t.Errorf("a shift past its planned end: %v %v %v", open, found, err)
	}
	// Replay.
	_, a, _ := f.sendKey("POST", "/riders/me/shifts/planned-end", token, "plan-key-0001", fmt.Sprintf(`{"planned_end_at":%q}`, time.Now().Add(2*time.Hour).UTC().Format(time.RFC3339)))
	_, b, _ := f.sendKey("POST", "/riders/me/shifts/planned-end", token, "plan-key-0001", fmt.Sprintf(`{"planned_end_at":%q}`, time.Now().Add(2*time.Hour).UTC().Format(time.RFC3339)))
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if string(aj) != string(bj) {
		t.Errorf("replay differs:\n%s\n%s", aj, bj)
	}
	if code, out, _ := f.sendKey("POST", "/riders/me/shifts/planned-end", token, "plan-key-0001", fmt.Sprintf(`{"planned_end_at":%q}`, time.Now().Add(4*time.Hour).UTC().Format(time.RFC3339))); code != 422 {
		t.Errorf("the same key with another time: %d %v", code, out)
	}
	if code, _ := f.send("POST", "/riders/me/shifts/planned-end", "", `{"planned_end_at":"2099-01-01T00:00:00Z"}`); code != 401 {
		t.Errorf("no token: %d", code)
	}
}

// startShift keeps only the codes the settings call lists.
func TestStartShiftKeepsOnlyTheChecklistTheSettingsListAndTheirTicks(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	rider, token := f.rider()
	all := make([]string, 0, len(riderapp.Checklist))
	for _, c := range riderapp.Checklist {
		all = append(all, fmt.Sprintf("%q", c.Code))
	}
	body := `{"node_id":"` + f.node + `","checklist":[` + strings.Join(all, ",") + `,"made_up","helmet_2"]}`
	if code, out := f.send("POST", "/riders/me/shifts/start", token, body); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	var kept []string
	if err := f.s.Pool.QueryRow(t.Context(), "SELECT checklist FROM shifts WHERE rider_id = $1", rider).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if strings.Join(kept, ",") != "helmet,phone_battery,scooter_battery,location" {
		t.Errorf("kept %v, want exactly the settings' codes in the order sent", kept)
	}
}
