package orders

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/outbox"
)

// support mounts the support routes on the customer's server.
func withSupport(f *fixture) {
	srv := httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		f.svc.MountCustomer(r, true, identity.RequireActor(f.sessions.Signer, identity.TypeCustomer))
		f.svc.MountSupport(r, identity.RequireActor(f.sessions.Signer, identity.TypeCustomer))
	}))
	f.t.Cleanup(srv.Close)
	f.srv = srv
}

type recorder struct {
	mu   sync.Mutex
	sent []map[string]any
}

func (r *recorder) Push(_ context.Context, platform, token, topic string, payload map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, map[string]any{"platform": platform, "token": token, "topic": topic, "stage": payload["stage"]})
	return nil
}

func (f *fixture) outbox(topic string) int {
	return f.count("SELECT count(*) FROM outbox_events WHERE topic = $1", topic)
}

func TestATicket(t *testing.T) {
	f := newFixture(t)
	withSupport(f)
	id := f.placed(f.chai, 1)
	code, out := f.send("POST", "/support/tickets", f.token, `{"order_id":"`+id+`","type":"cold_food","text":"The chai was cold"}`)
	if code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	check(t, out, "TicketResponse")
	ticket := out["ticket"].(map[string]any)
	check(t, ticket, "Ticket")
	if ticket["status"] != "open" || ticket["order_id"] != id || len(ticket["messages"].([]any)) != 1 {
		t.Errorf("%v", ticket)
	}
	check(t, ticket["messages"].([]any)[0].(map[string]any), "TicketMessage")
	if code, again := f.send("GET", "/support/tickets/"+ticket["id"].(string), f.token, ""); code != 200 || again["ticket"].(map[string]any)["id"] != ticket["id"] {
		t.Errorf("%d %v", code, again)
	}
	if f.outbox("support.ticket_created") != 1 {
		t.Error("support was not told")
	}
	_, other := f.customer(true)
	if code, _ := f.send("GET", "/support/tickets/"+ticket["id"].(string), other, ""); code != 404 {
		t.Errorf("another customer's ticket: %d", code)
	}
	if code, _ := f.send("POST", "/support/tickets", other, `{"order_id":"`+id+`","type":"other","text":"x"}`); code != 404 {
		t.Errorf("another customer's order: %d", code)
	}
	_, noPhone := f.customer(false)
	if code, out := f.send("POST", "/support/tickets", noPhone, `{"type":"other","text":"x"}`); code != 409 || out["code"] != "phone_required" {
		t.Errorf("%d %v", code, out)
	}
	for _, body := range []string{`{}`, `{"type":"angry","text":"x"}`, `{"type":"other","text":""}`, `{"type":"other","text":"x","order_id":"nope"}`} {
		if code, _ := f.send("POST", "/support/tickets", f.token, body); code != 400 {
			t.Errorf("%s: %d", body, code)
		}
	}
}

func TestNotifyMeWhenAvailable(t *testing.T) {
	f := newFixture(t)
	withSupport(f)
	ctx := context.Background()
	f.exec("UPDATE shifts SET ended_at = now()") // Nobody is on shift, so nothing is in reach.
	if code, out := f.send("POST", "/availability/notify", f.token, `{"gate_id":"`+f.gate+`","item_id":"`+f.chai+`"}`); code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	if n, err := f.svc.Notify(ctx, f.clock); err != nil || f.outbox("push.customer") != 0 {
		t.Fatalf("pushed with nothing in reach: %d %v", n, err)
	}
	// A rider starts a shift: the item is in reach, so one push goes out.
	f.scooterOnShift("SC-NEW")
	f.load(f.id("SELECT id::text FROM scooters WHERE code = 'SC-NEW'"), "L-NEW", f.chai, 3)
	f.svc.Notify(ctx, f.clock)
	f.svc.Notify(ctx, f.clock)
	if f.outbox("push.customer") != 1 || f.count("SELECT count(*) FROM availability_notifications WHERE sent_at IS NOT NULL") != 1 {
		t.Error("one push, once")
	}
	_, other := f.customer(true)
	if code, _ := f.send("POST", "/availability/notify", other, `{"gate_id":"`+f.gate+`"}`); code != 404 {
		t.Errorf("another customer's gate: %d", code)
	}
	_, noPhone := f.customer(false)
	if code, out := f.send("POST", "/availability/notify", noPhone, `{"gate_id":"`+f.gate+`"}`); code != 409 || out["code"] != "phone_required" {
		t.Errorf("%d %v", code, out)
	}
	if code, _ := f.send("POST", "/availability/notify", f.token, `{"gate_id":"x"}`); code != 400 {
		t.Errorf("bad input: %d", code)
	}
}

func TestARequestExpiresUnsent(t *testing.T) {
	f := newFixture(t)
	withSupport(f)
	// The chai is in reach now. A request in date is sent, and one more than a day old is not.
	f.exec("INSERT INTO availability_notifications (customer_id, gate_id, expires_at) VALUES ($1, $2, $3)", f.cust, f.gate, f.clock.Add(-time.Minute))
	f.svc.Notify(context.Background(), f.clock)
	if f.outbox("push.customer") != 0 {
		t.Error("an expired request was sent")
	}
	f.exec("INSERT INTO availability_notifications (customer_id, gate_id, item_id, expires_at) VALUES ($1, $2, $3, $4)", f.cust, f.gate, f.chai, f.clock.Add(time.Hour))
	f.svc.Notify(context.Background(), f.clock)
	if f.outbox("push.customer") != 1 {
		t.Error("a request in date was not sent")
	}
}

func TestOnlyListedStagesArePushedOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.placed(f.chai, 1)
	f.svc.Notify(ctx, f.clock) // payment: not pushed.
	f.pay(id, "failed")
	f.svc.Notify(ctx, f.clock) // payment_failed: pushed.
	f.exec("UPDATE orders SET seq = seq + 1 WHERE id = $1", id)
	f.svc.Notify(ctx, f.clock) // The same stage again: not pushed again.
	stages := []string{}
	rows, err := f.pool.Query(ctx, "SELECT payload->>'stage' FROM outbox_events WHERE topic = 'push.customer' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var st string
		_ = rows.Scan(&st)
		stages = append(stages, st)
	}
	rows.Close()
	if len(stages) != 1 || stages[0] != "payment_failed" {
		t.Errorf("pushes %v", stages)
	}
}

func TestStagePushesFollowTheCustomerStage(t *testing.T) {
	x := toNoScooter(t)
	ctx := context.Background()
	if _, err := x.svc.Notify(ctx, x.clock); err != nil {
		t.Fatal(err)
	}
	x.svc.Notify(ctx, x.clock)
	if n := x.count("SELECT count(*) FROM outbox_events WHERE topic = 'push.customer' AND payload->>'stage' = 'no_scooter'"); n != 1 {
		t.Errorf("%d pushes for no_scooter", n)
	}
	if x.count("SELECT count(*) FROM outbox_events WHERE topic = 'push.customer' AND payload->>'stage' IN ('looking_for_rider', 'payment')") != 0 {
		t.Error("only the listed stages are pushed")
	}
	if x.outbox("push.rider") != 1 {
		t.Error("the rider was not told of the offer")
	}
	// The worker sends each push to every device, through the pusher.
	x.exec("INSERT INTO devices (actor_type, actor_id, platform, push_token) VALUES ('customer', $1, 'android', 'tok-a'), ('customer', $1, 'ios', 'tok-b')", x.cust)
	rec := &recorder{}
	w := &outbox.Worker{Pool: x.pool, Handle: x.svc.Deliver(rec), Log: quiet}
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rec.sent) != 2 || x.count("SELECT count(*) FROM outbox_events WHERE delivered_at IS NULL") != 0 {
		t.Errorf("%v", rec.sent)
	}
}

func TestARejectedQueuedHandoverAlertsSupport(t *testing.T) {
	tr := begin(t, nil)
	wrong := "0000"
	if tr.otp() == wrong {
		wrong = "1111"
	}
	tr.batch(tr.handoverEvent("evt-alert-wrong", wrong, 2, tr.clock))
	if tr.outbox("support.alert") != 1 {
		t.Error("support was not alerted about a rejected handover")
	}
}

func TestNoRiderPushDuringTheMinimumWait(t *testing.T) {
	x := withRiders(newFixture(t))
	ctx := context.Background()
	start := x.clock
	id := x.placed(x.chai, 1)
	x.pay(id, "paid")
	x.exec("UPDATE orders SET min_wait_until = $2 WHERE id = $1", id, start.Add(2*time.Minute))
	_, tok := x.signIn(x.scooter)
	x.dispatch(start.Add(6 * time.Second))
	o := x.offer(tok)
	x.clock = start.Add(10 * time.Second)
	x.call("POST", "/offers/"+o["id"].(string)+"/accept", tok, `{"fencing_token":"`+o["fencing_token"].(string)+`"}`)
	x.svc.Notify(ctx, x.clock)
	if x.count("SELECT count(*) FROM outbox_events WHERE topic = 'push.customer' AND payload->>'stage' = 'rider_accepted'") != 0 {
		t.Fatal("a push showed the rider during the minimum wait")
	}
	x.clock = start.Add(121 * time.Second)
	x.dispatch(x.clock)
	x.svc.Notify(ctx, x.clock)
	if x.count("SELECT count(*) FROM outbox_events WHERE topic = 'push.customer' AND payload->>'stage' = 'rider_accepted' AND payload ? 'seq'") != 1 {
		t.Error("one rider_accepted push, with its seq, once the wait is over")
	}
}

func TestTheSameRequestIsKeptOnce(t *testing.T) {
	f := newFixture(t)
	withSupport(f)
	for i := 0; i < 3; i++ {
		if code, out := f.send("POST", "/availability/notify", f.token, `{"gate_id":"`+f.gate+`","item_id":"`+f.chai+`"}`); code != 201 {
			t.Errorf("asking again: %d %v", code, out)
		}
	}
	if n := f.count("SELECT count(*) FROM availability_notifications"); n != 1 {
		t.Errorf("%d open requests for one gate and item", n)
	}
	f.exec("UPDATE menu_items SET status = 'hidden' WHERE id = $1", f.samosa)
	if code, _ := f.send("POST", "/availability/notify", f.token, `{"gate_id":"`+f.gate+`","item_id":"`+f.samosa+`"}`); code != 404 {
		t.Errorf("a hidden item: %d", code)
	}
	if code, _ := f.send("POST", "/support/tickets", f.token, `{"type":"other","text":"a\u0000b"}`); code != 400 {
		t.Errorf("a NUL in the text: %d", code)
	}
}

func TestThePushWorkerLeavesOtherEvents(t *testing.T) {
	f := newFixture(t)
	withSupport(f)
	f.send("POST", "/support/tickets", f.token, `{"type":"other","text":"hello"}`)
	w := &outbox.Worker{Pool: f.pool, Handle: f.svc.Deliver(&recorder{}), Log: quiet, Topics: PushTopics}
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.count("SELECT count(*) FROM outbox_events WHERE topic = 'support.ticket_created' AND delivered_at IS NULL AND attempts = 0") != 1 {
		t.Error("the push worker took a support event")
	}
}

func TestARejectedHandoverAlertNamesItsOrder(t *testing.T) {
	tr := begin(t, nil)
	wrong := "0000"
	if tr.otp() == wrong {
		wrong = "1111"
	}
	tr.batch(tr.handoverEvent("evt-alert-order", wrong, 2, tr.clock))
	if tr.count("SELECT count(*) FROM outbox_events WHERE topic = 'support.alert' AND payload->>'order_id' = $1 AND payload->>'reason' = 'handover_wrong_otp'", tr.order) != 1 {
		t.Error("the alert must name the order")
	}
}
