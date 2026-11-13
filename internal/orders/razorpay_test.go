package orders

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestRazorpayKeysAreAllOrNothing(t *testing.T) {
	if p, err := NewRazorpay(env(nil)); p != nil || err != nil {
		t.Fatalf("no keys: %v %v", p, err)
	}
	for name, m := range map[string]map[string]string{
		"id only":     {"RAZORPAY_KEY_ID": "rzp_test_x"},
		"secret only": {"RAZORPAY_KEY_SECRET": "s"},
		"no webhook":  {"RAZORPAY_KEY_ID": "rzp_test_x", "RAZORPAY_KEY_SECRET": "s"},
		"not a key":   {"RAZORPAY_KEY_ID": "abc", "RAZORPAY_KEY_SECRET": "s", "RAZORPAY_WEBHOOK_SECRET": "w"},
	} {
		if _, err := NewRazorpay(env(m)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	p, err := NewRazorpay(env(map[string]string{"RAZORPAY_KEY_ID": "rzp_test_x", "RAZORPAY_KEY_SECRET": "s", "RAZORPAY_WEBHOOK_SECRET": "w"}))
	if err != nil || p.KeyID() != "rzp_test_x" || p.Name() != "razorpay" || p.Live() {
		t.Fatalf("%+v %v", p, err)
	}
	if CheckRazorpayKeys("staging", p) != nil {
		t.Fatal("test keys refused on staging")
	}
	live, _ := NewRazorpay(env(map[string]string{"RAZORPAY_KEY_ID": "rzp_live_x", "RAZORPAY_KEY_SECRET": "s", "RAZORPAY_WEBHOOK_SECRET": "w"}))
	if CheckRazorpayKeys("staging", live) == nil || CheckRazorpayKeys("local", live) == nil {
		t.Fatal("live keys accepted outside production")
	}
	if CheckRazorpayKeys("production", live) != nil {
		t.Fatal("live keys refused in production")
	}
}

func sign(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestTheSignatureIsChecked(t *testing.T) {
	body := []byte(`{"event":"payment.captured"}`)
	if !validSignature(body, sign(body, "w"), "w") {
		t.Fatal("a good signature was refused")
	}
	for name, s := range map[string]string{"wrong secret": sign(body, "x"), "empty": "", "not hex": "zz", "other body": sign([]byte("{}"), "w")} {
		if validSignature(body, s, "w") {
			t.Errorf("%s: accepted", name)
		}
	}
	if validSignature(body, sign(body, ""), "") {
		t.Error("an empty secret accepted a signature")
	}
}

// fakeRazorpay answers the three calls we make, and records them.
type fakeRazorpay struct {
	mu       sync.Mutex
	orders   int
	refunds  map[string][]rzRefund // by payment id
	posts    int
	fail     bool
	lastAuth string
}

func (z *fakeRazorpay) handler() http.Handler {
	r := chi.NewRouter()
	r.Post("/orders", func(w http.ResponseWriter, req *http.Request) {
		z.mu.Lock()
		defer z.mu.Unlock()
		u, p, _ := req.BasicAuth()
		z.lastAuth = u + ":" + p
		var in struct {
			Amount   int    `json:"amount"`
			Currency string `json:"currency"`
		}
		_ = json.NewDecoder(req.Body).Decode(&in)
		z.orders++
		_ = json.NewEncoder(w).Encode(map[string]any{"id": fmt.Sprintf("order_T%d", z.orders), "amount": in.Amount, "currency": in.Currency})
	})
	r.Get("/payments/{id}/refunds", func(w http.ResponseWriter, req *http.Request) {
		z.mu.Lock()
		defer z.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"items": z.refunds[chi.URLParam(req, "id")]})
	})
	r.Post("/payments/{id}/refund", func(w http.ResponseWriter, req *http.Request) {
		z.mu.Lock()
		defer z.mu.Unlock()
		if z.fail {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"description":"The payment is not captured"}}`))
			return
		}
		var in struct {
			Amount int               `json:"amount"`
			Notes  map[string]string `json:"notes"`
		}
		_ = json.NewDecoder(req.Body).Decode(&in)
		z.posts++
		rf := rzRefund{ID: fmt.Sprintf("rfnd_T%d", z.posts), Amount: in.Amount, Status: "pending", Notes: in.Notes}
		if z.refunds == nil {
			z.refunds = map[string][]rzRefund{}
		}
		id := chi.URLParam(req, "id")
		z.refunds[id] = append(z.refunds[id], rf)
		_ = json.NewEncoder(w).Encode(rf)
	})
	return r
}

type payFixture struct {
	*fixture
	z    *fakeRazorpay
	rz   *Razorpay
	hook *httptest.Server
}

func newPayFixture(t *testing.T) *payFixture {
	f := newFixture(t)
	z := &fakeRazorpay{}
	api := httptest.NewServer(z.handler())
	t.Cleanup(api.Close)
	rz, err := NewRazorpay(env(map[string]string{"RAZORPAY_KEY_ID": "rzp_test_k", "RAZORPAY_KEY_SECRET": "ks", "RAZORPAY_WEBHOOK_SECRET": "whsec"}))
	if err != nil {
		t.Fatal(err)
	}
	rz.BaseURL = api.URL
	f.svc.Payments = rz
	hook := httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		f.svc.MountRazorpayWebhook(r, rz)
	}))
	t.Cleanup(hook.Close)
	return &payFixture{fixture: f, z: z, rz: rz, hook: hook}
}

// deliver posts a signed webhook; secret "" signs with the right one.
func (p *payFixture) deliver(eventID, secret string, ev map[string]any) int {
	p.t.Helper()
	body, _ := json.Marshal(ev)
	if secret == "" {
		secret = "whsec"
	}
	req, _ := http.NewRequest("POST", p.hook.URL+"/v1/webhooks/razorpay", strings.NewReader(string(body)))
	req.Header.Set("X-Razorpay-Signature", sign(body, secret))
	if eventID != "" {
		req.Header.Set("X-Razorpay-Event-Id", eventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func paymentEvent(event, payID, orderID string, amount int) map[string]any {
	return map[string]any{"event": event, "payload": map[string]any{"payment": map[string]any{"entity": map[string]any{
		"id": payID, "order_id": orderID, "amount": amount, "status": "captured"}}}}
}

func refundEvent(event, refundID, ourID string) map[string]any {
	return map[string]any{"event": event, "payload": map[string]any{"refund": map[string]any{"entity": map[string]any{
		"id": refundID, "status": strings.TrimPrefix(event, "refund."), "notes": map[string]string{"refund_id": ourID}}}}}
}

func (p *payFixture) state(order string) string {
	return p.id("SELECT state FROM orders WHERE id = $1", order)
}

func (p *payFixture) providerOrder(order string) (string, int) {
	var id string
	var amount int
	if err := p.pool.QueryRow(context.Background(), "SELECT provider_order_id, amount_paise FROM payments WHERE order_id = $1 ORDER BY created_at DESC LIMIT 1", order).Scan(&id, &amount); err != nil {
		p.t.Fatal(err)
	}
	return id, amount
}

func TestARazorpayOrderIsMadeForThePayment(t *testing.T) {
	p := newPayFixture(t)
	code, out := p.order(p.chai, 2)
	if code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	order := orderOf(out)["id"].(string)
	rzOrder, amount := p.providerOrder(order)
	if rzOrder != "order_T1" || amount != 9800 || p.id("SELECT provider FROM payments WHERE order_id = $1", order) != "razorpay" {
		t.Fatalf("%s %d", rzOrder, amount)
	}
	if p.z.lastAuth != "rzp_test_k:ks" {
		t.Fatal("the API call was not signed with the key id and secret")
	}
	if !strings.Contains(fmt.Sprint(out), "rzp_test_k") || !strings.Contains(fmt.Sprint(out), "order_T1") {
		t.Errorf("the app is not given the key id and Razorpay order: %v", out)
	}
}

func TestABadWebhookChangesNothing(t *testing.T) {
	p := newPayFixture(t)
	order := p.placed(p.chai, 1)
	rzOrder, amount := p.providerOrder(order)
	ev := paymentEvent("payment.captured", "pay_A", rzOrder, amount)
	if code := p.deliver("evt_1", "wrong-secret", ev); code != 400 {
		t.Fatalf("a bad signature: %d", code)
	}
	if code := p.deliver("", "", ev); code != 400 {
		t.Fatalf("no event id: %d", code)
	}
	if p.state(order) != "PAYMENT_PENDING" || p.count("SELECT count(*) FROM webhook_events") != 0 {
		t.Fatal("a refused webhook changed something")
	}
}

func TestACapturedPaymentSecuresTheOrderOnce(t *testing.T) {
	p := newPayFixture(t)
	order := p.placed(p.chai, 1)
	rzOrder, amount := p.providerOrder(order)
	if code := p.deliver("evt_1", "", paymentEvent("payment.captured", "pay_A", rzOrder, amount)); code != 200 {
		t.Fatal(code)
	}
	if p.state(order) != "PAYMENT_SECURED" || p.id("SELECT provider_payment_id FROM payments WHERE order_id = $1", order) != "pay_A" {
		t.Fatal("not secured")
	}
	seq := p.count("SELECT seq FROM orders WHERE id = $1", order)
	// The same event again, and order.paid for the same payment: no second effect.
	if p.deliver("evt_1", "", paymentEvent("payment.captured", "pay_A", rzOrder, amount)) != 200 || p.deliver("evt_2", "", paymentEvent("order.paid", "pay_A", rzOrder, amount)) != 200 {
		t.Fatal("a repeat was refused")
	}
	if p.count("SELECT seq FROM orders WHERE id = $1", order) != seq || p.count("SELECT count(*) FROM order_events WHERE order_id = $1 AND to_state = 'PAYMENT_SECURED'", order) != 1 {
		t.Fatal("a repeat changed the order")
	}
	if p.count("SELECT count(*) FROM webhook_events") != 2 {
		t.Fatal("events not kept by id")
	}
}

func TestAFailedAttemptThenASuccessSecuresTheOrder(t *testing.T) {
	p := newPayFixture(t)
	order := p.placed(p.chai, 1)
	rzOrder, amount := p.providerOrder(order)
	p.deliver("evt_1", "", paymentEvent("payment.failed", "pay_A", rzOrder, amount))
	if p.state(order) != "PAYMENT_FAILED" {
		t.Fatal(p.state(order))
	}
	// The customer tries again on the same Razorpay order, and it works.
	p.deliver("evt_2", "", paymentEvent("payment.captured", "pay_B", rzOrder, amount))
	if p.state(order) != "PAYMENT_SECURED" || p.id("SELECT status FROM payments WHERE order_id = $1", order) != "paid" {
		t.Fatalf("money taken but the order is %s", p.state(order))
	}
}

func TestAWrongAmountIsNotAccepted(t *testing.T) {
	p := newPayFixture(t)
	order := p.placed(p.chai, 1)
	rzOrder, amount := p.providerOrder(order)
	if code := p.deliver("evt_1", "", paymentEvent("payment.captured", "pay_A", rzOrder, amount-100)); code != 200 {
		t.Fatal(code)
	}
	if p.state(order) != "PAYMENT_PENDING" {
		t.Fatal("a payment for the wrong amount secured the order")
	}
}

func TestARefundIsSentOnceAndSettledByWebhook(t *testing.T) {
	p := newPayFixture(t)
	order := p.placed(p.chai, 1)
	rzOrder, amount := p.providerOrder(order)
	p.deliver("evt_1", "", paymentEvent("payment.captured", "pay_A", rzOrder, amount))
	if code, out := p.send("POST", "/orders/"+order+"/cancel", p.token, `{}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	refundID := p.id("SELECT id::text FROM refunds WHERE order_id = $1", order)
	if p.id("SELECT status FROM refunds WHERE id = $1", refundID) != "pending" {
		t.Fatal("a Razorpay refund must wait for Razorpay")
	}
	ctx := context.Background()
	if n, err := p.svc.SendRefunds(ctx, p.rz, p.clock); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if p.z.posts != 1 || p.z.refunds["pay_A"][0].Notes["refund_id"] != refundID || p.z.refunds["pay_A"][0].Amount != amount {
		t.Fatalf("%+v", p.z.refunds)
	}
	if n, _ := p.svc.SendRefunds(ctx, p.rz, p.clock); n != 0 || p.z.posts != 1 {
		t.Fatal("a refund was sent twice")
	}
	p.deliver("evt_2", "", refundEvent("refund.processed", "rfnd_T1", refundID))
	if p.id("SELECT status FROM refunds WHERE id = $1", refundID) != "processed" {
		t.Fatal("not settled by the webhook")
	}
}

func TestACrashAfterSendingDoesNotRefundTwice(t *testing.T) {
	p := newPayFixture(t)
	order := p.placed(p.chai, 1)
	rzOrder, amount := p.providerOrder(order)
	p.deliver("evt_1", "", paymentEvent("payment.captured", "pay_A", rzOrder, amount))
	p.send("POST", "/orders/"+order+"/cancel", p.token, `{}`)
	refundID := p.id("SELECT id::text FROM refunds WHERE order_id = $1", order)
	// Razorpay already has this refund (sent, then the process died before its commit).
	p.z.refunds = map[string][]rzRefund{"pay_A": {{ID: "rfnd_OLD", Amount: amount, Status: "processed", Notes: map[string]string{"refund_id": refundID}}}}
	if n, err := p.svc.SendRefunds(context.Background(), p.rz, p.clock); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if p.z.posts != 0 || p.id("SELECT provider_refund_id FROM refunds WHERE id = $1", refundID) != "rfnd_OLD" || p.id("SELECT status FROM refunds WHERE id = $1", refundID) != "processed" {
		t.Fatal("the existing refund was not found, or a second was made")
	}
}

func TestARefusedRefundIsRetriedLater(t *testing.T) {
	p := newPayFixture(t)
	order := p.placed(p.chai, 1)
	rzOrder, amount := p.providerOrder(order)
	p.deliver("evt_1", "", paymentEvent("payment.captured", "pay_A", rzOrder, amount))
	p.send("POST", "/orders/"+order+"/cancel", p.token, `{}`)
	p.z.fail = true
	ctx := context.Background()
	if n, err := p.svc.SendRefunds(ctx, p.rz, p.clock); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if p.count("SELECT attempts FROM refunds WHERE order_id = $1", order) != 1 || !strings.Contains(p.id("SELECT last_error FROM refunds WHERE order_id = $1", order), "not captured") {
		t.Fatal("the failure was not kept")
	}
	p.z.fail = false
	if n, _ := p.svc.SendRefunds(ctx, p.rz, p.clock); n != 0 {
		t.Fatal("retried before its wait")
	}
	if n, _ := p.svc.SendRefunds(ctx, p.rz, p.clock.Add(2*60e9)); n != 1 {
		t.Fatal("not retried after its wait")
	}
}

func TestEmptyNotesAreRead(t *testing.T) {
	var r rzRefund
	if err := json.Unmarshal([]byte(`{"id":"rfnd_X","amount":100,"status":"processed","notes":[]}`), &r); err != nil || r.ID != "rfnd_X" || len(r.Notes) != 0 {
		t.Fatalf("notes as an empty array: %+v %v", r, err)
	}
	if err := json.Unmarshal([]byte(`{"notes":{"refund_id":"abc","n":5}}`), &r); err != nil || r.Notes["refund_id"] != "abc" {
		t.Fatalf("notes as an object: %+v %v", r, err)
	}
	if err := json.Unmarshal([]byte(`{"notes":"x"}`), &r); err == nil {
		t.Fatal("notes as a string accepted")
	}
}

// A refund made in the dashboard (no notes, sent as []) neither stops ours nor fails its webhook.
func TestARefundFromTheDashboardChangesNothing(t *testing.T) {
	p := newPayFixture(t)
	order := p.placed(p.chai, 1)
	rzOrder, amount := p.providerOrder(order)
	p.deliver("evt_1", "", paymentEvent("payment.captured", "pay_A", rzOrder, amount))
	p.send("POST", "/orders/"+order+"/cancel", p.token, `{}`)
	p.z.refunds = map[string][]rzRefund{"pay_A": {{ID: "rfnd_DASH", Amount: 100, Status: "processed", Notes: rzNotes{}}}}
	if n, err := p.svc.SendRefunds(context.Background(), p.rz, p.clock); err != nil || n != 1 || p.z.posts != 1 {
		t.Fatalf("%d %v, posts %d", n, err, p.z.posts)
	}
	ev := map[string]any{"event": "refund.processed", "payload": map[string]any{"refund": map[string]any{"entity": map[string]any{
		"id": "rfnd_DASH", "amount": 100, "status": "processed", "notes": []any{}}}}}
	if code := p.deliver("evt_2", "", ev); code != 200 {
		t.Fatalf("a dashboard refund's webhook failed: %d", code)
	}
}

func (p *payFixture) alerts(reason string) int {
	return p.count("SELECT count(*) FROM outbox_events WHERE topic = 'support.alert' AND payload->>'reason' = $1", reason)
}

func TestManyFailuresNeitherOverflowNorStopTheOthers(t *testing.T) {
	p := newPayFixture(t)
	first := p.placed(p.chai, 1)
	rzOrder, amount := p.providerOrder(first)
	p.deliver("evt_1", "", paymentEvent("payment.captured", "pay_A", rzOrder, amount))
	p.send("POST", "/orders/"+first+"/cancel", p.token, `{}`)
	p.exec("UPDATE refunds SET attempts = 45")
	p.z.fail = true
	if _, err := p.svc.SendRefunds(context.Background(), p.rz, p.clock); err != nil {
		t.Fatalf("a refund tried 45 times broke the pass: %v", err)
	}
	if p.count("SELECT attempts FROM refunds WHERE order_id = $1", first) != 46 ||
		p.count("SELECT extract(epoch FROM next_try_at - $2::timestamptz)::int FROM refunds WHERE order_id = $1", first, p.clock) != 3600 {
		t.Fatal("the wait is not capped at an hour")
	}
	// The 40th failure raises one support alert, and it opens an incident.
	p.exec("UPDATE refunds SET attempts = 39, next_try_at = NULL")
	p.svc.SendRefunds(context.Background(), p.rz, p.clock)
	if p.alerts("refund_failing") != 1 || p.count("SELECT count(*) FROM incidents WHERE kind = 'support_alert' AND order_id = $1", first) != 1 {
		t.Fatal("no alert after 40 failures")
	}
}

func TestAWrongAmountRaisesAnAlert(t *testing.T) {
	p := newPayFixture(t)
	order := p.placed(p.chai, 1)
	rzOrder, amount := p.providerOrder(order)
	p.deliver("evt_1", "", paymentEvent("payment.captured", "pay_A", rzOrder, amount+500))
	if p.state(order) != "PAYMENT_PENDING" || p.alerts("payment_amount_mismatch") != 1 {
		t.Fatal("a wrong amount must leave the order unpaid and alert support")
	}
}

func TestASecondCaptureIsPaidOnceAndAlerted(t *testing.T) {
	p := newPayFixture(t)
	order := p.placed(p.chai, 1)
	rzOrder, amount := p.providerOrder(order)
	// order.paid first, then payment.captured for the same payment: one payment, one secure.
	p.deliver("evt_1", "", paymentEvent("order.paid", "pay_A", rzOrder, amount))
	p.deliver("evt_2", "", paymentEvent("payment.captured", "pay_A", rzOrder, amount))
	if p.state(order) != "PAYMENT_SECURED" || p.alerts("payment_second_capture") != 0 {
		t.Fatal("the same payment twice is not a second capture")
	}
	p.deliver("evt_3", "", paymentEvent("payment.captured", "pay_B", rzOrder, amount))
	if p.id("SELECT provider_payment_id FROM payments WHERE order_id = $1", order) != "pay_A" || p.alerts("payment_second_capture") != 1 ||
		p.count("SELECT count(*) FROM order_events WHERE order_id = $1 AND to_state = 'PAYMENT_SECURED'", order) != 1 {
		t.Fatal("a second capture must leave the first payment and alert support")
	}
}

func TestAnOversizeWebhookIsRefused(t *testing.T) {
	p := newPayFixture(t)
	big := map[string]any{"event": "payment.captured", "pad": strings.Repeat("x", maxWebhookBody)}
	if code := p.deliver("evt_1", "", big); code != 400 {
		t.Fatalf("%d", code)
	}
}

func TestAFailedRefundAlertsSupport(t *testing.T) {
	p := newPayFixture(t)
	order := p.placed(p.chai, 1)
	rzOrder, amount := p.providerOrder(order)
	p.deliver("evt_1", "", paymentEvent("payment.captured", "pay_A", rzOrder, amount))
	p.send("POST", "/orders/"+order+"/cancel", p.token, `{}`)
	refundID := p.id("SELECT id::text FROM refunds WHERE order_id = $1", order)
	p.svc.SendRefunds(context.Background(), p.rz, p.clock)
	p.deliver("evt_2", "", refundEvent("refund.failed", "rfnd_T1", refundID))
	if p.id("SELECT status FROM refunds WHERE id = $1", refundID) != "failed" || p.alerts("refund_failed") != 1 {
		t.Fatal("a failed refund must be marked failed and alert support")
	}
}

func TestARefundRazorpayAlreadyFailedIsNotLeftPending(t *testing.T) {
	p := newPayFixture(t)
	order := p.placed(p.chai, 1)
	rzOrder, amount := p.providerOrder(order)
	p.deliver("evt_1", "", paymentEvent("payment.captured", "pay_A", rzOrder, amount))
	p.send("POST", "/orders/"+order+"/cancel", p.token, `{}`)
	refundID := p.id("SELECT id::text FROM refunds WHERE order_id = $1", order)
	p.z.refunds = map[string][]rzRefund{"pay_A": {{ID: "rfnd_OLD", Amount: amount, Status: "failed", Notes: rzNotes{"refund_id": refundID}}}}
	p.svc.SendRefunds(context.Background(), p.rz, p.clock)
	if p.id("SELECT status FROM refunds WHERE id = $1", refundID) != "failed" || p.alerts("refund_failed") != 1 || p.z.posts != 0 {
		t.Fatal("a refund Razorpay shows as failed must be marked failed and alert support, not sent again")
	}
	// A refund with our id but another amount is never taken as ours.
	p.exec("UPDATE refunds SET status = 'pending', provider_refund_id = NULL, next_try_at = NULL WHERE id = $1", refundID)
	p.z.refunds["pay_A"][0].Amount = amount - 1
	if n, _ := p.svc.SendRefunds(context.Background(), p.rz, p.clock); n != 0 || !strings.Contains(p.id("SELECT last_error FROM refunds WHERE id = $1", refundID), "different amount") {
		t.Fatal("a refund with our id and another amount was accepted")
	}
}
