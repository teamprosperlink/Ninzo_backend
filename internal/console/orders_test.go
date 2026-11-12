package console

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"github.com/teamprosperlink/Ninzo_backend/contracts"
)

// schemaFor compiles a schema of the console contract, so a reply is checked against the real
// contract and not a copy of it.
func schemaFor(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	raw, err := contracts.Dist.ReadFile("dist/console.openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	normal, _ := json.Marshal(doc)
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(normal))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource("mem://console", inst); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("mem://console#/components/schemas/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// matchesContract fails the test if the reply does not fit the named schema.
func matchesContract(t *testing.T, schema string, reply map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(reply)
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := schemaFor(t, schema).Validate(inst); err != nil {
		t.Fatalf("the reply does not match %s: %v\n%s", schema, err, raw)
	}
}

// shopper adds a customer with the phone.
func (f *fixture) shopper(phone string) string {
	f.t.Helper()
	person := f.id("INSERT INTO people (phone) VALUES ($1) RETURNING id::text", phone)
	return f.id("INSERT INTO customers (person_id) VALUES ($1) RETURNING id::text", person)
}

// courier adds an active rider with the phone and name.
func (f *fixture) courier(phone, name string) string {
	f.t.Helper()
	person := f.id("INSERT INTO people (phone) VALUES ($1) RETURNING id::text", phone)
	return f.id("INSERT INTO riders (person_id, name, status) VALUES ($1, $2, 'active') RETURNING id::text", person, name)
}

// orderSpec is what an order in these tests needs.
type orderSpec struct {
	customer, zone, state string
	rider                 string // Empty for none.
	ago                   time.Duration
	orderNo               int64 // Zero lets the database number it.
}

// makeOrder inserts an order created `ago` before now, with its own gate and scooter.
func (f *fixture) makeOrder(s orderSpec) string {
	f.t.Helper()
	f.n++
	tag := fmt.Sprintf("%d", f.n)
	label := "Tower " + tag + " gate"
	gate := f.id("INSERT INTO gates (customer_id, zone_id, label, lat, lng, entry_notes) VALUES ($1, $2, $3, 12.93, 77.62, 'Flat 4B, ring the bell twice') RETURNING id::text",
		s.customer, s.zone, label)
	history := f.id("INSERT INTO gate_history (gate_id, version, label, lat, lng, entry_notes, verified, reason) VALUES ($1, 1, $2, 12.93, 77.62, 'Flat 4B, ring the bell twice', true, 'test') RETURNING id::text",
		gate, label)
	node := f.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, $2, 12.93, 77.62, 4) RETURNING id::text", s.zone, "N"+tag)
	scooter := f.id("INSERT INTO scooters (code, zone_id, node_id) VALUES ($1, $2, $3) RETURNING id::text", "SC"+tag, s.zone, node)
	var ended *string
	if e, ok := map[string]string{"CANCELLED": "cancelled", "FAILED": "failed", "TIMED_OUT": "timed_out"}[s.state]; ok {
		ended = &e
	}
	var riderID *string
	if s.rider != "" {
		riderID = &s.rider
	}
	columns := "customer_id, idempotency_key, gate_id, gate_history_id, zone_id, scooter_id, global_settings, state, total_paise, hold_until, paid_at, created_at, rider_id, ended_reason"
	values := "$1, $2, $3, $4, $5, $6, (SELECT id FROM zone_settings WHERE zone_id IS NULL ORDER BY version DESC LIMIT 1), $7, 14900, $8, $8, $8, $9, $10"
	args := []any{s.customer, "k-" + tag, gate, history, s.zone, scooter, s.state, f.now.Add(-s.ago), riderID, ended}
	if s.orderNo != 0 {
		columns += ", order_no"
		values += ", $11"
		args = append(args, s.orderNo)
	}
	return f.id("INSERT INTO orders ("+columns+") OVERRIDING SYSTEM VALUE VALUES ("+values+") RETURNING id::text", args...)
}

// orderFixture is a small shop: two zones, two customers, two riders and seven orders.
type orderFixture struct {
	f                  *fixture
	token              string
	zoneA, zoneB       string
	c1, c2, r1, r2     string
	o1, o2, o3, o4, o5 string
	wrapB, wrapA       string // Numbered B1003, and A1003 again a full cycle after o3.
	c1Phone, r1Phone   string
}

func newOrderFixture(t *testing.T) *orderFixture {
	f := newFixture(t)
	_, temp := f.user("orders.ops", "ops")
	o := &orderFixture{f: f, token: f.signIn("orders.ops", temp)}
	o.zoneA, o.zoneB = f.zone2("OA"), f.zone2("OB")
	o.c1Phone, o.r1Phone = "+919800001111", "+919811113333"
	o.c1, o.c2 = f.shopper(o.c1Phone), f.shopper("+919800002222")
	o.r1, o.r2 = f.courier(o.r1Phone, "Ravi"), f.courier("+919811221111", "Meena")
	o.o1 = f.makeOrder(orderSpec{customer: o.c1, zone: o.zoneA, state: "COMPLETED", rider: o.r1, ago: 50 * time.Minute})     // A1001
	o.o2 = f.makeOrder(orderSpec{customer: o.c2, zone: o.zoneA, state: "CANCELLED", ago: 40 * time.Minute})                  // A1002
	o.o3 = f.makeOrder(orderSpec{customer: o.c1, zone: o.zoneB, state: "TIMER_RUNNING", rider: o.r1, ago: 30 * time.Minute}) // A1003
	o.o4 = f.makeOrder(orderSpec{customer: o.c2, zone: o.zoneA, state: "OFFERED", rider: o.r2, ago: 20 * time.Minute})       // A1004
	o.o5 = f.makeOrder(orderSpec{customer: o.c2, zone: o.zoneB, state: "FAILED", ago: 10 * time.Minute})                     // A1005
	o.wrapB = f.makeOrder(orderSpec{customer: o.c2, zone: o.zoneA, state: "COMPLETED", ago: 5 * time.Minute, orderNo: 11003})
	o.wrapA = f.makeOrder(orderSpec{customer: o.c2, zone: o.zoneA, state: "COMPLETED", ago: 4 * time.Minute, orderNo: 261003})
	return o
}

func (o *orderFixture) list(query string) (int, map[string]any) {
	o.f.t.Helper()
	return o.f.call("GET", "/admin/orders"+query, o.token, "")
}

// ids lists the order ids of a list reply, in order.
func ids(reply map[string]any) []string {
	out := []string{}
	for _, x := range reply["orders"].([]any) {
		out = append(out, x.(map[string]any)["id"].(string))
	}
	return out
}

func sameIDs(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

func TestSearchByOrderNumber(t *testing.T) {
	o := newOrderFixture(t)
	for q, want := range map[string][]string{
		"A1001": {o.o1}, "a1004": {o.o4}, " A1005 ": {o.o5}, "B1003": {o.wrapB}, "A1003": {o.wrapA, o.o3}, "A9999": {}, "Z0001": {},
	} {
		code, out := o.list("?q=" + url.QueryEscape(q))
		if code != 200 || !sameIDs(ids(out), want) {
			t.Errorf("q=%q: %d %v, want %v", q, code, ids(out), want)
		}
	}
	code, out := o.list("?q=A1002")
	if code != 200 {
		t.Fatal(code)
	}
	row := out["orders"].([]any)[0].(map[string]any)
	if row["order_number"] != "A1002" || row["state"] != "CANCELLED" || row["ended_reason"] != "cancelled" || row["rider"] != nil {
		t.Errorf("summary: %v", row)
	}
}

func TestSearchByPhoneDigitsAndOrderId(t *testing.T) {
	o := newOrderFixture(t)
	// 1111 ends customer 1's phone and rider 2's phone: both their orders come back.
	if code, out := o.list("?q=1111"); code != 200 || !sameIDs(ids(out), []string{o.o4, o.o3, o.o1}) {
		t.Errorf("q=1111: %d %v", code, ids(out))
	}
	// 3333 ends rider 1's phone only.
	if code, out := o.list("?q=3333"); code != 200 || !sameIDs(ids(out), []string{o.o3, o.o1}) {
		t.Errorf("q=3333: %d %v", code, ids(out))
	}
	if code, out := o.list("?q=2222"); code != 200 || !sameIDs(ids(out), []string{o.wrapA, o.wrapB, o.o5, o.o4, o.o2}) {
		t.Errorf("q=2222 (customer 2): %d %v", code, ids(out))
	}
	if code, out := o.list("?q=0000"); code != 200 || len(ids(out)) != 0 {
		t.Errorf("q=0000: %d %v", code, out)
	}
	if code, out := o.list("?q=" + strings.ToUpper(o.o2)); code != 200 || !sameIDs(ids(out), []string{o.o2}) {
		t.Errorf("q=order id: %d %v", code, ids(out))
	}
	if code, out := o.list("?q=00000000-0000-0000-0000-000000000000"); code != 200 || len(ids(out)) != 0 {
		t.Errorf("an id nobody has: %d %v", code, out)
	}
	for _, bad := range []string{"hello", "+919800001111", "12345", "A100", "AB1001", strings.Repeat("1", 65)} {
		if code, out := o.list("?q=" + url.QueryEscape(bad)); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("q=%q: %d %v", bad, code, out)
		}
	}
	if code, out := o.list("?q="); code != 200 || len(ids(out)) != 7 {
		t.Errorf("an empty q is no filter: %d %d", code, len(ids(out)))
	}
}

func TestOrderFiltersAndPaging(t *testing.T) {
	o := newOrderFixture(t)
	if _, out := o.list(""); !sameIDs(ids(out), []string{o.wrapA, o.wrapB, o.o5, o.o4, o.o3, o.o2, o.o1}) {
		t.Errorf("newest first: %v", ids(out))
	}
	if _, out := o.list("?state=TIMER_RUNNING"); !sameIDs(ids(out), []string{o.o3}) {
		t.Errorf("state: %v", ids(out))
	}
	if _, out := o.list("?state=CREATED"); len(ids(out)) != 0 {
		t.Errorf("a valid state nobody is in: %v", ids(out))
	}
	if _, out := o.list("?zone_id=" + o.zoneB); !sameIDs(ids(out), []string{o.o5, o.o3}) {
		t.Errorf("zone: %v", ids(out))
	}
	if _, out := o.list("?zone_id=" + o.zoneA + "&state=COMPLETED"); !sameIDs(ids(out), []string{o.wrapA, o.wrapB, o.o1}) {
		t.Errorf("zone and state: %v", ids(out))
	}
	from := url.QueryEscape(o.f.now.Add(-45 * time.Minute).Format(time.RFC3339))
	to := url.QueryEscape(o.f.now.Add(-15 * time.Minute).Format(time.RFC3339))
	if _, out := o.list("?from=" + from + "&to=" + to); !sameIDs(ids(out), []string{o.o4, o.o3, o.o2}) {
		t.Errorf("from and to: %v", ids(out))
	}
	if _, out := o.list("?from=" + to); !sameIDs(ids(out), []string{o.wrapA, o.wrapB, o.o5}) {
		t.Errorf("from alone: %v", ids(out))
	}
	if _, out := o.list("?q=1111&zone_id=" + o.zoneA); !sameIDs(ids(out), []string{o.o4, o.o1}) {
		t.Errorf("q with a filter: %v", ids(out))
	}
	for _, bad := range []string{"?state=LOST", "?zone_id=nope", "?from=yesterday", "?to=2026-13-40", "?limit=0", "?limit=101", "?limit=x", "?cursor=!!"} {
		if code, out := o.list(bad); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", bad, code, out)
		}
	}
	// Paging by twos walks all seven once, newest first.
	var got []string
	cursor := ""
	for i := 0; i < 10; i++ {
		code, out := o.list("?limit=2" + cursor)
		if code != 200 {
			t.Fatalf("page %d: %d %v", i, code, out)
		}
		got = append(got, ids(out)...)
		if out["next_cursor"] == nil {
			break
		}
		cursor = "&cursor=" + url.QueryEscape(out["next_cursor"].(string))
	}
	if !sameIDs(got, []string{o.wrapA, o.wrapB, o.o5, o.o4, o.o3, o.o2, o.o1}) {
		t.Errorf("paging: %v", got)
	}
	// A page that ends exactly at the end has no next cursor.
	if _, out := o.list("?limit=7"); out["next_cursor"] != nil || len(ids(out)) != 7 {
		t.Errorf("the last page: %v", out["next_cursor"])
	}
	// Orders made in the same instant still page without a gap or a repeat.
	o.f.exec("UPDATE orders SET created_at = $1", o.f.now.Add(-time.Hour))
	got, cursor = nil, ""
	for i := 0; i < 10; i++ {
		_, out := o.list("?limit=3" + cursor)
		got = append(got, ids(out)...)
		if out["next_cursor"] == nil {
			break
		}
		cursor = "&cursor=" + url.QueryEscape(out["next_cursor"].(string))
	}
	seen := map[string]bool{}
	for _, id := range got {
		seen[id] = true
	}
	if len(got) != 7 || len(seen) != 7 {
		t.Errorf("equal times: %v", got)
	}
}

func TestOrderRepliesFitTheContractAndHideThePhone(t *testing.T) {
	o := newOrderFixture(t)
	code, out := o.list("?q=A1001")
	if code != 200 {
		t.Fatal(code)
	}
	matchesContract(t, "AdminOrderList", out)
	row := out["orders"].([]any)[0].(map[string]any)
	customer := row["customer"].(map[string]any)
	if customer["phone_last4"] != "1111" || row["rider"].(map[string]any)["name"] != "Ravi" || row["total"].(map[string]any)["amount_minor"] != float64(14900) {
		t.Errorf("summary: %v", row)
	}
	_, all := o.list("?limit=100")
	matchesContract(t, "AdminOrderList", all)
	code, tl := o.f.call("GET", "/admin/orders/"+o.o1+"/timeline", o.token, "")
	if code != 200 {
		t.Fatal(code, tl)
	}
	matchesContract(t, "OrderTimeline", tl)
	for name, reply := range map[string]map[string]any{"list": all, "timeline": tl} {
		raw, _ := json.Marshal(reply)
		for _, secret := range []string{"9800001111", "9800002222", "9811113333", "9811221111", "+91", "Flat 4B", "ring the bell", "handover_otp"} {
			if strings.Contains(string(raw), secret) {
				t.Errorf("the %s reply holds %q: %s", name, secret, raw)
			}
		}
		if strings.Contains(string(raw), `"phone"`) {
			t.Errorf("the %s reply has a phone field", name)
		}
	}
	if !strings.Contains(fmt.Sprint(tl["order"].(map[string]any)["customer"]), "1111") {
		t.Error("the last four digits stay")
	}
}

func TestOrdersNeedThePermission(t *testing.T) {
	o := newOrderFixture(t)
	f := o.f
	_, temp := f.user("orders.hub", "hub_manager")
	hub := f.signIn("orders.hub", temp)
	for _, path := range []string{"/admin/orders", "/admin/orders/" + o.o1 + "/timeline"} {
		if code, out := f.call("GET", path, hub, ""); code != 403 || out["missing_permission"] != "orders.read" {
			t.Errorf("hub_manager %s: %d %v", path, code, out)
		}
		if code, _ := f.call("GET", path, "", ""); code != 401 {
			t.Errorf("no token %s: %d", path, code)
		}
	}
	for _, role := range []string{"support", "admin"} {
		login := "orders." + role
		_, temp := f.user(login, role)
		if code, _ := f.call("GET", "/admin/orders", f.signIn(login, temp), ""); code != 200 {
			t.Errorf("%s: %d", role, code)
		}
	}
}

func TestTheTimelineMergesEverySourceOldestFirst(t *testing.T) {
	o := newOrderFixture(t)
	f := o.f
	order := o.o1
	start := f.now.Add(-2 * time.Hour).Truncate(time.Microsecond)
	at := func(m float64) time.Time { return start.Add(time.Duration(m * float64(time.Minute))) }
	operator := f.id("SELECT id::text FROM console_users WHERE login_id = 'orders.ops'")
	f.exec("UPDATE orders SET created_at = $2, timer_started_at = $3, timer_stopped_at = $4, timer_length_s = 90, zone_radius_m = 350, arrival_perimeter_m = 100, timer_missed = false, arrival_source = 'gps' WHERE id = $1",
		order, at(0), at(7), at(8))
	str := func(s string) *string { return &s }
	ev := func(m float64, from *string, to, actor string, actorID *string, reason *string) {
		f.exec("INSERT INTO order_events (order_id, from_state, to_state, actor_type, actor_id, reason, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7)", order, from, to, actor, actorID, reason, at(m))
	}
	ev(0, nil, "PAYMENT_PENDING", "customer", nil, nil)
	ev(2, str("PAYMENT_PENDING"), "PAYMENT_SECURED", "system", nil, nil)
	ev(4, str("OFFERED"), "RIDER_ACCEPTED", "rider", &o.r1, nil)
	ev(13, str("RIDER_ACCEPTED"), "RIDER_ACCEPTED", "console", &operator, str("arrival review: late"))
	// The payment settles at the same instant as PAYMENT_SECURED, and the offer is accepted at the instant of RIDER_ACCEPTED.
	f.exec("INSERT INTO payments (order_id, provider, provider_order_id, amount_paise, status, created_at, updated_at) VALUES ($1, 'test', 'po-1', 14900, 'paid', $2, $3)", order, at(0.5), at(2))
	f.exec("INSERT INTO offers (order_id, rider_id, scooter_id, fencing_token, expires_at, state, rider_accepted_at, created_at, updated_at) VALUES ($1, $2, (SELECT scooter_id FROM orders WHERE id = $1), 'f1', $3, 'accepted', $4, $5, $4)",
		order, o.r1, at(5), at(4), at(3))
	f.exec("INSERT INTO remedies (order_id, type, reason, amount_paise, created_at) VALUES ($1, 'credit', 'missed the timer', 5000, $2)", order, at(10))
	f.exec("INSERT INTO refunds (order_id, payment_id, reason_code, amount_paise, status, created_at, updated_at) VALUES ($1, (SELECT id FROM payments WHERE order_id = $1), 'cold_food', 4000, 'processed', $2, $3)", order, at(12), at(14))
	f.exec("INSERT INTO problem_reports (rider_id, order_id, type, source, note, outcome, created_at) VALUES ($1, $2, 'gate_closed', 'rider', 'call me on +919900000000', 'rematch', $3)", o.r1, order, at(6))
	ticket := f.id("INSERT INTO support_tickets (customer_id, order_id, type, status, created_at, updated_at) VALUES ($1, $2, 'cold_food', 'open', $3, $3) RETURNING id::text", o.c1, order, at(11))
	f.exec("INSERT INTO ticket_messages (ticket_id, sender, text, created_at) VALUES ($1, 'customer', 'my number is 9876543210', $2)", ticket, at(11.5))
	f.exec("INSERT INTO arrival_reviews (order_id, lat, lng, device_time, due_at, status, decided_at, decided_by, decision_reason, created_at) VALUES ($1, 12.9, 77.6, $2, $3, 'late', $4, $5, 'pings show 1:40', $6)",
		order, at(8), at(18), at(13), operator, at(9))
	f.exec(`INSERT INTO audit_events (actor_type, actor_id, action, entity_type, entity_id, before, after, reason, created_at)
		VALUES ('console', $1, 'arrival_review.late', 'order', $2, '{"phone":"+919800001111"}', '{}', 'pings show 1:40, customer says call 9800002222', $3)`, operator, order, at(13))
	f.exec("INSERT INTO audit_events (actor_type, action, entity_type, entity_id, reason, created_at) VALUES ('system', 'refund.retry', 'refund', (SELECT id::text FROM refunds WHERE order_id = $1), 'retry', $2)", order, at(15))
	f.exec("INSERT INTO audit_events (actor_type, action, entity_type, entity_id, created_at) VALUES ('system', 'order.created', 'order', $1, $2)", o.o3, at(1)) // Another order's.
	for _, m := range []float64{5, 6, 7} {
		f.exec("INSERT INTO location_pings (rider_id, order_id, lat, lng, speed_mps, accuracy_m, device_time, received_at, applied) VALUES ($1, $2, 12.93, 77.62, 3, 5, $3, $3, true)", o.r1, order, at(m))
	}
	f.exec("INSERT INTO location_pings (rider_id, order_id, lat, lng, speed_mps, accuracy_m, device_time, received_at, applied) VALUES ($1, $2, 12.93, 77.62, 3, 5, $3, $3, true)", o.r1, o.o3, at(9)) // Another order's.

	code, out := f.call("GET", "/admin/orders/"+order+"/timeline", o.token, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	matchesContract(t, "OrderTimeline", out)
	var got []string
	var last time.Time
	events := out["events"].([]any)
	for _, e := range events {
		m := e.(map[string]any)
		when, err := time.Parse(time.RFC3339Nano, m["at"].(string))
		if err != nil || when.Before(last) {
			t.Errorf("events are not oldest first at %v", m)
		}
		last = when
		got = append(got, m["source"].(string)+":"+m["kind"].(string))
	}
	// Ties (minutes 2, 4 and 13) fall in the contract's source order: order, offer, payment, ..., review, audit.
	want := []string{"order:order.created", "payment:payment.created", "order:order.payment_secured", "payment:payment.captured", "offer:offer.made",
		"order:order.rider_accepted", "offer:offer.accepted", "problem:problem.reported", "review:review.opened", "remedy:remedy.owed", "ticket:ticket.opened",
		"ticket:ticket.message", "refund:refund.requested", "order:order.noted", "review:review.late", "audit:arrival_review.late",
		"refund:refund.processed", "audit:refund.retry"}
	if !sameIDs(got, want) {
		t.Errorf("events:\n got %v\nwant %v", got, want)
	}
	byKind := func(source, kind string) map[string]any {
		for _, e := range events {
			if m := e.(map[string]any); m["kind"] == kind && m["source"] == source {
				return m
			}
		}
		t.Fatalf("no %s event from %s", kind, source)
		return nil
	}
	if e := byKind("offer", "offer.accepted"); e["actor_type"] != "rider" || e["actor_name"] != "Ravi" {
		t.Errorf("offer.accepted: %v", e)
	}
	if e := byKind("review", "review.late"); e["actor_type"] != "console" || e["actor_name"] != "Name orders.ops" {
		t.Errorf("review.late: %v", e)
	}
	if e := byKind("audit", "arrival_review.late"); e["actor_name"] != "Name orders.ops" || e["detail"].(map[string]any)["reason"] != nil {
		t.Errorf("audit: %v", e)
	}
	if e := byKind("order", "order.created"); e["actor_type"] != "customer" || e["actor_name"] != nil {
		t.Errorf("a customer is never named: %v", e)
	}
	if e := byKind("payment", "payment.captured"); e["detail"].(map[string]any)["amount"].(map[string]any)["amount_minor"] != float64(14900) {
		t.Errorf("payment: %v", e)
	}
	n := 0
	for _, e := range events {
		if e.(map[string]any)["kind"] == "order.created" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("another order's audit entry is in this timeline: %d", n)
	}
	pings := out["pings"].(map[string]any)
	if pings["count"] != float64(3) || pings["first_at"] != at(5).UTC().Format(time.RFC3339Nano) || pings["last_at"] != at(7).UTC().Format(time.RFC3339Nano) {
		t.Errorf("pings: %v", pings)
	}
	if n := len(events); n != len(want) || strings.Contains(fmt.Sprint(events), "source:ping") || strings.Contains(fmt.Sprint(events), "location") {
		t.Error("a ping is never an event")
	}
	head := out["order"].(map[string]any)
	timer := head["timer"].(map[string]any)
	if timer["timer_duration_s"] != float64(90) || timer["perimeter_m"] != float64(100) || timer["arrival_source"] != "gps" || timer["timer_missed"] != false || timer["timer_started_at"] == nil {
		t.Errorf("timer: %v", timer)
	}
	if g := head["gate"].(map[string]any); g["lat"] != 12.93 || !strings.HasPrefix(g["label"].(string), "Tower") {
		t.Errorf("gate: %v", g)
	}
	if r := head["remedies"].([]any); len(r) != 1 || r[0].(map[string]any)["kind"] != "credit" || r[0].(map[string]any)["created_by"] != nil {
		t.Errorf("remedies: %v", r)
	}
	if r := head["refunds"].([]any); len(r) != 1 || r[0].(map[string]any)["status"] != "processed" || r[0].(map[string]any)["reason"] != "cold_food" {
		t.Errorf("refunds: %v", r)
	}
	// Free text a person typed never leaks.
	raw, _ := json.Marshal(out)
	for _, secret := range []string{"+919900000000", "9876543210", "9800001111", "my number", "Flat 4B", "9800002222"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the timeline holds %q", secret)
		}
	}
}

func TestATimelineWithLittleInIt(t *testing.T) {
	o := newOrderFixture(t)
	f := o.f
	menu := f.id("INSERT INTO menu_items (name, price_paise, status) VALUES ('Burger', 14900, 'active') RETURNING id::text")
	price := f.id("INSERT INTO price_history (item_id, price_paise, reason) VALUES ($1, 14900, 'test price') RETURNING id::text", menu)
	f.exec("INSERT INTO order_items (order_id, item_id, price_history_id, name, qty, unit_paise) VALUES ($1, $2, $3, 'Burger', 2, 14900)", o.o2, menu, price)
	code, out := f.call("GET", "/admin/orders/"+o.o2+"/timeline", o.token, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	matchesContract(t, "OrderTimeline", out)
	head := out["order"].(map[string]any)
	item := head["items"].([]any)[0].(map[string]any)
	if item["name"] != "Burger" || item["qty"] != float64(2) || item["unit_price"].(map[string]any)["amount_minor"] != float64(14900) {
		t.Errorf("item: %v", item)
	}
	if head["rider"] != nil || head["ended_reason"] != "cancelled" || len(out["events"].([]any)) != 0 {
		t.Errorf("%v", out)
	}
	if p := out["pings"].(map[string]any); p["count"] != float64(0) || p["first_at"] != nil || p["last_at"] != nil {
		t.Errorf("no pings: %v", p)
	}
	timer := head["timer"].(map[string]any)
	if timer["timer_started_at"] != nil || timer["timer_duration_s"] != nil || timer["perimeter_m"] != nil || timer["arrival_source"] != nil {
		t.Errorf("an order with no timer: %v", timer)
	}
}

func TestATimelineForAnOrderThatIsNotThere(t *testing.T) {
	o := newOrderFixture(t)
	for _, id := range []string{"00000000-0000-0000-0000-000000000000", "not-an-id", "12345"} {
		if code, out := o.f.call("GET", "/admin/orders/"+id+"/timeline", o.token, ""); code != 404 || out["code"] != "not_found" {
			t.Errorf("%s: %d %v", id, code, out)
		}
	}
}

func TestOrderNumberCandidates(t *testing.T) {
	if got := orderNumberCandidates('A', 1003, 261003); fmt.Sprint(got) != "[1003 261003]" {
		t.Errorf("%v", got)
	}
	if got := orderNumberCandidates('B', 3, 1500); len(got) != 0 {
		t.Errorf("a number never issued: %v", got)
	}
	if got := orderNumberCandidates('Z', 9999, 259999); fmt.Sprint(got) != "[259999]" {
		t.Errorf("%v", got)
	}
}
