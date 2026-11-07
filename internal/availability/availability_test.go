package availability

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
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// The customer stands at (12.935, 77.625). The node is about 220 m away. With the starting
// settings, reach is the nearer of 1,500 m and 15 km/h for 80 s, about 333 m.
const (
	custLat, custLng = 12.935, 77.625
	nodeLat, nodeLng = 12.937, 77.625
)

type fixture struct {
	t       *testing.T
	pool    *pgxpool.Pool
	srv     *httptest.Server
	svc     *Service
	zone    string
	node    string
	scooter string
	rider   string
	chai    string
	samosa  string
	token   string
	cust    string
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

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, pool: dbtest.Fresh(t)}
	sessions := &identity.Sessions{Pool: f.pool, Log: quiet, RefreshTTL: time.Hour,
		Signer: identity.Signer{Key: []byte("a-test-key-that-is-long-enough-to-use"), TTL: 15 * time.Minute}}
	f.svc = &Service{Pool: f.pool, Log: quiet}
	f.srv = httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		f.svc.MountCustomer(r, identity.RequireActor(sessions.Signer, identity.TypeCustomer))
	}))
	t.Cleanup(f.srv.Close)
	f.zone = f.id(`INSERT INTO zones (name, city, status, area) VALUES ('Z', 'Bengaluru', 'active',
		extensions.ST_GeomFromText('MULTIPOLYGON(((77.612 12.925, 77.640 12.925, 77.640 12.945, 77.612 12.945, 77.612 12.925)))', 4326)) RETURNING id::text`)
	f.node = f.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, 'N', $2, $3, 4) RETURNING id::text", f.zone, nodeLat, nodeLng)
	f.scooter = f.id("INSERT INTO scooters (code, zone_id, node_id) VALUES ('SC-1', $1, $2) RETURNING id::text", f.zone, f.node)
	f.chai = f.id("INSERT INTO menu_items (name, price_paise, status, sort_order) VALUES ('Chai', 4900, 'active', 1) RETURNING id::text")
	f.samosa = f.id("INSERT INTO menu_items (name, price_paise, status, sort_order) VALUES ('Samosa', 3900, 'active', 2) RETURNING id::text")
	person := f.id("INSERT INTO people (phone) VALUES ('+919800000501') RETURNING id::text")
	f.rider = f.id("INSERT INTO riders (person_id, status, zone_id) VALUES ($1, 'active', $2) RETURNING id::text", person, f.zone)
	f.exec("UPDATE scooters SET current_rider_id = $1 WHERE id = $2", f.rider, f.scooter)
	f.exec("INSERT INTO shifts (rider_id, node_id, scooter_id) VALUES ($1, $2, $3)", f.rider, f.node, f.scooter)
	f.load("L-CHAI-1", f.chai, 5)
	f.load("L-CHAI-2", f.chai, 3)
	f.load("L-SAMOSA", f.samosa, 4)
	cperson := f.id("INSERT INTO people (phone) VALUES ('+919800000502') RETURNING id::text")
	f.cust = f.id("INSERT INTO customers (person_id) VALUES ($1) RETURNING id::text", cperson)
	var pair identity.Pair
	if err := database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		var err error
		pair, _, err = sessions.Start(context.Background(), tx, identity.TypeCustomer, f.cust)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.token = pair.AccessToken
	return f
}

func (f *fixture) load(code, item string, qty int) string {
	f.t.Helper()
	lot := f.id("INSERT INTO lots (code, item_id, qty, expires_at) VALUES ($1, $2, $3, now() + interval '1 day') RETURNING id::text", code, item, qty)
	f.exec("INSERT INTO scooter_stock (scooter_id, lot_id, on_hand) VALUES ($1, $2, $3)", f.scooter, lot, qty)
	return lot
}

func (f *fixture) at(lat, lng float64) Result {
	f.t.Helper()
	res, err := At(context.Background(), f.pool, lat, lng, time.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

func qty(res Result, item string) int {
	for _, l := range res.Lines {
		if l.Item.ID == item {
			return l.QtyLeft
		}
	}
	return 0
}

func TestWhatIsInReach(t *testing.T) {
	f := newFixture(t)
	res := f.at(custLat, custLng)
	if !res.Offer || len(res.Lines) != 2 || qty(res, f.chai) != 8 || qty(res, f.samosa) != 4 {
		t.Fatalf("%+v", res)
	}
	if res.Lines[0].Item.ID != f.chai {
		t.Error("menu order is lost")
	}
	f.exec("UPDATE scooter_stock SET reserved = 2 WHERE lot_id = (SELECT id FROM lots WHERE code = 'L-CHAI-1')")
	if q := qty(f.at(custLat, custLng), f.chai); q != 6 {
		t.Errorf("held units must not count: %d", q)
	}
}

func TestOnlyGoodStockCounts(t *testing.T) {
	f := newFixture(t)
	f.exec("UPDATE lots SET status = 'quarantined' WHERE code = 'L-CHAI-1'")
	f.exec("UPDATE lots SET temp_status = 'alert' WHERE code = 'L-CHAI-2'")
	f.exec("UPDATE lots SET expires_at = now() - interval '1 minute' WHERE code = 'L-SAMOSA'")
	if res := f.at(custLat, custLng); res.Offer || res.Reason != ReasonSoldOut {
		t.Errorf("%+v", res)
	}
}

func TestReachFollowsTheSettings(t *testing.T) {
	f := newFixture(t)
	// About 440 m from the node: past reach.
	if res := f.at(12.933, 77.625); res.Offer || res.Reason != ReasonNoCoverage {
		t.Errorf("too far: %+v", res)
	}
	f.exec("INSERT INTO zone_settings (zone_id, version, sweet_spot_budget_s, reason, created_by) VALUES ($1, 1, 200, 'test', 'test')", f.zone)
	if res := f.at(12.933, 77.625); !res.Offer {
		t.Errorf("a longer budget in this zone must reach it: %+v", res)
	}
}

func TestOnlyRidersOnShiftCount(t *testing.T) {
	f := newFixture(t)
	f.exec("UPDATE riders SET status = 'suspended'")
	if res := f.at(custLat, custLng); res.Offer || res.Reason != ReasonNoCoverage {
		t.Errorf("a suspended rider: %+v", res)
	}
	f.exec("UPDATE riders SET status = 'active'")
	f.exec("UPDATE shifts SET ended_at = now()")
	if res := f.at(custLat, custLng); res.Offer || res.Reason != ReasonNoCoverage {
		t.Errorf("no shift: %+v", res)
	}
}

func TestAreasAndZones(t *testing.T) {
	f := newFixture(t)
	if res := f.at(13.1, 77.5); res.Offer || res.Reason != ReasonOutOfArea {
		t.Errorf("%+v", res)
	}
	f.exec("UPDATE zones SET status = 'paused'")
	if res := f.at(custLat, custLng); res.Offer || res.Reason != ReasonZoneOff {
		t.Errorf("%+v", res)
	}
}

// at is At at a fixed time in India, so the test never depends on when it runs.
func (f *fixture) atTime(weekday time.Weekday, hour, minute int) Result {
	f.t.Helper()
	// 2026-11-02 was a Monday.
	day := 2 + (int(weekday)+6)%7
	res, err := At(context.Background(), f.pool, custLat, custLng, time.Date(2026, 11, day, hour, minute, 0, 0, india))
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

func TestSellingHours(t *testing.T) {
	f := newFixture(t)
	// Chai from 10:00 to 11:00 every day. Samosa all day, on Sundays only.
	f.exec("INSERT INTO availability_rules (item_id, start_minute, end_minute) VALUES ($1, 600, 660)", f.chai)
	f.exec("INSERT INTO availability_rules (item_id, weekdays, start_minute, end_minute) VALUES ($1, 64, 0, 1440)", f.samosa)
	for _, c := range []struct {
		day          time.Weekday
		hour, minute int
		chai, samosa int
	}{
		{time.Monday, 9, 59, 0, 0},
		{time.Monday, 10, 0, 8, 0},
		{time.Monday, 10, 59, 8, 0},
		{time.Monday, 11, 0, 0, 0},
		{time.Sunday, 10, 30, 8, 4},
		{time.Sunday, 23, 59, 0, 4},
		{time.Sunday, 0, 0, 0, 4},
		{time.Saturday, 23, 59, 0, 0},
	} {
		res := f.atTime(c.day, c.hour, c.minute)
		if qty(res, f.chai) != c.chai || qty(res, f.samosa) != c.samosa {
			t.Errorf("%s %02d:%02d: chai %d, samosa %d", c.day, c.hour, c.minute, qty(res, f.chai), qty(res, f.samosa))
		}
	}
	// A rule for another zone does not apply here.
	other := f.id("INSERT INTO zones (name, city) VALUES ('Elsewhere', 'Bengaluru') RETURNING id::text")
	f.exec("DELETE FROM availability_rules WHERE item_id = $1", f.samosa)
	f.exec("INSERT INTO availability_rules (item_id, zone_id, start_minute, end_minute) VALUES ($1, $2, 0, 1)", f.samosa, other)
	if q := qty(f.atTime(time.Monday, 12, 0), f.samosa); q != 4 {
		t.Errorf("another zone's rule applied here: %d", q)
	}
}

func TestOneOrderComesFromOneScooter(t *testing.T) {
	f := newFixture(t)
	second := f.id("INSERT INTO scooters (code, zone_id, node_id) VALUES ('SC-2', $1, $2) RETURNING id::text", f.zone, f.node)
	person := f.id("INSERT INTO people (phone) VALUES ('+919800000504') RETURNING id::text")
	rider := f.id("INSERT INTO riders (person_id, status, zone_id) VALUES ($1, 'active', $2) RETURNING id::text", person, f.zone)
	f.exec("UPDATE scooters SET current_rider_id = $1 WHERE id = $2", rider, second)
	f.exec("INSERT INTO shifts (rider_id, node_id, scooter_id) VALUES ($1, $2, $3)", rider, f.node, second)
	lot := f.id("INSERT INTO lots (code, item_id, qty, expires_at) VALUES ('L-CHAI-B', $1, 6, now() + interval '1 day') RETURNING id::text", f.chai)
	f.exec("INSERT INTO scooter_stock (scooter_id, lot_id, on_hand) VALUES ($1, $2, 6)", second, lot)
	if q := qty(f.at(custLat, custLng), f.chai); q != 8 {
		t.Errorf("the best single scooter has 8, so 14 cannot be promised: %d", q)
	}
}

func TestARiderFromAnotherZoneDoesNotCount(t *testing.T) {
	f := newFixture(t)
	other := f.id("INSERT INTO zones (name, city) VALUES ('Elsewhere', 'Bengaluru') RETURNING id::text")
	f.exec("UPDATE riders SET zone_id = $1 WHERE id = $2", other, f.rider)
	if res := f.at(custLat, custLng); res.Offer || res.Reason != ReasonNoCoverage {
		t.Errorf("%+v", res)
	}
}

func get(t *testing.T, url, token string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func required(t *testing.T, schema string) []string {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/customer/openapi.yaml")
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

func TestTheEndpoint(t *testing.T) {
	f := newFixture(t)
	code, out := get(t, fmt.Sprintf("%s/v1/availability?lat=%v&lng=%v", f.srv.URL, custLat, custLng), f.token)
	if code != 200 || out["offer_available"] != true || out["unavailable_reason"] != nil {
		t.Fatalf("%d %v", code, out)
	}
	for _, field := range required(t, "Availability") {
		if _, ok := out[field]; !ok {
			t.Errorf("no %s", field)
		}
	}
	line := out["items"].([]any)[0].(map[string]any)
	for _, field := range required(t, "AvailableItem") {
		if _, ok := line[field]; !ok {
			t.Errorf("an item has no %s", field)
		}
	}
	gate := f.id("INSERT INTO gates (customer_id, zone_id, label, lat, lng, verified) VALUES ($1, $2, 'Home', $3, $4, true) RETURNING id::text", f.cust, f.zone, custLat, custLng)
	if code, out := get(t, f.srv.URL+"/v1/availability?gate_id="+gate, f.token); code != 200 || out["offer_available"] != true {
		t.Errorf("by gate: %d %v", code, out)
	}
	f.exec("UPDATE gates SET verified = false")
	if _, out := get(t, f.srv.URL+"/v1/availability?gate_id="+gate, f.token); out["offer_available"] != false || out["unavailable_reason"] != "gate_not_verified" {
		t.Errorf("an unverified gate: %v", out)
	}
	operson := f.id("INSERT INTO people (phone) VALUES ('+919800000503') RETURNING id::text")
	other := f.id("INSERT INTO customers (person_id) VALUES ($1) RETURNING id::text", operson)
	theirs := f.id("INSERT INTO gates (customer_id, zone_id, label, lat, lng, verified) VALUES ($1, $2, 'Theirs', $3, $4, true) RETURNING id::text", other, f.zone, custLat, custLng)
	for _, q := range []string{"gate_id=" + theirs, "gate_id=00000000-0000-0000-0000-000000000000", "gate_id=nope"} {
		if code, _ := get(t, f.srv.URL+"/v1/availability?"+q, f.token); code != 404 {
			t.Errorf("%s: %d", q, code)
		}
	}
	for _, q := range []string{"", "lat=12.9", "lat=abc&lng=77", "lat=NaN&lng=77", "lat=91&lng=77", "gate_id=" + gate + "&lat=1&lng=1"} {
		if code, _ := get(t, f.srv.URL+"/v1/availability?"+q, f.token); code != 400 {
			t.Errorf("%q: %d", q, code)
		}
	}
	f.exec("UPDATE people SET status = 'blocked' WHERE id = (SELECT person_id FROM customers WHERE id = $1)", f.cust)
	if code, out := get(t, fmt.Sprintf("%s/v1/availability?lat=%v&lng=%v", f.srv.URL, custLat, custLng), f.token); code != 403 || out["code"] != "account_blocked" {
		t.Errorf("blocked: %d %v", code, out)
	}
	f.exec("UPDATE people SET status = 'active'")
	f.exec("UPDATE refresh_tokens SET revoked_at = now()")
	if code, _ := get(t, fmt.Sprintf("%s/v1/availability?lat=%v&lng=%v", f.srv.URL, custLat, custLng), f.token); code != 401 {
		t.Errorf("an ended session: %d", code)
	}
}
