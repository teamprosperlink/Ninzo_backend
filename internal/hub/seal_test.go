package hub

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/restock"
	"github.com/teamprosperlink/Ninzo_backend/internal/shifts"
)

func (q *queue) accept() {
	q.t.Helper()
	if code, out := q.call("POST", "/hub/restock/"+q.request+"/accept", q.staffTok, ""); code != 200 {
		q.t.Fatalf("%d %v", code, out)
	}
}

func (q *queue) shortfall(body string) (int, map[string]any) {
	return q.call("POST", "/hub/restock/"+q.request+"/shortfall", q.staffTok, body)
}

func (q *queue) ready(lots ...string) (int, map[string]any) {
	b := `{"lot_numbers":[`
	for i, l := range lots {
		if i > 0 {
			b += ","
		}
		b += `"` + l + `"`
	}
	return q.call("POST", "/hub/restock/"+q.request+"/ready", q.staffTok, b+`]}`)
}

// answer is the rider's answer to the shortfall, as restock serves it.
func (q *queue) answer(accept bool) {
	q.t.Helper()
	svc := &restock.Service{Pool: q.pool, Log: quiet}
	if err := database.InTx(context.Background(), q.pool, func(tx pgx.Tx) error {
		_, err := svc.RespondTx(context.Background(), tx, q.rider, q.request, accept, false)
		return err
	}); err != nil {
		q.t.Fatal(err)
	}
}

func TestRecordingAShortfall(t *testing.T) {
	q := newQueue(t)
	body := `{"action":"partial","lines":[{"item_id":"` + q.samosa + `","qty_missing":4}]}`
	if code, out := q.shortfall(body); code != 409 || out["code"] != "not_accepted" {
		t.Errorf("before accept: %d %v", code, out)
	}
	q.accept()
	tea := q.id("INSERT INTO menu_items (name, price_paise, status, sort_order) VALUES ('Tea', 2900, 'active', 3) RETURNING id::text")
	far := q.id(`INSERT INTO zones (name, city, status) VALUES ('Far', 'Bengaluru', 'active') RETURNING id::text`)
	farHub := q.id("INSERT INTO hubs (zone_id, name, lat, lng) VALUES ($1, 'Far hub', 12.9, 77.7) RETURNING id::text", far)
	for _, bad := range []string{
		`{"action":"partial","lines":[]}`,
		`{"action":"partial","lines":[{"item_id":"` + tea + `","qty_missing":1}]}`,
		`{"action":"partial","lines":[{"item_id":"` + q.samosa + `","qty_missing":5}]}`,
		`{"action":"partial","lines":[{"item_id":"` + q.samosa + `","qty_missing":1},{"item_id":"` + q.samosa + `","qty_missing":1}]}`,
		`{"action":"substitute","lines":[{"item_id":"` + q.samosa + `","qty_missing":1}]}`,
		`{"action":"substitute","substitute_item_id":"` + q.chai + `","lines":[{"item_id":"` + q.samosa + `","qty_missing":1}]}`,
		`{"action":"partial","substitute_item_id":"` + tea + `","lines":[{"item_id":"` + q.samosa + `","qty_missing":1}]}`,
		`{"action":"later","lines":[{"item_id":"` + q.samosa + `","qty_missing":1}]}`,
	} {
		if code, out := q.shortfall(bad); code != 400 {
			t.Errorf("%s: %d %v", bad, code, out)
		}
	}
	if code, out := q.shortfall(`{"action":"other_hub","other_hub_id":"` + farHub + `","lines":[{"item_id":"` + q.samosa + `","qty_missing":4}]}`); code != 409 || out["code"] != "other_hub_unavailable" {
		t.Errorf("a hub in another zone: %d %v", code, out)
	}
	q.lot("TEA-REC", tea, 5, 6*time.Hour) // A substitute is offered only if the hub holds it.
	code, out := q.shortfall(`{"action":"substitute","substitute_item_id":"` + tea + `","reason":"out of samosa","lines":[{"item_id":"` + q.samosa + `","qty_missing":4}]}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	sf := out["request"].(map[string]any)["shortfall"].(map[string]any)
	check(t, sf, "HubShortfall")
	lines := sf["lines"].([]any)
	if sf["action"] != "substitute" || sf["substitute_item_id"] != tea || sf["status"] != "open" || len(lines) != 1 || lines[0].(map[string]any)["qty_missing"] != float64(4) {
		t.Errorf("%v", sf)
	}
	if code, out := q.shortfall(body); code != 409 || out["code"] != "shortfall_already_recorded" {
		t.Errorf("twice: %d %v", code, out)
	}
}

func TestSealingTheCassette(t *testing.T) {
	q := newQueue(t)
	q.lot("CHAI-A", q.chai, 5, 6*time.Hour)
	q.lot("CHAI-B", q.chai, 5, 7*time.Hour)
	q.lot("SAM-A", q.samosa, 4, 6*time.Hour)
	q.accept()
	if code, out := q.ready(); code != 409 || out["code"] != "nothing_packed" {
		t.Errorf("nothing scanned: %d %v", code, out)
	}
	q.scan(q.staffTok, "CHAI-A")
	q.scan(q.staffTok, "CHAI-B")
	if code, out := q.ready("CHAI-A", "CHAI-B"); code != 409 || out["code"] != "shortfall_needed" {
		t.Errorf("samosa short with no shortfall: %d %v", code, out)
	}
	q.scan(q.staffTok, "SAM-A")
	for _, lots := range [][]string{{"CHAI-A", "CHAI-B"}, {"CHAI-A", "CHAI-B", "SAM-A", "SAM-X"}, {"CHAI-A", "CHAI-A", "CHAI-B", "SAM-A"}} {
		if code, out := q.ready(lots...); code != 409 || out["code"] != "lots_mismatch" {
			t.Errorf("%v: %d %v", lots, code, out)
		}
	}
	code, out := q.ready("SAM-A", "CHAI-B", "CHAI-A")
	if code != 200 || out["request"].(map[string]any)["state"] != "ready" {
		t.Fatalf("%d %v", code, out)
	}
	if v, _ := restock.View(context.Background(), q.pool, q.request); v["state"] != "ready" {
		t.Errorf("the rider must see it ready: %v", v)
	}
	if n := q.count("SELECT count(*) FROM restock_lines WHERE request_id = $1 AND filled_qty = qty", q.request); n != 1 {
		t.Errorf("filled_qty not set: %d lines filled exactly", n)
	}
	seq := out["request"].(map[string]any)["seq"]
	if code, again := q.ready("SAM-A", "CHAI-B", "CHAI-A"); code != 200 || again["request"].(map[string]any)["seq"] != seq {
		t.Errorf("ready again must change nothing: %d %v", code, again)
	}
	if code, out := q.call("POST", "/hub/restock/"+q.request+"/ready", q.staffTok, `{}`); code != 400 {
		t.Errorf("no lot_numbers: %d %v", code, out)
	}
}

func (q *queue) count(sql string, args ...any) int {
	q.t.Helper()
	var n int
	if err := q.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		q.t.Fatal(err)
	}
	return n
}

func TestAnEmptyCassetteAfterAShortfall(t *testing.T) {
	q := newQueue(t)
	q.accept()
	q.shortfall(`{"action":"partial","lines":[{"item_id":"` + q.chai + `","qty_missing":8},{"item_id":"` + q.samosa + `","qty_missing":4}]}`)
	code, out := q.ready()
	if code != 200 || out["request"].(map[string]any)["state"] != "partial" {
		t.Fatalf("an empty cassette after a shortfall: %d %v", code, out)
	}
	// The rider can answer now. With nothing packed, accepting closes it.
	q.answer(true)
	if q.state() != "closed" {
		t.Errorf("%s", q.state())
	}
}

func TestASubstituteIsPackedAfterTheRiderAccepts(t *testing.T) {
	q := newQueue(t)
	tea := q.id("INSERT INTO menu_items (name, price_paise, status, sort_order) VALUES ('Tea', 2900, 'active', 3) RETURNING id::text")
	q.lot("CHAI-A", q.chai, 8, 6*time.Hour)
	q.lot("TEA-A", tea, 4, 6*time.Hour)
	q.accept()
	q.scan(q.staffTok, "CHAI-A")
	q.shortfall(`{"action":"substitute","substitute_item_id":"` + tea + `","lines":[{"item_id":"` + q.samosa + `","qty_missing":4}]}`)
	if code, out := q.ready("CHAI-A"); code != 200 || out["request"].(map[string]any)["state"] != "partial" {
		t.Fatalf("%d %v", code, out)
	}
	q.answer(true)
	if q.state() != "packing" {
		t.Fatalf("accepting a substitute must reopen packing: %s", q.state())
	}
	_, out := q.call("GET", "/hub/restock/"+q.request, q.staffTok, "")
	items := map[string]float64{}
	for _, raw := range out["request"].(map[string]any)["items"].([]any) {
		l := raw.(map[string]any)
		items[l["item_id"].(string)] = l["qty"].(float64)
	}
	if _, still := items[q.samosa]; still || items[tea] != 4 || items[q.chai] != 8 {
		t.Errorf("the samosa line gives way to 4 tea: %v", items)
	}
	if code, out := q.scan(q.staffTok, "TEA-A"); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if code, out := q.ready("CHAI-A", "TEA-A"); code != 200 || out["request"].(map[string]any)["state"] != "ready" {
		t.Errorf("sealed again with the substitute: %d %v", code, out)
	}
}

func TestTheLabel(t *testing.T) {
	q := newQueue(t)
	q.lot("CHAI-L", q.chai, 8, 6*time.Hour)
	q.lot("SAM-L", q.samosa, 4, 6*time.Hour)
	q.accept()
	if code, out := q.call("GET", "/hub/restock/"+q.request+"/label", q.staffTok, ""); code != 409 || out["code"] != "not_ready" {
		t.Errorf("before sealing: %d %v", code, out)
	}
	q.scan(q.staffTok, "CHAI-L")
	q.scan(q.staffTok, "SAM-L")
	q.ready("CHAI-L", "SAM-L")
	code, out := q.call("GET", "/hub/restock/"+q.request+"/label", q.staffTok, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "LabelResponse")
	l := out["label"].(map[string]any)
	check(t, l, "Label")
	if l["scooter_code"] != "SC-7" || l["hub_name"] != "Hub A" || len(l["lot_numbers"].([]any)) != 2 || !strings.HasPrefix(l["request_code"].(string), "R-") {
		t.Errorf("%v", l)
	}
}

func TestTheHubsLots(t *testing.T) {
	q := newQueue(t)
	q.lot("CHAI-OK", q.chai, 5, 6*time.Hour)
	q.lot("CHAI-OLD", q.chai, 5, -time.Hour)
	q.exec("UPDATE lots SET status = 'quarantined' WHERE id = $1", q.lot("SAM-Q", q.samosa, 5, time.Hour))
	q.accept()
	q.scan(q.staffTok, "CHAI-OK")
	code, out := q.call("GET", "/hub/lots", q.staffTok, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "HubLotList")
	got := map[string]map[string]any{}
	for _, raw := range out["lots"].([]any) {
		l := raw.(map[string]any)
		got[l["lot_code"].(string)] = l
	}
	check(t, got["CHAI-OK"], "HubLot")
	if got["CHAI-OK"]["in_cassette"] != true || got["CHAI-OLD"]["status"] != "expired" || got["SAM-Q"]["status"] != "quarantined" {
		t.Errorf("%v", got)
	}
	if _, out := q.call("GET", "/hub/lots?item_id="+q.samosa, q.staffTok, ""); len(out["lots"].([]any)) != 1 {
		t.Errorf("the item filter: %v", out)
	}
	if code, _ := q.call("GET", "/hub/lots?item_id=nope", q.staffTok, ""); code != 400 {
		t.Errorf("a bad item_id: %d", code)
	}
}

func TestQuarantine(t *testing.T) {
	q := newQueue(t)
	warm := q.lot("CHAI-W", q.chai, 5, 6*time.Hour)
	q.accept()
	q.scan(q.staffTok, "CHAI-W")
	path := "/hub/lots/" + warm + "/quarantine"
	if code, out := q.call("POST", path, q.staffTok, `{"reason":"temperature"}`); code != 403 || out["missing_permission"] != "lot.quarantine" {
		t.Errorf("hub staff: %d %v", code, out)
	}
	if code, _ := q.call("POST", path, q.leadTok, `{"reason":"too warm"}`); code != 400 {
		t.Errorf("a bad reason: %d", code)
	}
	code, out := q.call("POST", path, q.leadTok, `{"reason":"temperature","note":"9 C at the bench"}`)
	if code != 200 || out["lot"].(map[string]any)["status"] != "quarantined" || out["lot"].(map[string]any)["in_cassette"] != false {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "HubLotResponse")
	_, req := q.call("GET", "/hub/restock/"+q.request, q.staffTok, "")
	if len(req["request"].(map[string]any)["lot_numbers"].([]any)) != 0 {
		t.Error("a quarantined lot stays in the cassette")
	}
	if code, out := q.scan(q.staffTok, "CHAI-W"); code != 409 || out["code"] != "lot_quarantined" {
		t.Errorf("a quarantined lot was scanned again: %d %v", code, out)
	}
	if code, _ := q.call("POST", path, q.leadTok, `{"reason":"temperature"}`); code != 200 {
		t.Errorf("again: %d", code)
	}
	if n := q.count("SELECT count(*) FROM audit_events WHERE action = 'lot.quarantined'"); n != 1 {
		t.Errorf("%d audit events", n)
	}
	other := q.id("INSERT INTO hubs (zone_id, name, lat, lng) VALUES ($1, 'Hub B', 12.94, 77.63) RETURNING id::text", q.zone)
	away := q.id("INSERT INTO lots (code, item_id, qty, expires_at, hub_id) VALUES ('AWAY-Q', $1, 5, now() + interval '1 day', $2) RETURNING id::text", q.chai, other)
	if code, _ := q.call("POST", "/hub/lots/"+away+"/quarantine", q.leadTok, `{"reason":"damaged"}`); code != 404 {
		t.Errorf("another hub's lot: %d", code)
	}
}

func TestALotThatLeftCannotBeQuarantined(t *testing.T) {
	q := newQueue(t)
	lot := q.lot("CHAI-GONE", q.chai, 8, 6*time.Hour)
	q.lot("SAM-GONE", q.samosa, 4, 6*time.Hour)
	q.accept()
	q.scan(q.staffTok, "CHAI-GONE")
	q.scan(q.staffTok, "SAM-GONE")
	q.ready("CHAI-GONE", "SAM-GONE")
	if err := database.InTx(context.Background(), q.pool, func(tx pgx.Tx) error {
		_, err := shifts.ScanTx(context.Background(), tx, q.rider, "CHAI-GONE", false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if code, out := q.call("POST", "/hub/lots/"+lot+"/quarantine", q.leadTok, `{"reason":"damaged"}`); code != 409 || out["code"] != "lot_on_scooter" {
		t.Errorf("%d %v", code, out)
	}
}

func TestTheSimulatorWritesShortLines(t *testing.T) {
	q := newQueue(t)
	q.exec("UPDATE hub_staff SET status = 'suspended'") // The simulator plays a hub with no active staff.
	q.lot("CHAI-S", q.chai, 5, 6*time.Hour)
	sim := &restock.Simulator{Service: &restock.Service{Pool: q.pool, Log: quiet}}
	now := time.Now()
	for _, after := range []time.Duration{21 * time.Second, 42 * time.Second, 103 * time.Second} {
		sim.Step(context.Background(), now.Add(after))
	}
	v, err := View(context.Background(), q.pool, q.request)
	if err != nil {
		t.Fatal(err)
	}
	sf, _ := v["shortfall"].(map[string]any)
	if sf == nil || len(sf["lines"].([]map[string]any)) != 2 {
		t.Fatalf("chai 3 short and samosa 4 short: %v", v["shortfall"])
	}
	for _, l := range sf["lines"].([]map[string]any) {
		if want := map[string]int{q.chai: 3, q.samosa: 4}[l["item_id"].(string)]; l["qty_missing"] != want {
			t.Errorf("%v, want %d missing", l, want)
		}
	}
}

func TestShortLinesFollowWhatWasPacked(t *testing.T) {
	q := newQueue(t)
	q.lot("CHAI-F", q.chai, 8, 6*time.Hour)
	q.accept()
	q.scan(q.staffTok, "CHAI-F")
	// Recorded as 1 samosa short, but none was packed: 4 are short.
	q.shortfall(`{"action":"partial","lines":[{"item_id":"` + q.samosa + `","qty_missing":1}]}`)
	if code, out := q.ready("CHAI-F"); code != 200 || out["request"].(map[string]any)["state"] != "partial" {
		t.Fatalf("%d %v", code, out)
	}
	if n := q.count("SELECT qty_missing FROM shortfall_lines WHERE request_id = $1 AND item_id = $2", q.request, q.samosa); n != 4 {
		t.Errorf("samosa missing %d, want 4", n)
	}
}

func TestAShortfallNotNeededIsDropped(t *testing.T) {
	q := newQueue(t)
	q.lot("CHAI-N", q.chai, 8, 6*time.Hour)
	q.lot("SAM-N", q.samosa, 4, 6*time.Hour)
	q.accept()
	q.shortfall(`{"action":"partial","lines":[{"item_id":"` + q.samosa + `","qty_missing":4}]}`)
	q.scan(q.staffTok, "CHAI-N")
	q.scan(q.staffTok, "SAM-N") // Found after all.
	if code, out := q.ready("CHAI-N", "SAM-N"); code != 200 || out["request"].(map[string]any)["state"] != "ready" || out["request"].(map[string]any)["shortfall"] != nil {
		t.Errorf("%d %v", code, out)
	}
}

func TestASubstituteTheHubRunsShortOfStillSeals(t *testing.T) {
	q := newQueue(t)
	tea := q.id("INSERT INTO menu_items (name, price_paise, status, sort_order) VALUES ('Tea', 2900, 'active', 3) RETURNING id::text")
	q.lot("CHAI-R", q.chai, 8, 6*time.Hour)
	tealot := q.lot("TEA-R", tea, 4, 6*time.Hour)
	q.accept()
	q.scan(q.staffTok, "CHAI-R")
	q.shortfall(`{"action":"substitute","substitute_item_id":"` + tea + `","lines":[{"item_id":"` + q.samosa + `","qty_missing":4}]}`)
	q.ready("CHAI-R")
	q.answer(true)
	// The tea goes bad before it is packed: the hub seals what it has.
	q.exec("UPDATE lots SET status = 'quarantined' WHERE id = $1", tealot)
	if code, out := q.ready("CHAI-R"); code != 200 || out["request"].(map[string]any)["state"] != "ready" {
		t.Errorf("a request must never be stuck after a substitute: %d %v", code, out)
	}
}

func TestASubstituteMustBeInStock(t *testing.T) {
	q := newQueue(t)
	tea := q.id("INSERT INTO menu_items (name, price_paise, status, sort_order) VALUES ('Tea', 2900, 'active', 3) RETURNING id::text")
	q.lot("TEA-ONE", tea, 2, 6*time.Hour)
	q.accept()
	if code, out := q.shortfall(`{"action":"substitute","substitute_item_id":"` + tea + `","lines":[{"item_id":"` + q.samosa + `","qty_missing":4}]}`); code != 409 || out["code"] != "substitute_out_of_stock" {
		t.Errorf("%d %v", code, out)
	}
}

func TestAQuarantinedLotIsNeverSealed(t *testing.T) {
	q := newQueue(t)
	lot := q.lot("CHAI-QS", q.chai, 8, 6*time.Hour)
	q.lot("SAM-QS", q.samosa, 4, 6*time.Hour)
	q.accept()
	q.scan(q.staffTok, "CHAI-QS")
	q.scan(q.staffTok, "SAM-QS")
	// As if a quarantine raced the scan and left the lot in the cassette.
	q.exec("UPDATE lots SET status = 'quarantined' WHERE id = $1", lot)
	if code, out := q.ready("CHAI-QS", "SAM-QS"); code != 409 || out["code"] != "lot_quarantined" {
		t.Errorf("%d %v", code, out)
	}
}

func TestQuarantineEmptiesASealedCassetteToo(t *testing.T) {
	q := newQueue(t)
	lot := q.lot("CHAI-SQ", q.chai, 8, 6*time.Hour)
	q.lot("SAM-SQ", q.samosa, 4, 6*time.Hour)
	q.accept()
	q.scan(q.staffTok, "CHAI-SQ")
	q.scan(q.staffTok, "SAM-SQ")
	q.ready("CHAI-SQ", "SAM-SQ")
	if code, out := q.call("POST", "/hub/lots/"+lot+"/quarantine", q.leadTok, `{"reason":"damaged"}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	_, out := q.call("GET", "/hub/restock/"+q.request, q.staffTok, "")
	if lots := out["request"].(map[string]any)["lot_numbers"].([]any); len(lots) != 1 || lots[0] != "SAM-SQ" {
		t.Errorf("%v", lots)
	}
	if n := q.count("SELECT filled_qty FROM restock_lines WHERE request_id = $1 AND item_id = $2", q.request, q.chai); n != 0 {
		t.Errorf("filled chai %d after its lot left, want 0", n)
	}
	// The rider can still finish the pickup with what is left.
	if err := database.InTx(context.Background(), q.pool, func(tx pgx.Tx) error {
		_, err := shifts.ScanTx(context.Background(), tx, q.rider, "SAM-SQ", false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if q.state() != "received" {
		t.Errorf("%s", q.state())
	}
}
