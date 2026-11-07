package geo

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

// Inside and outside the test zone.
const (
	inLat, inLng   = 12.935, 77.625
	outLat, outLng = 13.10, 77.50
)

type fixture struct {
	t    *testing.T
	s    *identity.Sessions
	srv  *httptest.Server
	keys int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, s: &identity.Sessions{
		Pool: dbtest.Fresh(t), Log: quiet, RefreshTTL: 90 * 24 * time.Hour,
		Signer: identity.Signer{Key: []byte("a-test-key-that-is-long-enough-to-use"), TTL: 15 * time.Minute},
	}}
	svc := &Service{Pool: f.s.Pool, Log: quiet}
	f.srv = httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		svc.MountCustomer(r, identity.RequireActor(f.s.Signer, identity.TypeCustomer))
	}))
	t.Cleanup(f.srv.Close)
	f.exec(`INSERT INTO zones (name, city, status, is_test, area) VALUES ('Zone (test)', 'Bengaluru', 'active', true,
		extensions.ST_GeomFromText('MULTIPOLYGON(((77.612 12.925, 77.640 12.925, 77.640 12.945, 77.612 12.945, 77.612 12.925)))', 4326))`)
	return f
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

func (f *fixture) customer(phone any) (string, string) {
	f.t.Helper()
	ctx := context.Background()
	var person, id string
	if err := f.s.Pool.QueryRow(ctx, "INSERT INTO people (phone) VALUES ($1) RETURNING id::text", phone).Scan(&person); err != nil {
		f.t.Fatal(err)
	}
	if err := f.s.Pool.QueryRow(ctx, "INSERT INTO customers (person_id) VALUES ($1) RETURNING id::text", person).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	var pair identity.Pair
	if err := database.InTx(ctx, f.s.Pool, func(tx pgx.Tx) error {
		var err error
		pair, _, err = f.s.Start(ctx, tx, identity.TypeCustomer, id)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return id, pair.AccessToken
}

func (f *fixture) send(method, path, token, body string) (int, map[string]any) {
	f.t.Helper()
	f.keys++
	req, err := http.NewRequest(method, f.srv.URL+"/v1"+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Idempotency-Key", fmt.Sprintf("gate-key-%04d", f.keys))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func gateBody(label string, lat, lng float64) string {
	return fmt.Sprintf(`{"label":%q,"lat":%v,"lng":%v}`, label, lat, lng)
}

func contractRequired(t *testing.T, schema string) []string {
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
	if len(doc.Components.Schemas[schema].Required) == 0 {
		t.Fatalf("no required fields for %s", schema)
	}
	return doc.Components.Schemas[schema].Required
}

func checkFields(t *testing.T, got map[string]any, schema string) {
	t.Helper()
	for _, field := range contractRequired(t, schema) {
		if _, ok := got[field]; !ok {
			t.Errorf("%s has no %s: %v", schema, field, got)
		}
	}
}

func TestSavingAndListingGates(t *testing.T) {
	f := newFixture(t)
	_, token := f.customer("+919800000001")
	code, out := f.send("GET", "/gates", token, "")
	if code != 200 || len(out["gates"].([]any)) != 0 {
		t.Fatalf("%d %v", code, out)
	}
	code, out = f.send("POST", "/gates", token, `{"label":"  Home gate ","lat":12.935,"lng":77.625,"entry_notes":"Ring twice\r\nThen wait"}`)
	if code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	checkFields(t, out, "GateResponse")
	g := out["gate"].(map[string]any)
	checkFields(t, g, "Gate")
	if g["label"] != "Home gate" || g["verified"] != true || g["entry_notes"] != "Ring twice\nThen wait" {
		t.Errorf("%v", g)
	}
	_, out = f.send("GET", "/gates", token, "")
	checkFields(t, out, "GateList")
	if gates := out["gates"].([]any); len(gates) != 1 || gates[0].(map[string]any)["id"] != g["id"] {
		t.Errorf("%v", out)
	}
	_, other := f.customer("+919800000002")
	if _, out := f.send("GET", "/gates", other, ""); len(out["gates"].([]any)) != 0 {
		t.Error("one customer saw another's gates")
	}
	if n := f.count("SELECT count(*) FROM gate_history WHERE gate_id = $1 AND version = 1 AND label = 'Home gate'", g["id"]); n != 1 {
		t.Error("the first version is not in the history")
	}
	if _, err := f.s.Pool.Exec(context.Background(), "UPDATE gate_history SET label = 'x'"); err == nil {
		t.Error("history could be rewritten")
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'gate.created' AND after::text NOT LIKE '%12.93%' AND after::text NOT LIKE '%Home%'"); n != 1 {
		t.Error("the audit row is missing, or holds where someone lives")
	}
}

func TestAPinOutsideTheServiceAreaIsRefused(t *testing.T) {
	f := newFixture(t)
	_, token := f.customer("+919800000001")
	if code, out := f.send("POST", "/gates", token, gateBody("Far", outLat, outLng)); code != 400 || out["code"] != "out_of_service_area" {
		t.Errorf("%d %v", code, out)
	}
	f.exec("UPDATE zones SET status = 'paused'")
	if code, out := f.send("POST", "/gates", token, gateBody("Home", inLat, inLng)); code != 400 || out["code"] != "out_of_service_area" {
		t.Errorf("a paused zone served a pin: %d %v", code, out)
	}
}

func TestAGateNeedsAVerifiedPhone(t *testing.T) {
	f := newFixture(t)
	_, token := f.customer(nil)
	if code, out := f.send("POST", "/gates", token, gateBody("Home", inLat, inLng)); code != 409 || out["code"] != "phone_required" {
		t.Errorf("%d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM gates"); n != 0 {
		t.Error("a gate was saved")
	}
}

func TestDuplicatesAndTheLimit(t *testing.T) {
	f := newFixture(t)
	_, token := f.customer("+919800000001")
	f.send("POST", "/gates", token, gateBody("Home", inLat, inLng))
	// About 5 m away is the same gate. About 50 m away is another.
	if code, out := f.send("POST", "/gates", token, gateBody("Home again", inLat+0.00004, inLng)); code != 409 || out["code"] != "duplicate_gate" {
		t.Errorf("%d %v", code, out)
	}
	if code, _ := f.send("POST", "/gates", token, gateBody("Office", inLat+0.00045, inLng)); code != 201 {
		t.Errorf("a gate 50 m away: %d", code)
	}
	_, other := f.customer("+919800000002")
	if code, _ := f.send("POST", "/gates", other, gateBody("Same building", inLat, inLng)); code != 201 {
		t.Errorf("another customer at the same gate: %d", code)
	}
	for i := 2; i < maxGates; i++ {
		if code, out := f.send("POST", "/gates", token, gateBody(fmt.Sprint("Gate ", i), inLat+float64(i)*0.0005, inLng)); code != 201 {
			t.Fatalf("gate %d: %d %v", i, code, out)
		}
	}
	if code, out := f.send("POST", "/gates", token, gateBody("One too many", 12.926, 77.613)); code != 409 || out["code"] != "gate_limit" {
		t.Errorf("%d %v", code, out)
	}
}

func TestBadGatesAreRefused(t *testing.T) {
	f := newFixture(t)
	_, token := f.customer("+919800000001")
	for _, body := range []string{
		`{}`, `not json`, `{"label":"A","lat":12.9}`, `{"label":"A","lng":77.6}`, `{"label":"","lat":12.935,"lng":77.625}`,
		`{"label":"   ","lat":12.935,"lng":77.625}`, `{"label":"A","lat":91,"lng":77.625}`, `{"label":"A","lat":12.935,"lng":181}`,
		`{"label":"a‮b","lat":12.935,"lng":77.625}`, `{"label":"a\nb","lat":12.935,"lng":77.625}`,
		`{"label":"A","lat":"12.9","lng":77.625}`, `{"label":"` + strings.Repeat("a", 81) + `","lat":12.935,"lng":77.625}`,
		`{"label":"A","lat":12.935,"lng":77.625,"entry_notes":"` + strings.Repeat("a", 501) + `"}`,
		`{"label":"A","lat":12.935,"lng":77.625,"entry_notes":"a\u0000b"}`,
		`{"label":"a\u200fb","lat":12.935,"lng":77.625}`, `{"label":"a\u061cb","lat":12.935,"lng":77.625}`,
		`{"label":"A","lat":12.935,"lng":77.625,"entry_notes":"a\u2028b"}`,
	} {
		if code, out := f.send("POST", "/gates", token, body); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", body[:min(len(body), 40)], code, out)
		}
	}
}

func TestSignedOutAndBlockedCustomers(t *testing.T) {
	f := newFixture(t)
	if code, _ := f.send("GET", "/gates", "", ""); code != 401 {
		t.Errorf("no token: %d", code)
	}
	_, token := f.customer("+919800000001")
	f.exec("UPDATE people SET status = 'blocked'")
	for _, c := range [][3]string{{"GET", "/gates", ""}, {"POST", "/gates", gateBody("Home", inLat, inLng)}} {
		if code, out := f.send(c[0], c[1], token, c[2]); code != 403 || out["code"] != "account_blocked" {
			t.Errorf("%s: %d %v", c[1], code, out)
		}
	}
	f.exec("UPDATE people SET status = 'active'")
	f.exec("UPDATE refresh_tokens SET revoked_at = now()")
	for _, c := range [][3]string{{"GET", "/gates", ""}, {"POST", "/gates", gateBody("Home", inLat, inLng)}} {
		if code, _ := f.send(c[0], c[1], token, c[2]); code != 401 {
			t.Errorf("an ended session, %s: %d", c[1], code)
		}
	}
}

func TestCheckingALocationSavesNothing(t *testing.T) {
	f := newFixture(t)
	_, token := f.customer(nil) // A Google customer with no phone yet can look.
	code, out := f.send("GET", fmt.Sprintf("/locations/check?lat=%v&lng=%v", inLat, inLng), token, "")
	if code != 200 || out["in_service_area"] != true || out["zone_id"] == nil {
		t.Fatalf("%d %v", code, out)
	}
	checkFields(t, out, "LocationCheck")
	code, out = f.send("GET", fmt.Sprintf("/locations/check?lat=%v&lng=%v", outLat, outLng), token, "")
	if code != 200 || out["in_service_area"] != false || out["zone_id"] != nil {
		t.Errorf("%d %v", code, out)
	}
	for _, q := range []string{"", "?lat=12.9", "?lat=abc&lng=77.6", "?lat=91&lng=77.6", "?lat=NaN&lng=77.6", "?lat=12.9&lng=Inf"} {
		if code, _ := f.send("GET", "/locations/check"+q, token, ""); code != 400 {
			t.Errorf("%q: %d", q, code)
		}
	}
	if code, _ := f.send("GET", "/locations/check?lat=12.935&lng=77.625", "", ""); code != 401 {
		t.Errorf("no token: %d", code)
	}
	f.exec("UPDATE refresh_tokens SET revoked_at = now()")
	if code, _ := f.send("GET", "/locations/check?lat=12.935&lng=77.625", token, ""); code != 401 {
		t.Errorf("an ended session: %d", code)
	}
	if n := f.count("SELECT count(*) FROM gates") + f.count("SELECT count(*) FROM audit_events WHERE action LIKE 'gate.%'"); n != 0 {
		t.Error("a check saved something")
	}
}

func TestZoneAtEdgesAndOverlaps(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, ok, err := ZoneAt(ctx, f.s.Pool, 12.925, 77.612); err != nil || !ok {
		t.Errorf("a corner of the zone must count: %v %v", ok, err)
	}
	f.exec(`INSERT INTO zones (name, city, status, area) VALUES ('Later zone', 'Bengaluru', 'active',
		extensions.ST_GeomFromText('MULTIPOLYGON(((77.62 12.93, 77.63 12.93, 77.63 12.94, 77.62 12.94, 77.62 12.93)))', 4326))`)
	var first string
	if err := f.s.Pool.QueryRow(ctx, "SELECT id::text FROM zones WHERE name = 'Zone (test)'").Scan(&first); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if id, _, _ := ZoneAt(ctx, f.s.Pool, inLat, inLng); id != first {
			t.Fatal("overlapping zones must give the same answer every time")
		}
	}
	if _, err := f.s.Pool.Exec(ctx, `INSERT INTO zones (name, city, area) VALUES ('Bad shape', 'Bengaluru',
		extensions.ST_GeomFromText('MULTIPOLYGON(((77.6 12.9, 77.7 13.0, 77.7 12.9, 77.6 13.0, 77.6 12.9)))', 4326))`); err == nil {
		t.Error("a self-crossing zone was stored")
	}
}

func TestAnIndianNameWithAJoinerIsKept(t *testing.T) {
	f := newFixture(t)
	_, token := f.customer("+919800000001")
	if code, out := f.send("POST", "/gates", token, `{"label":"क्\u200dष गेट","lat":12.935,"lng":77.625}`); code != 201 {
		t.Errorf("%d %v", code, out)
	}
}

func TestTwoTapsSaveOneGate(t *testing.T) {
	f := newFixture(t)
	_, token := f.customer("+919800000001")
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			req, _ := http.NewRequest("POST", f.srv.URL+"/v1/gates", strings.NewReader(gateBody("Home", inLat, inLng)))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Idempotency-Key", fmt.Sprintf("two-taps-%d-key", i))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				codes <- 0
				return
			}
			resp.Body.Close()
			codes <- resp.StatusCode
		}(i)
	}
	got := map[int]int{<-codes: 1}
	got[<-codes]++
	if got[201] != 1 || got[409] != 1 {
		t.Errorf("want one saved and one duplicate: %v", got)
	}
}

func TestErasingACustomersGates(t *testing.T) {
	f := newFixture(t)
	id, token := f.customer("+919800000001")
	_, out := f.send("POST", "/gates", token, `{"label":"Flat 4B","lat":12.93512,"lng":77.62534,"entry_notes":"Blue door"}`)
	gid := out["gate"].(map[string]any)["id"].(string)
	if err := database.InTx(context.Background(), f.s.Pool, func(tx pgx.Tx) error { return RedactGates(context.Background(), tx, id) }); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM gate_history WHERE gate_id = $1 AND label = '(removed)' AND entry_notes IS NULL
		AND lat = 12.94 AND lng = 77.63 AND redacted_at IS NOT NULL`, gid); n != 1 {
		t.Error("the history still holds the address")
	}
	if n := f.count(`SELECT count(*) FROM gates WHERE id = $1 AND label = '(removed)' AND entry_notes IS NULL AND lat = 12.94`, gid); n != 1 {
		t.Error("the gate still holds the address")
	}
	// Redaction is the only change history allows, and only once.
	for _, sql := range []string{
		"UPDATE gate_history SET label = 'x'",
		"UPDATE gate_history SET redacted_at = now()",
		"DELETE FROM gate_history",
	} {
		if _, err := f.s.Pool.Exec(context.Background(), sql); err == nil {
			t.Errorf("%s was allowed", sql)
		}
	}
}

func TestAnEmptyZoneIsRefused(t *testing.T) {
	f := newFixture(t)
	if _, err := f.s.Pool.Exec(context.Background(), `INSERT INTO zones (name, city, area) VALUES ('Empty', 'Bengaluru',
		extensions.ST_GeomFromText('MULTIPOLYGON EMPTY', 4326))`); err == nil {
		t.Error("an empty area was stored")
	}
}
