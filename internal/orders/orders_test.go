package orders

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
	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/config"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

const gateLat, gateLng = 12.935, 77.625

type fixture struct {
	t        *testing.T
	pool     *pgxpool.Pool
	sessions *identity.Sessions
	svc      *Service
	srv      *httptest.Server
	zone     string
	node     string
	scooter  string
	chai     string
	samosa   string
	cust     string
	token    string
	gate     string
	keys     int
	phones   int
	clock    time.Time
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
	f := &fixture{t: t, pool: dbtest.Fresh(t), clock: time.Now()}
	f.sessions = &identity.Sessions{Pool: f.pool, Log: quiet, RefreshTTL: time.Hour,
		Signer: identity.Signer{Key: []byte("a-test-key-that-is-long-enough-to-use"), TTL: 15 * time.Minute}}
	f.svc = &Service{Pool: f.pool, Log: quiet, Payments: TestPayments{}, now: func() time.Time { return f.clock }}
	f.srv = httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		f.svc.MountCustomer(r, true, identity.RequireActor(f.sessions.Signer, identity.TypeCustomer))
	}))
	t.Cleanup(f.srv.Close)
	f.zone = f.id(`INSERT INTO zones (name, city, status, area) VALUES ('Z', 'Bengaluru', 'active',
		extensions.ST_GeomFromText('MULTIPOLYGON(((77.612 12.925, 77.640 12.925, 77.640 12.945, 77.612 12.945, 77.612 12.925)))', 4326)) RETURNING id::text`)
	f.node = f.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, 'N', 12.936, 77.625, 4) RETURNING id::text", f.zone)
	f.chai = f.id("INSERT INTO menu_items (name, price_paise, status, sort_order) VALUES ('Chai', 4900, 'active', 1) RETURNING id::text")
	f.samosa = f.id("INSERT INTO menu_items (name, price_paise, status, sort_order) VALUES ('Samosa', 3900, 'active', 2) RETURNING id::text")
	f.scooter = f.scooterOnShift("SC-1")
	f.load(f.scooter, "L-CHAI-1", f.chai, 3)
	f.load(f.scooter, "L-CHAI-2", f.chai, 2)
	f.exec("UPDATE lots SET expires_at = now() + interval '1 hour' WHERE code = 'L-CHAI-2'") // Sooner, so it goes first.
	f.load(f.scooter, "L-SAMOSA", f.samosa, 4)
	f.cust, f.token = f.customer(true)
	f.gate = f.id("INSERT INTO gates (customer_id, zone_id, label, lat, lng, verified) VALUES ($1, $2, 'Home', $3, $4, true) RETURNING id::text", f.cust, f.zone, gateLat, gateLng)
	f.exec("INSERT INTO gate_history (gate_id, version, label, lat, lng, verified, reason) VALUES ($1, 1, 'Home', $2, $3, true, 'test')", f.gate, gateLat, gateLng)
	return f
}

func (f *fixture) scooterOnShift(code string) string {
	f.t.Helper()
	f.phones++
	scooter := f.id("INSERT INTO scooters (code, zone_id, node_id) VALUES ($1, $2, $3) RETURNING id::text", code, f.zone, f.node)
	person := f.id("INSERT INTO people (phone) VALUES ($1) RETURNING id::text", fmt.Sprintf("+9198000006%02d", f.phones))
	rider := f.id("INSERT INTO riders (person_id, status, zone_id) VALUES ($1, 'active', $2) RETURNING id::text", person, f.zone)
	f.exec("UPDATE scooters SET current_rider_id = $1 WHERE id = $2", rider, scooter)
	f.exec("INSERT INTO shifts (rider_id, node_id, scooter_id) VALUES ($1, $2, $3)", rider, f.node, scooter)
	return scooter
}

func (f *fixture) load(scooter, code, item string, qty int) {
	f.t.Helper()
	lot := f.id("INSERT INTO lots (code, item_id, qty, expires_at) VALUES ($1, $2, $3, now() + interval '1 day') RETURNING id::text", code, item, qty)
	f.exec("INSERT INTO scooter_stock (scooter_id, lot_id, on_hand) VALUES ($1, $2, $3)", scooter, lot, qty)
}

func (f *fixture) customer(withPhone bool) (string, string) {
	f.t.Helper()
	f.phones++
	var phone any
	if withPhone {
		phone = fmt.Sprintf("+9198000007%02d", f.phones)
	}
	person := f.id("INSERT INTO people (phone) VALUES ($1) RETURNING id::text", phone)
	id := f.id("INSERT INTO customers (person_id) VALUES ($1) RETURNING id::text", person)
	var pair identity.Pair
	if err := database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		var err error
		pair, _, err = f.sessions.Start(context.Background(), tx, identity.TypeCustomer, id)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return id, pair.AccessToken
}

func (f *fixture) send(method, path, token, body string) (int, map[string]any) {
	f.t.Helper()
	f.keys++
	return f.sendKey(method, path, token, body, fmt.Sprintf("order-key-%05d", f.keys))
}

func (f *fixture) sendKey(method, path, token, body, key string) (int, map[string]any) {
	f.t.Helper()
	req, _ := http.NewRequest(method, f.srv.URL+"/v1"+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (f *fixture) basket(lines ...any) string {
	parts := []string{}
	for i := 0; i < len(lines); i += 2 {
		parts = append(parts, fmt.Sprintf(`{"item_id":"%s","qty":%d}`, lines[i], lines[i+1]))
	}
	return fmt.Sprintf(`{"gate_id":"%s","items":[%s]}`, f.gate, strings.Join(parts, ","))
}

func (f *fixture) order(lines ...any) (int, map[string]any) {
	f.t.Helper()
	return f.send("POST", "/orders", f.token, f.basket(lines...))
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
	if len(doc.Components.Schemas[schema].Required) == 0 {
		t.Fatalf("no required fields for %s", schema)
	}
	return doc.Components.Schemas[schema].Required
}

func check(t *testing.T, got map[string]any, schema string) {
	t.Helper()
	for _, field := range required(t, schema) {
		if _, ok := got[field]; !ok {
			t.Errorf("%s has no %s", schema, field)
		}
	}
}

func TestAnOrderHoldsItsStock(t *testing.T) {
	f := newFixture(t)
	code, out := f.order(f.chai, 4, f.samosa, 1)
	if code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "CreateOrderResult")
	o := out["order"].(map[string]any)
	check(t, o, "Order")
	check(t, out["payment"].(map[string]any), "PaymentRequest")
	if o["customer_stage"] != "payment" || o["payment_status"] != "pending" || o["cancellable"] != true || o["seq"] != float64(1) {
		t.Errorf("%v", o)
	}
	if total := o["total"].(map[string]any)["amount_minor"]; total != float64(4*4900+3900) {
		t.Errorf("total %v", total)
	}
	if _, leaked := o["state"]; leaked {
		t.Error("the true state was shown to the customer")
	}
	// Four chai from a lot of three and a lot of two, earliest first.
	if n := f.count("SELECT coalesce(sum(reserved), 0) FROM scooter_stock"); n != 5 {
		t.Errorf("%d units held", n)
	}
	if n := f.count("SELECT ss.reserved FROM scooter_stock ss JOIN lots l ON l.id = ss.lot_id WHERE l.code = 'L-CHAI-2'"); n != 2 {
		t.Errorf("the lot expiring first must be used first: %d held from it", n)
	}
	if n := f.count("SELECT coalesce(sum(delta_reserved), 0) FROM stock_ledger WHERE reason = 'reserve'"); n != 5 {
		t.Errorf("the ledger says %d", n)
	}
	if n := f.count("SELECT count(*) FROM reservations WHERE status = 'held'"); n != 3 {
		t.Errorf("%d reservations", n)
	}
	if n := f.count("SELECT count(*) FROM order_items oi JOIN price_history p ON p.id = oi.price_history_id WHERE p.item_id = oi.item_id"); n != 2 {
		t.Error("each line must point at the price it was sold at")
	}
	if n := f.count("SELECT count(*) FROM order_events WHERE to_state = 'PAYMENT_PENDING'"); n != 1 {
		t.Error("no history")
	}
}

func TestOneOrderComesFromOneScooter(t *testing.T) {
	f := newFixture(t)
	second := f.scooterOnShift("SC-2")
	f.load(second, "L-CHAI-B", f.chai, 4)
	// 5 chai on the first scooter, 4 on the second: 6 cannot be promised from one.
	if code, out := f.order(f.chai, 6); code != 409 || out["code"] != "sold_out" {
		t.Errorf("%d %v", code, out)
	}
	// 4 chai and a samosa: only the first scooter has both, even if the second is nearer.
	if code, out := f.order(f.chai, 4, f.samosa, 1); code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM orders WHERE scooter_id = $1", f.scooter); n != 1 {
		t.Error("the order did not come from the scooter holding the whole basket")
	}
}

func TestOrdersAreRefusedWhenTheyCannotBeMet(t *testing.T) {
	f := newFixture(t)
	if code, out := f.order(f.chai, 6); code != 409 || out["code"] != "sold_out" {
		t.Errorf("too many: %d %v", code, out)
	}
	f.exec("UPDATE menu_items SET status = 'hidden' WHERE id = $1", f.samosa)
	if code, out := f.order(f.samosa, 1); code != 409 || out["code"] != "sold_out" {
		t.Errorf("a hidden item: %d %v", code, out)
	}
	f.exec("UPDATE shifts SET ended_at = now()")
	if code, out := f.order(f.chai, 1); code != 409 || out["code"] != "no_offer" {
		t.Errorf("no rider: %d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM orders") + f.count("SELECT coalesce(sum(reserved), 0) FROM scooter_stock"); n != 0 {
		t.Error("a refused order left something behind")
	}
}

func TestTheGateAndThePhone(t *testing.T) {
	f := newFixture(t)
	other, _ := f.customer(true)
	theirs := f.id("INSERT INTO gates (customer_id, zone_id, label, lat, lng, verified) VALUES ($1, $2, 'Theirs', $3, $4, true) RETURNING id::text", other, f.zone, gateLat, gateLng)
	body := strings.Replace(f.basket(f.chai, 1), f.gate, theirs, 1)
	if code, _ := f.send("POST", "/orders", f.token, body); code != 404 {
		t.Errorf("another customer's gate: %d", code)
	}
	f.exec("UPDATE gates SET verified = false WHERE id = $1", f.gate)
	if code, out := f.order(f.chai, 1); code != 409 || out["code"] != "gate_not_verified" {
		t.Errorf("%d %v", code, out)
	}
	_, noPhone := f.customer(false)
	if code, out := f.send("POST", "/orders", noPhone, f.basket(f.chai, 1)); code != 409 || out["code"] != "phone_required" {
		t.Errorf("%d %v", code, out)
	}
	for _, body := range []string{`{}`, `{"gate_id":"x","items":[]}`, f.basket(f.chai, 0), f.basket(f.chai, 21), f.basket(f.chai, 15, f.chai, 10), f.basket("nope", 1)} {
		if code, _ := f.send("POST", "/orders", f.token, body); code != 400 {
			t.Errorf("%s: %d", body, code)
		}
	}
}

func TestOneTapMakesOneOrder(t *testing.T) {
	f := newFixture(t)
	code, first := f.sendKey("POST", "/orders", f.token, f.basket(f.chai, 1), "same-tap-0001")
	if code != 201 {
		t.Fatal(code)
	}
	code, again := f.sendKey("POST", "/orders", f.token, f.basket(f.chai, 1), "same-tap-0001")
	if code != 201 || again["order"].(map[string]any)["id"] != first["order"].(map[string]any)["id"] {
		t.Errorf("%d %v", code, again)
	}
	if n := f.count("SELECT count(*) FROM orders"); n != 1 {
		t.Errorf("%d orders", n)
	}
}

func TestTheLastUnitsGoToOneOrder(t *testing.T) {
	f := newFixture(t)
	tokens := []string{f.token}
	for i := 0; i < 5; i++ {
		c, tok := f.customer(true)
		g := f.id("INSERT INTO gates (customer_id, zone_id, label, lat, lng, verified) VALUES ($1, $2, 'G', $3, $4, true) RETURNING id::text", c, f.zone, gateLat, gateLng)
		f.exec("INSERT INTO gate_history (gate_id, version, label, lat, lng, verified, reason) VALUES ($1, 1, 'G', $2, $3, true, 'test')", g, gateLat, gateLng)
		tokens = append(tokens, tok+"|"+g)
	}
	var wg sync.WaitGroup
	codes := make(chan int, len(tokens))
	for i, entry := range tokens {
		tok, gate := entry, f.gate
		if i > 0 {
			tok, gate, _ = strings.Cut(entry, "|")
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"gate_id":"%s","items":[{"item_id":"%s","qty":5}]}`, gate, f.chai)
			req, _ := http.NewRequest("POST", f.srv.URL+"/v1/orders", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+tok)
			req.Header.Set("Idempotency-Key", fmt.Sprintf("race-%d-key", i))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				codes <- 0
				return
			}
			resp.Body.Close()
			codes <- resp.StatusCode
		}(i)
	}
	wg.Wait()
	close(codes)
	got := map[int]int{}
	for c := range codes {
		got[c]++
	}
	if got[201] != 1 || got[409] != len(tokens)-1 {
		t.Errorf("%v", got)
	}
	if n := f.count("SELECT coalesce(sum(reserved), 0) FROM scooter_stock WHERE reserved > on_hand"); n != 0 {
		t.Error("more was held than exists")
	}
}

func TestATestPayment(t *testing.T) {
	f := newFixture(t)
	_, out := f.order(f.chai, 1)
	id := out["order"].(map[string]any)["id"].(string)
	code, out := f.send("POST", "/test/payments/"+id, f.token, `{"result":"failed"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if o := out["order"].(map[string]any); code != 200 || o["customer_stage"] != "payment_failed" || o["payment_status"] != "failed" || o["payment_request"] != nil {
		t.Fatalf("%d %v", code, out)
	}
	f.exec("INSERT INTO payments (order_id, provider, provider_order_id, amount_paise) VALUES ($1, 'test', 'test_retry_1', 4900)", id)
	paidAt := f.clock
	code, out = f.send("POST", "/test/payments/"+id, f.token, `{"result":"paid"}`)
	if code != 200 {
		t.Fatalf("paying: %d %v", code, out)
	}
	o := out["order"].(map[string]any)
	if code != 200 || o["customer_stage"] != "looking_for_rider" || o["payment_status"] != "paid" || o["cancellable"] != true || o["cancel_until"] == nil || o["wait_deadline"] == nil {
		t.Fatalf("%d %v", code, out)
	}
	// After the 5 seconds, the customer cannot cancel until the wait deadline.
	f.clock = paidAt.Add(6 * time.Second)
	if _, out := f.send("GET", "/orders/"+id, f.token, ""); out["order"].(map[string]any)["cancellable"] != false {
		t.Errorf("after the window: %v", out)
	}
	f.clock = paidAt.Add(301 * time.Second)
	if _, out := f.send("GET", "/orders/"+id, f.token, ""); out["order"].(map[string]any)["cancellable"] != true {
		t.Errorf("after the wait deadline: %v", out)
	}
	// A repeated result changes nothing.
	seq := o["seq"]
	if _, out := f.send("POST", "/test/payments/"+id, f.token, `{"result":"paid"}`); out["order"].(map[string]any)["seq"] != seq {
		t.Errorf("a repeat changed the order: %v", out)
	}
	if n := f.count("SELECT count(*) FROM refunds WHERE order_id = $1", id); n != 0 {
		t.Error("a repeated result made a refund")
	}
	if n := f.count("SELECT count(*) FROM order_events WHERE order_id = $1", id); n != 3 {
		t.Errorf("%d events", n)
	}
}

func TestPaymentOnAnEndedOrderIsRefunded(t *testing.T) {
	f := newFixture(t)
	_, out := f.order(f.chai, 1)
	id := out["order"].(map[string]any)["id"].(string)
	f.exec("UPDATE orders SET state = 'CANCELLED', ended_reason = 'cancelled' WHERE id = $1", id)
	code, out := f.send("POST", "/test/payments/"+id, f.token, `{"result":"paid"}`)
	if o := out["order"].(map[string]any); code != 200 || o["customer_stage"] != "ended" || o["payment_status"] != "refunded" {
		t.Errorf("%d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM refunds WHERE order_id = $1 AND reason_code = 'customer_cancelled'", id); n != 1 {
		t.Error("no refund")
	}
}

func TestRefundsCannotExceedThePayment(t *testing.T) {
	f := newFixture(t)
	_, out := f.order(f.chai, 1)
	id := out["order"].(map[string]any)["id"].(string)
	payment := f.id("SELECT id::text FROM payments WHERE order_id = $1", id)
	f.exec("INSERT INTO refunds (order_id, payment_id, reason_code, amount_paise) VALUES ($1, $2, 'a', 4000)", id, payment)
	if _, err := f.pool.Exec(context.Background(), "INSERT INTO refunds (order_id, payment_id, reason_code, amount_paise) VALUES ($1, $2, 'b', 1000)", id, payment); err == nil {
		t.Error("refunds exceeded the payment")
	}
}

func TestReadingOrders(t *testing.T) {
	f := newFixture(t)
	ids := []string{}
	for i := 0; i < 3; i++ {
		_, out := f.order(f.samosa, 1)
		ids = append(ids, out["order"].(map[string]any)["id"].(string))
	}
	code, out := f.send("GET", "/orders?limit=2", f.token, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "OrderList")
	page := out["orders"].([]any)
	if len(page) != 2 || page[0].(map[string]any)["id"] != ids[2] || out["next_cursor"] == nil {
		t.Fatalf("%v", out)
	}
	_, out = f.send("GET", "/orders?limit=2&cursor="+out["next_cursor"].(string), f.token, "")
	if page := out["orders"].([]any); len(page) != 1 || page[0].(map[string]any)["id"] != ids[0] || out["next_cursor"] != nil {
		t.Errorf("%v", out)
	}
	_, other := f.customer(true)
	if code, _ := f.send("GET", "/orders/"+ids[0], other, ""); code != 404 {
		t.Errorf("another customer's order: %d", code)
	}
	if _, out := f.send("GET", "/orders", other, ""); len(out["orders"].([]any)) != 0 {
		t.Error("another customer's orders were listed")
	}
	if code, _ := f.send("POST", "/test/payments/"+ids[0], other, `{"result":"paid"}`); code != 404 {
		t.Errorf("paying another customer's order: %d", code)
	}
	for _, q := range []string{"?limit=0", "?limit=101", "?cursor=nope"} {
		if code, _ := f.send("GET", "/orders"+q, f.token, ""); code != 400 {
			t.Errorf("%s: %d", q, code)
		}
	}
	f.exec("UPDATE refresh_tokens SET revoked_at = now()")
	if code, _ := f.send("GET", "/orders", f.token, ""); code != 401 {
		t.Errorf("an ended session: %d", code)
	}
}

func TestProductionRefusesTheTestPayment(t *testing.T) {
	if err := RequirePayments(configFor("production")); err == nil {
		t.Error("production started with the test payment")
	}
	if err := RequirePayments(configFor("staging")); err != nil {
		t.Error(err)
	}
}

func configFor(env string) config.Config { return config.Config{Env: env} }

func TestTwoRefundsAtOnceCannotExceedThePayment(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, out := f.order(f.chai, 1)
	id := out["order"].(map[string]any)["id"].(string)
	payment := f.id("SELECT id::text FROM payments WHERE order_id = $1", id)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, "INSERT INTO refunds (order_id, payment_id, reason_code, amount_paise) VALUES ($1, $2, 'a', 3000)", id, payment); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.pool.Exec(ctx, "INSERT INTO refunds (order_id, payment_id, reason_code, amount_paise) VALUES ($1, $2, 'b', 3000)", id, payment)
		done <- err
	}()
	for i := 0; i < 200; i++ {
		if f.count("SELECT count(*) FROM pg_locks WHERE NOT granted AND database = (SELECT oid FROM pg_database WHERE datname = current_database())") > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Error("two refunds at once went over the payment")
	}
}

func TestASecondPaymentOnAPaidOrderGoesBack(t *testing.T) {
	f := newFixture(t)
	_, out := f.order(f.chai, 1)
	id := out["order"].(map[string]any)["id"].(string)
	f.send("POST", "/test/payments/"+id, f.token, `{"result":"paid"}`)
	second := "test_second_1"
	f.exec("INSERT INTO payments (order_id, provider, provider_order_id, amount_paise) VALUES ($1, 'test', $2, 4900)", id, second)
	if err := database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error { return Confirm(context.Background(), tx, id, second, true, f.clock) }); err != nil {
		t.Fatal(err)
	}
	if n := f.count("SELECT count(*) FROM payments WHERE order_id = $1 AND status = 'refunded'", id); n != 1 {
		t.Error("the second payment was not recorded as refunded")
	}
	if n := f.count("SELECT count(*) FROM refunds r JOIN payments p ON p.id = r.payment_id WHERE p.provider_order_id = $1 AND r.status = 'processed'", second); n != 1 {
		t.Error("the second payment was not refunded")
	}
}

func TestABlockedCustomerCannotConfirm(t *testing.T) {
	f := newFixture(t)
	_, out := f.order(f.chai, 1)
	id := out["order"].(map[string]any)["id"].(string)
	f.exec("UPDATE people SET status = 'blocked' WHERE id = (SELECT person_id FROM customers WHERE id = $1)", f.cust)
	if code, out := f.send("POST", "/test/payments/"+id, f.token, `{"result":"paid"}`); code != 403 || out["code"] != "account_blocked" {
		t.Errorf("%d %v", code, out)
	}
}

func TestAPriceChangeDuringCheckoutIsRefused(t *testing.T) {
	f := newFixture(t)
	got, err := quote(context.Background(), f.pool, map[string]int{f.chai: 2, f.samosa: 1})
	if err != nil || got != 2*4900+3900 {
		t.Errorf("%d %v", got, err)
	}
}
