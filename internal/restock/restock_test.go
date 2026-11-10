package restock

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/shifts"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type fixture struct {
	t                     *testing.T
	pool                  *pgxpool.Pool
	srv                   *httptest.Server
	sessions              *identity.Sessions
	svc                   *Service
	zone, hub, other      string
	scooter, rider, token string
	chai, samosa          string
	phones                int
}

func (f *fixture) id(sql string, args ...any) string {
	f.t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, pool: dbtest.Fresh(t)}
	f.sessions = &identity.Sessions{Pool: f.pool, Log: quiet, RefreshTTL: time.Hour,
		Signer: identity.Signer{Key: []byte("a-test-key-that-is-long-enough-to-use"), TTL: 15 * time.Minute}}
	f.svc = &Service{Pool: f.pool, Log: quiet}
	f.srv = httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		f.svc.MountRider(r, identity.RequireActor(f.sessions.Signer, identity.TypeRider))
	}))
	t.Cleanup(f.srv.Close)
	f.zone = f.id(`INSERT INTO zones (name, city, status, area) VALUES ('Z', 'Bengaluru', 'active',
		extensions.ST_GeomFromText('MULTIPOLYGON(((77.612 12.925, 77.640 12.925, 77.640 12.945, 77.612 12.945, 77.612 12.925)))', 4326)) RETURNING id::text`)
	node := f.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, 'N', 12.936, 77.625, 4) RETURNING id::text", f.zone)
	f.hub = f.id("INSERT INTO hubs (zone_id, name, lat, lng) VALUES ($1, 'Hub A', 12.930, 77.620) RETURNING id::text", f.zone)
	f.other = f.id("INSERT INTO hubs (zone_id, name, lat, lng) VALUES ($1, 'Hub B', 12.940, 77.630) RETURNING id::text", f.zone)
	f.chai = f.id("INSERT INTO menu_items (name, price_paise, status, sort_order) VALUES ('Chai', 4900, 'active', 1) RETURNING id::text")
	f.samosa = f.id("INSERT INTO menu_items (name, price_paise, status, sort_order) VALUES ('Samosa', 3900, 'active', 2) RETURNING id::text")
	f.scooter = f.id("INSERT INTO scooters (code, zone_id, node_id) VALUES ('SC-1', $1, $2) RETURNING id::text", f.zone, node)
	f.rider, f.token = f.signIn(node, f.scooter)
	return f
}

func (f *fixture) signIn(node, scooter string) (string, string) {
	f.t.Helper()
	f.phones++
	person := f.id("INSERT INTO people (phone) VALUES ($1) RETURNING id::text", fmt.Sprintf("+9198000005%02d", f.phones))
	rider := f.id("INSERT INTO riders (person_id, status, zone_id) VALUES ($1, 'active', $2) RETURNING id::text", person, f.zone)
	if scooter != "" {
		f.exec("INSERT INTO shifts (rider_id, node_id, scooter_id) VALUES ($1, $2, $3)", rider, node, scooter)
	}
	var pair identity.Pair
	if err := database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		var err error
		pair, _, err = f.sessions.Start(context.Background(), tx, identity.TypeRider, rider)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return rider, pair.AccessToken
}

// hubLot puts a whole lot at a hub, expiring in the given time.
func (f *fixture) hubLot(hub, code, item string, qty int, expires time.Duration) string {
	f.t.Helper()
	return f.id("INSERT INTO lots (code, item_id, qty, expires_at, hub_id) VALUES ($1, $2, $3, now() + make_interval(secs => $4), $5) RETURNING id::text",
		code, item, qty, expires.Seconds(), hub)
}

// request makes a confirmed request, as the endpoints of parts B and C will.
func (f *fixture) request(lines ...any) string {
	f.t.Helper()
	id := f.id("INSERT INTO restock_requests (rider_id, scooter_id, hub_id, source, state) VALUES ($1, $2, $3, 'manual', 'confirmed') RETURNING id::text",
		f.rider, f.scooter, f.hub)
	for i := 0; i < len(lines); i += 2 {
		f.exec("INSERT INTO restock_lines (request_id, item_id, qty) VALUES ($1, $2, $3)", id, lines[i], lines[i+1])
	}
	return id
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

func (f *fixture) state(id string) string {
	return f.id("SELECT state FROM restock_requests WHERE id = $1", id)
}

func required(t *testing.T, schema string) []string {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/rider/openapi.yaml")
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
	return doc.Components.Schemas[schema].Required
}

func check(t *testing.T, got map[string]any, schema string) {
	t.Helper()
	fields := required(t, schema)
	if len(fields) == 0 {
		t.Fatalf("no required fields for %s", schema)
	}
	for _, field := range fields {
		if _, ok := got[field]; !ok {
			t.Errorf("%s has no %s", schema, field)
		}
	}
}

func TestTheSimulatorMovesARequestToReady(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sim := &Simulator{Service: f.svc, AcceptAfter: 20 * time.Second, PackAfter: 20 * time.Second, ReadyAfter: time.Minute}
	soon := f.hubLot(f.hub, "LOT-SOON", f.chai, 10, 2*time.Hour)
	f.hubLot(f.hub, "LOT-LATER", f.chai, 10, 48*time.Hour)
	f.hubLot(f.hub, "LOT-SAM", f.samosa, 10, 48*time.Hour)
	id := f.request(f.chai, 8, f.samosa, 5)
	now := time.Now()
	if n, _ := sim.Step(ctx, now); n != 0 {
		t.Error("accepted before its time")
	}
	steps := map[time.Duration]string{21 * time.Second: "hub_accepted", 42 * time.Second: "packing", 103 * time.Second: "ready"}
	for _, after := range []time.Duration{21 * time.Second, 42 * time.Second, 103 * time.Second} {
		if _, err := sim.Step(ctx, now.Add(after)); err != nil {
			t.Fatal(err)
		}
		if got := f.state(id); got != steps[after] {
			t.Fatalf("after %s: %s, want %s", after, got, steps[after])
		}
	}
	code, out := f.get("/restock/"+id, f.token)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "RestockResponse")
	r := out["request"].(map[string]any)
	check(t, r, "RestockRequest")
	if r["slot_at"] == nil || r["arrive_by"] == nil || r["shortfall"] != nil {
		t.Errorf("%v", r)
	}
	// Whole lots, the one expiring first: 8 chai is one lot of 10.
	codes := r["lot_numbers"].([]any)
	if len(codes) != 2 || codes[0] != "LOT-SAM" || codes[1] != "LOT-SOON" {
		t.Errorf("packed %v", codes)
	}
	for _, l := range r["items"].([]any) {
		line := l.(map[string]any)
		check(t, line, "RestockLine")
		if line["filled_qty"] != float64(10) {
			t.Errorf("%v", line)
		}
	}
	if f.count("SELECT count(*) FROM restock_lots WHERE lot_id = $1", soon) != 1 {
		t.Error("the lot expiring first must go first")
	}
	if f.count("SELECT seq::int FROM rider_state WHERE rider_id = $1", f.rider) != 3 || f.count("SELECT count(*) FROM outbox_events WHERE topic = 'push.rider'") != 3 {
		t.Error("the rider must hear of each step")
	}
}

func TestAShortHubOffersAnotherHub(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sim := &Simulator{Service: f.svc}
	f.hubLot(f.hub, "LOT-A1", f.chai, 5, time.Hour)
	f.hubLot(f.other, "LOT-B1", f.chai, 10, time.Hour)
	id := f.request(f.chai, 8)
	now := time.Now()
	for _, after := range []time.Duration{21 * time.Second, 42 * time.Second, 103 * time.Second} {
		sim.Step(ctx, now.Add(after))
	}
	if f.state(id) != "partial" {
		t.Fatalf("%s", f.state(id))
	}
	_, out := f.get("/restock/"+id, f.token)
	sf, _ := out["request"].(map[string]any)["shortfall"].(map[string]any)
	if sf == nil || sf["action"] != "other_hub" || sf["other_hub"].(map[string]any)["id"] != f.other {
		t.Fatalf("%v", out)
	}
	check(t, sf, "RestockShortfall")
	if line := out["request"].(map[string]any)["items"].([]any)[0].(map[string]any); line["filled_qty"] != float64(5) {
		t.Errorf("%v", line)
	}
}

func TestNothingAnywhereIsAPartialFill(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sim := &Simulator{Service: f.svc}
	f.hubLot(f.hub, "LOT-OLD", f.chai, 10, -time.Hour) // Expired: never packed.
	lot := f.hubLot(f.hub, "LOT-ON-SCOOTER", f.chai, 10, time.Hour)
	f.exec("INSERT INTO scooter_stock (scooter_id, lot_id, on_hand) VALUES ($1, $2, 10)", f.scooter, lot) // Already on a scooter.
	f.exec("UPDATE lots SET status = 'quarantined' WHERE code = 'LOT-OLD'")
	id := f.request(f.chai, 3)
	now := time.Now()
	for _, after := range []time.Duration{21 * time.Second, 42 * time.Second, 103 * time.Second} {
		sim.Step(ctx, now.Add(after))
	}
	_, out := f.get("/restock/"+id, f.token)
	sf, _ := out["request"].(map[string]any)["shortfall"].(map[string]any)
	if f.state(id) != "partial" || sf == nil || sf["action"] != "partial" || sf["other_hub"] != nil || len(out["request"].(map[string]any)["lot_numbers"].([]any)) != 0 {
		t.Errorf("%v", out)
	}
}

func TestTwoRequestsNeverShareALot(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sim := &Simulator{Service: f.svc}
	f.hubLot(f.hub, "LOT-ONLY", f.chai, 10, time.Hour)
	first := f.request(f.chai, 5)
	node := f.id("SELECT node_id::text FROM shifts LIMIT 1")
	second := f.id("INSERT INTO scooters (code, zone_id, node_id) VALUES ('SC-2', $1, $2) RETURNING id::text", f.zone, node)
	rider2, _ := f.signIn(node, second)
	other := f.id("INSERT INTO restock_requests (rider_id, scooter_id, hub_id, source, state) VALUES ($1, $2, $3, 'manual', 'confirmed') RETURNING id::text", rider2, second, f.hub)
	f.exec("INSERT INTO restock_lines (request_id, item_id, qty) VALUES ($1, $2, 5)", other, f.chai)
	now := time.Now()
	for _, after := range []time.Duration{21 * time.Second, 42 * time.Second, 103 * time.Second} {
		sim.Step(ctx, now.Add(after))
	}
	if f.count("SELECT count(*) FROM restock_lots") != 1 || f.count("SELECT count(*) FROM restock_requests WHERE state = 'partial'") != 1 {
		t.Errorf("one lot, one cassette: %s %s", f.state(first), f.state(other))
	}
}

func TestOneOpenRequestPerScooter(t *testing.T) {
	f := newFixture(t)
	f.request(f.chai, 1)
	if _, err := f.pool.Exec(context.Background(), "INSERT INTO restock_requests (rider_id, scooter_id, hub_id, source, state) VALUES ($1, $2, $3, 'manual', 'confirmed')",
		f.rider, f.scooter, f.hub); err == nil {
		t.Error("two open requests for one scooter")
	}
}

func TestOptionsAndOwnRequestsOnly(t *testing.T) {
	f := newFixture(t)
	f.exec("INSERT INTO hubs (zone_id, name, lat, lng, status) VALUES ($1, 'Closed hub', 12.93, 77.62, 'paused')", f.zone)
	f.exec("UPDATE menu_items SET status = 'hidden' WHERE id = $1", f.samosa)
	code, out := f.get("/restock/options", f.token)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "RestockOptions")
	if len(out["hubs"].([]any)) != 2 || len(out["items"].([]any)) != 1 {
		t.Errorf("active hubs in the rider's zone, and items on sale: %v", out)
	}
	check(t, out["hubs"].([]any)[0].(map[string]any), "HubRef")
	check(t, out["items"].([]any)[0].(map[string]any), "RestockOption")
	id := f.request(f.chai, 1)
	_, other := f.signIn(f.id("SELECT node_id::text FROM shifts LIMIT 1"), "")
	if code, _ := f.get("/restock/"+id, other); code != 404 {
		t.Errorf("another rider's request: %d", code)
	}
	if code, _ := f.get("/restock/nope", f.token); code != 404 {
		t.Errorf("%d", code)
	}
	f.exec("UPDATE people SET status = 'blocked' WHERE id = (SELECT person_id FROM riders WHERE id = $1)", f.rider)
	if code, _ := f.get("/restock/options", f.token); code != 403 {
		t.Errorf("a blocked rider: %d", code)
	}
}

func TestCurrentShowsTheRequestInProgress(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if v, err := Current(ctx, f.pool, f.rider); err != nil || v != nil {
		t.Fatalf("%v %v", v, err)
	}
	id := f.request(f.chai, 1)
	v, err := Current(ctx, f.pool, f.rider)
	if err != nil || v == nil || v.(map[string]any)["id"] != id {
		t.Errorf("%v %v", v, err)
	}
	f.exec("UPDATE restock_requests SET state = 'closed' WHERE id = $1", id)
	if v, _ := Current(ctx, f.pool, f.rider); v != nil {
		t.Error("a closed request is not in progress")
	}
}

func TestOneStepMovesOneState(t *testing.T) {
	f := newFixture(t)
	sim := &Simulator{Service: f.svc}
	f.hubLot(f.hub, "LOT-X", f.chai, 10, time.Hour)
	id := f.request(f.chai, 1)
	// Ten minutes late, the hub still goes one step at a time, so the rider sees each state.
	sim.Step(context.Background(), time.Now().Add(10*time.Minute))
	if f.state(id) != "hub_accepted" {
		t.Errorf("one step went to %s", f.state(id))
	}
}

func TestAnotherHubMustHoldEnough(t *testing.T) {
	f := newFixture(t)
	sim := &Simulator{Service: f.svc}
	f.hubLot(f.other, "LOT-B-SMALL", f.chai, 10, time.Hour) // Holds 10; 30 are short.
	id := f.request(f.chai, 30)
	now := time.Now()
	for _, after := range []time.Duration{21 * time.Second, 42 * time.Second, 103 * time.Second} {
		sim.Step(context.Background(), now.Add(after))
	}
	_, out := f.get("/restock/"+id, f.token)
	if sf := out["request"].(map[string]any)["shortfall"].(map[string]any); sf["action"] != "partial" {
		t.Errorf("a hub with too little was offered: %v", sf)
	}
}

func TestPackingTwoRequestsAtOnceMissesNoLot(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sim := &Simulator{Service: f.svc}
	f.hubLot(f.hub, "LOT-P1", f.chai, 10, time.Hour)
	f.hubLot(f.hub, "LOT-P2", f.chai, 10, 2*time.Hour)
	first := f.request(f.chai, 5)
	node := f.id("SELECT node_id::text FROM shifts LIMIT 1")
	second := f.id("INSERT INTO scooters (code, zone_id, node_id) VALUES ('SC-3', $1, $2) RETURNING id::text", f.zone, node)
	rider2, _ := f.signIn(node, second)
	other := f.id("INSERT INTO restock_requests (rider_id, scooter_id, hub_id, source, state) VALUES ($1, $2, $3, 'manual', 'confirmed') RETURNING id::text", rider2, second, f.hub)
	f.exec("INSERT INTO restock_lines (request_id, item_id, qty) VALUES ($1, $2, 5)", other, f.chai)
	now := time.Now()
	sim.Step(ctx, now.Add(21*time.Second))
	sim.Step(ctx, now.Add(42*time.Second))
	// Two copies of the API pack at the same moment.
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, err := sim.Step(ctx, now.Add(103*time.Second)); done <- err }()
	}
	<-done
	<-done
	if f.state(first) != "ready" || f.state(other) != "ready" {
		t.Errorf("two lots for two requests, but %s and %s", f.state(first), f.state(other))
	}
}

func TestAReleasedLotCanBePackedAgain(t *testing.T) {
	f := newFixture(t)
	sim := &Simulator{Service: f.svc}
	lot := f.hubLot(f.hub, "LOT-AGAIN", f.chai, 10, time.Hour)
	gone := f.request(f.chai, 1)
	f.exec("INSERT INTO restock_lots (request_id, lot_id, released_at) VALUES ($1, $2, now())", gone, lot)
	f.exec("UPDATE restock_requests SET state = 'cancelled' WHERE id = $1", gone)
	id := f.request(f.chai, 1)
	now := time.Now()
	for _, after := range []time.Duration{21 * time.Second, 42 * time.Second, 103 * time.Second} {
		sim.Step(context.Background(), now.Add(after))
	}
	if f.state(id) != "ready" {
		t.Errorf("a released lot was not packed again: %s", f.state(id))
	}
}

func TestAHubLotIsScannedOnlyFromYourCassette(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	lot := f.hubLot(f.hub, "LOT-HUB-SCAN", f.chai, 10, time.Hour)
	scan := func() error {
		return database.InTx(ctx, f.pool, func(tx pgx.Tx) error {
			_, err := shifts.ScanTx(ctx, tx, f.rider, "LOT-HUB-SCAN", false)
			return err
		})
	}
	if code, _, ok := shifts.RefusalOf(scan()); !ok || code != "bad_scan" {
		t.Fatal("a lot at a hub was scanned without a cassette")
	}
	id := f.request(f.chai, 1)
	f.exec("UPDATE restock_requests SET state = 'ready' WHERE id = $1", id)
	f.exec("INSERT INTO restock_lots (request_id, lot_id) VALUES ($1, $2)", id, lot)
	if err := scan(); err != nil {
		t.Fatalf("the rider's own packed lot: %v", err)
	}
	if f.count("SELECT count(*) FROM lots WHERE id = $1 AND hub_id IS NULL", lot) != 1 || f.count("SELECT count(*) FROM scooter_stock WHERE lot_id = $1", lot) != 1 {
		t.Error("the lot did not leave the hub for the scooter")
	}
}
