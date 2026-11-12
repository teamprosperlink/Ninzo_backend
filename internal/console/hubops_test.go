package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

// Hub operations (issue 149): listHubOps, listStuckRestocks and expireRestock.

// opsDesk is two hubs in two zones, a manager for each, and the roles that may and may not look.
type opsDesk struct {
	f                               *fixture
	admin, ops, support, mgrA, mgrB string
	zoneA, zoneB, hubA, hubB        string
	chai                            string
	n                               int
}

func newOpsDesk(t *testing.T) *opsDesk {
	f := newFixture(t)
	d := &opsDesk{f: f}
	// This file's calls are mounted on a server of their own, so the shared fixture stays as it was.
	sessions := f.svc.Sessions
	f.srv.Close()
	f.srv = httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		signedIn := identity.RequireActor(sessions.Signer, identity.TypeConsole)
		f.svc.MountAuth(r, signedIn)
		f.svc.MountHubOps(r, signedIn)
	}))
	t.Cleanup(f.srv.Close)
	signIn := func(login, role string) (string, string) {
		id, temp := f.user(login, role)
		return id, f.signIn(login, temp)
	}
	_, d.admin = signIn("ops.admin", "admin")
	_, d.ops = signIn("ops.ops", "ops")
	_, d.support = signIn("ops.sup", "support")
	_, d.mgrA = signIn("ops.mgra", "hub_manager")
	_, d.mgrB = signIn("ops.mgrb", "hub_manager")
	f.pool.QueryRow(context.Background(), "SELECT h.id::text, h.zone_id::text FROM console_users u JOIN hubs h ON h.id = u.hub_id WHERE u.login_id = 'ops.mgra'").Scan(&d.hubA, &d.zoneA)
	f.pool.QueryRow(context.Background(), "SELECT h.id::text, h.zone_id::text FROM console_users u JOIN hubs h ON h.id = u.hub_id WHERE u.login_id = 'ops.mgrb'").Scan(&d.hubB, &d.zoneB)
	d.chai = f.id("INSERT INTO menu_items (name, price_paise, status) VALUES ('Chai', 4900, 'active') RETURNING id::text")
	return d
}

// request adds a restock request at a hub in a state, whose state changed `ago` before now. It has
// its own rider and scooter, as a scooter has one open request.
func (d *opsDesk) request(hub, state string, ago time.Duration) string {
	d.f.t.Helper()
	d.n++
	zone := d.zoneA
	if hub == d.hubB {
		zone = d.zoneB
	}
	rider := d.f.courier(fmt.Sprintf("+9198100%05d", d.n), fmt.Sprintf("Rider%d", d.n))
	scooter := d.f.id("INSERT INTO scooters (code, zone_id) VALUES ($1, $2) RETURNING id::text", fmt.Sprintf("OPS-%d", d.n), zone)
	return d.f.id(`INSERT INTO restock_requests (rider_id, scooter_id, hub_id, source, state, state_changed_at, created_at) VALUES ($1, $2, $3, 'manual', $4, $5::timestamptz, $5::timestamptz - interval '1 minute')
		RETURNING id::text`, rider, scooter, hub, state, d.f.now.Add(-ago))
}

func (d *opsDesk) list(token, query string) (int, map[string]any) {
	return d.f.call("GET", "/admin/hub-ops"+query, token, "")
}

func (d *opsDesk) stuck(token, query string) (int, map[string]any) {
	return d.f.call("GET", "/admin/hub-ops/stuck"+query, token, "")
}

func (d *opsDesk) ops4(out map[string]any, hub string) map[string]any {
	d.f.t.Helper()
	for _, h := range out["hubs"].([]any) {
		if h.(map[string]any)["hub"].(map[string]any)["id"] == hub {
			return h.(map[string]any)
		}
	}
	d.f.t.Fatalf("hub %s is not in %v", hub, out)
	return nil
}

func idsOf(list []any) []string {
	out := []string{}
	for _, x := range list {
		out = append(out, x.(map[string]any)["id"].(string))
	}
	return out
}

func (d *opsDesk) seqs(request string) (rider, hub, own int) {
	f := d.f
	f.pool.QueryRow(context.Background(), `SELECT coalesce((SELECT seq FROM rider_state WHERE rider_id = q.rider_id), 0)::int, coalesce((SELECT seq FROM hub_state WHERE hub_id = q.hub_id), 0)::int, q.seq::int
		FROM restock_requests q WHERE q.id = $1`, request).Scan(&rider, &hub, &own)
	return
}

func TestHubOpsCountsEachStateAndWhatSitsAroundIt(t *testing.T) {
	d := newOpsDesk(t)
	f := d.f
	for _, s := range []struct {
		state string
		n     int
	}{{"confirmed", 2}, {"hub_accepted", 1}, {"packing", 3}, {"partial", 1}, {"ready", 2}, {"picked_up", 1}, {"expired", 4}, {"cancelled", 1}, {"received", 1}} {
		for i := 0; i < s.n; i++ {
			id := d.request(d.hubA, s.state, time.Minute)
			if s.state == "partial" {
				f.exec("INSERT INTO shortfalls (request_id, action) VALUES ($1, 'partial')", id)
			}
		}
	}
	d.request(d.hubB, "packing", time.Minute) // Another hub's work is not hub A's.
	// A decided shortfall is not open.
	f.exec("INSERT INTO shortfalls (request_id, action, status, decided_at) VALUES ($1, 'partial', 'accepted', now())", d.request(d.hubA, "ready", time.Minute))
	f.exec("INSERT INTO lots (code, item_id, qty, expires_at, hub_id, status) VALUES ('Q-A-1', $1, 5, now() + interval '1 day', $2, 'quarantined'), ('Q-A-2', $1, 5, now() + interval '1 day', $2, 'quarantined'), ('OK-A-1', $1, 5, now() + interval '1 day', $2, 'ok'), ('Q-B-1', $1, 5, now() + interval '1 day', $3, 'quarantined')",
		d.chai, d.hubA, d.hubB)
	for i, status := range []string{"active", "active", "suspended"} {
		person := f.id("INSERT INTO people (phone) VALUES ($1) RETURNING id::text", fmt.Sprintf("+9198200000%02d", i))
		f.exec("INSERT INTO hub_staff (person_id, hub_id, role, status, name) VALUES ($1, $2, 'hub_staff', $3, 'Staff')", person, d.hubA, status)
	}
	code, out := d.list(d.admin, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	matchesContract(t, "HubOpsList", out)
	if len(out["hubs"].([]any)) != 2 {
		t.Fatalf("an admin sees every hub: %v", out)
	}
	a := d.ops4(out, d.hubA)
	queue := a["queue"].(map[string]any)
	want := map[string]float64{"confirmed": 2, "hub_accepted": 1, "packing": 3, "partial": 1, "ready": 3, "picked_up": 1}
	for state, n := range want {
		if queue[state] != n {
			t.Errorf("%s: %v, want %v", state, queue[state], n)
		}
	}
	if a["open_shortfalls"] != float64(1) || a["quarantined_lots"] != float64(2) || a["active_staff"] != float64(2) || a["stuck"] != float64(0) {
		t.Errorf("%v", a)
	}
	hub := a["hub"].(map[string]any)
	if hub["id"] != d.hubA || hub["zone_id"] != d.zoneA || hub["name"] != "Hub" {
		t.Errorf("%v", hub)
	}
	b := d.ops4(out, d.hubB)
	if b["queue"].(map[string]any)["packing"] != float64(1) || b["quarantined_lots"] != float64(1) || b["open_shortfalls"] != float64(0) || b["active_staff"] != float64(0) {
		t.Errorf("%v", b)
	}
	// ops reads the same; support has no hub_ops.read.
	if code, out := d.list(d.ops, ""); code != 200 || len(out["hubs"].([]any)) != 2 {
		t.Errorf("ops: %d %v", code, out)
	}
}

func TestAHubWithNothingToDoStillShowsWithZeroes(t *testing.T) {
	d := newOpsDesk(t)
	_, out := d.list(d.admin, "")
	matchesContract(t, "HubOpsList", out)
	a := d.ops4(out, d.hubA)
	for _, v := range a["queue"].(map[string]any) {
		if v != float64(0) {
			t.Errorf("%v", a)
		}
	}
	if a["stuck"] != float64(0) || a["open_shortfalls"] != float64(0) || a["quarantined_lots"] != float64(0) || a["active_staff"] != float64(0) {
		t.Errorf("%v", a)
	}
}

func TestStuckStartsPastTheSettingAndOnlyInTheStatesThatWaitOnTheHub(t *testing.T) {
	d := newOpsDesk(t)
	f := d.f
	// The default is 600 seconds. Waiting exactly that long is not yet longer than it.
	under := d.request(d.hubA, "confirmed", 599*time.Second)
	exact := d.request(d.hubA, "hub_accepted", 600*time.Second)
	over := d.request(d.hubA, "packing", 601*time.Second)
	d.request(d.hubA, "partial", 5*time.Hour) // Waits on the rider, not the hub.
	d.request(d.hubA, "ready", 5*time.Hour)
	d.request(d.hubA, "picked_up", 5*time.Hour)
	d.request(d.hubA, "expired", 5*time.Hour)
	_, out := d.list(d.admin, "")
	if got := d.ops4(out, d.hubA)["stuck"]; got != float64(1) {
		t.Errorf("stuck: %v", got)
	}
	code, list := d.stuck(d.admin, "")
	if code != 200 {
		t.Fatalf("%d %v", code, list)
	}
	matchesContract(t, "RestockList", list)
	if got := idsOf(list["requests"].([]any)); len(got) != 1 || got[0] != over {
		t.Errorf("%v, want only %s (not %s or %s)", got, over, under, exact)
	}
	// One second on, the request that waited exactly the limit is stuck.
	f.now = f.now.Add(time.Second)
	_, list = d.stuck(d.admin, "")
	if got := idsOf(list["requests"].([]any)); len(got) != 2 || got[0] != over || got[1] != exact {
		t.Errorf("longest first: %v", got)
	}
	r := list["requests"].([]any)[0].(map[string]any)
	if r["waiting_s"] != float64(602) || r["state"] != "packing" || r["hub_id"] != d.hubA || r["ended_reason"] != nil {
		t.Errorf("%v", r)
	}
	if name := r["rider"].(map[string]any)["name"].(string); !strings.HasPrefix(name, "Rider") {
		t.Errorf("rider: %v", r["rider"])
	}
	// The count and the list agree.
	_, out = d.list(d.admin, "")
	if got := d.ops4(out, d.hubA)["stuck"]; got != float64(2) {
		t.Errorf("stuck after a second: %v", got)
	}
}

func TestTheStuckSettingCanDifferByZone(t *testing.T) {
	d := newOpsDesk(t)
	f := d.f
	inA := d.request(d.hubA, "confirmed", 200*time.Second)
	inB := d.request(d.hubB, "confirmed", 200*time.Second)
	if _, list := d.stuck(d.admin, ""); len(list["requests"].([]any)) != 0 {
		t.Fatalf("200 seconds is not stuck anywhere yet: %v", list)
	}
	// Zone A's override: 120 seconds.
	set := func(zone string, v int) {
		tx, _ := f.pool.Begin(context.Background())
		if _, err := geo.ChangeSettings(context.Background(), tx, zone, []geo.Change{{Name: "restock_stuck_s", Value: &v}}, "test", "owner"); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	set(d.zoneA, 120)
	_, list := d.stuck(d.admin, "")
	if got := idsOf(list["requests"].([]any)); len(got) != 1 || got[0] != inA {
		t.Errorf("only zone A's: %v", got)
	}
	_, out := d.list(d.admin, "")
	if d.ops4(out, d.hubA)["stuck"] != float64(1) || d.ops4(out, d.hubB)["stuck"] != float64(0) {
		t.Errorf("the count follows the zone: %v", out)
	}
	// The global value moves zone B, which has no override of its own; zone A keeps its own.
	set("", 100)
	_, list = d.stuck(d.admin, "")
	if got := idsOf(list["requests"].([]any)); len(got) != 2 {
		t.Errorf("both: %v (%s, %s)", got, inA, inB)
	}
	set("", 300)
	_, list = d.stuck(d.admin, "")
	if got := idsOf(list["requests"].([]any)); len(got) != 1 || got[0] != inA {
		t.Errorf("zone B back under its limit, zone A still on 120: %v", got)
	}
	// The override cleared, zone A follows the global value again.
	tx, _ := f.pool.Begin(context.Background())
	if _, err := geo.ChangeSettings(context.Background(), tx, d.zoneA, []geo.Change{{Name: "restock_stuck_s"}}, "test", "owner"); err != nil {
		t.Fatal(err)
	}
	tx.Commit(context.Background())
	if _, list = d.stuck(d.admin, ""); len(list["requests"].([]any)) != 0 {
		t.Errorf("%v", list)
	}
}

func TestHubOpsFiltersByZone(t *testing.T) {
	d := newOpsDesk(t)
	d.request(d.hubB, "packing", time.Minute)
	code, out := d.list(d.admin, "?zone_id="+d.zoneB)
	if code != 200 || len(out["hubs"].([]any)) != 1 || d.ops4(out, d.hubB)["queue"].(map[string]any)["packing"] != float64(1) {
		t.Errorf("%d %v", code, out)
	}
	matchesContract(t, "HubOpsList", out)
	if _, out := d.list(d.ops, "?zone_id=00000000-0000-0000-0000-000000000001"); len(out["hubs"].([]any)) != 0 {
		t.Errorf("an unknown zone has no hubs: %v", out)
	}
	for _, bad := range []string{"?zone_id=nope", "?zone_id=1234"} {
		if code, out := d.list(d.admin, bad); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", bad, code, out)
		}
	}
}

func TestAHubManagerSeesOnlyTheirOwnHubOnBothLists(t *testing.T) {
	d := newOpsDesk(t)
	mineStuck := d.request(d.hubA, "packing", time.Hour)
	theirsStuck := d.request(d.hubB, "packing", 2*time.Hour)
	d.request(d.hubB, "ready", time.Minute)
	code, out := d.list(d.mgrA, "")
	if code != 200 || len(out["hubs"].([]any)) != 1 || d.ops4(out, d.hubA)["stuck"] != float64(1) {
		t.Fatalf("%d %v", code, out)
	}
	matchesContract(t, "HubOpsList", out)
	// The other hub's zone gives nothing, not an error that tells it exists.
	if _, out := d.list(d.mgrA, "?zone_id="+d.zoneB); len(out["hubs"].([]any)) != 0 {
		t.Errorf("%v", out)
	}
	if _, out := d.list(d.mgrA, "?zone_id="+d.zoneA); len(out["hubs"].([]any)) != 1 {
		t.Errorf("%v", out)
	}
	code, list := d.stuck(d.mgrA, "")
	if got := idsOf(list["requests"].([]any)); code != 200 || len(got) != 1 || got[0] != mineStuck {
		t.Errorf("%d %v, want only %s and not %s", code, got, mineStuck, theirsStuck)
	}
	matchesContract(t, "RestockList", list)
	if _, list := d.stuck(d.mgrB, ""); len(list["requests"].([]any)) != 1 || list["requests"].([]any)[0].(map[string]any)["id"] != theirsStuck {
		t.Errorf("%v", list)
	}
	// Paging does not leak the other hub either.
	_, p1 := d.stuck(d.mgrA, "?limit=1")
	if p1["next_cursor"] != nil {
		t.Errorf("one request is one page: %v", p1)
	}
	// A manager cannot expire, their own hub's request included.
	code, out = d.f.call("POST", "/admin/restock/"+mineStuck+"/expire", d.mgrA, `{"reason":"stuck for an hour"}`)
	if code != 403 || out["missing_permission"] != "hub_ops.manage" {
		t.Errorf("%d %v", code, out)
	}
	if d.f.count("SELECT count(*) FROM restock_requests WHERE state = 'expired'") != 0 {
		t.Error("a refused call changed a request")
	}
}

func TestHubOpsPermissions(t *testing.T) {
	d := newOpsDesk(t)
	id := d.request(d.hubA, "packing", time.Hour)
	for _, who := range []struct {
		name, token string
		read, mng   bool
	}{{"admin", d.admin, true, true}, {"ops", d.ops, true, true}, {"hub_manager", d.mgrA, true, false}, {"support", d.support, false, false}} {
		for _, path := range []string{"/admin/hub-ops", "/admin/hub-ops/stuck"} {
			code, out := d.f.call("GET", path, who.token, "")
			if who.read && code != 200 || !who.read && (code != 403 || out["missing_permission"] != "hub_ops.read") {
				t.Errorf("%s %s: %d %v", who.name, path, code, out)
			}
		}
		if !who.mng {
			code, out := d.f.call("POST", "/admin/restock/"+id+"/expire", who.token, `{"reason":"not allowed to"}`)
			if code != 403 || out["missing_permission"] != "hub_ops.manage" {
				t.Errorf("%s expire: %d %v", who.name, code, out)
			}
		}
	}
	for _, path := range []string{"/admin/hub-ops", "/admin/hub-ops/stuck"} {
		if code, _ := d.f.call("GET", path, "", ""); code != 401 {
			t.Errorf("%s with no sign-in: %d", path, code)
		}
	}
	if code, _ := d.f.call("POST", "/admin/restock/"+id+"/expire", "", `{"reason":"anyone"}`); code != 401 {
		t.Errorf("expire with no sign-in: %d", code)
	}
	if d.f.state(id) != "packing" {
		t.Error("a refused call changed the request")
	}
}

func (f *fixture) state(id string) string {
	f.t.Helper()
	return f.id("SELECT state FROM restock_requests WHERE id = $1", id)
}

func TestTheStuckListPagesLongestFirst(t *testing.T) {
	d := newOpsDesk(t)
	oldest := d.request(d.hubA, "confirmed", 5*time.Hour)
	middle := d.request(d.hubB, "packing", 3*time.Hour)
	newest := d.request(d.hubA, "hub_accepted", time.Hour)
	_, p1 := d.stuck(d.ops, "?limit=2")
	if got := idsOf(p1["requests"].([]any)); len(got) != 2 || got[0] != oldest || got[1] != middle || p1["next_cursor"] == nil {
		t.Fatalf("%v %v", got, p1["next_cursor"])
	}
	_, p2 := d.stuck(d.ops, "?limit=2&cursor="+p1["next_cursor"].(string))
	if got := idsOf(p2["requests"].([]any)); len(got) != 1 || got[0] != newest || p2["next_cursor"] != nil {
		t.Fatalf("%v %v", got, p2["next_cursor"])
	}
	matchesContract(t, "RestockList", p2)
	for _, bad := range []string{"?limit=0", "?limit=101", "?limit=x", "?cursor=!!", "?cursor=" + Cursor(time.Now(), "not-an-id")} {
		if code, out := d.stuck(d.ops, bad); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", bad, code, out)
		}
	}
	// Requests with the same moment still page without gaps or repeats.
	for i := 0; i < 3; i++ {
		d.request(d.hubA, "packing", 10*time.Hour)
	}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 10; page++ {
		_, out := d.stuck(d.ops, "?limit=2&cursor="+cursor)
		for _, id := range idsOf(out["requests"].([]any)) {
			if seen[id] {
				t.Errorf("%s twice", id)
			}
			seen[id] = true
		}
		next, _ := out["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 6 {
		t.Errorf("%d requests seen, want 6", len(seen))
	}
}

// opsNoise is everything these three calls must never carry: a phone number or a customer.
func assertNoPersonalData(t *testing.T, label string, out map[string]any, phones ...string) {
	t.Helper()
	raw, _ := json.Marshal(out)
	text := string(raw)
	for _, p := range append(phones, "+91", "phone", "customer", "address", "entry_notes") {
		if strings.Contains(text, p) {
			t.Errorf("%s holds %q: %s", label, p, text)
		}
	}
}

func TestHubOpsCarryNoPhoneNumbersOrCustomers(t *testing.T) {
	d := newOpsDesk(t)
	id := d.request(d.hubA, "packing", time.Hour)
	person := d.f.id("INSERT INTO people (phone) VALUES ('+919877700001') RETURNING id::text")
	d.f.exec("INSERT INTO hub_staff (person_id, hub_id, role, name) VALUES ($1, $2, 'hub_lead', 'Lead')", person, d.hubA)
	_, hubs := d.list(d.admin, "")
	assertNoPersonalData(t, "hub-ops", hubs, "9198100", "98777")
	_, stuck := d.stuck(d.admin, "")
	if len(stuck["requests"].([]any)) != 1 {
		t.Fatal(stuck)
	}
	assertNoPersonalData(t, "stuck", stuck, "9198100", "98777")
	_, expired := d.f.call("POST", "/admin/restock/"+id+"/expire", d.admin, `{"reason":"nobody is packing it"}`)
	assertNoPersonalData(t, "expire", expired, "9198100", "98777")
	if expired["restock"].(map[string]any)["rider"].(map[string]any)["name"] == "" {
		t.Error("the rider's name is part of the contract")
	}
}

// packed gives a request a packed lot and an open shortfall, the things an expiry must undo.
func (d *opsDesk) packed(request string) (lot string) {
	d.n++
	lot = d.f.id("INSERT INTO lots (code, item_id, qty, expires_at, hub_id) VALUES ($1, $2, 5, now() + interval '1 day', $3) RETURNING id::text", fmt.Sprintf("PK-%d", d.n), d.chai, d.hubA)
	d.f.exec("INSERT INTO restock_lots (request_id, lot_id) VALUES ($1, $2)", request, lot)
	return lot
}

func TestExpiringAStuckRequestEndsItAsAnExpiryDoes(t *testing.T) {
	d := newOpsDesk(t)
	f := d.f
	id := d.request(d.hubA, "packing", time.Hour)
	d.packed(id)
	f.exec("INSERT INTO shortfalls (request_id, action) VALUES ($1, 'partial')", id)
	riderBefore, hubBefore, ownBefore := d.seqs(id)
	pushes := f.count("SELECT count(*) FROM outbox_events WHERE topic = 'push.rider'")
	code, out := f.call("POST", "/admin/restock/"+id+"/expire", d.ops, `{"reason":"  the hub has gone home  "}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	matchesContract(t, "RestockResponse", out)
	r := out["restock"].(map[string]any)
	if r["id"] != id || r["state"] != "expired" || r["ended_reason"] != "expired_by_console" || r["hub_id"] != d.hubA || r["waiting_s"] != float64(0) {
		t.Errorf("%v", r)
	}
	if f.state(id) != "expired" {
		t.Error("not expired")
	}
	if n := f.count("SELECT count(*) FROM restock_lots WHERE request_id = $1 AND released_at IS NULL", id); n != 0 {
		t.Error("what was packed did not go back to the hub's stock")
	}
	if n := f.count("SELECT count(*) FROM shortfalls WHERE request_id = $1 AND status = 'refused' AND decided_at IS NOT NULL", id); n != 1 {
		t.Error("the open shortfall was left open")
	}
	riderAfter, hubAfter, ownAfter := d.seqs(id)
	if riderAfter != riderBefore+1 || hubAfter != hubBefore+1 || ownAfter != ownBefore+1 {
		t.Errorf("seqs: rider %d->%d, hub %d->%d, request %d->%d", riderBefore, riderAfter, hubBefore, hubAfter, ownBefore, ownAfter)
	}
	if n := f.count("SELECT count(*) FROM outbox_events WHERE topic = 'push.rider'"); n != pushes+1 {
		t.Errorf("%d rider pushes", n-pushes)
	}
	if n := f.count("SELECT count(*) FROM rider_notifications WHERE kind = 'restock' AND title = 'Restock ended' AND rider_id = (SELECT rider_id FROM restock_requests WHERE id = $1)", id); n != 1 {
		t.Errorf("%d rider notifications", n)
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'restock.expired_by_console' AND entity_id = $1 AND reason = 'the hub has gone home' AND actor_type = 'console'", id); n != 1 {
		t.Errorf("%d audit rows", n)
	}
	// The scooter is free to ask again.
	if n := f.count("SELECT count(*) FROM restock_requests q WHERE q.id = $1 AND q.state IN ('confirmed', 'hub_accepted', 'packing', 'ready', 'picked_up', 'partial')", id); n != 0 {
		t.Error("still open")
	}
	// It is out of the stuck list and the queue.
	if _, list := d.stuck(d.admin, ""); len(list["requests"].([]any)) != 0 {
		t.Errorf("%v", list)
	}
	_, hubs := d.list(d.admin, "")
	if d.ops4(hubs, d.hubA)["queue"].(map[string]any)["packing"] != float64(0) || d.ops4(hubs, d.hubA)["open_shortfalls"] != float64(0) {
		t.Errorf("%v", hubs)
	}
}

func TestEveryStateBeforeReadyCanBeExpiredAndNoOtherCan(t *testing.T) {
	d := newOpsDesk(t)
	f := d.f
	for _, state := range []string{"confirmed", "hub_accepted", "packing", "partial"} {
		// Not stuck at all: the contract lets these four be ended whatever they have waited.
		id := d.request(d.hubA, state, time.Second)
		if code, out := f.call("POST", "/admin/restock/"+id+"/expire", d.admin, `{"reason":"ended by hand"}`); code != 200 || f.state(id) != "expired" {
			t.Errorf("%s: %d %v", state, code, out)
		}
	}
	for _, state := range []string{"ready", "picked_up", "received", "closed", "expired", "cancelled", "suggested"} {
		id := d.request(d.hubA, state, time.Hour)
		riderBefore, hubBefore, ownBefore := d.seqs(id)
		audits := f.count("SELECT count(*) FROM audit_events")
		code, out := f.call("POST", "/admin/restock/"+id+"/expire", d.admin, `{"reason":"too late for this"}`)
		if code != 409 || out["code"] != "restock_not_expirable" {
			t.Errorf("%s: %d %v", state, code, out)
		}
		riderAfter, hubAfter, ownAfter := d.seqs(id)
		if f.state(id) != state || riderAfter != riderBefore || hubAfter != hubBefore || ownAfter != ownBefore || f.count("SELECT count(*) FROM audit_events") != audits {
			t.Errorf("%s: a refusal changed something", state)
		}
	}
}

func TestExpiringASuggestedRequestThatWasConfirmedStillMovesTheHubSeq(t *testing.T) {
	d := newOpsDesk(t)
	id := d.request(d.hubA, "confirmed", time.Hour)
	d.f.exec("UPDATE restock_requests SET source = 'suggested' WHERE id = $1", id)
	_, hubBefore, _ := d.seqs(id)
	if code, out := d.f.call("POST", "/admin/restock/"+id+"/expire", d.admin, `{"reason":"nobody took it"}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if _, hubAfter, _ := d.seqs(id); hubAfter != hubBefore+1 {
		t.Errorf("hub seq %d -> %d: the hub saw this request while it was confirmed", hubBefore, hubAfter)
	}
}

func TestExpireRefusesWhatTheContractRefuses(t *testing.T) {
	d := newOpsDesk(t)
	f := d.f
	id := d.request(d.hubA, "packing", time.Hour)
	for _, c := range []struct{ name, body string }{
		{"no reason", `{}`}, {"short reason", `{"reason":"ab"}`}, {"blank reason", `{"reason":"     "}`},
		{"long reason", `{"reason":"` + strings.Repeat("x", 501) + `"}`}, {"not json", `nope`}, {"two bodies", `{"reason":"fine reason"} {}`},
		{"empty", ``},
	} {
		if code, out := f.call("POST", "/admin/restock/"+id+"/expire", d.admin, c.body); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", c.name, code, out)
		}
	}
	// A reason of exactly 3 and exactly 500 characters passes.
	other := d.request(d.hubA, "packing", time.Hour)
	if code, out := f.call("POST", "/admin/restock/"+other+"/expire", d.admin, `{"reason":"abc"}`); code != 200 {
		t.Errorf("3 characters: %d %v", code, out)
	}
	last := d.request(d.hubA, "packing", time.Hour)
	if code, out := f.call("POST", "/admin/restock/"+last+"/expire", d.admin, `{"reason":"`+strings.Repeat("é", 500)+`"}`); code != 200 {
		t.Errorf("500 characters: %d %v", code, out)
	}
	if f.state(id) != "packing" {
		t.Error("a refused call changed the request")
	}
	for _, missing := range []string{"00000000-0000-0000-0000-000000000009", "not-an-id"} {
		if code, out := f.call("POST", "/admin/restock/"+missing+"/expire", d.admin, `{"reason":"does not exist"}`); code != 404 || out["code"] != "not_found" {
			t.Errorf("%s: %d %v", missing, code, out)
		}
	}
	// No Idempotency-Key.
	if code, out := f.callKey("POST", "/admin/restock/"+id+"/expire", d.admin, "", `{"reason":"no key given"}`); code != 400 || out["code"] != "idempotency_key_required" {
		t.Errorf("%d %v", code, out)
	}
	if f.state(id) != "packing" {
		t.Error("a call with no key changed the request")
	}
}

func TestExpiringTwiceReplaysOrIsRefusedAndNeverRunsTwice(t *testing.T) {
	d := newOpsDesk(t)
	f := d.f
	id := d.request(d.hubA, "packing", time.Hour)
	body := `{"reason":"the hub has gone home"}`
	code1, first := f.callKey("POST", "/admin/restock/"+id+"/expire", d.admin, "expire-key-0001", body)
	riderAfter, hubAfter, ownAfter := d.seqs(id)
	code2, second := f.callKey("POST", "/admin/restock/"+id+"/expire", d.admin, "expire-key-0001", body)
	if code1 != 200 || code2 != 200 {
		t.Fatalf("%d %d %v", code1, code2, second)
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Errorf("a replay answers the same:\n%s\n%s", a, b)
	}
	if r, h, o := d.seqs(id); r != riderAfter || h != hubAfter || o != ownAfter {
		t.Error("a replay raised a seq")
	}
	if f.count("SELECT count(*) FROM audit_events WHERE action = 'restock.expired_by_console'") != 1 || f.count("SELECT count(*) FROM rider_notifications WHERE title = 'Restock ended'") != 1 {
		t.Error("a replay did the work again")
	}
	// The same key with another body is a mistake, not a replay.
	if code, out := f.callKey("POST", "/admin/restock/"+id+"/expire", d.admin, "expire-key-0001", `{"reason":"a different reason"}`); code != 422 {
		t.Errorf("%d %v", code, out)
	}
	// A new key finds the request already ended.
	if code, out := f.callKey("POST", "/admin/restock/"+id+"/expire", d.admin, "expire-key-0002", body); code != 409 || out["code"] != "restock_not_expirable" {
		t.Errorf("%d %v", code, out)
	}
	if r, _, _ := d.seqs(id); r != riderAfter {
		t.Error("the refusal raised a seq")
	}
}

func TestTwoAtOnceExpireARequestOnce(t *testing.T) {
	d := newOpsDesk(t)
	f := d.f
	id := d.request(d.hubA, "packing", time.Hour)
	d.packed(id)
	riderBefore, hubBefore, _ := d.seqs(id)
	var wg sync.WaitGroup
	// One person's calls already take turns (the session check locks them), so the callers are
	// eight different people.
	tokens := []string{d.admin, d.ops}
	for i := 0; i < 6; i++ {
		login := fmt.Sprintf("race.ops%d", i)
		_, temp := f.user(login, "ops")
		tokens = append(tokens, f.signIn(login, temp))
	}
	codes := make([]int, len(tokens))
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token := tokens[i]
			codes[i], _ = doCall(t, f.srv.URL, "POST", "/admin/restock/"+id+"/expire", token, fmt.Sprintf("race-key-%04d", i), `{"reason":"both of us saw it"}`)
		}()
	}
	wg.Wait()
	ok, refused := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			refused++
		default:
			t.Errorf("code %d", c)
		}
	}
	if ok != 1 || refused != len(codes)-1 {
		t.Errorf("%v: one call wins and the rest are refused", codes)
	}
	riderAfter, hubAfter, _ := d.seqs(id)
	if riderAfter != riderBefore+1 || hubAfter != hubBefore+1 || f.count("SELECT count(*) FROM audit_events WHERE action = 'restock.expired_by_console'") != 1 {
		t.Errorf("it ran more than once: rider %d->%d hub %d->%d", riderBefore, riderAfter, hubBefore, hubAfter)
	}
}

func TestTheRoleTableGivesHubOpsToTheRightRoles(t *testing.T) {
	for role, want := range map[string][2]bool{"admin": {true, true}, "ops": {true, true}, "hub_manager": {true, false}, "support": {false, false}} {
		if Can(role, "hub_ops.read") != want[0] || Can(role, "hub_ops.manage") != want[1] {
			t.Errorf("%s: read %v manage %v", role, Can(role, "hub_ops.read"), Can(role, "hub_ops.manage"))
		}
	}
}

func TestAManagerNeverSeesAnotherHubEvenInTheirOwnZone(t *testing.T) {
	d := newOpsDesk(t)
	mine := d.request(d.hubA, "packing", time.Hour)
	// A second hub in the same zone, with a stuck request of its own.
	next := d.f.id("INSERT INTO hubs (zone_id, name, lat, lng) VALUES ($1, 'Hub Next Door', 12.94, 77.63) RETURNING id::text", d.zoneA)
	theirs := d.request(next, "packing", 2*time.Hour)
	_, list := d.stuck(d.mgrA, "")
	if got := idsOf(list["requests"].([]any)); len(got) != 1 || got[0] != mine {
		t.Errorf("%v, want only %s and not %s", got, mine, theirs)
	}
	_, out := d.list(d.mgrA, "?zone_id="+d.zoneA)
	if len(out["hubs"].([]any)) != 1 || d.ops4(out, d.hubA) == nil {
		t.Errorf("%v", out)
	}
	if _, all := d.stuck(d.admin, ""); len(all["requests"].([]any)) != 2 {
		t.Errorf("an admin sees both: %v", all)
	}
}
