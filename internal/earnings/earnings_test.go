package earnings

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

type fixture struct {
	t        *testing.T
	pool     *pgxpool.Pool
	srv      *httptest.Server
	sessions *identity.Sessions
	zone     string
	node     string
	scooter  string
	n        int
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

func (f *fixture) tx(fn func(pgx.Tx) error) {
	f.t.Helper()
	if err := database.InTx(context.Background(), f.pool, fn); err != nil {
		f.t.Fatal(err)
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, pool: dbtest.Fresh(t)}
	f.sessions = &identity.Sessions{Pool: f.pool, Log: quiet, RefreshTTL: time.Hour,
		Signer: identity.Signer{Key: []byte("a-test-key-that-is-long-enough-to-use"), TTL: 15 * time.Minute}}
	svc := &Service{Pool: f.pool, Log: quiet}
	f.srv = httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		svc.MountRider(r, identity.RequireActor(f.sessions.Signer, identity.TypeRider))
	}))
	t.Cleanup(f.srv.Close)
	f.zone = f.id(`INSERT INTO zones (name, city, status, area) VALUES ('Z', 'Bengaluru', 'active',
		extensions.ST_GeomFromText('MULTIPOLYGON(((77.612 12.925, 77.640 12.925, 77.640 12.945, 77.612 12.945, 77.612 12.925)))', 4326)) RETURNING id::text`)
	f.node = f.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, 'A', 12.930, 77.620, 6) RETURNING id::text", f.zone)
	f.scooter = f.id("INSERT INTO scooters (code, zone_id, node_id) VALUES ('SC-E', $1, $2) RETURNING id::text", f.zone, f.node)
	return f
}

func (f *fixture) rider() (string, string) {
	f.t.Helper()
	f.n++
	person := f.id("INSERT INTO people (phone) VALUES ($1) RETURNING id::text", fmt.Sprintf("+9198000008%02d", f.n))
	rider := f.id("INSERT INTO riders (person_id, status, zone_id) VALUES ($1, 'active', $2) RETURNING id::text", person, f.zone)
	var pair identity.Pair
	f.tx(func(tx pgx.Tx) error {
		var err error
		pair, _, err = f.sessions.Start(context.Background(), tx, identity.TypeRider, rider)
		return err
	})
	return rider, pair.AccessToken
}

func (f *fixture) order() string {
	f.t.Helper()
	f.n++
	person := f.id("INSERT INTO people (phone) VALUES ($1) RETURNING id::text", fmt.Sprintf("+9197000008%02d", f.n))
	customer := f.id("INSERT INTO customers (person_id) VALUES ($1) RETURNING id::text", person)
	gate := f.id("INSERT INTO gates (customer_id, zone_id, label, lat, lng) VALUES ($1, $2, 'Gate', 12.93, 77.62) RETURNING id::text", customer, f.zone)
	history := f.id("INSERT INTO gate_history (gate_id, version, label, lat, lng, verified, reason) VALUES ($1, 1, 'Gate', 12.93, 77.62, false, 'test') RETURNING id::text", gate)
	return f.id(`INSERT INTO orders (customer_id, idempotency_key, gate_id, gate_history_id, zone_id, scooter_id, global_settings, state, total_paise, hold_until, paid_at)
		VALUES ($1, $2, $3, $4, $5, $6, (SELECT id FROM zone_settings WHERE zone_id IS NULL ORDER BY version DESC LIMIT 1), 'COMPLETED', 9900, now(), now())
		RETURNING id::text`, customer, fmt.Sprintf("key-%04d", f.n), gate, history, f.zone, f.scooter)
}

func (f *fixture) handed(rider, order string, at time.Time) {
	f.t.Helper()
	f.tx(func(tx pgx.Tx) error { return RecordOrder(context.Background(), tx, rider, order, at) })
}

func (f *fixture) get(token, date string) (int, map[string]any) {
	f.t.Helper()
	req, _ := http.NewRequest("GET", f.srv.URL+"/v1/riders/me/earnings?date="+date, nil)
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

// required lists a contract schema's required fields.
func required(t *testing.T, schema string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "contracts", "rider", "openapi.yaml"))
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
	for _, k := range required(t, schema) {
		if _, ok := got[k]; !ok {
			t.Errorf("%s is missing %s: %v", schema, k, got)
		}
	}
}

// noon is 12:00 local time on a fixed day.
var noon = time.Date(2026, 11, 10, 12, 0, 0, 0, Local)

func TestADayOfEarnings(t *testing.T) {
	f := newFixture(t)
	rider, token := f.rider()
	first, second := f.order(), f.order()
	f.handed(rider, first, noon)
	f.handed(rider, second, noon.Add(time.Hour))
	f.handed(rider, first, noon.Add(2*time.Hour)) // A repeat pays nothing more.
	code, out := f.get(token, "2026-11-10")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "Earnings")
	list := out["order_earnings"].([]any)
	if out["orders"] != float64(2) || len(list) != 2 || list[0].(map[string]any)["order_id"] != first {
		t.Fatalf("%v", out)
	}
	check(t, list[0].(map[string]any), "OrderEarning")
	if amt := list[0].(map[string]any)["amount"].(map[string]any); amt["amount_minor"] != float64(4000) || amt["currency"] != "INR" {
		t.Errorf("%v", amt)
	}
	if out["total"].(map[string]any)["amount_minor"] != float64(8000) || out["incentives"].(map[string]any)["amount_minor"] != float64(0) {
		t.Errorf("%v", out)
	}
	if _, other := f.get(token, "2026-11-11"); other["orders"] != float64(0) {
		t.Errorf("another day: %v", other)
	}
}

func TestTheDayIsTheLocalDay(t *testing.T) {
	f := newFixture(t)
	rider, token := f.rider()
	late := time.Date(2026, 11, 10, 23, 50, 0, 0, Local) // 18:20 UTC, the same UTC day as the next.
	early := time.Date(2026, 11, 11, 0, 20, 0, 0, Local) // 18:50 UTC on 10 November.
	f.handed(rider, f.order(), late)
	f.handed(rider, f.order(), early)
	if _, out := f.get(token, "2026-11-10"); out["orders"] != float64(1) {
		t.Errorf("10 November: %v", out)
	}
	if _, out := f.get(token, "2026-11-11"); out["orders"] != float64(1) {
		t.Errorf("11 November: %v", out)
	}
}

func TestTheIncentiveIsEarnedOnceAtTheLevel(t *testing.T) {
	f := newFixture(t)
	rider, token := f.rider()
	f.exec("INSERT INTO zone_settings (zone_id, version, incentive_orders, incentive_paise, pay_per_order_paise, reason, created_by) VALUES ($1, 1, 3, 15000, 3000, 'test', 'test')", f.zone)
	for i := 0; i < 2; i++ {
		f.handed(rider, f.order(), noon.Add(time.Duration(i)*time.Minute))
	}
	if f.count("SELECT count(*) FROM earnings WHERE kind = 'incentive'") != 0 {
		t.Fatal("an incentive below the level")
	}
	for i := 0; i < 3; i++ {
		f.handed(rider, f.order(), noon.Add(time.Duration(10+i)*time.Minute))
	}
	_, out := f.get(token, "2026-11-10")
	if out["incentives"].(map[string]any)["amount_minor"] != float64(15000) || out["total"].(map[string]any)["amount_minor"] != float64(5*3000+15000) {
		t.Errorf("one incentive and the zone's rate: %v", out)
	}
	// The next day starts again.
	f.handed(rider, f.order(), noon.Add(24*time.Hour))
	if f.count("SELECT count(*) FROM earnings WHERE kind = 'incentive'") != 1 {
		t.Error("an incentive on the first order of a new day")
	}
}

func TestARateChangeKeepsPastPay(t *testing.T) {
	f := newFixture(t)
	rider, token := f.rider()
	f.handed(rider, f.order(), noon)
	f.exec("INSERT INTO zone_settings (zone_id, version, pay_per_order_paise, reason, created_by) VALUES ($1, 1, 5500, 'raise', 'test')", f.zone)
	f.handed(rider, f.order(), noon.Add(time.Hour))
	if _, out := f.get(token, "2026-11-10"); out["total"].(map[string]any)["amount_minor"] != float64(4000+5500) {
		t.Errorf("%v", out)
	}
}

func (f *fixture) move(rider, state string, accepted, ended time.Duration) string {
	f.t.Helper()
	to := f.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, $2, 12.94, 77.63, 4) RETURNING id::text", f.zone, fmt.Sprintf("B-%d", f.n))
	f.n++
	shift := f.id("INSERT INTO shifts (rider_id, node_id, scooter_id, started_at, ended_at) VALUES ($1, $2, $3, $4, $4) RETURNING id::text", rider, f.node, f.scooter, noon.Add(-2*time.Hour))
	var arrived any
	if state == "arrived" {
		arrived = noon.Add(ended)
	}
	return f.id(`INSERT INTO relocations (rider_id, shift_id, from_node_id, to_node_id, reason, state, expires_at, accepted_at, arrived_at, state_changed_at)
		VALUES ($1, $2, $3, $4, 'test', $5, $6, $7, $8, $9) RETURNING id::text`,
		rider, shift, f.node, to, state, noon, noon.Add(accepted), arrived, noon.Add(ended))
}

func TestMovingIsPaidTime(t *testing.T) {
	f := newFixture(t)
	rider, token := f.rider()
	arrived := f.move(rider, "arrived", 0, 7*time.Minute+30*time.Second) // 8 started minutes.
	interrupted := f.move(rider, "cancelled", 0, 3*time.Minute)          // An offer came after 3 minutes.
	long := f.move(rider, "expired", 0, time.Hour)                       // Capped at 20 minutes.
	for _, id := range []string{arrived, interrupted, long, arrived} {
		f.tx(func(tx pgx.Tx) error { return RecordRelocation(context.Background(), tx, id) })
	}
	if f.count("SELECT amount_paise FROM earnings WHERE relocation_id = $1", arrived) != 8*200 || f.count("SELECT paid_seconds FROM earnings WHERE relocation_id = $1", arrived) != 450 {
		t.Error("an arrived move is paid by the started minute")
	}
	if f.count("SELECT amount_paise FROM earnings WHERE relocation_id = $1", interrupted) != 3*200 {
		t.Error("an interrupted move is paid up to the interruption")
	}
	if f.count("SELECT paid_seconds FROM earnings WHERE relocation_id = $1", long) != 1200 {
		t.Error("paid moving time is capped")
	}
	_, out := f.get(token, "2026-11-10")
	if out["repositioning_seconds"] != float64(450+180+1200) || out["total"].(map[string]any)["amount_minor"] != float64((8+3+20)*200) {
		t.Errorf("%v", out)
	}
}

func TestAMoveNeverAcceptedOrNotEndedIsNotPaid(t *testing.T) {
	f := newFixture(t)
	rider, _ := f.rider()
	open := f.move(rider, "accepted", 0, time.Minute)
	f.exec("UPDATE relocations SET arrived_at = NULL WHERE id = $1", open)
	ignored := f.move(rider, "expired", 0, 5*time.Minute)
	f.exec("UPDATE relocations SET accepted_at = NULL WHERE id = $1", ignored)
	for _, id := range []string{open, ignored} {
		f.tx(func(tx pgx.Tx) error { return RecordRelocation(context.Background(), tx, id) })
	}
	if f.count("SELECT count(*) FROM earnings") != 0 {
		t.Error("paid for a move still open, or never accepted")
	}
}

func TestEarningsRequests(t *testing.T) {
	f := newFixture(t)
	rider, token := f.rider()
	_, other := f.rider()
	f.handed(rider, f.order(), noon)
	if _, out := f.get(other, "2026-11-10"); out["orders"] != float64(0) {
		t.Errorf("another rider's earnings were shown: %v", out)
	}
	for _, d := range []string{"", "2026-13-01", "10-11-2026", "2026-11-10T00:00:00Z", "2026-02-30"} {
		if code, _ := f.get(token, d); code != 400 {
			t.Errorf("%q: %d", d, code)
		}
	}
	if code, _ := f.get("nope", "2026-11-10"); code != 401 {
		t.Errorf("signed out: %d", code)
	}
	f.exec("UPDATE riders SET status = 'suspended' WHERE id = $1", rider)
	if code, out := f.get(token, "2026-11-10"); code != 200 || out["orders"] != float64(1) {
		t.Errorf("a suspended rider still sees what they earned: %d %v", code, out)
	}
	f.exec("UPDATE people SET status = 'blocked' WHERE id = (SELECT person_id FROM riders WHERE id = $1)", rider)
	if code, _ := f.get(token, "2026-11-10"); code != 403 {
		t.Errorf("a blocked person: %d", code)
	}
}

// TestTwoHandoversAtOnceEarnOneIncentive runs two handovers for one rider in two transactions at
// once, at a level of two. The second must wait and count the first.
func TestTwoHandoversAtOnceEarnOneIncentive(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rider, _ := f.rider()
	f.exec("INSERT INTO zone_settings (zone_id, version, incentive_orders, reason, created_by) VALUES ($1, 1, 2, 'test', 'test')", f.zone)
	first, second := f.order(), f.order()
	tx1, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx1.Rollback(ctx)
	if err := RecordOrder(ctx, tx1, rider, first, noon); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- database.InTx(ctx, f.pool, func(tx pgx.Tx) error { return RecordOrder(ctx, tx, rider, second, noon.Add(time.Minute)) })
	}()
	time.Sleep(500 * time.Millisecond)
	if err := tx1.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if f.count("SELECT count(*) FROM earnings WHERE kind = 'incentive'") != 1 {
		t.Error("two handovers at once missed the incentive")
	}
}

func TestRepositioningPayIsShown(t *testing.T) {
	f := newFixture(t)
	rider, token := f.rider()
	id := f.move(rider, "arrived", 0, 5*time.Minute)
	f.tx(func(tx pgx.Tx) error { return RecordRelocation(context.Background(), tx, id) })
	f.handed(rider, f.order(), noon)
	_, out := f.get(token, "2026-11-10")
	if out["repositioning"].(map[string]any)["amount_minor"] != float64(1000) || out["total"].(map[string]any)["amount_minor"] != float64(5000) {
		t.Errorf("%v", out)
	}
}
