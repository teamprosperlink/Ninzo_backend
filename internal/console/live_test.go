package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// liveFixture is two zones with an ops user, and helpers to put riders, shifts, offers and
// orders in them.
type liveFixture struct {
	*fixture
	token, opsID string
	zoneA, zoneB string
	cust         string
}

func newLiveFixture(t *testing.T) *liveFixture {
	f := newFixture(t)
	opsID, temp := f.user("live.ops", "ops")
	l := &liveFixture{fixture: f, token: f.signIn("live.ops", temp), opsID: opsID}
	l.zoneA, l.zoneB = f.zone2("LA"), f.zone2("LB")
	l.cust = f.shopper("+919800007777")
	return l
}

// poll reads the zone as the ops user, after the five seconds the contract asks for.
func (l *liveFixture) poll(zone string) (int, map[string]any) {
	l.t.Helper()
	l.now = l.now.Add(6 * time.Second)
	return l.call("GET", "/admin/zones/"+zone+"/live", l.token, "")
}

// onShift is a rider with an open shift in the zone, on their own node and scooter.
type onShift struct{ rider, shift, node, scooter string }

var phoneCounter int

func (l *liveFixture) onShift(zone, name, plate string) onShift {
	l.t.Helper()
	phoneCounter++
	var s onShift
	s.rider = l.courier(fmt.Sprintf("+9198111%05d", phoneCounter), name)
	l.exec("UPDATE riders SET zone_id = $2 WHERE id = $1", s.rider, zone)
	s.node = l.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, $2, 12.9300, 77.6200, 4) RETURNING id::text", zone, "Node "+plate)
	s.scooter = l.id("INSERT INTO scooters (code, zone_id, node_id, current_rider_id) VALUES ($1, $2, $3, $4) RETURNING id::text", plate, zone, s.node, s.rider)
	s.shift = l.id("INSERT INTO shifts (rider_id, node_id, scooter_id, started_at) VALUES ($1, $2, $3, $4) RETURNING id::text", s.rider, s.node, s.scooter, l.now.Add(-time.Hour))
	return s
}

func (l *liveFixture) ping(rider string, lat, lng float64, ago time.Duration) {
	l.t.Helper()
	l.exec(`INSERT INTO location_pings (rider_id, lat, lng, speed_mps, accuracy_m, device_time, received_at, applied)
		VALUES ($1, $2, $3, 3, 5, $4, $4, false)`, rider, lat, lng, l.now.Add(-ago))
}

func (l *liveFixture) offer(s onShift, order, state string, expires time.Duration) {
	l.t.Helper()
	released := "NULL"
	if state != "open" && state != "accepted" {
		released = "now()"
	}
	accepted := "NULL"
	if state == "accepted" {
		accepted = "now()"
	}
	l.n++
	l.exec(`INSERT INTO offers (order_id, rider_id, scooter_id, state, fencing_token, expires_at, rider_accepted_at, released_at)
		VALUES ($1, $2, $3, $4, $5, $6, `+accepted+`, `+released+`)`, order, s.rider, s.scooter, state, fmt.Sprintf("fence-%d", l.n), l.now.Add(expires))
}

func (l *liveFixture) relocate(s onShift, state string) {
	l.t.Helper()
	to := l.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ((SELECT zone_id FROM staging_nodes WHERE id = $1), $2, 12.95, 77.64, 4) RETURNING id::text", s.node, "To "+s.node[:8])
	accepted := "NULL"
	if state == "accepted" {
		accepted = "now()"
	}
	l.exec(`INSERT INTO relocations (rider_id, shift_id, from_node_id, to_node_id, reason, state, expires_at, accepted_at)
		VALUES ($1, $2, $3, $4, 'demand', $5, $6, `+accepted+`)`, s.rider, s.shift, s.node, to, state, l.now.Add(time.Hour))
}

func (l *liveFixture) onBreak(s onShift) {
	l.t.Helper()
	l.exec("UPDATE shifts SET break_started_at = $2, break_ends_at = $3 WHERE id = $1", s.shift, l.now.Add(-time.Minute), l.now.Add(20*time.Minute))
}

// order adds a paid order in the zone, which the rider (or nobody) holds.
func (l *liveFixture) order(zone, state, rider string) string {
	l.t.Helper()
	return l.makeOrder(orderSpec{customer: l.cust, zone: zone, state: state, rider: rider, ago: 10 * time.Minute})
}

func (l *liveFixture) incident(kind, order, rider, status string) {
	l.t.Helper()
	l.n++
	var o, r any
	if order != "" {
		o = order
	}
	if rider != "" {
		r = rider
	}
	closed := "NULL"
	if status == "closed" {
		closed = "now()"
	}
	assignee := "NULL"
	if status == "assigned" {
		assignee = "'" + l.opsID + "'"
	}
	l.exec(`INSERT INTO incidents (kind, source_id, order_id, rider_id, status, assigned_to, closed_at) VALUES ($1, $2, $3, $4, $5, `+assignee+`, `+closed+`)`,
		kind, fmt.Sprintf("src-%d", l.n), o, r, status)
}

// riderByName and friends pick a row out of a reply.
func pick(reply map[string]any, list, key, value string) map[string]any {
	for _, x := range reply[list].([]any) {
		if m := x.(map[string]any); m[key] == value {
			return m
		}
	}
	return nil
}

func TestEachRiderStateAndTheOneThatWins(t *testing.T) {
	l := newLiveFixture(t)
	l.onShift(l.zoneA, "Idle", "KA-IDLE")
	offered := l.onShift(l.zoneA, "Offered", "KA-OFFERED")
	onOrder := l.onShift(l.zoneA, "OnOrder", "KA-ONORDER")
	moving := l.onShift(l.zoneA, "Moving", "KA-MOVING")
	onBreak := l.onShift(l.zoneA, "OnBreak", "KA-BREAK")
	expired := l.onShift(l.zoneA, "Expired", "KA-EXPIRED")
	suggested := l.onShift(l.zoneA, "Suggested", "KA-SUGGESTED")
	released := l.onShift(l.zoneA, "Released", "KA-RELEASED")
	// Several at once: the order wins over an offer-less move and a break; an offer over a move
	// and a break; a move over a break.
	orderMoveBreak := l.onShift(l.zoneA, "OrderMoveBreak", "KA-OMB")
	offerMoveBreak := l.onShift(l.zoneA, "OfferMoveBreak", "KA-FMB")
	moveBreak := l.onShift(l.zoneA, "MoveBreak", "KA-MB")

	l.offer(offered, l.order(l.zoneA, "OFFERED", ""), "open", 30*time.Second)
	l.offer(onOrder, l.order(l.zoneA, "TIMER_RUNNING", onOrder.rider), "accepted", time.Minute)
	l.relocate(moving, "accepted")
	l.onBreak(onBreak)
	l.offer(expired, l.order(l.zoneA, "OFFERED", ""), "open", -time.Second)                 // Waiting for the sweep, no longer an offer.
	l.relocate(suggested, "suggested")                                                      // Only asked for, not moving.
	l.offer(released, l.order(l.zoneA, "COMPLETED", released.rider), "declined", time.Hour) // Over.
	l.offer(orderMoveBreak, l.order(l.zoneA, "PACKED", orderMoveBreak.rider), "accepted", time.Minute)
	l.relocate(orderMoveBreak, "accepted")
	l.onBreak(orderMoveBreak)
	l.offer(offerMoveBreak, l.order(l.zoneA, "OFFERED", ""), "open", 30*time.Second)
	l.relocate(offerMoveBreak, "accepted")
	l.onBreak(offerMoveBreak)
	l.relocate(moveBreak, "accepted")
	l.onBreak(moveBreak)

	code, out := l.poll(l.zoneA)
	if code != 200 {
		t.Fatal(code, out)
	}
	matchesContract(t, "LiveZone", out)
	want := map[string]string{"Idle": "idle", "Offered": "offered", "OnOrder": "on_order", "Moving": "moving", "OnBreak": "on_break", "Expired": "idle",
		"Suggested": "idle", "Released": "idle", "OrderMoveBreak": "on_order", "OfferMoveBreak": "offered", "MoveBreak": "moving"}
	if got := len(out["riders"].([]any)); got != len(want) {
		t.Errorf("%d riders, want %d", got, len(want))
	}
	for name, state := range want {
		r := pick(out, "riders", "name", name)
		if r == nil {
			t.Errorf("%s is missing", name)
		} else if r["state"] != state {
			t.Errorf("%s is %v, want %s", name, r["state"], state)
		}
	}
}

func TestLatestPing(t *testing.T) {
	l := newLiveFixture(t)
	a := l.onShift(l.zoneA, "Pinged", "KA-PING")
	l.onShift(l.zoneA, "Silent", "KA-SILENT")
	other := l.onShift(l.zoneB, "Elsewhere", "KB-PING")
	l.ping(a.rider, 12.9500, 77.6500, 4*time.Second)
	l.ping(a.rider, 12.9100, 77.6100, 90*time.Second)
	l.ping(a.rider, 12.9200, 77.6300, 30*time.Second)
	l.ping(other.rider, 13.5, 77.9, 0)

	code, out := l.poll(l.zoneA)
	if code != 200 {
		t.Fatal(code, out)
	}
	matchesContract(t, "LiveZone", out)
	p := pick(out, "riders", "name", "Pinged")
	if p["lat"] != 12.95 || p["lng"] != 77.65 {
		t.Errorf("not the newest ping: %v", p)
	}
	at, err := time.Parse(time.RFC3339Nano, p["last_ping_at"].(string))
	if err != nil || l.now.Sub(at) < 9*time.Second || l.now.Sub(at) > 11*time.Second {
		t.Errorf("last_ping_at %v (now %v): %v", p["last_ping_at"], l.now, err)
	}
	s := pick(out, "riders", "name", "Silent")
	if s["last_ping_at"] != nil || s["lat"] != 12.93 || s["lng"] != 77.62 || s["state"] != "idle" {
		t.Errorf("a rider with no ping: %v", s)
	}
}

func TestOtherZonesEndedShiftsAndFinishedOrdersAreLeftOut(t *testing.T) {
	l := newLiveFixture(t)
	mine := l.onShift(l.zoneA, "Mine", "KA-MINE")
	gone := l.onShift(l.zoneA, "Gone", "KA-GONE")
	l.exec("UPDATE shifts SET ended_at = now() WHERE id = $1", gone.shift)
	l.exec("UPDATE scooters SET current_rider_id = NULL WHERE id = $1", gone.scooter)
	far := l.onShift(l.zoneB, "Far", "KB-FAR")
	// Orders: the ones in progress, then one of each kind that is not.
	inProgress := map[string]string{}
	for _, st := range []string{"PAYMENT_SECURED", "OFFERED", "NO_SCOOTER", "RIDER_ACCEPTED", "PACKED", "TIMER_RUNNING", "ARRIVED"} {
		rider := ""
		if st == "TIMER_RUNNING" {
			rider = mine.rider
		}
		inProgress[st] = l.order(l.zoneA, st, rider)
	}
	notLive := map[string]string{}
	for _, st := range []string{"PAYMENT_PENDING", "PAYMENT_FAILED", "HANDED_OVER", "COMPLETED", "CANCELLED", "TIMED_OUT", "FAILED"} {
		notLive[st] = l.order(l.zoneA, st, "")
	}
	farOrder := l.order(l.zoneB, "TIMER_RUNNING", far.rider)

	code, out := l.poll(l.zoneA)
	if code != 200 {
		t.Fatal(code, out)
	}
	matchesContract(t, "LiveZone", out)
	if pick(out, "riders", "id", mine.rider) == nil || pick(out, "riders", "id", gone.rider) != nil || pick(out, "riders", "id", far.rider) != nil {
		t.Errorf("riders: %v", out["riders"])
	}
	if pick(out, "scooters", "id", mine.scooter) == nil || pick(out, "scooters", "id", far.scooter) != nil {
		t.Errorf("scooters: %v", out["scooters"])
	}
	for st, id := range inProgress {
		if o := pick(out, "orders", "id", id); o == nil || o["state"] != st {
			t.Errorf("%s order: %v", st, o)
		}
	}
	for st, id := range notLive {
		if pick(out, "orders", "id", id) != nil {
			t.Errorf("a %s order is on the map", st)
		}
	}
	if pick(out, "orders", "id", farOrder) != nil {
		t.Error("an order of another zone")
	}
	if got := len(out["orders"].([]any)); got != len(inProgress) {
		t.Errorf("%d orders, want %d", got, len(inProgress))
	}
	// The other zone sees its own.
	_, b := l.poll(l.zoneB)
	matchesContract(t, "LiveZone", b)
	if pick(b, "orders", "id", farOrder) == nil || len(b["orders"].([]any)) != 1 || len(b["riders"].([]any)) != 1 {
		t.Errorf("zone B: %v", b)
	}
}

func TestScootersAndOrdersCarryTheirDetails(t *testing.T) {
	l := newLiveFixture(t)
	rider := l.onShift(l.zoneA, "Ravi", "KA01AB1234")
	parked := l.id("INSERT INTO scooters (code, zone_id, node_id) VALUES ('KA01AB9999', $1, $2) RETURNING id::text", l.zoneA, rider.node)
	broken := l.id("INSERT INTO scooters (code, zone_id, status) VALUES ('KA01AB0000', $1, 'maintenance') RETURNING id::text", l.zoneA)
	item := l.id("INSERT INTO menu_items (name, price_paise, status) VALUES ('Chai', 4900, 'active') RETURNING id::text")
	for i, sc := range []string{rider.scooter, rider.scooter, parked} {
		lot := l.id("INSERT INTO lots (code, item_id, qty, expires_at) VALUES ($1, $2, 9, now() + interval '1 day') RETURNING id::text", fmt.Sprintf("LOT-LIVE-%d", i), item)
		l.exec("INSERT INTO scooter_stock (scooter_id, lot_id, on_hand, reserved) VALUES ($1, $2, $3, 1)", sc, lot, 3+i)
	}
	order := l.order(l.zoneA, "TIMER_RUNNING", rider.rider)
	started := l.now.Add(-40 * time.Second).Truncate(time.Microsecond)
	l.exec("UPDATE orders SET timer_started_at = $2 WHERE id = $1", order, started)
	waiting := l.order(l.zoneA, "PAYMENT_SECURED", "")

	code, out := l.poll(l.zoneA)
	if code != 200 {
		t.Fatal(code, out)
	}
	matchesContract(t, "LiveZone", out)
	s := pick(out, "scooters", "id", rider.scooter)
	if s["plate"] != "KA01AB1234" || s["node_id"] != rider.node || s["rider_id"] != rider.rider || s["stock_units"] != float64(3+4) {
		t.Errorf("riding scooter: %v", s)
	}
	p := pick(out, "scooters", "id", parked)
	if p["rider_id"] != nil || p["node_id"] != rider.node || p["stock_units"] != float64(5) {
		t.Errorf("parked scooter: %v", p)
	}
	if pick(out, "scooters", "id", broken) != nil {
		t.Error("a scooter in maintenance is on the map")
	}
	empty := pick(out, "scooters", "plate", "SC1") // makeOrder's own scooters hold nothing
	if empty != nil && empty["stock_units"] != float64(0) {
		t.Errorf("empty scooter: %v", empty)
	}
	o := pick(out, "orders", "id", order)
	gate := o["gate"].(map[string]any)
	if !regexp.MustCompile(`^[A-Z][0-9]{4}$`).MatchString(o["order_number"].(string)) || o["state"] != "TIMER_RUNNING" || o["rider_id"] != rider.rider ||
		gate["lat"] != 12.93 || gate["lng"] != 77.62 {
		t.Errorf("order: %v", o)
	}
	at, err := time.Parse(time.RFC3339Nano, o["timer_started_at"].(string))
	if err != nil || !at.Equal(started) {
		t.Errorf("timer_started_at %v, want %v: %v", o["timer_started_at"], started, err)
	}
	w := pick(out, "orders", "id", waiting)
	if w["rider_id"] != nil || w["timer_started_at"] != nil {
		t.Errorf("waiting order: %v", w)
	}
}

func TestOpenIncidentsAreCountedInTheirZone(t *testing.T) {
	l := newLiveFixture(t)
	a := l.onShift(l.zoneA, "A", "KA-A")
	b := l.onShift(l.zoneB, "B", "KB-B")
	oa, ob := l.order(l.zoneA, "TIMER_RUNNING", a.rider), l.order(l.zoneB, "TIMER_RUNNING", b.rider)
	_, empty := l.poll(l.zoneA)
	if empty["incidents_open"] != float64(0) {
		t.Errorf("none yet: %v", empty["incidents_open"])
	}
	l.incident("signal_lost", oa, "", "open")             // By its order.
	l.incident("problem_report", "", a.rider, "assigned") // By its rider, and assigned still counts.
	l.incident("ticket", oa, b.rider, "open")             // The order's zone wins over the rider's.
	l.incident("arrival_review", oa, "", "closed")        // Closed.
	l.incident("support_alert", ob, "", "open")           // Zone B.
	l.incident("ticket", "", b.rider, "open")             // Zone B.
	l.incident("ticket", "", "", "open")                  // No order and no rider: in no zone.

	_, ra := l.poll(l.zoneA)
	_, rb := l.poll(l.zoneB)
	matchesContract(t, "LiveZone", ra)
	if ra["incidents_open"] != float64(3) || rb["incidents_open"] != float64(2) {
		t.Errorf("zone A %v (want 3), zone B %v (want 2)", ra["incidents_open"], rb["incidents_open"])
	}
}

func TestAnEmptyZoneFitsTheContract(t *testing.T) {
	l := newLiveFixture(t)
	code, out := l.poll(l.zoneA)
	if code != 200 {
		t.Fatal(code, out)
	}
	matchesContract(t, "LiveZone", out)
	for _, k := range []string{"riders", "scooters", "orders"} {
		if list, ok := out[k].([]any); !ok || len(list) != 0 {
			t.Errorf("%s: %v", k, out[k])
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, out["server_time"].(string)); err != nil {
		t.Error(err)
	}
}

func TestTheLiveZoneNeedsMapRead(t *testing.T) {
	l := newLiveFixture(t)
	_, temp := l.user("live.hub", "hub_manager")
	hub := l.signIn("live.hub", temp)
	path := "/admin/zones/" + l.zoneA + "/live"
	if code, out := l.call("GET", path, hub, ""); code != 403 || out["missing_permission"] != "map.read" {
		t.Errorf("hub_manager: %d %v", code, out)
	}
	if code, _ := l.call("GET", path, "", ""); code != 401 {
		t.Errorf("no token: %d", code)
	}
	for _, role := range []string{"admin", "support"} {
		login := "live." + role
		_, temp := l.user(login, role)
		if code, out := l.call("GET", path, l.signIn(login, temp), ""); code != 200 {
			t.Errorf("%s: %d %v", role, code, out)
		}
	}
	if code, _ := l.poll(l.zoneA); code != 200 {
		t.Error("ops")
	}
}

func TestAnUnknownZoneIsNotFound(t *testing.T) {
	l := newLiveFixture(t)
	for _, id := range []string{"00000000-0000-0000-0000-000000000000", "not-an-id", "12345"} {
		l.now = l.now.Add(6 * time.Second)
		if code, out := l.call("GET", "/admin/zones/"+id+"/live", l.token, ""); code != 404 || out["code"] != "not_found" {
			t.Errorf("%s: %d %v", id, code, out)
		}
	}
}

func TestPollingFasterThanFiveSecondsIsRefused(t *testing.T) {
	l := newLiveFixture(t)
	get := func(zone string) (int, string) {
		req, _ := http.NewRequest("GET", l.srv.URL+"/v1/admin/zones/"+zone+"/live", nil)
		req.Header.Set("Authorization", "Bearer "+l.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if resp.StatusCode == 429 && body["code"] != "rate_limited" {
			t.Errorf("429 code %v", body["code"])
		}
		return resp.StatusCode, resp.Header.Get("Retry-After")
	}
	l.now = l.now.Add(time.Minute)
	if code, _ := get(l.zoneA); code != 200 {
		t.Fatal(code)
	}
	l.now = l.now.Add(2 * time.Second)
	if code, after := get(l.zoneA); code != 429 || after != "3" {
		t.Errorf("after 2 s: %d, Retry-After %q (want 429 and 3)", code, after)
	}
	if code, _ := get(l.zoneB); code != 200 {
		t.Errorf("another zone is not limited by this one: %d", code)
	}
	l.now = l.now.Add(3 * time.Second) // 5 s after the answered poll: the refused one did not restart the wait.
	if code, _ := get(l.zoneA); code != 200 {
		t.Errorf("after 5 s: %d", code)
	}
}

func TestTheLiveZoneHidesWhatStaffDoNotNeed(t *testing.T) {
	l := newLiveFixture(t)
	s := l.onShift(l.zoneA, "Ravi", "KA-PRIVATE")
	l.ping(s.rider, 12.9355, 77.624, time.Second)
	l.offer(s, l.order(l.zoneA, "TIMER_RUNNING", s.rider), "accepted", time.Minute)
	l.exec("UPDATE orders SET handover_otp = '4821'")
	l.incident("signal_lost", l.order(l.zoneA, "PACKED", s.rider), "", "open")
	var riderPhone string
	if err := l.pool.QueryRow(context.Background(), "SELECT p.phone FROM riders r JOIN people p ON p.id = r.person_id WHERE r.id = $1", s.rider).Scan(&riderPhone); err != nil {
		t.Fatal(err)
	}
	code, out := l.poll(l.zoneA)
	if code != 200 {
		t.Fatal(code, out)
	}
	matchesContract(t, "LiveZone", out)
	raw, _ := json.Marshal(out)
	for _, secret := range []string{"9800007777", riderPhone, strings.TrimPrefix(riderPhone, "+91"), "+91", "Flat 4B", "ring the bell", "Tower", "4821", "handover_otp", "entry_notes", "phone", "customer", "label"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the reply holds %q: %s", secret, raw)
		}
	}
	// Exactly the contract's fields, no more.
	keys := func(m map[string]any) string {
		var ks []string
		for k := range m {
			ks = append(ks, k)
		}
		return strings.Join(sortStrings(ks), ",")
	}
	if got := keys(out["riders"].([]any)[0].(map[string]any)); got != "id,last_ping_at,lat,lng,name,state" {
		t.Errorf("rider fields %s", got)
	}
	if got := keys(out["orders"].([]any)[0].(map[string]any)); got != "gate,id,order_number,rider_id,state,timer_started_at" {
		t.Errorf("order fields %s", got)
	}
	if got := keys(out["orders"].([]any)[0].(map[string]any)["gate"].(map[string]any)); got != "lat,lng" {
		t.Errorf("gate fields %s", got)
	}
	if got := keys(out["scooters"].([]any)[0].(map[string]any)); got != "id,node_id,plate,rider_id,stock_units" {
		t.Errorf("scooter fields %s", got)
	}
}

func sortStrings(s []string) []string {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
	return s
}

// counting counts the statements a read sends.
type counting struct {
	q reader
	n int
}

func (c *counting) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.n++
	return c.q.QueryRow(ctx, sql, args...)
}

func (c *counting) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.n++
	return c.q.Query(ctx, sql, args...)
}

func TestTheLiveZoneIsAFewQueriesHoweverBigTheZone(t *testing.T) {
	l := newLiveFixture(t)
	count := func() int {
		c := &counting{q: l.pool}
		if _, err := LiveZone(context.Background(), c, l.zoneA, l.now); err != nil {
			t.Fatal(err)
		}
		return c.n
	}
	small := count()
	for i := 0; i < 12; i++ {
		s := l.onShift(l.zoneA, fmt.Sprintf("R%d", i), fmt.Sprintf("KA-%02d", i))
		l.ping(s.rider, 12.93, 77.62, time.Second)
		l.order(l.zoneA, "TIMER_RUNNING", s.rider)
	}
	if big := count(); small != 4 || big != 4 {
		t.Errorf("%d statements with an empty zone and %d with a busy one, want 4 each", small, big)
	}
}

func TestTheIndexesTheLiveZoneReadsAreUsed(t *testing.T) {
	l := newLiveFixture(t)
	s := l.onShift(l.zoneA, "Idx", "KA-IDX")
	l.ping(s.rider, 12.93, 77.62, time.Second)
	l.order(l.zoneA, "TIMER_RUNNING", s.rider)
	// With a seq scan made as dear as possible, a plan that still names the index proves it can
	// answer the query; a missing index, or one whose condition the query does not imply, cannot.
	explain := func(sql string, args ...any) string {
		var plan []string
		err := pgx.BeginFunc(context.Background(), l.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(context.Background(), "SET LOCAL enable_seqscan = off"); err != nil {
				return err
			}
			rows, err := tx.Query(context.Background(), "EXPLAIN "+sql, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					return err
				}
				plan = append(plan, line)
			}
			return rows.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(plan, "\n")
	}
	if plan := explain(liveOrdersSQL, l.zoneA); !strings.Contains(plan, "orders_live_zone_idx") {
		t.Errorf("orders are not read by orders_live_zone_idx:\n%s", plan)
	}
	if plan := explain(liveRidersSQL, l.zoneA, l.now); !strings.Contains(plan, "location_pings_rider_idx") {
		t.Errorf("the latest ping is not read by location_pings_rider_idx:\n%s", plan)
	}
}

// A rider still on shift on a scooter put into maintenance keeps their scooter on the map, so ops
// never see a rider without one (#150 review).
func TestAScooterInMaintenanceWithARiderOnShiftStaysOnTheMap(t *testing.T) {
	l := newLiveFixture(t)
	rider := l.onShift(l.zoneA, "Ravi", "KA01AB1234")
	l.exec("UPDATE scooters SET status = 'maintenance' WHERE id = $1", rider.scooter)
	code, out := l.poll(l.zoneA)
	if code != 200 {
		t.Fatal(code, out)
	}
	if pick(out, "riders", "id", rider.rider) == nil {
		t.Fatal("the rider is not on the map")
	}
	if s := pick(out, "scooters", "id", rider.scooter); s == nil || s["rider_id"] != rider.rider {
		t.Errorf("the rider's scooter in maintenance: %v", s)
	}
}
