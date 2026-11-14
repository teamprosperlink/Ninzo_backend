package console

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Issue 235, part B3: support and ops see the lots taken out of stock, in a list and on an order's
// timeline. Each test uses the console's real handlers and checks replies against the bundled contract.

const riderWords = "the probe said nine degrees, secret rider words"

// quarantined is a lot a rider quarantined `ago` before now on a scooter, with the one stock ledger entry the
// rider API writes. withNote puts words the rider typed on it, which no console reply may show.
func (f *fixture) quarantined(item, code, scooter, rider, reason string, qty int, ago time.Duration) string {
	f.t.Helper()
	at := f.now.Add(-ago).Truncate(time.Microsecond)
	lot := f.id(`INSERT INTO lots (code, item_id, qty, expires_at, status, quarantine_reason, quarantine_note, quarantined_by_rider, quarantined_at)
		VALUES ($1, $2, $3, now() + interval '1 day', 'quarantined', $4, $5, $6, $7) RETURNING id::text`, code, item, qty, reason, riderWords, rider, at)
	f.exec("INSERT INTO scooter_stock (scooter_id, lot_id, on_hand) VALUES ($1, $2, 0)", scooter, lot)
	f.exec("INSERT INTO stock_ledger (scooter_id, lot_id, delta_on_hand, reason, actor_type, actor_id) VALUES ($1, $2, $3, 'quarantine', 'rider', $4)", scooter, lot, -qty, rider)
	return lot
}

func (f *fixture) scooterOf(order string) string {
	f.t.Helper()
	return f.id("SELECT scooter_id::text FROM orders WHERE id = $1", order)
}

type quarantineDesk struct {
	*orderFixture
	chai, burger                     string
	l1, l2, l3, l4, h1               string // Newest is h1, then l4, l3, l2, l1.
	scooterA1, scooterA4, scooterB   string
	support, admin, hubMgr, opsToken string
	hub                              string
}

func newQuarantineDesk(t *testing.T) *quarantineDesk {
	o := newOrderFixture(t)
	f := o.f
	d := &quarantineDesk{orderFixture: o}
	// Signing in moves the fixture's clock on, so it is done before any time is read from it.
	signIn := func(login, role string) string {
		_, temp := f.user(login, role)
		return f.signIn(login, temp)
	}
	d.support, d.admin, d.hubMgr, d.opsToken = signIn("qua.sup", "support"), signIn("qua.adm", "admin"), signIn("qua.hub", "hub_manager"), o.token
	d.chai = f.id("INSERT INTO menu_items (name, price_paise, status) VALUES ('Chai', 4900, 'active') RETURNING id::text")
	d.burger = f.id("INSERT INTO menu_items (name, price_paise, status) VALUES ('Burger', 9900, 'active') RETURNING id::text")
	d.scooterA1, d.scooterA4, d.scooterB = f.scooterOf(o.o1), f.scooterOf(o.o4), f.scooterOf(o.o3)
	// o1 had units from l2, and o3 from l4.
	d.l1 = f.quarantined(d.chai, "LOT-1", d.scooterA1, o.r1, "expired", 6, 5*time.Hour)
	d.l2 = f.quarantined(d.chai, "LOT-2", d.scooterA1, o.r1, "temperature", 8, 4*time.Hour)
	d.l3 = f.quarantined(d.burger, "LOT-3", d.scooterA4, o.r2, "damaged", 3, 3*time.Hour)
	d.l4 = f.quarantined(d.burger, "LOT-4", d.scooterB, o.r1, "other", 5, 2*time.Hour)
	for _, r := range []struct{ order, scooter, lot, item string }{{o.o1, d.scooterA1, d.l2, d.chai}, {o.o3, d.scooterB, d.l4, d.burger}} {
		f.exec("INSERT INTO reservations (order_id, scooter_id, lot_id, item_id, qty, status) VALUES ($1, $2, $3, $4, 1, 'handed_over')", r.order, r.scooter, r.lot, r.item)
	}
	// A hub lead's lot, in zone A: no scooter, no rider.
	d.hub = f.id("INSERT INTO hubs (zone_id, name, lat, lng) VALUES ($1, 'Hub A', 12.93, 77.63) RETURNING id::text", o.zoneA)
	person := f.id("INSERT INTO people (phone) VALUES ('+919800007777') RETURNING id::text")
	staff := f.id("INSERT INTO hub_staff (person_id, hub_id, role, name) VALUES ($1, $2, 'hub_lead', 'Lead') RETURNING id::text", person, d.hub)
	d.h1 = f.id(`INSERT INTO lots (code, item_id, qty, expires_at, hub_id, status, quarantine_reason, quarantine_note, quarantined_by, quarantined_at)
		VALUES ('LOT-H1', $1, 12, now() + interval '1 day', $2, 'quarantined', 'contamination', $3, $4, $5) RETURNING id::text`,
		d.chai, d.hub, riderWords, staff, f.now.Add(-time.Hour).Truncate(time.Microsecond))
	// A lot that is fine, one quarantined with no time (nothing to sort by) and a lot that is not quarantined at all.
	f.exec("INSERT INTO lots (code, item_id, qty, expires_at, hub_id) VALUES ('LOT-OK', $1, 5, now() + interval '1 day', $2)", d.chai, d.hub)
	return d
}

func (d *quarantineDesk) list(token, query string) (int, map[string]any) {
	d.f.t.Helper()
	return d.f.call("GET", "/admin/quarantined-stock"+query, token, "")
}

func lotIDs(out map[string]any) []string {
	ids := []string{}
	for _, x := range out["lots"].([]any) {
		ids = append(ids, x.(map[string]any)["lot_id"].(string))
	}
	return ids
}

func rowOf(t *testing.T, out map[string]any, lot string) map[string]any {
	t.Helper()
	for _, x := range out["lots"].([]any) {
		if x.(map[string]any)["lot_id"] == lot {
			return x.(map[string]any)
		}
	}
	t.Fatalf("lot %s is not in %v", lot, out)
	return nil
}

func TestSupportReadsTheQuarantinedStockAndOthersAreRefused(t *testing.T) {
	d := newQuarantineDesk(t)
	for name, token := range map[string]string{"support": d.support, "admin": d.admin, "ops": d.opsToken} {
		code, out := d.list(token, "")
		if code != 200 {
			t.Fatalf("%s: %d %v", name, code, out)
		}
		matchesContract(t, "QuarantinedStockList", out)
	}
	code, out := d.list(d.support, "")
	// Newest first, and every quarantined lot with a time is there once.
	want := []string{d.h1, d.l4, d.l3, d.l2, d.l1}
	if got := lotIDs(out); strings.Join(got, ",") != strings.Join(want, ",") || code != 200 {
		t.Errorf("order %v, want %v", got, want)
	}
	// A rider's lot: item, units, scooter, rider, reason, and the orders that had units from it.
	l2 := rowOf(t, out, d.l2)
	scooter, rider := l2["scooter"].(map[string]any), l2["rider"].(map[string]any)
	if l2["source"] != "rider" || l2["reason"] != "temperature" || l2["qty"] != float64(8) || l2["lot_code"] != "LOT-2" || l2["hub"] != nil ||
		l2["item"].(map[string]any)["name"] != "Chai" || scooter["id"] != d.scooterA1 || scooter["code"] != d.f.id("SELECT code FROM scooters WHERE id = $1", d.scooterA1) || rider["id"] != d.r1 || rider["name"] != "Ravi" ||
		len(l2["order_ids"].([]any)) != 1 || l2["order_ids"].([]any)[0] != d.o1 {
		t.Errorf("a rider's lot: %v", l2)
	}
	if got := rowOf(t, out, d.l1)["order_ids"].([]any); len(got) != 0 {
		t.Errorf("a lot no order used lists orders: %v", got)
	}
	// A hub lead's lot: from the hub, no scooter, no rider.
	h1 := rowOf(t, out, d.h1)
	if h1["source"] != "hub" || h1["reason"] != "contamination" || h1["qty"] != float64(12) || h1["scooter"] != nil || h1["rider"] != nil || h1["hub"].(map[string]any)["name"] != "Hub A" {
		t.Errorf("a hub lead's lot: %v", h1)
	}
	// What the rider or hub lead typed never leaves.
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "secret rider words") || strings.Contains(string(raw), "probe") || strings.Contains(string(raw), "note") {
		t.Errorf("a typed note leaked: %s", raw)
	}
	// Who may read.
	if code, out := d.list(d.hubMgr, ""); code != 403 || out["missing_permission"] != "orders.read" {
		t.Errorf("a hub manager: %d %v", code, out)
	}
	if code, _ := d.list("", ""); code != 401 {
		t.Errorf("no token: %d", code)
	}
	// It only reads.
	for _, method := range []string{"POST", "PATCH", "DELETE", "PUT"} {
		if code, _ := d.f.call(method, "/admin/quarantined-stock", d.admin, `{}`); code != 405 && code != 404 {
			t.Errorf("%s: %d", method, code)
		}
	}
}

func TestQuarantinedStockFiltersAndPages(t *testing.T) {
	d := newQuarantineDesk(t)
	at := func(ago time.Duration) string {
		return url.QueryEscape(d.f.now.Add(-ago).Truncate(time.Microsecond).Format(time.RFC3339Nano))
	}
	for name, tc := range map[string]struct {
		query string
		want  []string
	}{
		"zone A (rider and hub lots)":     {"?zone_id=" + d.zoneA, []string{d.h1, d.l3, d.l2, d.l1}},
		"zone B":                          {"?zone_id=" + d.zoneB, []string{d.l4}},
		"a scooter":                       {"?scooter_id=" + d.scooterA1, []string{d.l2, d.l1}},
		"a scooter leaves hub lots out":   {"?scooter_id=" + d.scooterA4, []string{d.l3}},
		"from":                            {"?from=" + at(150*time.Minute), []string{d.h1, d.l4}},
		"from is inclusive":               {"?from=" + at(2*time.Hour), []string{d.h1, d.l4}},
		"to":                              {"?to=" + at(150*time.Minute), []string{d.l3, d.l2, d.l1}},
		"to is exclusive":                 {"?to=" + at(2*time.Hour), []string{d.l3, d.l2, d.l1}},
		"from and to":                     {"?from=" + at(210*time.Minute) + "&to=" + at(90*time.Minute), []string{d.l4, d.l3}},
		"zone and window":                 {"?zone_id=" + d.zoneA + "&from=" + at(250*time.Minute), []string{d.h1, d.l3, d.l2}},
		"nothing in the window":           {"?from=" + at(30*time.Minute), []string{}},
		"a scooter nobody quarantined on": {"?scooter_id=00000000-0000-4000-8000-000000000001", []string{}},
	} {
		code, out := d.list(d.support, tc.query)
		if code != 200 {
			t.Errorf("%s: %d %v", name, code, out)
			continue
		}
		matchesContract(t, "QuarantinedStockList", out)
		if got := lotIDs(out); strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
	// Paging: two at a time, newest first, no lot twice, and the cursor ends.
	var all []string
	cursor := ""
	for page := 0; page < 5; page++ {
		code, out := d.list(d.support, "?limit=2"+cursor)
		if code != 200 {
			t.Fatalf("page %d: %d %v", page, code, out)
		}
		matchesContract(t, "QuarantinedStockList", out)
		all = append(all, lotIDs(out)...)
		next, _ := out["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = "&cursor=" + next
	}
	if strings.Join(all, ",") != strings.Join([]string{d.h1, d.l4, d.l3, d.l2, d.l1}, ",") {
		t.Errorf("pages gave %v", all)
	}
	// A filter keeps its meaning across pages.
	_, first := d.list(d.support, "?limit=1&zone_id="+d.zoneA)
	_, second := d.list(d.support, "?limit=1&zone_id="+d.zoneA+"&cursor="+first["next_cursor"].(string))
	if lotIDs(first)[0] != d.h1 || lotIDs(second)[0] != d.l3 {
		t.Errorf("a filtered page: %v then %v", lotIDs(first), lotIDs(second))
	}
	for name, bad := range map[string]string{
		"a zone that is not an id": "?zone_id=nope", "a scooter that is not an id": "?scooter_id=nope", "from that is not a time": "?from=yesterday",
		"to that is not a time": "?to=2026-11-14", "from after to": "?from=" + at(time.Hour) + "&to=" + at(2*time.Hour), "from equal to": "?from=" + at(time.Hour) + "&to=" + at(time.Hour),
		"limit 0": "?limit=0", "limit 101": "?limit=101", "a bad cursor": "?cursor=zzz", "a cursor that is not ours": "?cursor=" + url.QueryEscape("MQ"),
	} {
		if code, out := d.list(d.support, bad); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
}

func TestTheTimelineShowsAQuarantineOnTheOrdersScooterWithinTheWindow(t *testing.T) {
	d := newQuarantineDesk(t)
	f := d.f
	// o1 was made 50 minutes ago and handed over 30 minutes ago. Its scooter had a quarantine 4 hours
	// ago and 5 hours ago (before the window, which starts an hour before the order), and these three:
	// 70 minutes ago (inside), 20 minutes ago (after the order ended), and one on a scooter that
	// is not its own.
	created := f.now.Add(-50 * time.Minute)
	f.exec("UPDATE orders SET handed_over_at = $2 WHERE id = $1", d.o1, created.Add(20*time.Minute))
	inside := f.quarantined(d.burger, "LOT-IN", d.scooterA1, d.r1, "temperature", 4, 70*time.Minute)
	f.quarantined(d.burger, "LOT-LATE", d.scooterA1, d.r1, "temperature", 4, 20*time.Minute)
	f.quarantined(d.burger, "LOT-OTHER", d.scooterA4, d.r2, "temperature", 4, 60*time.Minute)
	f.exec("INSERT INTO reservations (order_id, scooter_id, lot_id, item_id, qty, status) VALUES ($1, $2, $3, $4, 1, 'handed_over')", d.o1, d.scooterA1, inside, d.burger)
	code, out := f.call("GET", "/admin/orders/"+d.o1+"/timeline", d.support, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	matchesContract(t, "OrderTimeline", out)
	var stock []map[string]any
	for _, e := range out["events"].([]any) {
		if e.(map[string]any)["source"] == "stock" {
			stock = append(stock, e.(map[string]any))
		}
	}
	if len(stock) != 1 {
		t.Fatalf("%d stock events, want 1 (the one 70 minutes ago): %v", len(stock), stock)
	}
	e := stock[0]
	detail := e["detail"].(map[string]any)
	if e["kind"] != "stock.quarantined" || e["actor_type"] != "rider" || e["actor_name"] != "Ravi" || detail["lot_id"] != inside || detail["lot_code"] != "LOT-IN" ||
		detail["item_name"] != "Burger" || detail["reason"] != "temperature" || detail["qty"] != float64(4) || detail["had_units_for_this_order"] != true {
		t.Errorf("the event: %v", e)
	}
	if at(t, e["at"]).Sub(f.now.Add(-70*time.Minute)) > time.Second || at(t, e["at"]).Before(f.now.Add(-71*time.Minute)) {
		t.Errorf("the event is at %v", e["at"])
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "secret rider words") {
		t.Errorf("a typed note leaked into the timeline: %s", raw)
	}
	// Oldest first overall: the quarantine (70 min ago) comes before the order was made (50 min ago).
	events := out["events"].([]any)
	var stockAt, firstAt time.Time
	for i, ev := range events {
		m := ev.(map[string]any)
		if i == 0 {
			firstAt = at(t, m["at"])
		}
		if m["source"] == "stock" {
			stockAt = at(t, m["at"])
		}
		if i > 0 && at(t, m["at"]).Before(at(t, events[i-1].(map[string]any)["at"])) {
			t.Errorf("events are out of order at %d: %v", i, events)
		}
	}
	if !firstAt.Equal(stockAt) {
		t.Errorf("the quarantine, 70 minutes ago, should come first: %v vs %v", firstAt, stockAt)
	}

	// An order still open runs to now: o3 (30 minutes old) shows a quarantine 5 minutes ago, and an
	// hour-and-a-half-old one on its scooter is outside (its window starts 90 minutes ago... so
	// one at 100 minutes ago is out, and l4 at 2 hours is out too).
	recent := f.quarantined(d.burger, "LOT-NOW", d.scooterB, d.r1, "expired", 2, 5*time.Minute)
	f.quarantined(d.burger, "LOT-OLD", d.scooterB, d.r1, "expired", 2, 100*time.Minute)
	_, out = f.call("GET", "/admin/orders/"+d.o3+"/timeline", d.support, "")
	matchesContract(t, "OrderTimeline", out)
	var lots []string
	for _, e := range out["events"].([]any) {
		if m := e.(map[string]any); m["source"] == "stock" {
			lots = append(lots, m["detail"].(map[string]any)["lot_id"].(string))
		}
	}
	if len(lots) != 1 || lots[0] != recent {
		t.Errorf("an open order shows %v, want only %s", lots, recent)
	}
	// An order whose scooter never had a quarantine, and an order with a hub lead's quarantine in its zone, show none.
	_, out = f.call("GET", "/admin/orders/"+d.o5+"/timeline", d.support, "")
	for _, e := range out["events"].([]any) {
		if e.(map[string]any)["source"] == "stock" {
			t.Errorf("an order with no quarantine shows %v", e)
		}
	}
}

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

func TestHubOpsCountsALotARiderQuarantinedThatTheHubPacked(t *testing.T) {
	d := newOpsDesk(t)
	f := d.f
	_, base := d.list(d.admin, "")
	was := int(d.ops4(base, d.hubA)["quarantined_lots"].(float64))
	wasB := int(d.ops4(base, d.hubB)["quarantined_lots"].(float64))
	// Hub A packed a lot, a rider took it, and the rider quarantined it on the scooter.
	request := d.request(d.hubA, "received", time.Hour)
	rider, scooter := f.id("SELECT rider_id::text FROM restock_requests WHERE id = $1", request), f.id("SELECT scooter_id::text FROM restock_requests WHERE id = $1", request)
	lot := f.quarantined(d.chai, "RQ-1", scooter, rider, "temperature", 5, 10*time.Minute)
	f.exec("INSERT INTO restock_lots (request_id, lot_id) VALUES ($1, $2)", request, lot)
	// A rider's lot that no hub packed (a test scan) belongs to no hub.
	f.quarantined(d.chai, "RQ-2", scooter, rider, "damaged", 2, 5*time.Minute)
	// A lot a rider still has, not quarantined, is not counted.
	other := f.id("INSERT INTO lots (code, item_id, qty, expires_at) VALUES ('RQ-3', $1, 5, now() + interval '1 day') RETURNING id::text", d.chai)
	f.exec("INSERT INTO restock_lots (request_id, lot_id) VALUES ($1, $2)", request, other)
	_, out := d.list(d.admin, "")
	if got := int(d.ops4(out, d.hubA)["quarantined_lots"].(float64)); got != was+1 {
		t.Errorf("hub A counts %d quarantined lots, want %d", got, was+1)
	}
	if got := int(d.ops4(out, d.hubB)["quarantined_lots"].(float64)); got != wasB {
		t.Errorf("hub B counts %d, want %d", got, wasB)
	}
}
