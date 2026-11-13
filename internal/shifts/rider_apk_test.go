package shifts

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/teamprosperlink/Ninzo_backend/internal/contracttest"
)

// Issue 235, part B1: what the finished rider app reads and sends on a shift. Every test goes through
// the real HTTP handlers and checks the reply against the bundled rider contract.

const riderContract = "rider.openapi.yaml"

func at(t *testing.T, v any) time.Time {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("not a time: %v", v)
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestAShiftCarriesTheFieldsTheRiderAppReads(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	rider, token := f.rider()
	code, out := f.start(token)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	contracttest.Check(t, riderContract, "ShiftResponse", out)
	sh := shiftOf(out)
	for _, field := range []string{"online_at", "planned_end_at", "break"} {
		if v, ok := sh[field]; !ok || v != nil {
			t.Errorf("a new shift must send %s as null: %v", field, sh)
		}
	}
	// goOnline and setPlannedEnd are part B2, so their columns are set here by hand. The shift then
	// shows them in the replies of end and in the open shift the state reads.
	f.exec("UPDATE shifts SET online_at = '2026-11-13T04:00:00Z', planned_end_at = '2026-11-13T12:30:00Z' WHERE rider_id = $1", rider)
	open, found, err := OpenShift(t.Context(), f.s.Pool, rider)
	if err != nil || !found {
		t.Fatalf("%v %v", found, err)
	}
	contracttest.Check(t, riderContract, "Shift", open)
	online, planned := open["online_at"].(*string), open["planned_end_at"].(*string)
	if online == nil || planned == nil || *online != "2026-11-13T04:00:00Z" || *planned != "2026-11-13T12:30:00Z" || open["break"] != nil {
		t.Errorf("the open shift: %v", open)
	}
	code, out = f.send("POST", "/riders/me/shifts/end", token, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	contracttest.Check(t, riderContract, "ShiftResponse", out)
	if sh := shiftOf(out); sh["online_at"] != "2026-11-13T04:00:00Z" || sh["planned_end_at"] != "2026-11-13T12:30:00Z" {
		t.Errorf("the ended shift: %v", sh)
	}
}

func TestStartShiftKeepsTheChecklist(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	rider, token := f.rider()
	start := func(extra string) (int, map[string]any) {
		return f.send("POST", "/riders/me/shifts/start", token, `{"node_id":"`+f.node+`"`+extra+`}`)
	}
	twenty := make([]string, 0, 21)
	for i := 0; i < 21; i++ {
		twenty = append(twenty, fmt.Sprintf(`"item_%02d"`, i))
	}
	for name, extra := range map[string]string{
		"21 codes":          `,"checklist":[` + strings.Join(twenty, ",") + `]`,
		"a code twice":      `,"checklist":["helmet","helmet"]`,
		"an empty code":     `,"checklist":[""]`,
		"a number":          `,"checklist":[1]`,
		"not a list":        `,"checklist":"helmet"`,
		"an object in list": `,"checklist":[{"code":"helmet"}]`,
	} {
		if code, out := start(extra); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if n := f.count("SELECT count(*) FROM shifts WHERE rider_id = $1", rider); n != 0 {
		t.Fatalf("a refused start made %d shifts", n)
	}
	// Up to 20 is allowed. A code of a shape the settings never use is ignored, not an error.
	code, out := start(`,"checklist":[` + strings.Join(twenty[:19], ",") + `,"Not A Code"]`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	contracttest.Check(t, riderContract, "ShiftResponse", out)
	var kept []string
	if err := f.s.Pool.QueryRow(t.Context(), "SELECT checklist FROM shifts WHERE rider_id = $1", rider).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if len(kept) != 19 || kept[0] != "item_00" || kept[18] != "item_18" {
		t.Errorf("kept %v", kept)
	}
	// No checklist keeps an empty one, never null.
	f.send("POST", "/riders/me/shifts/end", token, "")
	if code, out := start(""); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM shifts WHERE rider_id = $1 AND checklist = '{}'", rider); n != 1 {
		t.Errorf("%d shifts with an empty checklist, want 1", n)
	}
	// The database refuses more than 20 whatever the code does.
	if _, err := f.s.Pool.Exec(t.Context(), "UPDATE shifts SET checklist = $2 WHERE rider_id = $1", rider, make([]string, 21)); err == nil {
		t.Error("the database accepted 21 checklist codes")
	}
}

func TestBreakIsInBothFormsAndTheyAgree(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	_, token := f.rider()
	f.breakLimit(600)
	f.start(token)
	code, out := f.send("POST", "/riders/me/breaks/start", token, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	contracttest.Check(t, riderContract, "ShiftResponse", out)
	sh := shiftOf(out)
	b, _ := sh["break"].(map[string]any)
	if b == nil || b["started_at"] != sh["break_started_at"] || b["ends_at"] != sh["break_ends_at"] {
		t.Errorf("break %v and the flat fields %v / %v must say the same", b, sh["break_started_at"], sh["break_ends_at"])
	}
	code, out = f.send("POST", "/riders/me/breaks/end", token, "")
	if code != 200 || shiftOf(out)["break"] != nil {
		t.Errorf("after the break: %d %v", code, out)
	}
}

func TestTakeBreakIsCappedByTheConsoleAndCanBeExtended(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	rider, token := f.rider()
	f.breakLimit(900)
	take := func(body string) (int, map[string]any) {
		return f.send("POST", "/riders/me/shifts/break", token, body)
	}
	if code, out := take(`{"length_s":600}`); code != 409 || out["code"] != "no_open_shift" {
		t.Errorf("no shift: %d %v", code, out)
	}
	f.start(token)
	for name, body := range map[string]string{
		"no length": `{}`, "too short": `{"length_s":59}`, "too long": `{"length_s":3601}`, "a fraction": `{"length_s":90.5}`,
		"text": `{"length_s":"600"}`, "null": `{"length_s":null}`, "not json": `[`, "negative": `{"length_s":-60}`,
	} {
		if code, out := take(body); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if n := f.count("SELECT count(*) FROM shift_breaks"); n != 0 {
		t.Fatalf("a refused break made %d rows", n)
	}
	seq := f.seq(rider)
	// 10 minutes is less than the console's 15, so the rider gets what they asked for.
	code, out := take(`{"length_s":600}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	contracttest.Check(t, riderContract, "ShiftResponse", out)
	sh := shiftOf(out)
	started, ends := at(t, sh["break_started_at"]), at(t, sh["break_ends_at"])
	if ends.Sub(started) != 600*time.Second {
		t.Errorf("the break lasts %v, want 10 minutes", ends.Sub(started))
	}
	if b := sh["break"].(map[string]any); b["started_at"] != sh["break_started_at"] || b["ends_at"] != sh["break_ends_at"] {
		t.Errorf("break %v", b)
	}
	if f.seq(rider) != seq+1 {
		t.Error("starting a break did not raise the rider seq")
	}
	// A longer length extends the break that is on, from its start. It is not an error.
	code, out = take(`{"length_s":800}`)
	if code != 200 || !at(t, shiftOf(out)["break_started_at"]).Equal(started) || at(t, shiftOf(out)["break_ends_at"]).Sub(started) != 800*time.Second {
		t.Errorf("extending: %d %v", code, out)
	}
	// An hour asked for is capped at the console's 15 minutes, and says so in both forms.
	code, out = take(`{"length_s":3600}`)
	capped := shiftOf(out)
	if code != 200 || at(t, capped["break_ends_at"]).Sub(started) != 900*time.Second || capped["break"].(map[string]any)["ends_at"] != capped["break_ends_at"] {
		t.Errorf("the cap: %d %v", code, out)
	}
	// A shorter one never shortens it.
	code, out = take(`{"length_s":120}`)
	if code != 200 || at(t, shiftOf(out)["break_ends_at"]).Sub(started) != 900*time.Second {
		t.Errorf("a shorter length shortened the break: %d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM shift_breaks WHERE ended_at IS NULL"); n != 1 {
		t.Errorf("%d open break rows, want 1", n)
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'shift.break_extended'"); n != 2 {
		t.Errorf("%d extension audit rows, want 2 (the shorter one changed nothing)", n)
	}
	// The alias ends it, as endBreak does, and a second end is harmless.
	code, out = f.send("POST", "/riders/me/shifts/break/end", token, "")
	if code != 200 || shiftOf(out)["break"] != nil || shiftOf(out)["break_started_at"] != nil {
		t.Errorf("ending through the alias: %d %v", code, out)
	}
	contracttest.Check(t, riderContract, "ShiftResponse", out)
	if n := f.count("SELECT count(*) FROM shift_breaks WHERE ended_by = 'rider' AND ended_at IS NOT NULL"); n != 1 {
		t.Error("the break row was not closed by the rider")
	}
	if code, out := f.send("POST", "/riders/me/shifts/break/end", token, ""); code != 200 || shiftOf(out)["break"] != nil {
		t.Errorf("a second end: %d %v", code, out)
	}
}

func TestTakeBreakRefusesWhatStartBreakRefuses(t *testing.T) {
	f := newFixture(t)
	scooter := f.scooter("SC-1")
	rider, token := f.rider()
	f.start(token)
	order := f.order(rider, scooter, "RIDER_ACCEPTED")
	f.offerFor(order, rider, scooter, "accepted", "1 hour")
	if code, out := f.send("POST", "/riders/me/shifts/break", token, `{"length_s":300}`); code != 409 || out["code"] != "order_in_progress" {
		t.Errorf("a live order: %d %v", code, out)
	}
	f.exec("UPDATE offers SET state = 'open', rider_accepted_at = NULL")
	if code, out := f.send("POST", "/riders/me/shifts/break", token, `{"length_s":300}`); code != 409 || out["code"] != "offer_open" {
		t.Errorf("an open offer: %d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM shift_breaks"); n != 0 {
		t.Errorf("%d break rows", n)
	}
	f.exec("UPDATE riders SET status = 'suspended' WHERE id = $1", rider)
	if code, out := f.send("POST", "/riders/me/shifts/break", token, `{"length_s":300}`); code != 403 || out["code"] != "account_blocked" {
		t.Errorf("a suspended rider: %d %v", code, out)
	}
}

func TestTakeBreakReplaysWithTheSameKey(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	rider, token := f.rider()
	f.start(token)
	c1, o1, _ := f.sendKey("POST", "/riders/me/shifts/break", token, "take-break-0001", `{"length_s":300}`)
	c2, o2, _ := f.sendKey("POST", "/riders/me/shifts/break", token, "take-break-0001", `{"length_s":300}`)
	if c1 != 200 || c2 != 200 || o1["server_time"] != o2["server_time"] {
		t.Errorf("a replay must give the same answer: %d %v / %d %v", c1, o1, c2, o2)
	}
	if n := f.count("SELECT count(*) FROM shift_breaks WHERE shift_id IN (SELECT id FROM shifts WHERE rider_id = $1)", rider); n != 1 {
		t.Errorf("%d break rows after a replay", n)
	}
	if c, o, _ := f.sendKey("POST", "/riders/me/shifts/break", token, "take-break-0001", `{"length_s":600}`); c != 422 {
		t.Errorf("the same key with another length: %d %v", c, o)
	}
}

func TestListShiftsTakesDaysAndSendsTheAppsFields(t *testing.T) {
	f := newFixture(t)
	scooter := f.scooter("SC-1")
	rider, token := f.rider()
	other, _ := f.rider()
	f.shiftAt(rider, scooter, 40*24*time.Hour, 2*time.Hour)
	mid := f.shiftAt(rider, scooter, 3*24*time.Hour+3*time.Hour, 2*time.Hour)
	recent := f.shiftAt(rider, scooter, 5*time.Hour, 2*time.Hour)
	f.shiftAt(other, scooter, 2*time.Hour, time.Hour)
	f.exec("UPDATE shifts SET online_at = started_at + interval '5 minutes', planned_end_at = started_at + interval '9 hours' WHERE id = $1", recent)
	// Two orders handed over in the recent shift: 2 and 1 portions handed, 4 and 6 minutes from accept.
	price := f.id("INSERT INTO price_history (item_id, price_paise, reason) VALUES ($1, 4900, 'test') RETURNING id::text", f.item)
	for _, p := range []struct{ qty, handed, minutes, accepted int }{{2, 2, 30, 4}, {3, 1, 50, 6}} {
		o := f.order(rider, scooter, "COMPLETED")
		f.exec(`UPDATE orders SET handed_over_at = (SELECT started_at + make_interval(mins => $3) FROM shifts WHERE id = $2),
			rider_accepted_at = (SELECT started_at + make_interval(mins => $3 - $4) FROM shifts WHERE id = $2) WHERE id = $1`, o, recent, p.minutes, p.accepted)
		f.exec(`INSERT INTO order_items (order_id, item_id, price_history_id, name, qty, unit_paise, qty_handed) VALUES ($1, $2, $3, 'Chai', $4, 4900, $5)`,
			o, f.item, price, p.qty, p.handed)
	}
	code, out := f.get("/riders/me/shifts?days=7", token)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	contracttest.Check(t, riderContract, "ShiftList", out)
	list := out["shifts"].([]any)
	if len(list) != 2 || out["next_cursor"] != nil {
		t.Fatalf("days=7 must give the 2 shifts of the week, in one page: %v", out)
	}
	first, second := list[0].(map[string]any), list[1].(map[string]any)
	if first["id"] != recent || second["id"] != mid {
		t.Errorf("newest first: %v then %v", first["id"], second["id"])
	}
	if first["orders"] != float64(2) || first["orders"] != first["orders_handed_over"] || first["portions"] != float64(3) || first["avg_delivery_s"] != float64(300) {
		t.Errorf("the recent shift: %v", first)
	}
	if first["online_at"] == nil || first["planned_end_at"] == nil {
		t.Errorf("online_at and planned_end_at: %v", first)
	}
	if second["orders"] != float64(0) || second["portions"] != float64(0) || second["avg_delivery_s"] != nil || second["online_at"] != nil || second["planned_end_at"] != nil {
		t.Errorf("a shift with no orders: %v", second)
	}
	if _, out := f.get("/riders/me/shifts?days=31", token); len(out["shifts"].([]any)) != 2 {
		t.Errorf("days=31 reaches back 31 days, not 40: %v", out)
	}
	for _, q := range []string{"days=0", "days=32", "days=-1", "days=x", "days=7&from=2026-11-01", "days=7&to=2026-11-01"} {
		if code, out := f.get("/riders/me/shifts?"+q, token); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", q, code, out)
		}
	}
	// Without days nothing changed: pages of limit, a cursor, and the old shift is still there.
	code, out = f.get("/riders/me/shifts?limit=1", token)
	contracttest.Check(t, riderContract, "ShiftList", out)
	if code != 200 || len(out["shifts"].([]any)) != 1 || out["next_cursor"] == nil {
		t.Errorf("limit=1: %d %v", code, out)
	}
	seen, cursor := 0, ""
	for page := 0; page < 5; page++ {
		path := "/riders/me/shifts?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		_, out = f.get(path, token)
		seen += len(out["shifts"].([]any))
		if out["next_cursor"] == nil {
			break
		}
		cursor = out["next_cursor"].(string)
	}
	if seen != 3 {
		t.Errorf("without days all 3 of the rider's shifts page through, saw %d", seen)
	}
}

func TestADaysWindowIsInIndiaTime(t *testing.T) {
	f := newFixture(t)
	scooter := f.scooter("SC-1")
	rider, token := f.rider()
	// days=2 starts at 00:00 India time yesterday. A shift a minute before that is outside, a minute after is inside.
	f.exec(`INSERT INTO shifts (rider_id, node_id, scooter_id, started_at, ended_at)
		SELECT $1, $2, $3, s, s + interval '30 minutes' FROM (VALUES
			(date_trunc('day', now() AT TIME ZONE 'Asia/Kolkata') AT TIME ZONE 'Asia/Kolkata' - interval '1 day' - interval '1 minute'),
			(date_trunc('day', now() AT TIME ZONE 'Asia/Kolkata') AT TIME ZONE 'Asia/Kolkata' - interval '1 day' + interval '1 minute')) v(s)`,
		rider, f.node, scooter)
	_, out := f.get("/riders/me/shifts?days=2", token)
	if n := len(out["shifts"].([]any)); n != 1 {
		t.Errorf("days=2 must start at midnight India time yesterday, got %d shifts: %v", n, out)
	}
}

func TestDaysNeverCutsShiftsAtTheDefaultLimitAndRefusesPagingParameters(t *testing.T) {
	f := newFixture(t)
	scooter := f.scooter("SC-1")
	rider, token := f.rider()
	// 25 short shifts in the last 3 hours of today, one every 6 minutes: more than the default page of 20.
	f.exec(`INSERT INTO shifts (rider_id, node_id, scooter_id, started_at, ended_at)
		SELECT $1, $2, $3, now() - make_interval(mins => g * 6 + 5), now() - make_interval(mins => g * 6 + 1) FROM generate_series(0, 24) g`, rider, f.node, scooter)
	code, out := f.get("/riders/me/shifts?days=31", token)
	if code != 200 || len(out["shifts"].([]any)) != 25 || out["next_cursor"] != nil {
		t.Fatalf("days must hold all 25 shifts, not 20: %d, %d shifts, cursor %v", code, len(out["shifts"].([]any)), out["next_cursor"])
	}
	contracttest.Check(t, riderContract, "ShiftList", out)
	for _, q := range []string{"days=7&limit=5", "days=7&limit=100", "days=7&cursor=abc"} {
		if code, out := f.get("/riders/me/shifts?"+q, token); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", q, code, out)
		}
	}
	// Without days the default page is still 20.
	if _, out := f.get("/riders/me/shifts", token); len(out["shifts"].([]any)) != 20 || out["next_cursor"] == nil {
		t.Errorf("the default page: %v", out)
	}
}

func TestChecklistIsNotStoredOnTakeOver(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	rider, token := f.rider()
	device := f.device(rider)
	body := func(extra string) string {
		return `{"node_id":"` + f.node + `","device_id":"` + device + `"` + extra + `}`
	}
	if code, out := f.send("POST", "/riders/me/shifts/start", token, body(`,"checklist":["helmet"]`)); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	other := f.device(rider)
	if code, out := f.send("POST", "/riders/me/shifts/start", token, `{"node_id":"`+f.node+`","device_id":"`+other+`","take_over":true,"checklist":["gloves"]}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if code, out := f.send("POST", "/riders/me/shifts/start", token, body(`,"checklist":["boots"]`)); code != 409 || out["code"] != "shift_already_open" {
		t.Errorf("%d %v", code, out)
	}
	var kept []string
	if err := f.s.Pool.QueryRow(t.Context(), "SELECT checklist FROM shifts WHERE rider_id = $1", rider).Scan(&kept); err != nil || len(kept) != 1 || kept[0] != "helmet" {
		t.Errorf("only the call that started the shift stores its checklist: %v %v", kept, err)
	}
}

func TestADaysReplyHoldsAtMost1000ShiftsNewestFirst(t *testing.T) {
	f := newFixture(t)
	scooter := f.scooter("SC-1")
	rider, token := f.rider()
	f.exec(`INSERT INTO shifts (rider_id, node_id, scooter_id, started_at, ended_at)
		SELECT $1, $2, $3, now() - make_interval(secs => g * 20 + 10), now() - make_interval(secs => g * 20 + 5) FROM generate_series(0, 1004) g`, rider, f.node, scooter)
	code, out := f.get("/riders/me/shifts?days=31", token)
	list, _ := out["shifts"].([]any)
	if code != 200 || len(list) != 1000 || out["next_cursor"] != nil {
		t.Fatalf("%d, %d shifts, cursor %v: want exactly 1000 and no cursor", code, len(list), out["next_cursor"])
	}
	first, last := at(t, list[0].(map[string]any)["started_at"]), at(t, list[999].(map[string]any)["started_at"])
	if !first.After(last) {
		t.Errorf("newest first: %v then %v", first, last)
	}
}
