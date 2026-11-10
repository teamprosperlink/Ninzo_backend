package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/orders"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/restock"
	"github.com/teamprosperlink/Ninzo_backend/internal/shifts"
)

// queue is a hub with a lead and a staff member, a rider on shift, lots at the hub and a request.
type queue struct {
	*fixture
	lead, staff, leadTok, staffTok string
	zone, scooter, rider           string
	chai, samosa                   string
	request                        string
	n                              int
}

func newQueue(t *testing.T) *queue {
	t.Helper()
	f := newFixture(t)
	q := &queue{fixture: f}
	svc := &Service{Pool: f.pool, Log: quiet}
	streams := &orders.Service{Pool: f.pool, Log: quiet}
	f.srv = httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		svc.MountStaff(r, identity.RequireActor(f.sessions.Signer, identity.TypeStaff))
		svc.MountQueue(r, identity.RequireActor(f.sessions.Signer, identity.TypeStaff))
		svc.MountSeal(r, identity.RequireActor(f.sessions.Signer, identity.TypeStaff))
		svc.MountEvents(r, identity.RequireActor(f.sessions.Signer, identity.TypeStaff))
		streams.MountHubStream(r, &orders.Streams{Signer: f.sessions.Signer}, svc.Feed())
	}))
	t.Cleanup(f.srv.Close)
	q.zone = f.id("SELECT zone_id::text FROM hubs WHERE id = $1", f.hub)
	q.lead, q.leadTok = f.staff("+919800000101", "hub_lead")
	q.staff, q.staffTok = f.staff("+919800000102", "hub_staff")
	q.chai = f.id("INSERT INTO menu_items (name, price_paise, status, sort_order) VALUES ('Chai', 4900, 'active', 2) RETURNING id::text")
	q.samosa = f.id("INSERT INTO menu_items (name, price_paise, status, sort_order) VALUES ('Samosa', 3900, 'active', 1) RETURNING id::text")
	node := f.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, 'N', 12.93, 77.62, 4) RETURNING id::text", q.zone)
	q.scooter = f.id("INSERT INTO scooters (code, zone_id, node_id) VALUES ('SC-7', $1, $2) RETURNING id::text", q.zone, node)
	person := f.id("INSERT INTO people (phone) VALUES ('+919800000103') RETURNING id::text")
	q.rider = f.id("INSERT INTO riders (person_id, status, zone_id) VALUES ($1, 'active', $2) RETURNING id::text", person, q.zone)
	f.exec("INSERT INTO shifts (rider_id, node_id, scooter_id) VALUES ($1, $2, $3)", q.rider, node, q.scooter)
	q.request = q.confirmed(q.chai, 8, q.samosa, 4)
	return q
}

// confirmed makes a request a rider has confirmed. The first is for the rider's scooter; each one
// after is for another scooter, as a scooter has one open request at a time.
func (q *queue) confirmed(lines ...any) string {
	q.t.Helper()
	scooter := q.scooter
	if q.request != "" {
		q.n++
		scooter = q.id("INSERT INTO scooters (code, zone_id) VALUES ($1, $2) RETURNING id::text", fmt.Sprintf("SC-X%d", q.n), q.zone)
	}
	id := q.id("INSERT INTO restock_requests (rider_id, scooter_id, hub_id, source, state) VALUES ($1, $2, $3, 'manual', 'confirmed') RETURNING id::text", q.rider, scooter, q.hub)
	for i := 0; i < len(lines); i += 2 {
		q.exec("INSERT INTO restock_lines (request_id, item_id, qty) VALUES ($1, $2, $3)", id, lines[i], lines[i+1])
	}
	return id
}

func (q *queue) lot(code, item string, qty int, expires time.Duration) string {
	q.t.Helper()
	return q.id("INSERT INTO lots (code, item_id, qty, expires_at, hub_id) VALUES ($1, $2, $3, now() + make_interval(secs => $4), $5) RETURNING id::text",
		code, item, qty, expires.Seconds(), q.hub)
}

func (q *queue) call(method, path, token, body string) (int, map[string]any) {
	q.t.Helper()
	q.n++
	req, _ := http.NewRequest(method, q.srv.URL+"/v1"+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if method == "POST" {
		req.Header.Set("Idempotency-Key", fmt.Sprintf("hub-key-%05d", q.n))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		q.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (q *queue) scan(token, code string) (int, map[string]any) {
	return q.call("POST", "/hub/restock/"+q.request+"/scan", token, `{"lot_code":"`+code+`","scanned_at":"2026-11-10T10:00:00Z"}`)
}

func (q *queue) state() string {
	return q.id("SELECT state FROM restock_requests WHERE id = $1", q.request)
}

func (q *queue) riderSeq() int {
	var n int
	_ = q.pool.QueryRow(context.Background(), "SELECT coalesce((SELECT seq FROM rider_state WHERE rider_id = $1), 0)::int", q.rider).Scan(&n)
	return n
}

// check confirms every field the partner contract requires is present.
func check(t *testing.T, got map[string]any, schema string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "contracts", "partner", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Required []string `yaml:"required"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	req := doc.Components.Schemas[schema].Required
	if len(req) == 0 {
		t.Fatalf("no schema %s", schema)
	}
	for _, k := range req {
		if _, ok := got[k]; !ok {
			t.Errorf("%s is missing %s: %v", schema, k, got)
		}
	}
}

func TestTheQueue(t *testing.T) {
	q := newQueue(t)
	other := q.id("INSERT INTO hubs (zone_id, name, lat, lng) VALUES ($1, 'Hub B', 12.94, 77.63) RETURNING id::text", q.zone)
	elsewhere := q.id("INSERT INTO restock_requests (rider_id, scooter_id, hub_id, source, state) VALUES ($1, $2, $3, 'manual', 'cancelled') RETURNING id::text", q.rider, q.scooter, other)
	code, out := q.call("GET", "/hub/restock", q.staffTok, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "HubRequestList")
	list := out["requests"].([]any)
	if len(list) != 1 {
		t.Fatalf("%v", out)
	}
	r := list[0].(map[string]any)
	check(t, r, "HubRequest")
	if r["id"] != q.request || r["state"] != "confirmed" || r["scooter_code"] != "SC-7" || len(r["items"].([]any)) != 2 {
		t.Errorf("%v", r)
	}
	for _, k := range []string{"rider_id", "rider", "phone", "rider_name"} {
		if _, leak := r[k]; leak {
			t.Errorf("hub staff must not see %s", k)
		}
	}
	if _, out := q.call("GET", "/hub/restock?state=ready", q.staffTok, ""); len(out["requests"].([]any)) != 0 {
		t.Errorf("a state filter: %v", out)
	}
	if code, _ := q.call("GET", "/hub/restock?state=nope", q.staffTok, ""); code != 400 {
		t.Errorf("a bad state: %d", code)
	}
	if code, _ := q.call("GET", "/hub/restock/"+elsewhere, q.staffTok, ""); code != 404 {
		t.Errorf("another hub's request: %d", code)
	}
	if code, out := q.call("GET", "/hub/restock/"+q.request, q.staffTok, ""); code != 200 {
		t.Errorf("%d %v", code, out)
	} else {
		check(t, out, "HubRequestResponse")
	}
}

func TestOneAcceptanceOnly(t *testing.T) {
	q := newQueue(t)
	before := q.riderSeq()
	code, out := q.call("POST", "/hub/restock/"+q.request+"/accept", q.staffTok, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	r := out["request"].(map[string]any)
	if r["state"] != "hub_accepted" || r["accepted_by"] != "Asha" || r["slot_at"] == nil || r["rider_arrival_at"] == nil {
		t.Errorf("%v", r)
	}
	if q.riderSeq() <= before {
		t.Error("the rider was not told the hub accepted")
	}
	if code, again := q.call("POST", "/hub/restock/"+q.request+"/accept", q.staffTok, ""); code != 200 || again["request"].(map[string]any)["state"] != "hub_accepted" {
		t.Errorf("the same staff member again: %d %v", code, again)
	}
	if code, out := q.call("POST", "/hub/restock/"+q.request+"/accept", q.leadTok, ""); code != 409 || out["code"] != "request_taken" {
		t.Errorf("a second staff member: %d %v", code, out)
	}
	withdrawn := q.confirmed(q.chai, 1)
	q.exec("UPDATE restock_requests SET state = 'cancelled' WHERE id = $1", withdrawn)
	if code, out := q.call("POST", "/hub/restock/"+withdrawn+"/accept", q.staffTok, ""); code != 409 || out["code"] != "request_withdrawn" {
		t.Errorf("%d %v", code, out)
	}
}

func TestAPausedHubAcceptsNothing(t *testing.T) {
	q := newQueue(t)
	q.exec("UPDATE hubs SET status = 'paused'")
	if code, out := q.call("POST", "/hub/restock/"+q.request+"/accept", q.staffTok, ""); code != 409 || out["code"] != "hub_closed" {
		t.Errorf("%d %v", code, out)
	}
}

func TestThePickList(t *testing.T) {
	q := newQueue(t)
	if code, out := q.call("GET", "/hub/restock/"+q.request+"/pick-list", q.staffTok, ""); code != 409 || out["code"] != "not_accepted" {
		t.Errorf("before accept: %d %v", code, out)
	}
	q.lot("CHAI-LATE", q.chai, 10, 48*time.Hour)
	q.lot("CHAI-SOON", q.chai, 10, 6*time.Hour)
	q.lot("CHAI-OLD", q.chai, 10, -time.Hour)
	q.exec("UPDATE lots SET status = 'quarantined' WHERE id = $1", q.lot("CHAI-BAD", q.chai, 10, time.Hour))
	q.exec("UPDATE lots SET temp_status = 'alert' WHERE id = $1", q.lot("CHAI-WARM", q.chai, 10, time.Hour))
	q.lot("SAM-1", q.samosa, 5, 30*time.Hour)
	q.call("POST", "/hub/restock/"+q.request+"/accept", q.staffTok, "")
	code, out := q.call("GET", "/hub/restock/"+q.request+"/pick-list", q.staffTok, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "PickList")
	lines := out["lines"].([]any)
	if len(lines) != 2 {
		t.Fatalf("%v", out)
	}
	first := lines[0].(map[string]any)
	check(t, first, "PickLine")
	lots := first["lots"].([]any)
	if first["item_id"] != q.chai || len(lots) != 2 || lots[0].(map[string]any)["lot_code"] != "CHAI-SOON" {
		t.Errorf("the line with the soonest lot, then soonest first, only lots that can be picked: %v", first)
	}
	check(t, lots[0].(map[string]any), "PickLot")
	// A line already picked in full leaves the list.
	q.scan(q.staffTok, "SAM-1")
	_, out = q.call("GET", "/hub/restock/"+q.request+"/pick-list", q.staffTok, "")
	if len(out["lines"].([]any)) != 1 {
		t.Errorf("a picked line stays: %v", out)
	}
}

func TestScanning(t *testing.T) {
	q := newQueue(t)
	q.lot("CHAI-1", q.chai, 10, 6*time.Hour)
	if code, out := q.scan(q.staffTok, "CHAI-1"); code != 409 || out["code"] != "not_accepted" {
		t.Errorf("before accept: %d %v", code, out)
	}
	q.call("POST", "/hub/restock/"+q.request+"/accept", q.staffTok, "")
	code, out := q.scan(q.staffTok, "CHAI-1")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "ScanResult")
	r := out["request"].(map[string]any)
	if out["duplicate"] != false || r["state"] != "packing" || r["lot_numbers"].([]any)[0] != "CHAI-1" {
		t.Errorf("%v", out)
	}
	for _, line := range r["items"].([]any) {
		if l := line.(map[string]any); l["item_id"] == q.chai && l["picked_qty"] != float64(10) {
			t.Errorf("picked_qty: %v", l)
		}
	}
	seq := r["seq"]
	if code, again := q.scan(q.staffTok, "CHAI-1"); code != 200 || again["duplicate"] != true || again["request"].(map[string]any)["seq"] != seq {
		t.Errorf("a duplicate must change nothing: %d %v", code, again)
	}
	// The rider sees packing too.
	if v, err := restock.View(context.Background(), q.pool, q.request); err != nil || v["state"] != "packing" {
		t.Errorf("%v %v", v, err)
	}
}

func TestScanRefusals(t *testing.T) {
	q := newQueue(t)
	q.call("POST", "/hub/restock/"+q.request+"/accept", q.staffTok, "")
	other := q.id("INSERT INTO hubs (zone_id, name, lat, lng) VALUES ($1, 'Hub B', 12.94, 77.63) RETURNING id::text", q.zone)
	q.exec("INSERT INTO lots (code, item_id, qty, expires_at, hub_id) VALUES ('AWAY-1', $1, 5, now() + interval '1 day', $2)", q.chai, other)
	tea := q.id("INSERT INTO menu_items (name, price_paise, status, sort_order) VALUES ('Tea', 2900, 'active', 3) RETURNING id::text")
	q.lot("TEA-1", tea, 5, time.Hour)
	q.exec("UPDATE lots SET status = 'quarantined' WHERE id = $1", q.lot("CHAI-Q", q.chai, 5, time.Hour))
	q.lot("CHAI-X", q.chai, 5, -time.Minute)
	warm := q.lot("CHAI-W", q.chai, 5, time.Hour)
	q.exec("UPDATE lots SET temp_status = 'alert' WHERE id = $1", warm)
	packed := q.lot("CHAI-P", q.chai, 5, time.Hour)
	elsewhere := q.confirmed(q.chai, 1)
	q.exec("INSERT INTO restock_lots (request_id, lot_id) VALUES ($1, $2)", elsewhere, packed)
	for code, want := range map[string]string{"NOPE": "wrong_lot", "AWAY-1": "wrong_lot", "TEA-1": "wrong_lot", "CHAI-Q": "lot_quarantined",
		"CHAI-X": "lot_expired", "CHAI-W": "temp_alert", "CHAI-P": "lot_already_packed"} {
		status, out := q.scan(q.staffTok, code)
		if status != 409 || out["code"] != want {
			t.Errorf("%s: %d %v", code, status, out)
			continue
		}
		check(t, out, "ScanRejection")
		lot, _ := out["lot"].(map[string]any)
		if (code == "NOPE" || code == "AWAY-1") != (lot == nil) {
			t.Errorf("%s: the lot is shown only when it is at this hub: %v", code, out)
		}
	}
	if _, out := q.scan(q.staffTok, "CHAI-W"); out["lot"].(map[string]any)["lot_id"] != warm {
		t.Error("a temperature alert must name the lot to quarantine")
	}
	if q.state() != "hub_accepted" {
		t.Errorf("a refused scan changed the request: %s", q.state())
	}
	if code, _ := q.call("POST", "/hub/restock/"+q.request+"/scan", q.staffTok, `{"lot_code":"CHAI-1"}`); code != 400 {
		t.Errorf("no scanned_at: %d", code)
	}
}

func TestUnscan(t *testing.T) {
	q := newQueue(t)
	q.lot("CHAI-1", q.chai, 10, 6*time.Hour)
	q.call("POST", "/hub/restock/"+q.request+"/accept", q.staffTok, "")
	q.scan(q.staffTok, "CHAI-1")
	code, out := q.call("POST", "/hub/restock/"+q.request+"/unscan", q.staffTok, `{"lot_code":"CHAI-1"}`)
	if code != 200 || len(out["request"].(map[string]any)["lot_numbers"].([]any)) != 0 {
		t.Fatalf("%d %v", code, out)
	}
	if code, out := q.call("POST", "/hub/restock/"+q.request+"/unscan", q.staffTok, `{"lot_code":"CHAI-1"}`); code != 409 || out["code"] != "not_in_cassette" {
		t.Errorf("%d %v", code, out)
	}
	// The lot can be scanned again.
	if code, _ := q.scan(q.staffTok, "CHAI-1"); code != 200 {
		t.Errorf("%d", code)
	}
	// A sealed cassette cannot be changed.
	q.exec("UPDATE restock_requests SET state = 'ready' WHERE id = $1", q.request)
	if code, out := q.call("POST", "/hub/restock/"+q.request+"/unscan", q.staffTok, `{"lot_code":"CHAI-1"}`); code != 409 || out["code"] != "request_closed" {
		t.Errorf("unscan of a sealed cassette: %d %v", code, out)
	}
}

func TestTheSimulatorLeavesAStaffedHubAlone(t *testing.T) {
	q := newQueue(t)
	sim := &restock.Simulator{Service: &restock.Service{Pool: q.pool, Log: quiet}}
	sim.Step(context.Background(), time.Now().Add(time.Hour))
	if q.state() != "confirmed" {
		t.Errorf("the simulator moved a request at a hub with staff: %s", q.state())
	}
	q.exec("UPDATE hub_staff SET status = 'suspended'")
	sim.Step(context.Background(), time.Now().Add(time.Hour))
	if q.state() != "hub_accepted" {
		t.Errorf("a hub with no active staff is still simulated: %s", q.state())
	}
}

type socket struct {
	t    *testing.T
	conn *websocket.Conn
}

func (q *queue) open(token string) *socket {
	q.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(q.srv.URL, "http")+"/v1/hub/stream", nil)
	if err != nil {
		q.t.Fatal(err)
	}
	q.t.Cleanup(func() { _ = conn.CloseNow() })
	raw, _ := json.Marshal(map[string]string{"type": "auth", "token": token})
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		q.t.Fatal(err)
	}
	return &socket{q.t, conn}
}

func (s *socket) next(kind string) map[string]any {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, raw, err := s.conn.Read(ctx)
		if err != nil {
			s.t.Fatalf("no %s: %v", kind, err)
		}
		var f map[string]any
		if err := json.Unmarshal(raw, &f); err != nil {
			s.t.Fatal(err)
		}
		if f["type"] == kind {
			return f
		}
	}
}

func TestTheLiveChannel(t *testing.T) {
	q := newQueue(t)
	s := q.open(q.leadTok)
	s.next("auth_ok")
	// The rider confirms a new request: it arrives within the poll.
	fresh := q.confirmed(q.samosa, 2)
	// A confirm bumps through restock, as here.
	if err := database.InTx(context.Background(), q.pool, func(tx pgx.Tx) error {
		return restock.BumpRider(context.Background(), tx, q.rider, fresh)
	}); err != nil {
		t.Fatal(err)
	}
	f := s.next("request_new")
	if f["data"].(map[string]any)["id"] != fresh || f["seq"] == nil {
		t.Errorf("%v", f)
	}
	q.call("POST", "/hub/restock/"+fresh+"/accept", q.staffTok, "")
	if f := s.next("request_changed"); f["data"].(map[string]any)["state"] != "hub_accepted" {
		t.Errorf("%v", f)
	}
	// A rider's token is refused.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rs := q.open(q.token(identity.TypeRider, q.rider))
	if _, _, err := rs.conn.Read(ctx); websocket.CloseStatus(err) != 4401 {
		t.Errorf("a rider token on the hub channel: %v", err)
	}
}

func TestASuggestionIsNotTheHubs(t *testing.T) {
	q := newQueue(t)
	ctx := context.Background()
	s := q.open(q.leadTok)
	s.next("auth_ok")
	seq := func() int64 {
		n, err := HubSeq(ctx, q.pool, q.hub)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := seq()
	scooter := q.id("INSERT INTO scooters (code, zone_id) VALUES ('SC-S', $1) RETURNING id::text", q.zone)
	sug := q.id("INSERT INTO restock_requests (rider_id, scooter_id, hub_id, source, state, expires_at) VALUES ($1, $2, $3, 'suggested', 'suggested', now() + interval '10 minutes') RETURNING id::text", q.rider, scooter, q.hub)
	q.exec("INSERT INTO restock_lines (request_id, item_id, qty) VALUES ($1, $2, 3)", sug, q.chai)
	bump := func() {
		if err := database.InTx(ctx, q.pool, func(tx pgx.Tx) error { return restock.BumpRider(ctx, tx, q.rider, sug) }); err != nil {
			t.Fatal(err)
		}
	}
	bump()
	if seq() != before {
		t.Error("a suggestion raised the hub's seq")
	}
	if code, _ := q.call("GET", "/hub/restock/"+sug, q.staffTok, ""); code != 404 {
		t.Errorf("a suggestion is visible to the hub: %d", code)
	}
	if code, _ := q.call("POST", "/hub/restock/"+sug+"/accept", q.staffTok, ""); code != 404 {
		t.Errorf("a suggestion can be accepted: %d", code)
	}
	// Once confirmed, it is new to the hub.
	q.exec("UPDATE restock_requests SET state = 'confirmed' WHERE id = $1", sug)
	bump()
	if f := s.next("request_new"); f["data"].(map[string]any)["id"] != sug {
		t.Errorf("%v", f)
	}
}

func TestTheRidersPickupReachesTheHub(t *testing.T) {
	q := newQueue(t)
	ctx := context.Background()
	q.lot("CHAI-PU", q.chai, 10, 6*time.Hour)
	q.call("POST", "/hub/restock/"+q.request+"/accept", q.staffTok, "")
	q.scan(q.staffTok, "CHAI-PU")
	q.exec("UPDATE restock_requests SET state = 'ready' WHERE id = $1", q.request)
	s := q.open(q.leadTok)
	s.next("auth_ok")
	if err := database.InTx(ctx, q.pool, func(tx pgx.Tx) error {
		_, err := shifts.ScanTx(ctx, tx, q.rider, "CHAI-PU", false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if f := s.next("request_changed"); f["data"].(map[string]any)["state"] != "received" {
		t.Errorf("the hub did not see the pickup: %v", f)
	}
}

func TestThePickListSortsByTimeNotText(t *testing.T) {
	q := newQueue(t)
	// The same second: one lot at .5 seconds, one on the second. Text order would put .5 first.
	q.exec("INSERT INTO lots (code, item_id, qty, expires_at, hub_id) VALUES ('CHAI-HALF', $1, 10, date_trunc('second', now()) + interval '6 hours 500 milliseconds', $2)", q.chai, q.hub)
	q.exec("INSERT INTO lots (code, item_id, qty, expires_at, hub_id) VALUES ('SAM-WHOLE', $1, 10, date_trunc('second', now()) + interval '6 hours', $2)", q.samosa, q.hub)
	q.call("POST", "/hub/restock/"+q.request+"/accept", q.staffTok, "")
	_, out := q.call("GET", "/hub/restock/"+q.request+"/pick-list", q.staffTok, "")
	if first := out["lines"].([]any)[0].(map[string]any); first["item_id"] != q.samosa {
		t.Errorf("the line with the sooner lot must come first: %v", out)
	}
}
