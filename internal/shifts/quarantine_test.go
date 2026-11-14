package shifts

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/teamprosperlink/Ninzo_backend/internal/availability"
	"github.com/teamprosperlink/Ninzo_backend/internal/contracttest"
	"github.com/teamprosperlink/Ninzo_backend/internal/restock"
)

// Issue 235, part B3: a rider quarantines a lot on their own scooter. Every test goes through the real
// HTTP handlers and checks the reply against the bundled rider contract.

// shopper stands about 55 m from the fixture's node, inside its reach.
const shopperLat, shopperLng = 12.9305, 77.62

// zoneArea gives the fixture's zone a service area, so availability can be asked of it.
func (f *fixture) zoneArea() {
	f.t.Helper()
	f.exec(`UPDATE zones SET area = extensions.ST_GeomFromText('MULTIPOLYGON(((77.60 12.92, 77.64 12.92, 77.64 12.94, 77.60 12.94, 77.60 12.92)))', 4326) WHERE id = $1`, f.zone)
}

// offered is how many of the item a customer standing at the shopper's place can order now.
func (f *fixture) offered() int {
	f.t.Helper()
	res, err := availability.At(f.t.Context(), f.s.Pool, shopperLat, shopperLng, time.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	for _, l := range res.Lines {
		if l.Item.ID == f.item {
			return l.QtyLeft
		}
	}
	return 0
}

func (f *fixture) scan(token, code string) {
	f.t.Helper()
	if status, out := f.send("POST", "/riders/me/stock/scan", token, `{"lot_code":"`+code+`","scanned_at":"2026-11-14T10:00:00Z"}`); status != 200 {
		f.t.Fatalf("scan %s: %d %v", code, status, out)
	}
}

func (f *fixture) quarantine(token, lot, body string) (int, map[string]any) {
	f.t.Helper()
	return f.send("POST", "/riders/me/stock/"+lot+"/quarantine", token, body)
}

// state is everything a quarantine writes, so a refused call can prove it wrote nothing.
func (f *fixture) state(rider string) string {
	f.t.Helper()
	var s string
	err := f.s.Pool.QueryRow(f.t.Context(), `SELECT
		coalesce((SELECT string_agg(code || ':' || status || ':' || coalesce(quarantined_by_rider::text, '-'), ',' ORDER BY code) FROM lots), '') || '|' ||
		coalesce((SELECT string_agg(lot_id || ':' || on_hand || ':' || reserved, ',' ORDER BY lot_id) FROM scooter_stock), '') || '|' ||
		(SELECT count(*) FROM stock_ledger) || '|' || (SELECT count(*) FROM audit_events WHERE action = 'lot.quarantined') || '|' ||
		coalesce((SELECT seq FROM rider_state WHERE rider_id = $1), 0)`, rider).Scan(&s)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func TestARiderQuarantinesALotAndItLeavesEveryStockCount(t *testing.T) {
	f := newFixture(t)
	f.zoneArea()
	scooter := f.scooter("SC-1")
	warm := f.lot("LOT-WARM", 10, 48*time.Hour)
	good := f.lot("LOT-GOOD", 6, 48*time.Hour)
	rider, token := f.rider()
	if code, out := f.start(token); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	f.scan(token, "LOT-WARM")
	f.scan(token, "LOT-GOOD")
	if n := f.offered(); n != 16 {
		t.Fatalf("a customer is offered %d before, want 16", n)
	}
	seq := f.seq(rider)
	ledger := f.count("SELECT count(*) FROM stock_ledger")

	code, out := f.quarantine(token, warm, `{"reason":"temperature","note":"9 C on the probe"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	contracttest.Check(t, riderContract, "QuarantineResponse", out)
	if out["lot_id"] != warm {
		t.Errorf("lot_id %v", out["lot_id"])
	}
	first := at(t, out["quarantined_at"])
	if time.Since(first) > time.Minute {
		t.Errorf("quarantined_at %v", first)
	}

	// Who, why and when are on the lot, and it is the rider's column, never a hub staff member's.
	var status, reason string
	var note *string
	var byRider, byStaff *string
	var stored time.Time
	if err := f.s.Pool.QueryRow(t.Context(), `SELECT status, quarantine_reason, quarantine_note, quarantined_by_rider::text, quarantined_by::text, quarantined_at
		FROM lots WHERE id = $1`, warm).Scan(&status, &reason, &note, &byRider, &byStaff, &stored); err != nil {
		t.Fatal(err)
	}
	if status != "quarantined" || reason != "temperature" || note == nil || *note != "9 C on the probe" || byRider == nil || *byRider != rider || byStaff != nil || !stored.Equal(first) {
		t.Errorf("the lot: %s %s %v %v %v %v, replied %v", status, reason, note, byRider, byStaff, stored, first)
	}
	// The whole lot left the scooter: the row stays (the lot can never be loaded again) and counts 0.
	var onHand, reserved int
	if err := f.s.Pool.QueryRow(t.Context(), "SELECT on_hand, reserved FROM scooter_stock WHERE scooter_id = $1 AND lot_id = $2", scooter, warm).Scan(&onHand, &reserved); err != nil || onHand != 0 || reserved != 0 {
		t.Errorf("stock row: %d %d %v", onHand, reserved, err)
	}
	// One ledger row says so, and the ledger still adds up to the stock on the scooter.
	if n := f.count("SELECT count(*) FROM stock_ledger"); n != ledger+1 {
		t.Errorf("ledger rows %d, want %d", n, ledger+1)
	}
	if n := f.count("SELECT delta_on_hand FROM stock_ledger WHERE lot_id = $1 AND reason = 'quarantine' AND actor_type = 'rider' AND actor_id = $2", warm, rider); n != -10 {
		t.Errorf("quarantine ledger entry %d, want -10", n)
	}
	if n := f.count("SELECT coalesce(sum(delta_on_hand), 0) FROM stock_ledger WHERE lot_id = $1", warm); n != 0 {
		t.Errorf("the ledger for the lot adds up to %d, want 0", n)
	}
	// The rider's seq rose once, and the audit row names who, what and why.
	if n := f.seq(rider); n != seq+1 {
		t.Errorf("seq %d, want %d", n, seq+1)
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'lot.quarantined' AND actor_type = 'rider' AND actor_id = $1 AND entity_id = $2 AND after->>'reason' = 'temperature' AND (after->>'qty')::int = 10", rider, warm); n != 1 {
		t.Errorf("%d audit rows, want 1", n)
	}

	// Every place that counts stock stopped counting it.
	if n := f.offered(); n != 6 {
		t.Errorf("a customer is offered %d after, want 6", n)
	}
	_, stock := f.get("/riders/me/stock", token)
	contracttest.Check(t, riderContract, "StockList", stock)
	lots := stock["lots"].([]any)
	if len(lots) != 1 || lots[0].(map[string]any)["lot_id"] != good {
		t.Errorf("the rider's stock: %v", stock)
	}
	if code, out := f.send("POST", "/riders/me/stock/scan", token, `{"lot_code":"LOT-WARM","scanned_at":"2026-11-14T10:00:00Z"}`); code != 409 || out["code"] != "lot_quarantined" {
		t.Errorf("a scan of it again: %d %v", code, out)
	}

	// The same call again, with a new key, answers 200 with the first time and changes nothing.
	after := f.state(rider)
	time.Sleep(20 * time.Millisecond)
	code, again := f.quarantine(token, warm, `{"reason":"damaged"}`)
	if code != 200 || again["quarantined_at"] != out["quarantined_at"] {
		t.Errorf("a repeat: %d %v, first %v", code, again, out["quarantined_at"])
	}
	contracttest.Check(t, riderContract, "QuarantineResponse", again)
	if now := f.state(rider); now != after {
		t.Errorf("a repeat changed something:\n%s\n%s", after, now)
	}
	if n := f.count("SELECT count(*) FROM lots WHERE id = $1 AND quarantine_reason = 'temperature'", warm); n != 1 {
		t.Error("a repeat rewrote the reason")
	}
	// The same key replies the same.
	_, a, _ := f.sendKey("POST", "/riders/me/stock/"+good+"/quarantine", token, "quarantine-replay-0001", `{"reason":"expired"}`)
	_, b, _ := f.sendKey("POST", "/riders/me/stock/"+good+"/quarantine", token, "quarantine-replay-0001", `{"reason":"expired"}`)
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if string(aj) != string(bj) || a["lot_id"] != good {
		t.Errorf("replay differs:\n%s\n%s", aj, bj)
	}
	if n := f.offered(); n != 0 {
		t.Errorf("with both lots quarantined a customer is offered %d", n)
	}
	// No note stores nothing, and the shift's end summary no longer returns what was quarantined.
	if n := f.count("SELECT count(*) FROM lots WHERE id = $1 AND quarantine_note IS NULL", good); n != 1 {
		t.Error("a quarantine without a note stored one")
	}
	f.send("POST", "/riders/me/shifts/end", token, "")
	if n := f.count("SELECT count(*) FROM shifts WHERE rider_id = $1 AND returned_items = '[]'::jsonb", rider); n != 1 {
		t.Error("the shift summary still returns quarantined stock")
	}
}

func TestQuarantineRefusalsChangeNothing(t *testing.T) {
	f := newFixture(t)
	f.zoneArea()
	f.scooter("SC-1")
	f.scooter("SC-2")
	mine := f.lot("LOT-MINE", 10, 48*time.Hour)
	sold := f.lot("LOT-SOLD", 4, 48*time.Hour)
	theirs := f.lot("LOT-THEIRS", 5, 48*time.Hour)
	idle := f.lot("LOT-IDLE", 5, 48*time.Hour)
	atHub := f.lot("LOT-HUB", 5, 48*time.Hour)
	rider, token := f.rider()
	rival, rivalToken := f.rider()

	// No shift: no_open_shift whatever the lot is.
	if code, out := f.quarantine(token, mine, `{"reason":"temperature"}`); code != 409 || out["code"] != "no_open_shift" {
		t.Fatalf("no shift: %d %v", code, out)
	}
	if code, out := f.start(token); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if code, out := f.start(rivalToken); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	f.scan(token, "LOT-MINE")
	f.scan(token, "LOT-SOLD")
	f.scan(rivalToken, "LOT-THEIRS")
	// A lot on a scooter nobody is riding, and a lot waiting at a hub.
	idleScooter := f.scooter("SC-3")
	f.exec("INSERT INTO scooter_stock (scooter_id, lot_id, on_hand) VALUES ($1, $2, 5)", idleScooter, idle)
	hub := f.id("INSERT INTO hubs (zone_id, name, lat, lng) VALUES ($1, 'Hub', 12.93, 77.63) RETURNING id::text", f.zone)
	f.exec("UPDATE lots SET hub_id = $2 WHERE id = $1", atHub, hub)
	// LOT-SOLD is all gone (sold, so on_hand is 0).
	f.exec("UPDATE scooter_stock SET on_hand = 0 WHERE lot_id = $1", sold)

	before := f.state(rider)
	rivalBefore := f.state(rival)
	missing := "00000000-0000-4000-8000-000000000001"
	for name, tc := range map[string]struct {
		token, lot string
		status     int
		code       string
	}{
		"already all gone":               {token, sold, 409, "lot_not_on_scooter"},
		"another rider's scooter":        {token, theirs, 404, "not_found"},
		"the same lot, from the other":   {rivalToken, mine, 404, "not_found"},
		"a scooter nobody rides":         {token, idle, 404, "not_found"},
		"a lot at a hub, not in my case": {token, atHub, 404, "not_found"},
		"a lot that does not exist":      {token, missing, 404, "not_found"},
		"an id that is not an id":        {token, "not-an-id", 404, "not_found"},
	} {
		code, out := f.quarantine(tc.token, tc.lot, `{"reason":"temperature"}`)
		if code != tc.status || out["code"] != tc.code {
			t.Errorf("%s: %d %v, want %d %s", name, code, out, tc.status, tc.code)
		}
		if tc.status == 404 && (strings.Contains(fmt.Sprint(out), "LOT-") || strings.Contains(fmt.Sprint(out), "SC-")) {
			t.Errorf("%s: a 404 says too much: %v", name, out)
		}
	}
	// A 404 for another rider's lot is the same whether the lot exists or not.
	_, real := f.quarantine(token, theirs, `{"reason":"temperature"}`)
	_, fake := f.quarantine(token, missing, `{"reason":"temperature"}`)
	for _, m := range []map[string]any{real, fake} {
		delete(m, "server_time")
		delete(m, "request_id")
	}
	rj, _ := json.Marshal(real)
	fj, _ := json.Marshal(fake)
	if string(rj) != string(fj) {
		t.Errorf("another rider's lot answers differently from a missing one:\n%s\n%s", rj, fj)
	}
	// Bad bodies.
	long := strings.Repeat("é", 501)
	for name, body := range map[string]string{
		"no reason": `{}`, "an unknown reason": `{"reason":"smelly"}`, "a hub-only reason": `{"reason":"contamination"}`, "a number": `{"reason":1}`,
		"null": `{"reason":null}`, "not json": `x`, "a note too long": `{"reason":"other","note":"` + long + `"}`, "a list": `[]`,
	} {
		if code, out := f.quarantine(token, mine, body); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	// Nothing above changed a thing, for either rider.
	if now := f.state(rider); now != before {
		t.Errorf("a refused call changed something:\n%s\n%s", before, now)
	}
	if now := f.state(rival); now != rivalBefore {
		t.Errorf("the other rider's data changed:\n%s\n%s", rivalBefore, now)
	}
	// 500 characters (not bytes) are allowed.
	if code, out := f.quarantine(token, mine, `{"reason":"other","note":"`+strings.Repeat("é", 500)+`"}`); code != 200 {
		t.Errorf("a 500 character note: %d %v", code, out)
	}
}

func TestQuarantineNeedsASignedInUnblockedRider(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	lot := f.lot("LOT-1", 10, 48*time.Hour)
	rider, token := f.rider()
	f.start(token)
	f.scan(token, "LOT-1")
	if code, _ := f.quarantine("", lot, `{"reason":"temperature"}`); code != 401 {
		t.Errorf("no token: %d", code)
	}
	before := f.state(rider)
	f.exec("UPDATE riders SET status = 'suspended' WHERE id = $1", rider)
	if code, out := f.quarantine(token, lot, `{"reason":"temperature"}`); code != 403 || out["code"] != "account_blocked" {
		t.Errorf("a suspended rider: %d %v", code, out)
	}
	if now := f.state(rider); now != before {
		t.Errorf("a refused call changed something:\n%s\n%s", before, now)
	}
}

func TestANewRiderOnTheScooterCannotQuarantineWhatTheLastOneDid(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	lot := f.lot("LOT-1", 10, 48*time.Hour)
	first, token := f.rider()
	f.start(token)
	f.scan(token, "LOT-1")
	if code, out := f.quarantine(token, lot, `{"reason":"expired"}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	f.send("POST", "/riders/me/shifts/end", token, "")
	// A lot a rider quarantined stays in that scooter's stock rows with nothing on hand. The next
	// rider on the scooter did not do it, and it is not on the scooter any more.
	second, secondToken := f.rider()
	if code, out := f.start(secondToken); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	seq := f.seq(second)
	if code, out := f.quarantine(secondToken, lot, `{"reason":"expired"}`); code != 409 || out["code"] != "lot_not_on_scooter" {
		t.Errorf("the next rider: %d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM lots WHERE id = $1 AND quarantined_by_rider = $2", lot, first); n != 1 {
		t.Error("the lot lost the rider who quarantined it")
	}
	if n := f.seq(second); n != seq {
		t.Errorf("the refused call raised seq to %d", n)
	}
}

func TestRestockSuggestionsSeeTheLowerStock(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	hub := f.id("INSERT INTO hubs (zone_id, name, lat, lng) VALUES ($1, 'Hub', 12.93, 77.63) RETURNING id::text", f.zone)
	f.exec("INSERT INTO lots (code, item_id, qty, expires_at, hub_id) VALUES ('HUB-LOT', $1, 40, now() + interval '2 days', $2)", f.item, hub)
	lot := f.lot("LOT-WARM", 20, 48*time.Hour)
	_, token := f.rider()
	f.start(token)
	f.scan(token, "LOT-WARM")
	svc := &restock.Service{Pool: f.s.Pool, Log: quiet}
	if n, err := svc.Suggest(t.Context(), time.Now()); err != nil || n != 0 {
		t.Fatalf("a full scooter gets %d suggestions, %v", n, err)
	}
	if code, out := f.quarantine(token, lot, `{"reason":"temperature"}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	n, err := svc.Suggest(t.Context(), time.Now())
	if err != nil || n != 1 {
		t.Fatalf("after the quarantine the scooter gets %d suggestions, %v", n, err)
	}
	if c := f.count("SELECT count(*) FROM restock_lines l JOIN restock_requests r ON r.id = l.request_id WHERE r.state = 'suggested' AND l.item_id = $1 AND l.qty = 10", f.item); c != 1 {
		t.Error("the suggestion does not ask for the target quantity")
	}
}

func TestOneLotIsQuarantinedByOnePartyAndTheLedgerKnowsItsReasons(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	lot := f.lot("LOT-1", 10, 48*time.Hour)
	_, token := f.rider()
	f.start(token)
	f.scan(token, "LOT-1")
	f.quarantine(token, lot, `{"reason":"damaged"}`)
	// Both a hub staff member and a rider on one lot is refused by the database. A staff row is needed
	// to try, so make a hub and a staff member by hand.
	hub := f.id("INSERT INTO hubs (zone_id, name, lat, lng) VALUES ($1, 'Hub', 12.93, 77.63) RETURNING id::text", f.zone)
	person := f.id("INSERT INTO people (phone) VALUES ('+919800009999') RETURNING id::text")
	staff := f.id("INSERT INTO hub_staff (person_id, hub_id, role) VALUES ($1, $2, 'hub_lead') RETURNING id::text", person, hub)
	if _, err := f.s.Pool.Exec(t.Context(), `UPDATE lots SET quarantined_by = $2 WHERE id = $1`, lot, staff); err == nil {
		t.Error("the database let both a hub staff member and a rider quarantine one lot")
	}
	if _, err := f.s.Pool.Exec(t.Context(), `INSERT INTO stock_ledger (scooter_id, lot_id, delta_on_hand, reason, actor_type) SELECT scooter_id, lot_id, -1, 'made-up', 'rider' FROM scooter_stock`); err == nil {
		t.Error("the ledger accepted an unknown reason")
	}
}
