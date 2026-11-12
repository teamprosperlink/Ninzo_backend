package orders

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/outbox"
)

// acceptedOrder places a paid order, has the rider accept it, and returns the order, the scooter's
// code and the offer details. The minimum wait is 60 seconds from the start.
func acceptedOrder(t *testing.T) (x *riders, id, plate string, start time.Time) {
	t.Helper()
	x = withRiders(newFixture(t))
	start = x.clock
	id = x.placed(x.chai, 1)
	x.pay(id, "paid")
	x.exec("UPDATE orders SET min_wait_until = $2 WHERE id = $1", id, start.Add(60*time.Second))
	_, tok := x.signIn(x.scooter)
	x.dispatch(start.Add(6 * time.Second))
	o := x.offer(tok)
	x.clock = start.Add(10 * time.Second)
	if code, out := x.call("POST", "/offers/"+o["id"].(string)+"/accept", tok, `{"fencing_token":"`+o["fencing_token"].(string)+`"}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	return x, id, x.id("SELECT code FROM scooters WHERE id = $1", x.scooter), start
}

func riderOnOrder(x *riders, id string) any {
	_, out := x.send("GET", "/orders/"+id, x.token, "")
	return orderOf(out)["rider"]
}

// TestTheScooterPlateShowsWithTheRider follows the plate with the first name (issue 178).
func TestTheScooterPlateShowsWithTheRider(t *testing.T) {
	x, id, plate, start := acceptedOrder(t)
	// During the minimum wait the rider is not shown, so neither is the plate.
	if r := riderOnOrder(x, id); r != nil {
		t.Fatalf("the rider was shown during the minimum wait: %v", r)
	}
	// Once the wait is over the rider is shown with the plate of the scooter on their shift.
	x.clock = start.Add(61 * time.Second)
	r, ok := riderOnOrder(x, id).(map[string]any)
	if !ok || r["scooter_plate"] != plate || plate == "" || r["first_name"] == nil {
		t.Fatalf("rider %v, plate on the scooter %q", r, plate)
	}
	// Another rider's scooter is not the one shown.
	second := x.scooterOnShift("SC-2")
	otherPlate := x.id("SELECT code FROM scooters WHERE id = $1", second)
	if r, _ := riderOnOrder(x, id).(map[string]any); second == x.scooter || r["scooter_plate"] == otherPlate || r["scooter_plate"] != plate {
		t.Errorf("the plate is not this rider's: %v, the other rider has %s", r, otherPlate)
	}
	// The plate stays after the shift ends, as the order does.
	x.exec("UPDATE shifts SET ended_at = now() WHERE scooter_id = $1 AND ended_at IS NULL", x.scooter)
	if r := riderOnOrder(x, id).(map[string]any); r["scooter_plate"] != plate {
		t.Errorf("after the shift ended: %v", r)
	}
	// A later shift of the same rider on another scooter, even one that seems to start before the
	// accept (clocks that disagree), does not change the order's plate: it is the accepted offer's
	// scooter (#178 review).
	rider := x.id("SELECT rider_id::text FROM orders WHERE id = $1", id)
	x.exec("UPDATE shifts SET rider_id = $1, started_at = (SELECT rider_accepted_at FROM orders WHERE id = $3) - interval '1 second' WHERE scooter_id = $2 AND ended_at IS NULL", rider, second, id)
	if r := riderOnOrder(x, id).(map[string]any); r["scooter_plate"] != plate {
		t.Errorf("another shift of the rider changed the plate: %v", r)
	}
}

func TestTheScooterPlateIsNullWithoutOne(t *testing.T) {
	x, id, _, start := acceptedOrder(t)
	x.clock = start.Add(61 * time.Second)
	// The scooter the order was accepted on has no plate on record (a blank code).
	x.exec("UPDATE scooters SET code = ' ' WHERE id = $1", x.scooter)
	r, ok := riderOnOrder(x, id).(map[string]any)
	if !ok {
		t.Fatal("the rider is still shown")
	}
	if v, present := r["scooter_plate"]; !present || v != nil {
		t.Errorf("scooter_plate %v (present %v)", v, present)
	}
	if r["first_name"] == nil {
		t.Error("the first name is still shown")
	}
}

// pushTo puts a push on the outbox for a customer.
func pushTo(t *testing.T, x *fixture, customer, stage string) {
	t.Helper()
	err := database.InTx(context.Background(), x.pool, func(tx pgx.Tx) error {
		return outbox.Add(context.Background(), tx, "push.customer", map[string]any{"customer_id": customer, "stage": stage})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (x *fixture) otherCustomer(phone string) string {
	x.t.Helper()
	var person, id string
	if err := x.pool.QueryRow(context.Background(), "INSERT INTO people (phone) VALUES ($1) RETURNING id::text", phone).Scan(&person); err != nil {
		x.t.Fatal(err)
	}
	if err := x.pool.QueryRow(context.Background(), "INSERT INTO customers (person_id) VALUES ($1) RETURNING id::text", person).Scan(&id); err != nil {
		x.t.Fatal(err)
	}
	return id
}

func stagesSentTo(rec *recorder, token string) []string {
	var out []string
	for _, s := range rec.sent {
		if s["token"] == token {
			out = append(out, s["stage"].(string))
		}
	}
	return out
}

// TestSupportReplyPushesFollowTheCustomersSetting: a ticket reply is skipped when support replies
// are off, an order push never is, and another customer's setting does not matter.
func TestSupportReplyPushesFollowTheCustomersSetting(t *testing.T) {
	x := newFixture(t)
	ctx := context.Background()
	quietOne := x.cust                             // turns support replies off
	defaultOne := x.otherCustomer("+919800000077") // no row: the default is on
	onOne := x.otherCustomer("+919800000078")      // switched off and on again
	x.exec("INSERT INTO devices (actor_type, actor_id, platform, push_token) VALUES ('customer', $1, 'android', 'tok-off'), ('customer', $2, 'android', 'tok-default'), ('customer', $3, 'android', 'tok-on')", quietOne, defaultOne, onOne)
	x.exec("INSERT INTO customer_notification_settings (customer_id, support_replies) VALUES ($1, false)", quietOne)
	x.exec("INSERT INTO customer_notification_settings (customer_id, support_replies, offers_and_news) VALUES ($1, true, true)", onOne)
	for _, c := range []string{quietOne, defaultOne, onOne} {
		pushTo(t, x, c, "ticket_reply")
		pushTo(t, x, c, "no_scooter") // An order push.
		pushTo(t, x, c, "arrived")
	}
	rec := &recorder{}
	w := &outbox.Worker{Pool: x.pool, Handle: x.svc.Deliver(rec), Log: quiet, Topics: PushTopics}
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := stagesSentTo(rec, "tok-off"); len(got) != 2 || got[0] == "ticket_reply" || got[1] == "ticket_reply" {
		t.Errorf("support replies off: sent %v, want only the two order pushes", got)
	}
	for _, tok := range []string{"tok-default", "tok-on"} {
		if got := stagesSentTo(rec, tok); len(got) != 3 {
			t.Errorf("%s: sent %v, want the reply and both order pushes", tok, got)
		}
	}
	// The skipped push is finished, not left to retry for ever.
	if n := x.count("SELECT count(*) FROM outbox_events WHERE delivered_at IS NULL"); n != 0 {
		t.Errorf("%d pushes left undelivered", n)
	}
	// Turning it back on sends the next reply.
	x.exec("UPDATE customer_notification_settings SET support_replies = true WHERE customer_id = $1", quietOne)
	pushTo(t, x, quietOne, "ticket_reply")
	rec.sent = nil
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := stagesSentTo(rec, "tok-off"); len(got) != 1 || got[0] != "ticket_reply" {
		t.Errorf("after switching it on: %v", got)
	}
}
