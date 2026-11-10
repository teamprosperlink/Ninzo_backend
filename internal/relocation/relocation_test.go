package relocation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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
	svc      *Service
	zone     string
	a, b, c  string // Nodes: a and b are close together, c is across the zone.
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
	f.a = f.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, 'A', 12.930, 77.620, 6) RETURNING id::text", f.zone)
	f.b = f.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, 'B', 12.932, 77.622, 6) RETURNING id::text", f.zone)
	f.c = f.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, 'C', 12.942, 77.636, 6) RETURNING id::text", f.zone)
	return f
}

// rider starts a shift at a node, a minute after the last one, and returns the rider and a token.
func (f *fixture) rider(node string) (string, string) {
	f.t.Helper()
	f.n++
	person := f.id("INSERT INTO people (phone) VALUES ($1) RETURNING id::text", fmt.Sprintf("+9198000007%02d", f.n))
	rider := f.id("INSERT INTO riders (person_id, status, zone_id) VALUES ($1, 'active', $2) RETURNING id::text", person, f.zone)
	scooter := f.id("INSERT INTO scooters (code, zone_id, node_id, current_rider_id) VALUES ($1, $2, $3, $4) RETURNING id::text", fmt.Sprintf("SC-%02d", f.n), f.zone, node, rider)
	f.exec("INSERT INTO shifts (rider_id, node_id, scooter_id, started_at) VALUES ($1, $2, $3, now() - make_interval(mins => 60 - $4))", rider, node, scooter, f.n)
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

// orders adds paid orders to gates at a point, created ago before now.
func (f *fixture) orders(n int, lat, lng float64, ago time.Duration) []string {
	f.t.Helper()
	ids := []string{}
	for i := 0; i < n; i++ {
		f.n++
		person := f.id("INSERT INTO people (phone) VALUES ($1) RETURNING id::text", fmt.Sprintf("+9197000007%02d", f.n))
		customer := f.id("INSERT INTO customers (person_id) VALUES ($1) RETURNING id::text", person)
		gate := f.id("INSERT INTO gates (customer_id, zone_id, label, lat, lng) VALUES ($1, $2, 'Gate', $3, $4) RETURNING id::text", customer, f.zone, lat, lng)
		history := f.id("INSERT INTO gate_history (gate_id, version, label, lat, lng, verified, reason) VALUES ($1, 1, 'Gate', $2, $3, false, 'test') RETURNING id::text", gate, lat, lng)
		scooter := f.id("SELECT id::text FROM scooters LIMIT 1")
		ids = append(ids, f.id(`INSERT INTO orders (customer_id, idempotency_key, gate_id, gate_history_id, zone_id, scooter_id, global_settings, state, total_paise,
				hold_until, paid_at, created_at, ended_reason, ended_at)
			VALUES ($1, $2, $3, $4, $5, $6, (SELECT id FROM zone_settings WHERE zone_id IS NULL ORDER BY version DESC LIMIT 1), 'COMPLETED', 9900,
				now(), now() - make_interval(secs => $7), now() - make_interval(secs => $7), NULL, NULL) RETURNING id::text`,
			customer, fmt.Sprintf("order-key-%04d", f.n), gate, history, f.zone, scooter, ago.Seconds()))
	}
	return ids
}

func (f *fixture) post(path, token string) (int, map[string]any) {
	f.t.Helper()
	f.n++
	req, _ := http.NewRequest("POST", f.srv.URL+"/v1"+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", fmt.Sprintf("relocation-key-%05d", f.n))
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
	return f.id("SELECT state FROM relocations WHERE id = $1", id)
}

func (f *fixture) suggestion(rider string) string {
	return f.id("SELECT id::text FROM relocations WHERE rider_id = $1 AND state = 'suggested'", rider)
}

func TestAShortNodeAsksTheNearestSpareRider(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first, _ := f.rider(f.b) // Waiting longest at B, the node nearest C.
	f.rider(f.b)
	f.rider(f.a)
	f.rider(f.a)
	f.orders(3, 12.9425, 77.6365, 10*time.Minute) // Near C, where nobody waits.
	now := time.Now()
	if n, err := f.svc.Check(ctx, now); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	id := f.suggestion(first)
	if f.id("SELECT to_node_id::text FROM relocations WHERE id = $1", id) != f.c || f.id("SELECT from_node_id::text FROM relocations WHERE id = $1", id) != f.b {
		t.Error("the move must go from the nearest node with a spare rider to the short one")
	}
	cur, err := Current(ctx, f.pool, first, now)
	if err != nil || cur == nil {
		t.Fatalf("%v %v", cur, err)
	}
	if m := cur.(map[string]any); m["node_id"] != f.c || m["reason"] != "3 orders expected, nobody nearby" {
		t.Errorf("%v", m)
	}
	if n, _ := f.svc.Check(ctx, now); n != 0 {
		t.Error("a second move toward the same node")
	}
	if f.count("SELECT count(*) FROM outbox_events WHERE topic = 'push.rider'") != 1 {
		t.Error("the rider was not told")
	}
}

func TestNoMoveWithoutDemandOrASpareRider(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.rider(f.a)
	f.orders(3, 12.9425, 77.6365, 10*time.Minute)
	if n, _ := f.svc.Check(ctx, time.Now()); n != 0 {
		t.Error("the only rider at a node was moved away")
	}
	f.rider(f.a)
	f.exec("DELETE FROM orders")
	f.orders(1, 12.9425, 77.6365, 10*time.Minute) // Below the 2 orders the rule needs.
	f.orders(5, 12.9425, 77.6365, 40*time.Minute) // Outside the 30-minute window.
	if n, _ := f.svc.Check(ctx, time.Now()); n != 0 {
		t.Error("moved for orders below the level or outside the window")
	}
	f.orders(1, 12.9425, 77.6365, 5*time.Minute)
	if n, _ := f.svc.Check(ctx, time.Now()); n != 1 {
		t.Error("two recent orders and a spare rider must make a move")
	}
}

func TestADonorKeepsOneRider(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.rider(f.a)
	f.rider(f.a)
	f.orders(4, 12.9425, 77.6365, 5*time.Minute) // Near C.
	f.orders(2, 12.9321, 77.6221, 5*time.Minute) // Near B.
	// Two riders at A, two short nodes: one rider moves, to C, which has more orders.
	if n, _ := f.svc.Check(ctx, time.Now()); n != 1 || f.count("SELECT count(*) FROM relocations WHERE to_node_id = $1", f.c) != 1 {
		t.Fatalf("%d", n)
	}
	f.exec("DELETE FROM relocations")
	f.rider(f.a)
	// Three riders at A: both short nodes get one.
	if n, _ := f.svc.Check(ctx, time.Now()); n != 2 || f.count("SELECT count(*) FROM relocations WHERE to_node_id = $1", f.b) != 1 {
		t.Errorf("%d", n)
	}
}

func TestAcceptingAMove(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rider, token := f.rider(f.b)
	f.rider(f.b)
	f.orders(2, 12.9425, 77.6365, 5*time.Minute)
	f.svc.Check(ctx, time.Now())
	id := f.suggestion(rider)
	if code, out := f.post("/riders/me/relocations/"+id+"/accept", token); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if f.state(id) != "accepted" || f.id("SELECT node_id::text FROM shifts WHERE rider_id = $1 AND ended_at IS NULL", rider) != f.c ||
		f.id("SELECT node_id::text FROM scooters WHERE current_rider_id = $1", rider) != f.c {
		t.Error("accepting must move the rider's assigned node and scooter")
	}
	if f.count("SELECT count(*) FROM relocations WHERE id = $1 AND accepted_at IS NOT NULL", id) != 1 || f.count("SELECT count(*) FROM audit_events WHERE action = 'relocation.accepted'") != 1 {
		t.Error("paid time starts at accepted_at, and is audited")
	}
	if cur, _ := Current(ctx, f.pool, rider, time.Now()); cur != nil {
		t.Error("an accepted move is not shown as a suggestion")
	}
	if code, _ := f.post("/riders/me/relocations/"+id+"/accept", token); code != 200 {
		t.Errorf("a repeat: %d", code)
	}
	_, other := f.rider(f.a)
	if code, _ := f.post("/riders/me/relocations/"+id+"/accept", other); code != 404 {
		t.Errorf("another rider's move: %d", code)
	}
	if code, _ := f.post("/riders/me/relocations/nope/accept", token); code != 404 {
		t.Errorf("a bad id: %d", code)
	}
}

func TestAMoveThatEndedCannotBeAccepted(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rider, token := f.rider(f.b)
	f.rider(f.b)
	f.orders(2, 12.9425, 77.6365, 5*time.Minute)
	now := time.Now()
	f.svc.Check(ctx, now)
	id := f.suggestion(rider)
	f.exec("UPDATE relocations SET expires_at = now() - interval '1 second' WHERE id = $1", id)
	if code, out := f.post("/riders/me/relocations/"+id+"/accept", token); code != 409 || out["code"] != "relocation_expired" {
		t.Errorf("%d %v", code, out)
	}
	f.exec("UPDATE relocations SET expires_at = now() + interval '5 minutes' WHERE id = $1", id)
	f.exec("UPDATE shifts SET ended_at = now() WHERE rider_id = $1", rider)
	if code, out := f.post("/riders/me/relocations/"+id+"/accept", token); code != 409 || out["code"] != "relocation_expired" {
		t.Errorf("after the shift ended: %d %v", code, out)
	}
	if f.id("SELECT node_id::text FROM shifts WHERE rider_id = $1", rider) != f.b {
		t.Error("an ended shift was moved")
	}
}

func TestSuggestionsEndAndTheRiderRests(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first, _ := f.rider(f.b)
	second, _ := f.rider(f.b)
	f.rider(f.b)
	f.orders(2, 12.9425, 77.6365, 5*time.Minute)
	now := time.Now()
	f.svc.Check(ctx, now)
	id := f.suggestion(first)
	// Ignored: it expires after 5 minutes, with no penalty, and the next rider is asked.
	f.svc.Check(ctx, now.Add(6*time.Minute))
	if f.state(id) != "expired" {
		t.Errorf("%s", f.state(id))
	}
	if f.count("SELECT count(*) FROM relocations WHERE rider_id = $1 AND state = 'suggested'", second) != 1 {
		t.Error("the next rider was not asked while the first rests")
	}
	if f.count("SELECT count(*) FROM relocations WHERE rider_id = $1", first) != 1 {
		t.Error("the first rider was asked again within the rest time")
	}
}

func TestAnOfferOrAnEndedShiftCancelsASuggestion(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first, _ := f.rider(f.b)
	f.rider(f.b)
	f.rider(f.b)
	order := f.orders(2, 12.9425, 77.6365, 5*time.Minute)[0]
	now := time.Now()
	f.svc.Check(ctx, now)
	id := f.suggestion(first)
	f.exec(`INSERT INTO offers (order_id, rider_id, scooter_id, fencing_token, expires_at)
		VALUES ($1, $2, (SELECT scooter_id FROM shifts WHERE rider_id = $2 AND ended_at IS NULL), 'fence-1', now() + interval '1 minute')`, order, first)
	f.svc.Check(ctx, now.Add(time.Second))
	if f.state(id) != "cancelled" {
		t.Errorf("an offer: %s", f.state(id))
	}
}

func TestArrivalIsMarkedFromPings(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rider, token := f.rider(f.b)
	f.rider(f.b)
	f.orders(2, 12.9425, 77.6365, 5*time.Minute)
	f.svc.Check(ctx, time.Now())
	id := f.suggestion(rider)
	f.post("/riders/me/relocations/"+id+"/accept", token)
	ping := func(lat, lng float64) {
		f.exec(`INSERT INTO location_pings (rider_id, lat, lng, speed_mps, accuracy_m, device_time, applied)
			VALUES ($1, $2, $3, 4, 5, now(), true)`, rider, lat, lng)
	}
	ping(12.936, 77.629) // On the way.
	f.svc.Check(ctx, time.Now().Add(time.Second))
	if f.state(id) != "accepted" {
		t.Fatalf("arrived too early: %s", f.state(id))
	}
	ping(12.9423, 77.6362) // About 50 metres from C.
	f.svc.Check(ctx, time.Now().Add(2*time.Second))
	if f.state(id) != "arrived" || f.count("SELECT count(*) FROM relocations WHERE id = $1 AND arrived_at >= accepted_at", id) != 1 {
		t.Errorf("%s", f.state(id))
	}
}

func TestAnUnfinishedMoveIsClosed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rider, token := f.rider(f.b)
	f.rider(f.b)
	f.orders(2, 12.9425, 77.6365, 5*time.Minute)
	f.svc.Check(ctx, time.Now())
	id := f.suggestion(rider)
	f.post("/riders/me/relocations/"+id+"/accept", token)
	f.svc.Check(ctx, time.Now().Add(30*time.Minute))
	if f.state(id) != "accepted" {
		t.Fatalf("%s", f.state(id))
	}
	f.svc.Check(ctx, time.Now().Add(61*time.Minute))
	if f.state(id) != "expired" {
		t.Errorf("%s", f.state(id))
	}
}

func TestANodeWithARiderWaitingAsksForNobody(t *testing.T) {
	f := newFixture(t)
	f.rider(f.a)
	f.rider(f.a)
	f.rider(f.c)                                 // C has one rider waiting already.
	f.orders(5, 12.9425, 77.6365, 5*time.Minute) // Busy near C.
	if n, _ := f.svc.Check(context.Background(), time.Now()); n != 0 {
		t.Error("a node with a rider waiting asked for another")
	}
}

func TestAnOfferEndsAMoveAndAnEndedShiftEndsASuggestion(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rider, token := f.rider(f.b)
	other, _ := f.rider(f.b)
	f.rider(f.b)
	order := f.orders(2, 12.9425, 77.6365, 5*time.Minute)[0]
	f.svc.Check(ctx, time.Now())
	id := f.suggestion(rider)
	f.post("/riders/me/relocations/"+id+"/accept", token)
	f.exec(`INSERT INTO offers (order_id, rider_id, scooter_id, fencing_token, expires_at)
		VALUES ($1, $2, (SELECT scooter_id FROM shifts WHERE rider_id = $2 AND ended_at IS NULL), 'fence-2', now() + interval '1 minute')`, order, rider)
	f.svc.Check(ctx, time.Now().Add(time.Second))
	if f.state(id) != "cancelled" {
		t.Errorf("an offer during the move: %s", f.state(id))
	}
	// The next suggestion goes to another rider; ending their shift ends it.
	f.exec("DELETE FROM offers")
	f.exec("UPDATE relocations SET state_changed_at = now() - interval '1 hour' WHERE id = $1", id)
	f.exec("UPDATE shifts SET node_id = $1 WHERE rider_id = $2", f.b, rider)
	f.svc.Check(ctx, time.Now().Add(2*time.Second))
	next := f.id("SELECT id::text FROM relocations WHERE state = 'suggested'")
	who := f.id("SELECT rider_id::text FROM relocations WHERE id = $1", next)
	f.exec("UPDATE shifts SET ended_at = now() WHERE rider_id = $1", who)
	if cur, _ := Current(ctx, f.pool, who, time.Now()); cur != nil {
		t.Error("a suggestion for an ended shift is shown")
	}
	f.svc.Check(ctx, time.Now().Add(3*time.Second))
	if f.state(next) != "cancelled" {
		t.Errorf("an ended shift: %s", f.state(next))
	}
	_ = other
}

func TestOnlyAnAccurateNewPingMarksArrival(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rider, token := f.rider(f.b)
	f.rider(f.b)
	f.orders(2, 12.9425, 77.6365, 5*time.Minute)
	f.svc.Check(ctx, time.Now())
	id := f.suggestion(rider)
	f.post("/riders/me/relocations/"+id+"/accept", token)
	ping := func(lat, lng, accuracy float64, recorded string) {
		f.exec(`INSERT INTO location_pings (rider_id, lat, lng, speed_mps, accuracy_m, device_time, applied)
			VALUES ($1, $2, $3, 4, $4, now() + $5::interval, true)`, rider, lat, lng, accuracy, recorded)
	}
	ping(12.9423, 77.6362, 5, "-10 minutes") // Near C, but recorded before accepting and sent late.
	ping(12.9423, 77.6362, 900, "0 s")       // Near C, but 900 metres inaccurate.
	f.svc.Check(ctx, time.Now().Add(time.Second))
	if f.state(id) != "accepted" {
		t.Fatalf("a stale or inaccurate ping marked arrival: %s", f.state(id))
	}
	ping(12.9423, 77.6362, 5, "0 s") // There.
	ping(12.936, 77.629, 5, "1 s")   // And gone again before the next check.
	f.svc.Check(ctx, time.Now().Add(2*time.Second))
	if f.state(id) != "arrived" {
		t.Errorf("a rider who reached the node was missed: %s", f.state(id))
	}
}

func TestAnArrivedMoveIsPaid(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rider, token := f.rider(f.b)
	f.rider(f.b)
	f.orders(2, 12.9425, 77.6365, 5*time.Minute)
	f.svc.Check(ctx, time.Now())
	id := f.suggestion(rider)
	f.post("/riders/me/relocations/"+id+"/accept", token)
	f.exec(`INSERT INTO location_pings (rider_id, lat, lng, speed_mps, accuracy_m, device_time, applied) VALUES ($1, 12.9423, 77.6362, 4, 5, now(), true)`, rider)
	f.svc.Check(ctx, time.Now().Add(time.Second))
	if f.state(id) != "arrived" || f.count("SELECT count(*) FROM earnings WHERE relocation_id = $1 AND kind = 'relocation'", id) != 1 {
		t.Error("an arrived move was not paid")
	}
}

func TestAnOfferEndsPaidTimeWhenItWasMade(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rider, token := f.rider(f.b)
	f.rider(f.b)
	order := f.orders(2, 12.9425, 77.6365, 5*time.Minute)[0]
	f.svc.Check(ctx, time.Now())
	id := f.suggestion(rider)
	f.post("/riders/me/relocations/"+id+"/accept", token)
	f.exec("UPDATE relocations SET accepted_at = now() - interval '4 minutes' WHERE id = $1", id)
	f.exec(`INSERT INTO offers (order_id, rider_id, scooter_id, fencing_token, expires_at, created_at)
		VALUES ($1, $2, (SELECT scooter_id FROM shifts WHERE rider_id = $2 AND ended_at IS NULL), 'fence-3', now() + interval '1 minute', now() - interval '1 minute')`, order, rider)
	f.svc.Check(ctx, time.Now().Add(10*time.Minute)) // Checked long after the offer.
	if paid := f.count("SELECT paid_seconds FROM earnings WHERE relocation_id = $1", id); paid < 170 || paid > 190 {
		t.Errorf("paid %d seconds, want about 180: until the offer, not the check", paid)
	}
}

func TestAnArrivalBeforeAnOfferIsKept(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rider, token := f.rider(f.b)
	f.rider(f.b)
	order := f.orders(2, 12.9425, 77.6365, 5*time.Minute)[0]
	f.svc.Check(ctx, time.Now())
	id := f.suggestion(rider)
	f.post("/riders/me/relocations/"+id+"/accept", token)
	f.exec(`INSERT INTO location_pings (rider_id, lat, lng, speed_mps, accuracy_m, device_time, applied) VALUES ($1, 12.9423, 77.6362, 4, 5, now(), true)`, rider)
	f.exec(`INSERT INTO offers (order_id, rider_id, scooter_id, fencing_token, expires_at)
		VALUES ($1, $2, (SELECT scooter_id FROM shifts WHERE rider_id = $2 AND ended_at IS NULL), 'fence-4', now() + interval '1 minute')`, order, rider)
	f.svc.Check(ctx, time.Now().Add(time.Second))
	if f.state(id) != "arrived" {
		t.Errorf("an arrival before the offer was lost: %s", f.state(id))
	}
}
