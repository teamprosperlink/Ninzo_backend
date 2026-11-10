package shifts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type fixture struct {
	t      *testing.T
	s      *identity.Sessions
	srv    *httptest.Server
	keys   int
	zone   string
	node   string
	item   string
	phones int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, s: &identity.Sessions{
		Pool: dbtest.Fresh(t), Log: quiet, RefreshTTL: time.Hour,
		Signer: identity.Signer{Key: []byte("a-test-key-that-is-long-enough-to-use"), TTL: 15 * time.Minute},
	}}
	svc := &Service{Pool: f.s.Pool, Log: quiet}
	f.srv = httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		svc.MountRider(r, identity.RequireActor(f.s.Signer, identity.TypeRider))
	}))
	t.Cleanup(f.srv.Close)
	f.zone = f.id("INSERT INTO zones (name, city, status) VALUES ('Zone', 'Bengaluru', 'active') RETURNING id::text")
	f.node = f.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, 'Node A', 12.93, 77.62, 4) RETURNING id::text", f.zone)
	f.item = f.id("INSERT INTO menu_items (name, price_paise, status) VALUES ('Chai', 4900, 'active') RETURNING id::text")
	return f
}

func (f *fixture) id(sql string, args ...any) string {
	f.t.Helper()
	var id string
	if err := f.s.Pool.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.s.Pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.s.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) scooter(code string) string {
	f.t.Helper()
	return f.id("INSERT INTO scooters (code, zone_id, node_id) VALUES ($1, $2, $3) RETURNING id::text", code, f.zone, f.node)
}

func (f *fixture) lot(code string, qty int, expires time.Duration) string {
	f.t.Helper()
	return f.id("INSERT INTO lots (code, item_id, qty, expires_at) VALUES ($1, $2, $3, now() + make_interval(secs => $4)) RETURNING id::text",
		code, f.item, qty, expires.Seconds())
}

// rider makes an approved, verified rider in the zone, signed in.
func (f *fixture) rider() (string, string) {
	f.t.Helper()
	f.phones++
	person := f.id("INSERT INTO people (phone) VALUES ($1) RETURNING id::text", fmt.Sprintf("+9198000001%02d", f.phones))
	id := f.id("INSERT INTO riders (person_id, status, zone_id) VALUES ($1, 'active', $2) RETURNING id::text", person, f.zone)
	f.exec("INSERT INTO rider_verifications (rider_id, status, method) VALUES ($1, 'verified', 'manual_upload')", id)
	var pair identity.Pair
	if err := database.InTx(context.Background(), f.s.Pool, func(tx pgx.Tx) error {
		var err error
		pair, _, err = f.s.Start(context.Background(), tx, identity.TypeRider, id)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return id, pair.AccessToken
}

func (f *fixture) send(method, path, token, body string) (int, map[string]any) {
	f.t.Helper()
	f.keys++
	req, _ := http.NewRequest(method, f.srv.URL+"/v1"+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Idempotency-Key", fmt.Sprintf("shift-key-%05d", f.keys))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (f *fixture) start(token string) (int, map[string]any) {
	f.t.Helper()
	return f.send("POST", "/riders/me/shifts/start", token, `{"node_id":"`+f.node+`"}`)
}

func check(t *testing.T, got map[string]any, schema string) {
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
	fields := doc.Components.Schemas[schema].Required
	if len(fields) == 0 {
		t.Fatalf("no required fields for %s", schema)
	}
	for _, field := range fields {
		if _, ok := got[field]; !ok {
			t.Errorf("%s has no %s: %v", schema, field, got)
		}
	}
}

func TestAShiftStartsAndEnds(t *testing.T) {
	f := newFixture(t)
	scooter := f.scooter("SC-1")
	id, token := f.rider()
	code, out := f.start(token)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "ShiftResponse")
	sh := out["shift"].(map[string]any)
	check(t, sh, "Shift")
	if sh["node_id"] != f.node || sh["ended_at"] != nil {
		t.Errorf("%v", sh)
	}
	if n := f.count("SELECT count(*) FROM scooters WHERE id = $1 AND current_rider_id = $2", scooter, id); n != 1 {
		t.Error("the scooter is not the rider's")
	}
	if code, out := f.start(token); code != 409 || out["code"] != "shift_already_open" {
		t.Errorf("a second start: %d %v", code, out)
	}
	code, out = f.send("POST", "/riders/me/shifts/end", token, "")
	if code != 200 || out["shift"].(map[string]any)["ended_at"] == nil {
		t.Fatalf("%d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM scooters WHERE id = $1 AND current_rider_id IS NULL", scooter); n != 1 {
		t.Error("the scooter was not freed")
	}
	if code, out := f.send("POST", "/riders/me/shifts/end", token, ""); code != 409 || out["code"] != "no_open_shift" {
		t.Errorf("ending again: %d %v", code, out)
	}
	if n := f.count("SELECT seq FROM rider_state WHERE rider_id = $1", id); n != 2 {
		t.Errorf("seq %d, want 2", n)
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action IN ('shift.started', 'shift.ended')"); n != 2 {
		t.Errorf("%d audit rows", n)
	}
}

func TestOnlyApprovedVerifiedRidersStart(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	id, token := f.rider()
	f.exec("UPDATE riders SET status = 'applicant' WHERE id = $1", id)
	if code, out := f.start(token); code != 409 || out["code"] != "rider_not_approved" {
		t.Errorf("an applicant: %d %v", code, out)
	}
	f.exec("UPDATE riders SET status = 'active' WHERE id = $1", id)
	f.exec("INSERT INTO rider_verifications (rider_id, status, method, created_at) VALUES ($1, 'expired', 'manual_upload', now() + interval '1 second')", id)
	if code, out := f.start(token); code != 409 || out["code"] != "rider_not_verified" {
		t.Errorf("expired documents: %d %v", code, out)
	}
	f.exec("INSERT INTO rider_verifications (rider_id, status, method, expires_at, created_at) VALUES ($1, 'verified', 'manual_upload', now() - interval '1 day', now() + interval '2 seconds')", id)
	if code, out := f.start(token); code != 409 || out["code"] != "rider_not_verified" {
		t.Errorf("a verification past its date: %d %v", code, out)
	}
	f.exec("UPDATE riders SET status = 'suspended' WHERE id = $1", id)
	if code, out := f.start(token); code != 403 || out["code"] != "account_blocked" {
		t.Errorf("suspended: %d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM shifts"); n != 0 {
		t.Error("a shift started")
	}
}

func TestNodesAndScooters(t *testing.T) {
	f := newFixture(t)
	_, token := f.rider()
	if code, out := f.start(token); code != 409 || out["code"] != "no_scooter_at_node" {
		t.Errorf("no scooter: %d %v", code, out)
	}
	other := f.id("INSERT INTO zones (name, city, status) VALUES ('Other', 'Bengaluru', 'active') RETURNING id::text")
	far := f.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, 'Far', 12.9, 77.6, 2) RETURNING id::text", other)
	for _, body := range []string{`{"node_id":"` + far + `"}`, `{"node_id":"00000000-0000-0000-0000-000000000000"}`} {
		if code, _ := f.send("POST", "/riders/me/shifts/start", token, body); code != 404 {
			t.Errorf("%s: %d", body, code)
		}
	}
	for _, body := range []string{`{}`, `{"node_id":"x"}`, `nope`} {
		if code, _ := f.send("POST", "/riders/me/shifts/start", token, body); code != 400 {
			t.Errorf("%s: %d", body, code)
		}
	}
	f.exec("INSERT INTO scooters (code, zone_id, node_id, status) VALUES ('SC-BROKEN', $1, $2, 'maintenance')", f.zone, f.node)
	if code, out := f.start(token); code != 409 || out["code"] != "no_scooter_at_node" {
		t.Errorf("a scooter in maintenance: %d %v", code, out)
	}
}

func TestTwoRidersNeverGetOneScooter(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	tokens := make([]string, 6)
	for i := range tokens {
		_, tokens[i] = f.rider()
	}
	var wg sync.WaitGroup
	codes := make(chan int, len(tokens))
	for _, tok := range tokens {
		wg.Add(1)
		go func(tok string) {
			defer wg.Done()
			code, _ := f.start(tok)
			codes <- code
		}(tok)
	}
	wg.Wait()
	close(codes)
	got := map[int]int{}
	for c := range codes {
		got[c]++
	}
	if got[200] != 1 || got[409] != len(tokens)-1 {
		t.Errorf("%v", got)
	}
}

func TestListingNodes(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	f.scooter("SC-2")
	_, token := f.rider()
	code, out := f.send("GET", "/nodes?zone_id="+f.zone, token, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "NodeList")
	nodes := out["nodes"].([]any)
	if len(nodes) != 1 || nodes[0].(map[string]any)["fullness_pct"] != float64(50) || out["assigned_node_id"] != nil {
		t.Errorf("%v", out)
	}
	check(t, nodes[0].(map[string]any), "Node")
	f.start(token)
	_, out = f.send("GET", "/nodes?zone_id="+f.zone, token, "")
	if out["assigned_node_id"] != f.node || out["nodes"].([]any)[0].(map[string]any)["fullness_pct"] != float64(25) {
		t.Errorf("after starting: %v", out)
	}
	other := f.id("INSERT INTO zones (name, city, status) VALUES ('Other', 'Bengaluru', 'active') RETURNING id::text")
	for _, z := range []string{"00000000-0000-0000-0000-000000000000", other} {
		if code, _ := f.send("GET", "/nodes?zone_id="+z, token, ""); code != 404 {
			t.Errorf("a zone that is not the rider's: %d", code)
		}
	}
	if code, _ := f.send("GET", "/nodes", token, ""); code != 400 {
		t.Errorf("no zone: %d", code)
	}
}

func TestScanningStock(t *testing.T) {
	f := newFixture(t)
	scooter := f.scooter("SC-1")
	lot := f.lot("LOT-0001", 10, 48*time.Hour)
	_, token := f.rider()
	if code, out := f.send("POST", "/riders/me/stock/scan", token, `{"lot_code":"LOT-0001","scanned_at":"2026-11-07T10:00:00Z"}`); code != 409 || out["code"] != "no_open_shift" {
		t.Errorf("no shift: %d %v", code, out)
	}
	f.start(token)
	code, out := f.send("POST", "/riders/me/stock/scan", token, `{"lot_code":"LOT-0001","scanned_at":"2026-11-07T10:00:00Z"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "StockUnitResponse")
	u := out["unit"].(map[string]any)
	check(t, u, "StockUnit")
	if u["lot_id"] != lot || u["item_id"] != f.item || u["temp_status"] != "ok" {
		t.Errorf("%v", u)
	}
	if n := f.count("SELECT on_hand FROM scooter_stock WHERE scooter_id = $1 AND lot_id = $2", scooter, lot); n != 10 {
		t.Errorf("on hand %d", n)
	}
	if n := f.count("SELECT coalesce(sum(delta_on_hand), 0) FROM stock_ledger WHERE lot_id = $1 AND reason = 'scan'", lot); n != 10 {
		t.Errorf("the ledger says %d", n)
	}
	if code, out := f.send("POST", "/riders/me/stock/scan", token, `{"lot_code":"LOT-0001","scanned_at":"2026-11-07T10:00:00Z"}`); code != 409 || out["code"] != "lot_already_on_scooter" {
		t.Errorf("scanned twice: %d %v", code, out)
	}
	code, out = f.send("GET", "/riders/me/stock", token, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "StockList")
	lots := out["lots"].([]any)
	if len(lots) != 1 || lots[0].(map[string]any)["qty"] != float64(10) {
		t.Errorf("%v", out)
	}
	check(t, lots[0].(map[string]any), "StockLot")
	// The stock stays with the scooter when the shift ends.
	f.send("POST", "/riders/me/shifts/end", token, "")
	if _, out := f.send("GET", "/riders/me/stock", token, ""); len(out["lots"].([]any)) != 0 {
		t.Errorf("no shift, so no scooter: %v", out)
	}
	if n := f.count("SELECT on_hand FROM scooter_stock WHERE lot_id = $1", lot); n != 10 {
		t.Error("ending a shift moved stock")
	}
}

func TestBadScans(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	f.lot("LOT-OLD", 5, -time.Hour)
	q := f.lot("LOT-QUAR", 5, time.Hour)
	f.exec("UPDATE lots SET status = 'quarantined' WHERE id = $1", q)
	hot := f.lot("LOT-HOT", 5, time.Hour)
	f.exec("UPDATE lots SET temp_status = 'alert' WHERE id = $1", hot)
	_, token := f.rider()
	f.start(token)
	for body, want := range map[string]string{
		`{"lot_code":"NOPE-1234","scanned_at":"2026-11-07T10:00:00Z"}`: "bad_scan",
		`{"lot_code":"LOT-OLD","scanned_at":"2026-11-07T10:00:00Z"}`:   "lot_expired",
		`{"lot_code":"LOT-QUAR","scanned_at":"2026-11-07T10:00:00Z"}`:  "lot_quarantined",
	} {
		if code, out := f.send("POST", "/riders/me/stock/scan", token, body); code != 409 || out["code"] != want {
			t.Errorf("%s: %d %v", body, code, out)
		}
	}
	for _, body := range []string{`{}`, `{"lot_code":"   "}`, `{"lot_code":"LOT-HOT"}`, `{"lot_code":"LOT-HOT","scanned_at":"yesterday"}`, `{"lot_code":"` + strings.Repeat("a", 65) + `"}`} {
		if code, _ := f.send("POST", "/riders/me/stock/scan", token, body); code != 400 {
			t.Errorf("%s: %d", body[:min(len(body), 30)], code)
		}
	}
	// A temperature alert loads, and tells the app, so it can show the quarantine prompt.
	if code, out := f.send("POST", "/riders/me/stock/scan", token, `{"lot_code":"LOT-HOT","scanned_at":"2026-11-07T10:00:00Z"}`); code != 200 || out["unit"].(map[string]any)["temp_status"] != "alert" {
		t.Errorf("%d %v", code, out)
	}
}

func TestOneLotCannotGoOnTwoScooters(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	f.scooter("SC-2")
	f.lot("LOT-0001", 10, time.Hour)
	_, a := f.rider()
	_, b := f.rider()
	f.start(a)
	f.start(b)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, tok := range []string{a, b} {
		wg.Add(1)
		go func(tok string) {
			defer wg.Done()
			code, _ := f.send("POST", "/riders/me/stock/scan", tok, `{"lot_code":"LOT-0001","scanned_at":"2026-11-07T10:00:00Z"}`)
			codes <- code
		}(tok)
	}
	wg.Wait()
	close(codes)
	got := map[int]int{}
	for c := range codes {
		got[c]++
	}
	if got[200] != 1 || got[409] != 1 {
		t.Errorf("%v", got)
	}
}

func TestStockRulesHoldInTheDatabase(t *testing.T) {
	f := newFixture(t)
	scooter := f.scooter("SC-1")
	lot := f.lot("LOT-0001", 10, time.Hour)
	f.exec("INSERT INTO scooter_stock (scooter_id, lot_id, on_hand) VALUES ($1, $2, 10)", scooter, lot)
	for _, sql := range []string{
		"UPDATE scooter_stock SET reserved = 11",
		"UPDATE scooter_stock SET on_hand = -1",
		"INSERT INTO stock_ledger (scooter_id, lot_id, delta_on_hand, reason, actor_type) VALUES ('" + scooter + "', '" + lot + "', 0, 'scan', 'rider')",
	} {
		if _, err := f.s.Pool.Exec(context.Background(), sql); err == nil {
			t.Errorf("allowed: %s", sql)
		}
	}
	f.exec("INSERT INTO stock_ledger (scooter_id, lot_id, delta_on_hand, reason, actor_type) VALUES ($1, $2, 10, 'scan', 'rider')", scooter, lot)
	for _, sql := range []string{"UPDATE stock_ledger SET delta_on_hand = 5", "DELETE FROM stock_ledger", "TRUNCATE stock_ledger"} {
		if _, err := f.s.Pool.Exec(context.Background(), sql); err == nil {
			t.Errorf("the ledger allowed: %s", sql)
		}
	}
}

func TestSignedOutAndBlockedRiders(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	if code, _ := f.send("GET", "/riders/me/stock", "", ""); code != 401 {
		t.Errorf("no token: %d", code)
	}
	_, token := f.rider()
	f.exec("UPDATE people SET status = 'blocked'")
	for _, c := range [][3]string{{"POST", "/riders/me/shifts/start", `{"node_id":"` + f.node + `"}`}, {"GET", "/riders/me/stock", ""}, {"GET", "/nodes?zone_id=" + f.zone, ""}} {
		if code, out := f.send(c[0], c[1], token, c[2]); code != 403 || out["code"] != "account_blocked" {
			t.Errorf("%s: %d %v", c[1], code, out)
		}
	}
	f.exec("UPDATE people SET status = 'active'")
	f.exec("UPDATE refresh_tokens SET revoked_at = now()")
	if code, _ := f.start(token); code != 401 {
		t.Errorf("an ended session: %d", code)
	}
	for _, p := range []string{"/riders/me/stock", "/nodes?zone_id=" + f.zone} {
		if code, _ := f.send("GET", p, token, ""); code != 401 {
			t.Errorf("an ended session reading %s: %d", p, code)
		}
	}
}

func TestAScooterLeftHoldingARiderIsFreed(t *testing.T) {
	f := newFixture(t)
	stuck := f.scooter("SC-STUCK")
	f.scooter("SC-2")
	id, token := f.rider()
	f.exec("UPDATE scooters SET current_rider_id = $1 WHERE id = $2", id, stuck)
	if code, out := f.start(token); code != 200 {
		t.Errorf("a stuck scooter stopped the rider: %d %v", code, out)
	}
}

func TestAVerificationTieIsNotVerified(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	id, token := f.rider()
	f.exec(`INSERT INTO rider_verifications (rider_id, status, method, created_at) VALUES ($1, 'verified', 'manual_upload', now() + interval '1 hour'),
		($1, 'failed', 'manual_upload', now() + interval '1 hour')`, id)
	if code, out := f.start(token); code != 409 || out["code"] != "rider_not_verified" {
		t.Errorf("%d %v", code, out)
	}
}

func TestNodesShowAMoveAndEndingTheShiftCancelsIt(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	rider, token := f.rider()
	f.start(token)
	to := f.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, 'Node B', 12.94, 77.63, 4) RETURNING id::text", f.zone)
	move := f.id(`INSERT INTO relocations (rider_id, shift_id, from_node_id, to_node_id, reason, expires_at)
		SELECT $1, id, node_id, $2, '2 orders expected, nobody nearby', now() + interval '5 minutes' FROM shifts WHERE rider_id = $1 AND ended_at IS NULL RETURNING id::text`, rider, to)
	_, out := f.send("GET", "/nodes?zone_id="+f.zone, token, "")
	check(t, out, "NodeList")
	m, _ := out["relocation"].(map[string]any)
	if m == nil || m["id"] != move || m["node_id"] != to {
		t.Fatalf("%v", out)
	}
	check(t, m, "RelocationSuggestion")
	if code, out := f.send("POST", "/riders/me/shifts/end", token, `{}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if f.id("SELECT state FROM relocations WHERE id = $1", move) != "cancelled" {
		t.Error("ending the shift must cancel the move")
	}
}

func TestEndingAShiftPaysTheMoveSoFar(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	rider, token := f.rider()
	f.start(token)
	to := f.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, 'Node B', 12.94, 77.63, 4) RETURNING id::text", f.zone)
	move := f.id(`INSERT INTO relocations (rider_id, shift_id, from_node_id, to_node_id, reason, state, expires_at, accepted_at)
		SELECT $1, id, node_id, $2, 'test', 'accepted', now() + interval '5 minutes', now() - interval '4 minutes' FROM shifts WHERE rider_id = $1 AND ended_at IS NULL RETURNING id::text`, rider, to)
	if code, out := f.send("POST", "/riders/me/shifts/end", token, `{}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if f.count("SELECT paid_seconds FROM earnings WHERE relocation_id = $1", move) < 240 {
		t.Error("the move so far was not paid when the shift ended")
	}
}
