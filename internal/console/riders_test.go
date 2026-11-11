package console

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
)

// riderFixture adds a zone, a node, a scooter and riders in each state.
func (f *fixture) rider(phone, status, verification string) string {
	f.t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(), `WITH p AS (INSERT INTO people (phone) VALUES ($1) RETURNING id)
		INSERT INTO riders (person_id, name, status) SELECT id, 'Kiran', $2 FROM p RETURNING id::text`, phone, status).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	if verification != "" {
		f.exec("INSERT INTO rider_verifications (rider_id, status, method) VALUES ($1, $2, 'manual_upload')", id, verification)
	}
	return id
}

func (f *fixture) zone() string {
	f.t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(), "INSERT INTO zones (name, city, status) VALUES ('Rider zone', 'Bengaluru', 'active') RETURNING id::text").Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func TestTheRiderReviewQueue(t *testing.T) {
	f := newFixture(t)
	_, temp := f.user("ops.one", "ops")
	token := f.signIn("ops.one", temp)
	applicant := f.rider("+919811100001", "applicant", "pending")
	failed := f.rider("+919811100002", "active", "failed")
	f.rider("+919811100003", "active", "verified") // Not in the queue.
	code, out := f.call("GET", "/admin/riders/review", token, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	list := out["riders"].([]any)
	if len(list) != 2 || list[0].(map[string]any)["rider_id"] != applicant || list[1].(map[string]any)["rider_id"] != failed {
		t.Fatalf("applicants and failed verifications, oldest first: %v", out)
	}
	first := list[0].(map[string]any)
	if first["phone_last4"] != "0001" || first["verification"] != "pending" || first["status"] != "applicant" {
		t.Errorf("%v", first)
	}
	for k := range first {
		if k == "phone" {
			t.Error("the full phone number is shown")
		}
	}
}

func TestDecidingOnARider(t *testing.T) {
	f := newFixture(t)
	_, temp := f.user("ops.two", "ops")
	token := f.signIn("ops.two", temp)
	zone := f.zone()
	pending := f.rider("+919811100011", "applicant", "pending")
	if code, out := f.call("POST", "/admin/riders/"+pending+"/decision", token, `{"decision":"approve","zone_id":"`+zone+`","reason":"looks fine"}`); code != 409 || out["code"] != "not_verified" {
		t.Errorf("approving an unverified rider: %d %v", code, out)
	}
	ok := f.rider("+919811100012", "applicant", "verified")
	if code, _ := f.call("POST", "/admin/riders/"+ok+"/decision", token, `{"decision":"approve","reason":"documents checked"}`); code != 400 {
		t.Errorf("approving with no zone: %d", code)
	}
	// The contract asks for zone_id on approval, even when the applicant already has a zone.
	zoned := f.rider("+919811100013", "applicant", "verified")
	f.exec("UPDATE riders SET zone_id = $2 WHERE id = $1", zoned, zone)
	if code, _ := f.call("POST", "/admin/riders/"+zoned+"/decision", token, `{"decision":"approve","reason":"documents checked"}`); code != 400 {
		t.Errorf("approving with no zone_id: %d", code)
	}
	code, out := f.call("POST", "/admin/riders/"+ok+"/decision", token, `{"decision":"approve","zone_id":"`+zone+`","reason":"documents checked"}`)
	if code != 200 || out["rider"].(map[string]any)["status"] != "active" || out["rider"].(map[string]any)["zone_id"] != zone {
		t.Fatalf("%d %v", code, out)
	}
	if code, out := f.call("POST", "/admin/riders/"+ok+"/decision", token, `{"decision":"approve","zone_id":"`+zone+`","reason":"again"}`); code != 409 || out["code"] != "not_allowed" {
		t.Errorf("approving twice: %d %v", code, out)
	}
	// Suspending ends the rider's sessions and shift.
	var node, scooter string
	f.pool.QueryRow(context.Background(), "INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, 'N', 12.93, 77.62, 4) RETURNING id::text", zone).Scan(&node)
	f.pool.QueryRow(context.Background(), "INSERT INTO scooters (code, zone_id, node_id, current_rider_id) VALUES ('SC-R1', $1, $2, $3) RETURNING id::text", zone, node, ok).Scan(&scooter)
	f.exec("INSERT INTO shifts (rider_id, node_id, scooter_id) VALUES ($1, $2, $3)", ok, node, scooter)
	if err := database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		_, _, err := f.svc.Sessions.Start(context.Background(), tx, "rider", ok)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	code, out = f.call("POST", "/admin/riders/"+ok+"/decision", token, `{"decision":"suspend","reason":"left the scooter unattended"}`)
	if code != 200 || out["rider"].(map[string]any)["status"] != "suspended" {
		t.Fatalf("%d %v", code, out)
	}
	if f.count("SELECT count(*) FROM shifts WHERE rider_id = $1 AND ended_at IS NULL", ok) != 0 || f.count("SELECT count(*) FROM scooters WHERE current_rider_id = $1", ok) != 0 {
		t.Error("the shift and scooter were not freed")
	}
	if f.count("SELECT count(*) FROM refresh_tokens WHERE actor_type = 'rider' AND actor_id = $1 AND revoked_at IS NULL", ok) != 0 {
		t.Error("the rider's sessions were not ended")
	}
	if code, _ := f.call("POST", "/admin/riders/"+ok+"/decision", token, `{"decision":"reinstate","reason":"cleared"}`); code != 200 {
		t.Errorf("reinstate: %d", code)
	}
	if f.count("SELECT count(*) FROM audit_events WHERE entity_id = $1 AND action IN ('rider.approve', 'rider.suspend', 'rider.reinstate')", ok) != 3 {
		t.Error("every decision is audited")
	}
	if code, _ := f.call("POST", "/admin/riders/"+ok+"/decision", token, `{"decision":"reject","reason":"no"}`); code != 400 {
		t.Errorf("an unknown decision: %d", code)
	}
	_, supTemp := f.user("sup.two", "support")
	if code, out := f.call("GET", "/admin/riders/review", f.signIn("sup.two", supTemp), ""); code != 403 || out["missing_permission"] != "riders.review" {
		t.Errorf("support: %d %v", code, out)
	}
}

func (f *fixture) id(sql string, args ...any) string {
	f.t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

// onShift puts a rider on a shift with a scooter, and returns the scooter.
func (f *fixture) onShift(rider, zone, code string) string {
	f.t.Helper()
	node := f.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, $2, 12.93, 77.62, 4) RETURNING id::text", zone, "N-"+code)
	scooter := f.id("INSERT INTO scooters (code, zone_id, node_id, current_rider_id) VALUES ($1, $2, $3, $4) RETURNING id::text", code, zone, node, rider)
	f.exec("INSERT INTO shifts (rider_id, node_id, scooter_id) VALUES ($1, $2, $3)", rider, node, scooter)
	return scooter
}

// offer gives the rider an offer in the given state, and returns the order.
func (f *fixture) offer(rider, zone, scooter, state string) string {
	f.t.Helper()
	person := f.id("INSERT INTO people (phone) VALUES ('+9198222'||lpad((floor(random()*100000))::int::text, 5, '0')) RETURNING id::text")
	customer := f.id("INSERT INTO customers (person_id) VALUES ($1) RETURNING id::text", person)
	gate := f.id("INSERT INTO gates (customer_id, zone_id, label, lat, lng) VALUES ($1, $2, 'Gate', 12.93, 77.62) RETURNING id::text", customer, zone)
	history := f.id("INSERT INTO gate_history (gate_id, version, label, lat, lng, verified, reason) VALUES ($1, 1, 'Gate', 12.93, 77.62, false, 'test') RETURNING id::text", gate)
	order := f.id(`INSERT INTO orders (customer_id, idempotency_key, gate_id, gate_history_id, zone_id, scooter_id, global_settings, state, total_paise, hold_until, paid_at)
		VALUES ($1, $2, $3, $4, $5, $6, (SELECT id FROM zone_settings WHERE zone_id IS NULL ORDER BY version DESC LIMIT 1), 'OFFERED', 9900, now(), now()) RETURNING id::text`,
		customer, "order-"+scooter, gate, history, zone, scooter)
	f.exec(`INSERT INTO offers (order_id, rider_id, scooter_id, fencing_token, expires_at, state, rider_accepted_at)
		VALUES ($1, $2, $3, $4, now() + interval '1 minute', $5::text, CASE WHEN $5::text = 'accepted' THEN now() END)`,
		order, rider, scooter, "fence-"+scooter, state)
	return order
}

func TestSuspendingARiderOnTheRoad(t *testing.T) {
	f := newFixture(t)
	_, temp := f.user("ops.four", "ops")
	token := f.signIn("ops.four", temp)
	zone := f.zone()
	carrying := f.rider("+919811100021", "active", "verified")
	f.exec("UPDATE riders SET zone_id = $2 WHERE id = $1", carrying, zone)
	order := f.offer(carrying, zone, f.onShift(carrying, zone, "SC-R2"), "accepted")
	if code, out := f.call("POST", "/admin/riders/"+carrying+"/decision", token, `{"decision":"suspend","reason":"complaint received"}`); code != 409 || out["code"] != "order_in_progress" {
		t.Errorf("suspending a rider carrying an order: %d %v", code, out)
	}
	if f.count("SELECT count(*) FROM riders WHERE id = $1 AND status = 'active'", carrying) != 1 || f.count("SELECT count(*) FROM shifts WHERE rider_id = $1 AND ended_at IS NULL", carrying) != 1 {
		t.Error("a refused suspension changed something")
	}
	// An order only offered goes back to be offered again, and the history names the console user.
	offered := f.rider("+919811100022", "active", "verified")
	f.exec("UPDATE riders SET zone_id = $2 WHERE id = $1", offered, zone)
	back := f.offer(offered, zone, f.onShift(offered, zone, "SC-R3"), "open")
	if code, out := f.call("POST", "/admin/riders/"+offered+"/decision", token, `{"decision":"suspend","reason":"complaint received"}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if f.count("SELECT count(*) FROM orders WHERE id = $1 AND state = 'PAYMENT_SECURED'", back) != 1 {
		t.Error("the offered order did not go back")
	}
	if f.count("SELECT count(*) FROM order_events WHERE order_id = $1 AND actor_type = 'console' AND reason = 'shift ended'", back) != 1 {
		t.Error("the order history must name the console user, not the rider")
	}
	if f.count("SELECT count(*) FROM audit_events WHERE action = 'shift.ended' AND actor_type = 'console'") != 1 {
		t.Error("the shift end is audited as the console user's")
	}
	_ = order
}

func TestZonesQueueRulesAndRejection(t *testing.T) {
	f := newFixture(t)
	_, temp := f.user("ops.five", "ops")
	token := f.signIn("ops.five", temp)
	paused := f.id("INSERT INTO zones (name, city, status) VALUES ('Paused', 'Bengaluru', 'paused') RETURNING id::text")
	ok := f.rider("+919811100031", "applicant", "verified")
	if code, _ := f.call("POST", "/admin/riders/"+ok+"/decision", token, `{"decision":"approve","zone_id":"`+paused+`","reason":"documents checked"}`); code != 400 {
		t.Errorf("approving into a paused zone: %d", code)
	}
	if code, _ := f.call("POST", "/admin/riders/"+ok+"/decision", token, `{"decision":"approve","zone_id":"00000000-0000-0000-0000-000000000001","reason":"documents checked"}`); code != 400 {
		t.Errorf("approving into an unknown zone: %d", code)
	}
	// Turning an applicant down is a suspension; they leave the queue.
	no := f.rider("+919811100032", "applicant", "failed")
	if code, _ := f.call("POST", "/admin/riders/"+no+"/decision", token, `{"decision":"suspend","reason":"documents rejected"}`); code != 200 {
		t.Errorf("rejecting an applicant: %d", code)
	}
	// A verification past its expiry shows as expired, and the rider is in the queue.
	lapsed := f.rider("+919811100033", "active", "")
	f.exec("INSERT INTO rider_verifications (rider_id, status, method, expires_at) VALUES ($1, 'verified', 'manual_upload', now() - interval '1 day')", lapsed)
	_, out := f.call("GET", "/admin/riders/review", token, "")
	seen := map[string]string{}
	for _, r := range out["riders"].([]any) {
		seen[r.(map[string]any)["rider_id"].(string)] = r.(map[string]any)["verification"].(string)
	}
	if _, listed := seen[no]; listed {
		t.Error("a turned-down applicant is still listed")
	}
	if seen[lapsed] != "expired" || seen[ok] != "verified" || len(seen) != 2 {
		t.Errorf("%v", seen)
	}
	// Paging: one at a time, oldest first, no repeats.
	_, first := f.call("GET", "/admin/riders/review?limit=1", token, "")
	next, _ := first["next_cursor"].(string)
	_, second := f.call("GET", "/admin/riders/review?limit=1&cursor="+next, token, "")
	a, b := first["riders"].([]any), second["riders"].([]any)
	if next == "" || len(a) != 1 || len(b) != 1 || a[0].(map[string]any)["rider_id"] != ok || b[0].(map[string]any)["rider_id"] != lapsed || second["next_cursor"] != nil {
		t.Errorf("paging: %v then %v", first, second)
	}
}
