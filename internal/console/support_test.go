package console

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// callKey is call with the Idempotency-Key chosen by the test, for replays and for concurrent
// calls (call numbers its keys with a counter that is not safe to share between goroutines).
func (f *fixture) callKey(method, path, token, key, body string) (int, map[string]any) {
	f.t.Helper()
	return doCall(f.t, f.srv.URL, method, path, token, key, body)
}

func doCall(t *testing.T, base, method, path, token, key, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, base+"/v1"+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Error(err)
		return 0, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// shop is a small support desk: a support agent, an ops user, a hub manager and two admins, a
// customer, and two menu items.
type shop struct {
	f                                 *fixture
	support, ops, hub, adminA, adminB string
	supportID                         string
	zone, customer, phone             string
	chai, samosa                      string
}

func newShop(t *testing.T) *shop {
	f := newFixture(t)
	s := &shop{f: f, phone: "+919800001111"}
	var temp string
	s.supportID, temp = f.user("desk.sup", "support")
	s.support = f.signIn("desk.sup", temp)
	_, temp = f.user("desk.ops", "ops")
	s.ops = f.signIn("desk.ops", temp)
	_, temp = f.user("desk.hub", "hub_manager")
	s.hub = f.signIn("desk.hub", temp)
	s.adminA, s.adminB = f.admins("desk")
	s.zone = f.zone2("Desk")
	s.customer = f.shopper(s.phone)
	s.chai = f.id("INSERT INTO menu_items (name, price_paise, status) VALUES ('Chai', 4900, 'active') RETURNING id::text")
	s.samosa = f.id("INSERT INTO menu_items (name, price_paise, status) VALUES ('Samosa', 3900, 'active') RETURNING id::text")
	return s
}

// order makes a finished order with two chai and one samosa. paid is the amount of its paid test
// payment; zero makes an order that was never paid.
func (s *shop) order(paid int) string {
	s.f.t.Helper()
	id := s.f.makeOrder(orderSpec{customer: s.customer, zone: s.zone, state: "COMPLETED", ago: time.Hour})
	for _, it := range []struct {
		item  string
		qty   int
		price int
	}{{s.chai, 2, 4900}, {s.samosa, 1, 3900}} {
		history := s.f.id("INSERT INTO price_history (item_id, price_paise, reason) VALUES ($1, $2, 'test') RETURNING id::text", it.item, it.price)
		s.f.exec("INSERT INTO order_items (order_id, item_id, price_history_id, name, qty, unit_paise) VALUES ($1, $2, $3, 'Item', $4, $5)", id, it.item, history, it.qty, it.price)
	}
	if paid > 0 {
		s.f.exec("INSERT INTO payments (order_id, provider, provider_order_id, amount_paise, status) VALUES ($1, 'test', $2, $3, 'paid')", id, "po-"+id, paid)
	}
	return id
}

func (s *shop) refund(token, order string, amount int, reason string) (int, map[string]any) {
	s.f.t.Helper()
	return s.f.call("POST", "/admin/orders/"+order+"/refund", token, fmt.Sprintf(`{"amount_paise":%d,"reason":%q}`, amount, reason))
}

func (f *fixture) refundedSum(order string) int {
	return f.count("SELECT coalesce(sum(amount_paise), 0)::int FROM refunds WHERE order_id = $1 AND requested_by IS NOT NULL", order)
}

func TestTicketsAreListedReadRepliedToAndClosed(t *testing.T) {
	s := newShop(t)
	f := s.f
	order := s.order(14900)
	// Two tickets: one on the order, one with none. The customer's words hold a phone number, as
	// customers do; support may read it in the ticket, but it must not travel any further.
	cold := f.id("INSERT INTO support_tickets (customer_id, order_id, type, created_at) VALUES ($1, $2, 'cold_food', now() - interval '2 hours') RETURNING id::text", s.customer, order)
	f.exec("INSERT INTO ticket_messages (ticket_id, sender, text, created_at) VALUES ($1, 'customer', 'The chai was   cold,\nplease call 9800001111', now() - interval '2 hours')", cold)
	pay := f.id("INSERT INTO support_tickets (customer_id, type, created_at) VALUES ($1, 'payment', now() - interval '1 hour') RETURNING id::text", s.customer)
	f.exec("INSERT INTO ticket_messages (ticket_id, sender, text, created_at) VALUES ($1, 'customer', $2, now() - interval '1 hour')", pay, strings.Repeat("long ", 40))

	code, out := f.call("GET", "/admin/tickets", s.support, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	matchesContract(t, "TicketList", out)
	list := out["tickets"].([]any)
	if len(list) != 2 || list[0].(map[string]any)["id"] != pay || list[1].(map[string]any)["id"] != cold {
		t.Fatalf("newest first: %v", list)
	}
	first, second := list[0].(map[string]any), list[1].(map[string]any)
	if second["subject"] != "The chai was cold, please call 9800001111" || second["order_id"] != order || second["status"] != "open" || second["closed_at"] != nil || second["close_reason"] != nil {
		t.Errorf("%v", second)
	}
	if len(first["subject"].(string)) != 80 || first["order_id"] != nil || len(first["messages"].([]any)) != 0 {
		t.Errorf("a long subject is cut at 80 characters and the list leaves messages out: %v", first)
	}
	// Filters and paging.
	if _, out := f.call("GET", "/admin/tickets?type=cold_food", s.support, ""); len(out["tickets"].([]any)) != 1 {
		t.Errorf("type filter: %v", out)
	}
	if _, out := f.call("GET", "/admin/tickets?status=resolved", s.support, ""); len(out["tickets"].([]any)) != 0 {
		t.Errorf("status filter: %v", out)
	}
	_, page := f.call("GET", "/admin/tickets?limit=1", s.support, "")
	if len(page["tickets"].([]any)) != 1 || page["next_cursor"] == nil {
		t.Fatalf("%v", page)
	}
	_, page2 := f.call("GET", "/admin/tickets?limit=1&cursor="+page["next_cursor"].(string), s.support, "")
	if page2["tickets"].([]any)[0].(map[string]any)["id"] != cold || page2["next_cursor"] != nil {
		t.Errorf("%v", page2)
	}
	for _, bad := range []string{"?status=closed", "?type=rude", "?limit=0", "?limit=101", "?cursor=!!"} {
		if code, out := f.call("GET", "/admin/tickets"+bad, s.support, ""); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", bad, code, out)
		}
	}

	// One ticket, with its messages oldest first.
	code, out = f.call("GET", "/admin/tickets/"+cold, s.support, "")
	matchesContract(t, "TicketResponse", out)
	msgs := out["ticket"].(map[string]any)["messages"].([]any)
	if code != 200 || len(msgs) != 1 || msgs[0].(map[string]any)["author_type"] != "customer" || msgs[0].(map[string]any)["author_name"] != nil {
		t.Fatalf("%d %v", code, out)
	}

	// A reply: the customer's ticket gets a support message, and one push names the ticket.
	code, out = f.callKey("POST", "/admin/tickets/"+cold+"/messages", s.support, "reply-key-0001", `{"text":"Sorry about the chai. A refund is on its way."}`)
	if code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	matchesContract(t, "TicketResponse", out)
	msgs = out["ticket"].(map[string]any)["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if len(msgs) != 2 || last["author_type"] != "support" || last["author_name"] != "Name desk.sup" || last["text"] != "Sorry about the chai. A refund is on its way." {
		t.Errorf("%v", msgs)
	}
	if f.count("SELECT count(*) FROM ticket_messages WHERE ticket_id = $1 AND sender = 'support' AND author_id = $2", cold, s.supportID) != 1 {
		t.Error("the customer reads the reply as a support message, and it names who wrote it")
	}
	var push string
	if err := f.pool.QueryRow(t.Context(), "SELECT payload::text FROM outbox_events WHERE topic = 'push.customer'").Scan(&push); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	_ = json.Unmarshal([]byte(push), &payload)
	if len(payload) != 3 || payload["customer_id"] != s.customer || payload["ticket_id"] != cold || payload["stage"] != "ticket_reply" {
		t.Errorf("the push names the customer, the ticket and the stage: %s", push)
	}
	for _, leak := range []string{"9800001111", "refund", "chai", "Tower", "Flat"} {
		if strings.Contains(strings.ToLower(push), strings.ToLower(leak)) {
			t.Errorf("the push carries %q: %s", leak, push)
		}
	}
	if f.count("SELECT count(*) FROM audit_events WHERE action = 'ticket.replied' AND entity_id = $1 AND actor_id = $2", cold, s.supportID) != 1 {
		t.Error("a reply is audited")
	}
	var auditAfter string
	_ = f.pool.QueryRow(t.Context(), "SELECT after::text FROM audit_events WHERE action = 'ticket.replied'").Scan(&auditAfter)
	if strings.Contains(auditAfter, "chai") {
		t.Errorf("the audit row keeps the words: %s", auditAfter)
	}

	// The same key again gives the same answer and adds nothing.
	code2, again := f.callKey("POST", "/admin/tickets/"+cold+"/messages", s.support, "reply-key-0001", `{"text":"Sorry about the chai. A refund is on its way."}`)
	if code2 != 201 || len(again["ticket"].(map[string]any)["messages"].([]any)) != 2 || f.count("SELECT count(*) FROM ticket_messages WHERE ticket_id = $1", cold) != 2 ||
		f.count("SELECT count(*) FROM outbox_events WHERE topic = 'push.customer'") != 1 {
		t.Errorf("replay: %d %v", code2, again)
	}
	// The same key with other words is refused.
	if code, out := f.callKey("POST", "/admin/tickets/"+cold+"/messages", s.support, "reply-key-0001", `{"text":"Something else"}`); code != 422 {
		t.Errorf("key reuse: %d %v", code, out)
	}

	// Bad bodies and unknown tickets.
	for _, body := range []string{`{}`, `{"text":""}`, `{"text":"   "}`, `{"text":"` + strings.Repeat("x", 2001) + `"}`, `not json`} {
		if code, out := f.call("POST", "/admin/tickets/"+cold+"/messages", s.support, body); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%.30s: %d %v", body, code, out)
		}
	}
	if code, _ := f.call("POST", "/admin/tickets/"+cold+"/messages", s.support, `{"text":"`+strings.Repeat("é", 2000)+`"}`); code != 201 {
		t.Errorf("2000 characters are allowed (counted as characters, not bytes): %d", code)
	}
	for _, id := range []string{"00000000-0000-0000-0000-000000000009", "not-an-id"} {
		for _, c := range []struct{ method, path, body string }{
			{"GET", "/admin/tickets/" + id, ""}, {"POST", "/admin/tickets/" + id + "/messages", `{"text":"hi"}`}, {"POST", "/admin/tickets/" + id + "/close", `{"reason":"done now"}`},
		} {
			if code, out := f.call(c.method, c.path, s.support, c.body); code != 404 || out["code"] != "not_found" {
				t.Errorf("%s %s: %d %v", c.method, c.path, code, out)
			}
		}
	}

	// Closing: a short reason is refused; then it is resolved, with who, when and why.
	if code, _ := f.call("POST", "/admin/tickets/"+cold+"/close", s.support, `{"reason":"ok"}`); code != 400 {
		t.Errorf("a short reason: %d", code)
	}
	code, out = f.call("POST", "/admin/tickets/"+cold+"/close", s.support, `{"reason":"refund sent, customer happy"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	matchesContract(t, "TicketResponse", out)
	closed := out["ticket"].(map[string]any)
	if closed["status"] != "resolved" || closed["close_reason"] != "refund sent, customer happy" || closed["closed_at"] == nil {
		t.Errorf("%v", closed)
	}
	if f.count("SELECT count(*) FROM support_tickets WHERE id = $1 AND closed_by = $2 AND status = 'resolved'", cold, s.supportID) != 1 ||
		f.count("SELECT count(*) FROM audit_events WHERE action = 'ticket.closed' AND entity_id = $1 AND reason = 'refund sent, customer happy'", cold) != 1 {
		t.Error("who closed it, and the audit row")
	}
	// A closed ticket takes no reply and no second close: ticket_closed (409).
	if code, out := f.call("POST", "/admin/tickets/"+cold+"/messages", s.support, `{"text":"one more thing"}`); code != 409 || out["code"] != "ticket_closed" {
		t.Errorf("reply on a closed ticket: %d %v", code, out)
	}
	if code, out := f.call("POST", "/admin/tickets/"+cold+"/close", s.support, `{"reason":"closing it again"}`); code != 409 || out["code"] != "ticket_closed" {
		t.Errorf("close twice: %d %v", code, out)
	}
	if f.count("SELECT count(*) FROM ticket_messages WHERE ticket_id = $1 AND sender = 'support'", cold) != 2 {
		t.Error("a refused reply was stored")
	}
	if _, out := f.call("GET", "/admin/tickets?status=resolved", s.support, ""); len(out["tickets"].([]any)) != 1 {
		t.Errorf("%v", out)
	}
	// Admin has the permission too.
	if code, _ := f.call("GET", "/admin/tickets", s.adminA, ""); code != 200 {
		t.Errorf("admin: %d", code)
	}
}

func TestTicketCallsNeedTheirPermission(t *testing.T) {
	s := newShop(t)
	f := s.f
	ticket := f.id("INSERT INTO support_tickets (customer_id, type) VALUES ($1, 'other') RETURNING id::text", s.customer)
	f.exec("INSERT INTO ticket_messages (ticket_id, sender, text) VALUES ($1, 'customer', 'hello')", ticket)
	for name, token := range map[string]string{"ops": s.ops, "hub_manager": s.hub} {
		for _, c := range []struct{ method, path, body string }{
			{"GET", "/admin/tickets", ""}, {"GET", "/admin/tickets/" + ticket, ""},
			{"POST", "/admin/tickets/" + ticket + "/messages", `{"text":"hi"}`}, {"POST", "/admin/tickets/" + ticket + "/close", `{"reason":"done now"}`},
		} {
			if code, out := f.call(c.method, c.path, token, c.body); code != 403 || out["missing_permission"] != "tickets.manage" {
				t.Errorf("%s %s %s: %d %v", name, c.method, c.path, code, out)
			}
		}
	}
	if code, _ := f.call("GET", "/admin/tickets", "", ""); code != 401 {
		t.Errorf("no token: %d", code)
	}
	if f.count("SELECT count(*) FROM ticket_messages") != 1 || f.count("SELECT count(*) FROM support_tickets WHERE status = 'open'") != 1 {
		t.Error("a refused call changed a ticket")
	}
}

func TestRefundsAtTheApprovalLimit(t *testing.T) {
	s := newShop(t)
	f := s.f
	// Exactly the limit is made at once: 20000 paise on a 100000 order.
	a := s.order(100000)
	code, out := s.refund(s.support, a, 20000, "cold food, refunded in full")
	if code != 200 {
		t.Fatalf("exactly at the limit: %d %v", code, out)
	}
	matchesContract(t, "RefundResponse", out)
	r := out["refund"].(map[string]any)
	by := r["requested_by"].(map[string]any)
	if r["status"] != "processed" || r["reason"] != "cold food, refunded in full" || r["order_id"] != a || by["id"] != s.supportID || by["name"] != "Name desk.sup" {
		t.Errorf("%v", r)
	}
	amount := r["amount"].(map[string]any)
	if amount["amount_minor"] != float64(20000) || amount["currency"] != "INR" {
		t.Errorf("%v", amount)
	}
	// The same row the customer's getRefund and order views read.
	var code1, status, method string
	var paise int
	if err := f.pool.QueryRow(t.Context(), "SELECT reason_code, method, status, amount_paise FROM refunds WHERE order_id = $1", a).Scan(&code1, &method, &status, &paise); err != nil {
		t.Fatal(err)
	}
	if code1 != "support_refund_1" || method != "original_payment_method" || status != "processed" || paise != 20000 {
		t.Errorf("%s %s %s %d", code1, method, status, paise)
	}
	if f.count("SELECT count(*) FROM audit_events WHERE action = 'refund.issued' AND entity_id = $1 AND actor_id = $2 AND reason = 'cold food, refunded in full'", r["id"], s.supportID) != 1 ||
		f.count("SELECT count(*) FROM order_events WHERE order_id = $1 AND actor_type = 'console' AND reason = 'support refund'", a) != 1 {
		t.Error("audit row and order event")
	}
	// One paisa more in total: over the limit, so it waits for a second admin (202).
	code, out = s.refund(s.support, a, 1, "one more paisa")
	if code != 202 {
		t.Fatalf("one paisa over: %d %v", code, out)
	}
	matchesContract(t, "ApprovalResponse", out)
	ap := out["approval"].(map[string]any)
	change := ap["change"].(map[string]any)
	if ap["kind"] != "refund" || ap["status"] != "pending" || change["order_id"] != a || change["amount"].(map[string]any)["amount_minor"] != float64(1) || ap["request_reason"] != "one more paisa" {
		t.Errorf("%v", ap)
	}
	if f.refundedSum(a) != 20000 {
		t.Error("the waiting refund was made")
	}
	// While it waits, another refund on the order is approval_pending, not a second approval.
	if code, out := s.refund(s.support, a, 5, "and some more"); code != 409 || out["code"] != "approval_pending" {
		t.Errorf("%d %v", code, out)
	}

	// Cumulative across two requests: 12000 then 8000 reaches the limit exactly (both at once);
	// a third request of 1 goes over.
	b := s.order(100000)
	for i, amount := range []int{12000, 8000} {
		if code, out := s.refund(s.support, b, amount, "two part refund"); code != 200 {
			t.Fatalf("part %d: %d %v", i, code, out)
		}
	}
	if code, out := s.refund(s.support, b, 1, "over the line"); code != 202 {
		t.Errorf("the third: %d %v", code, out)
	}
	if got := f.count("SELECT count(DISTINCT reason_code) FROM refunds WHERE order_id = $1", b); got != 2 {
		t.Errorf("each console refund has its own code: %d", got)
	}
	// 12000 then 9000 is 21000 in all: the second waits.
	c := s.order(100000)
	if code, _ := s.refund(s.support, c, 12000, "first part"); code != 200 {
		t.Fatal(code)
	}
	if code, out := s.refund(s.support, c, 9000, "second part"); code != 202 {
		t.Errorf("cumulative over the limit: %d %v", code, out)
	}
	// A refund the system made does not use up the support limit.
	d := s.order(100000)
	f.exec("INSERT INTO refunds (order_id, payment_id, reason_code, amount_paise, status) SELECT order_id, id, 'missing_food', 15000, 'processed' FROM payments WHERE order_id = $1", d)
	if code, out := s.refund(s.support, d, 20000, "fresh limit"); code != 200 {
		t.Errorf("an automatic refund counts against what is left to refund, not against the limit: %d %v", code, out)
	}

	// Too large: more than is left to refund (paid less refunds made or waiting).
	e := s.order(30000)
	if code, out := s.refund(s.support, e, 30001, "too much"); code != 409 || out["code"] != "refund_too_large" {
		t.Errorf("more than paid: %d %v", code, out)
	}
	if code, out := s.refund(s.support, e, 25000, "most of it"); code != 202 {
		t.Fatalf("%d %v", code, out)
	}
	if code, out := s.refund(s.support, e, 5001, "more than is left"); code != 409 || out["code"] != "refund_too_large" {
		t.Errorf("the waiting refund is spoken for: %d %v", code, out)
	}
	g := s.order(10000)
	if code, _ := s.refund(s.support, g, 10000, "all of it"); code != 200 {
		t.Fatal(code)
	}
	if code, out := s.refund(s.support, g, 1, "after all of it"); code != 409 || out["code"] != "refund_too_large" {
		t.Errorf("a fully refunded order answers refund_too_large, not not_paid: %d %v", code, out)
	}
	// Not paid.
	if code, out := s.refund(s.support, s.order(0), 100, "never paid"); code != 409 || out["code"] != "not_paid" {
		t.Errorf("%d %v", code, out)
	}
	f.exec("UPDATE payments SET status = 'failed' WHERE order_id = $1", e)
	if code, out := s.refund(s.support, e, 100, "payment failed"); code != 409 || out["code"] != "not_paid" {
		t.Errorf("a failed payment: %d %v", code, out)
	}

	// Bad bodies, unknown orders.
	for _, body := range []string{`{"reason":"no amount"}`, `{"amount_paise":0,"reason":"zero"}`, `{"amount_paise":-5,"reason":"negative"}`, `{"amount_paise":10000001,"reason":"too big"}`,
		`{"amount_paise":10.5,"reason":"fraction"}`, `{"amount_paise":"100","reason":"text"}`, `{"amount_paise":100}`, `{"amount_paise":100,"reason":"no"}`, `nonsense`} {
		if code, out := f.call("POST", "/admin/orders/"+a+"/refund", s.support, body); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", body, code, out)
		}
	}
	for _, id := range []string{"00000000-0000-0000-0000-000000000009", "A1001"} {
		if code, out := s.refund(s.support, id, 100, "no such order"); code != 404 || out["code"] != "not_found" {
			t.Errorf("%s: %d %v", id, code, out)
		}
	}
	// 10,000,000 is the largest amount the contract allows (it is then too large for this order).
	if code, out := s.refund(s.support, a, 10000000, "the biggest"); code != 409 || out["code"] != "refund_too_large" {
		t.Errorf("%d %v", code, out)
	}

	// Permission: ops and a hub manager cannot request a refund; admin can.
	for name, token := range map[string]string{"ops": s.ops, "hub_manager": s.hub} {
		if code, out := s.refund(token, a, 100, "not mine to do"); code != 403 || out["missing_permission"] != "refunds.request" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if code, _ := s.refund(s.adminA, s.order(5000), 100, "admin may refund"); code != 200 {
		t.Errorf("admin: %d", code)
	}
}

func TestARefundAboveTheLimitIsMadeWhenASecondAdminApproves(t *testing.T) {
	s := newShop(t)
	f := s.f
	order := s.order(40000)
	code, out := s.refund(s.support, order, 30000, "customer got nothing")
	if code != 202 {
		t.Fatalf("%d %v", code, out)
	}
	approval := out["approval"].(map[string]any)["id"].(string)
	if f.count("SELECT count(*) FROM refunds WHERE order_id = $1", order) != 0 {
		t.Fatal("made before the approval")
	}
	// Support cannot decide; a requester who is an admin cannot approve their own.
	if code, out := f.call("POST", "/admin/approvals/"+approval+"/approve", s.support, ""); code != 403 || out["missing_permission"] != "approvals.decide" {
		t.Errorf("%d %v", code, out)
	}
	own := s.order(40000)
	_, ownOut := s.refund(s.adminA, own, 30000, "admin asks")
	if code, out := f.call("POST", "/admin/approvals/"+ownOut["approval"].(map[string]any)["id"].(string)+"/approve", s.adminA, ""); code != 409 || out["code"] != "own_change" {
		t.Errorf("%d %v", code, out)
	}
	// A second admin approves: the refund is made, in the name of the person who asked.
	code, out = f.call("POST", "/admin/approvals/"+approval+"/approve", s.adminB, "")
	if code != 200 || out["approval"].(map[string]any)["status"] != "approved" || out["approval"].(map[string]any)["failure_code"] != nil {
		t.Fatalf("%d %v", code, out)
	}
	var by, status, note, reason string
	var paise int
	if err := f.pool.QueryRow(t.Context(), "SELECT requested_by::text, status, note, reason_code, amount_paise FROM refunds WHERE order_id = $1", order).Scan(&by, &status, &note, &reason, &paise); err != nil {
		t.Fatal(err)
	}
	if by != s.supportID || status != "processed" || note != "customer got nothing" || reason != "support_refund_1" || paise != 30000 {
		t.Errorf("%s %s %s %s %d", by, status, note, reason, paise)
	}
	if f.count("SELECT count(*) FROM audit_events WHERE action = 'refund.issued' AND entity_type = 'refund' AND reason = 'customer got nothing'") != 1 {
		t.Error("the approved refund is audited")
	}
	// It now counts towards the order: 30000 made, so 10000 is left and 10001 is too large.
	if code, out := s.refund(s.support, order, 10001, "too much now"); code != 409 || out["code"] != "refund_too_large" {
		t.Errorf("%d %v", code, out)
	}
	// An approval cannot be approved twice.
	if code, out := f.call("POST", "/admin/approvals/"+approval+"/approve", s.adminB, ""); code != 409 || out["code"] != "not_pending" {
		t.Errorf("%d %v", code, out)
	}
}

func TestARejectedOrLapsedRefundApprovalReleasesItsAmount(t *testing.T) {
	s := newShop(t)
	f := s.f
	order := s.order(40000)
	_, out := s.refund(s.support, order, 30000, "first request")
	id := out["approval"].(map[string]any)["id"].(string)
	// While it waits, 30000 is spoken for: 15000 is too large (10000 is left).
	if code, out := s.refund(s.support, order, 15000, "second request"); code != 409 || out["code"] != "refund_too_large" {
		t.Fatalf("%d %v", code, out)
	}
	if code, out := f.call("POST", "/admin/approvals/"+id+"/reject", s.adminB, `{"reason":"not enough evidence"}`); code != 200 || out["approval"].(map[string]any)["status"] != "rejected" {
		t.Fatalf("%d %v", code, out)
	}
	if f.count("SELECT count(*) FROM refunds WHERE order_id = $1", order) != 0 {
		t.Fatal("a rejected approval made a refund")
	}
	// The amount is free again.
	if code, out := s.refund(s.support, order, 15000, "second request"); code != 200 {
		t.Errorf("after the rejection: %d %v", code, out)
	}
	// A new approval for the order may be asked for, and lapses after 24 hours, which frees it too.
	_, out = s.refund(s.support, order, 22000, "third request")
	if out["approval"] == nil {
		t.Fatalf("%v", out)
	}
	if code, out := s.refund(s.support, order, 9000, "waiting still"); code != 409 || out["code"] != "refund_too_large" {
		t.Errorf("%d %v", code, out)
	}
	f.now = f.now.Add(25 * time.Hour)
	if code, out := s.refund(s.support, order, 9000, "after the lapse"); code != 202 {
		t.Errorf("a lapsed approval no longer holds the amount (a new one is asked for, 15000 + 9000 is over the limit): %d %v", code, out)
	}
	if f.count("SELECT count(*) FROM approvals WHERE kind = 'refund' AND status = 'lapsed'") != 1 {
		t.Error("the old approval lapsed")
	}
}

func TestAnApprovedRefundThatNoLongerFitsFailsAndMakesNothing(t *testing.T) {
	s := newShop(t)
	f := s.f
	// Other refunds were made after it was asked for, so it is more than is left: the approval
	// fails with refund_too_large and the order is unchanged.
	order := s.order(40000)
	_, out := s.refund(s.support, order, 30000, "asked while there was room")
	id := out["approval"].(map[string]any)["id"].(string)
	f.exec("INSERT INTO refunds (order_id, payment_id, reason_code, amount_paise, status) SELECT order_id, id, 'missing_food', 15000, 'processed' FROM payments WHERE order_id = $1", order)
	code, out := f.call("POST", "/admin/approvals/"+id+"/approve", s.adminB, "")
	ap := out["approval"].(map[string]any)
	if code != 200 || ap["status"] != "failed" || ap["failure_code"] != "refund_too_large" {
		t.Fatalf("%d %v", code, out)
	}
	if f.count("SELECT count(*) FROM refunds WHERE order_id = $1", order) != 1 || f.refundedSum(order) != 0 {
		t.Error("a failed approval made a refund")
	}
	// The payment stopped being paid: not_paid.
	other := s.order(40000)
	_, out = s.refund(s.support, other, 30000, "asked before it failed")
	f.exec("UPDATE payments SET status = 'failed' WHERE order_id = $1", other)
	_, out = f.call("POST", "/admin/approvals/"+out["approval"].(map[string]any)["id"].(string)+"/approve", s.adminB, "")
	if ap := out["approval"].(map[string]any); ap["status"] != "failed" || ap["failure_code"] != "not_paid" {
		t.Errorf("%v", ap)
	}
}

func TestTwoRefundRequestsAtOnceNeverPassThePaidAmountOrTheLimit(t *testing.T) {
	s := newShop(t)
	f := s.f
	many := func(n, amount int) []int {
		out := make([]int, n)
		for i := range out {
			out[i] = amount
		}
		return out
	}
	// Calls by one person take turns (the session check locks the person), so the race is between
	// several people refunding one order: eight of them.
	tokens := []string{s.support, s.adminA, s.adminB}
	for i := 0; i < 5; i++ {
		login := fmt.Sprintf("race.sup%d", i)
		_, temp := f.user(login, "support")
		tokens = append(tokens, f.signIn(login, temp))
	}
	run := func(order string, amounts []int) (codes map[int]int, errCodes map[string]int) {
		codes, errCodes = map[int]int{}, map[string]int{}
		var wg sync.WaitGroup
		var mu sync.Mutex
		start := make(chan struct{})
		for i, amount := range amounts {
			wg.Add(1)
			go func(i, amount int) {
				defer wg.Done()
				<-start
				code, out := doCall(t, f.srv.URL, "POST", "/admin/orders/"+order+"/refund", tokens[i%len(tokens)], fmt.Sprintf("race-%s-%02d", order[:8], i),
					fmt.Sprintf(`{"amount_paise":%d,"reason":"racing refund %d"}`, amount, i))
				mu.Lock()
				codes[code]++
				if c, ok := out["code"].(string); ok {
					errCodes[c]++
				}
				mu.Unlock()
			}(i, amount)
		}
		close(start)
		wg.Wait()
		return
	}

	// Never past the paid amount: 10000 paid, 30 requests of 1000. Ten fit (10000 is under the
	// limit, so they are made at once); the other twenty are too large. Many requests, so that
	// without the locks they would overlap and read the same totals.
	paid := s.order(10000)
	codes, errs := run(paid, many(30, 1000))
	if codes[200] != 10 || codes[409] != 20 || errs["refund_too_large"] != 20 {
		t.Errorf("paid amount: %v %v", codes, errs)
	}
	if got := f.refundedSum(paid); got != 10000 {
		t.Errorf("refunded %d of 10000", got)
	}

	// Never around the approval limit: 60000 paid, 30 requests of 1000 (limit 20000). Twenty are
	// made at once (20000); one waits for approval; the others see that approval and are refused.
	limit := s.order(60000)
	codes, errs = run(limit, many(30, 1000))
	if codes[200] != 20 || codes[202] != 1 || codes[409] != 9 || errs["approval_pending"] != 9 {
		t.Errorf("limit: %v %v", codes, errs)
	}
	if got := f.refundedSum(limit); got != 20000 {
		t.Errorf("made at once: %d, the limit is 20000", got)
	}
	if f.count("SELECT count(*) FROM approvals WHERE kind = 'refund' AND status = 'pending' AND target = $1", limit) != 1 {
		t.Error("one approval waits")
	}

	// Large requests at once: each over the limit alone, only one approval may wait, and the
	// refund-too-large check holds the waiting amount, so the rest cannot add up past the payment.
	big := s.order(50000)
	codes, errs = run(big, []int{30000, 30000, 30000, 30000})
	if codes[202] != 1 || codes[409] != 3 {
		t.Errorf("big: %v %v", codes, errs)
	}
}

func TestReplacementsAndCredits(t *testing.T) {
	s := newShop(t)
	f := s.f
	order := s.order(14900)
	seq := func() int { return f.count("SELECT seq::int FROM orders WHERE id = $1", order) }
	before := seq()

	// A replacement lists items on the order.
	body := fmt.Sprintf(`{"items":[{"item_id":%q,"qty":1},{"item_id":%q,"qty":1}],"reason":"samosa was crushed"}`, s.chai, s.samosa)
	code, out := f.callKey("POST", "/admin/orders/"+order+"/replacement", s.support, "rep-key-0001", body)
	if code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	matchesContract(t, "RemedyResponse", out)
	r := out["remedy"].(map[string]any)
	by := r["created_by"].(map[string]any)
	items := r["items"].([]any)
	if r["kind"] != "replacement" || r["amount"] != nil || r["status"] != "owed" || r["reason"] != "samosa was crushed" || by["id"] != s.supportID || len(items) != 2 ||
		items[0].(map[string]any)["item_id"] != s.chai || items[1].(map[string]any)["qty"] != float64(1) {
		t.Errorf("%v", r)
	}
	if seq() != before+1 {
		t.Error("the customer's order view must refresh (seq)")
	}
	if f.count("SELECT count(*) FROM audit_events WHERE action = 'remedy.issued' AND entity_id = $1 AND actor_id = $2 AND reason = 'samosa was crushed'", order, s.supportID) != 1 ||
		f.count("SELECT count(*) FROM order_events WHERE order_id = $1 AND reason = 'support replacement'", order) != 1 {
		t.Error("audit row and order event")
	}
	// Replay: the same key, the same reply, nothing added.
	code, again := f.callKey("POST", "/admin/orders/"+order+"/replacement", s.support, "rep-key-0001", body)
	if code != 201 || again["remedy"].(map[string]any)["id"] != r["id"] || f.count("SELECT count(*) FROM remedies WHERE order_id = $1", order) != 1 {
		t.Errorf("replay: %d %v", code, again)
	}
	// One replacement an order.
	if code, out := f.call("POST", "/admin/orders/"+order+"/replacement", s.support, body); code != 409 || out["code"] != "remedy_exists" {
		t.Errorf("%d %v", code, out)
	}

	// Items not on the order, more than ordered, repeated, or malformed.
	other := s.order(14900)
	notOrdered := f.id("INSERT INTO menu_items (name, price_paise, status) VALUES ('Vada', 3000, 'active') RETURNING id::text")
	for _, c := range []struct {
		body string
		code int
		want string
	}{
		{fmt.Sprintf(`{"items":[{"item_id":%q,"qty":1}],"reason":"not on it"}`, notOrdered), 409, "item_not_on_order"},
		{fmt.Sprintf(`{"items":[{"item_id":%q,"qty":1},{"item_id":%q,"qty":1}],"reason":"one is not"}`, s.chai, notOrdered), 409, "item_not_on_order"},
		{fmt.Sprintf(`{"items":[{"item_id":%q,"qty":3}],"reason":"two were ordered"}`, s.chai), 409, "item_not_on_order"},
		{fmt.Sprintf(`{"items":[{"item_id":%q,"qty":1},{"item_id":%q,"qty":1}],"reason":"twice"}`, s.chai, s.chai), 400, "invalid_request"},
		{fmt.Sprintf(`{"items":[{"item_id":%q,"qty":0}],"reason":"nothing"}`, s.chai), 400, "invalid_request"},
		{fmt.Sprintf(`{"items":[{"item_id":%q,"qty":21}],"reason":"too many"}`, s.chai), 400, "invalid_request"},
		{fmt.Sprintf(`{"items":[{"item_id":%q}],"reason":"no qty"}`, s.chai), 400, "invalid_request"},
		{`{"items":[{"item_id":"nope","qty":1}],"reason":"not an id"}`, 400, "invalid_request"},
		{`{"items":[],"reason":"none"}`, 400, "invalid_request"},
		{fmt.Sprintf(`{"items":[{"item_id":%q,"qty":1}]}`, s.chai), 400, "invalid_request"},
	} {
		if code, out := f.call("POST", "/admin/orders/"+other+"/replacement", s.support, c.body); code != c.code || out["code"] != c.want {
			t.Errorf("%s: %d %v", c.body, code, out)
		}
	}
	if f.count("SELECT count(*) FROM remedies WHERE order_id = $1", other) != 0 {
		t.Error("a refused replacement was stored")
	}

	// A credit.
	code, out = f.call("POST", "/admin/orders/"+order+"/credit", s.ops, `{"amount_paise":5000,"reason":"a late apology"}`)
	if code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	matchesContract(t, "RemedyResponse", out)
	c := out["remedy"].(map[string]any)
	if c["kind"] != "credit" || c["amount"].(map[string]any)["amount_minor"] != float64(5000) || c["items"] != nil || c["created_by"].(map[string]any)["name"] != "Name desk.ops" {
		t.Errorf("%v", c)
	}
	if code, out := f.call("POST", "/admin/orders/"+order+"/credit", s.support, `{"amount_paise":100,"reason":"a second credit"}`); code != 409 || out["code"] != "remedy_exists" {
		t.Errorf("%d %v", code, out)
	}
	// The credit for a missed timer (the system's) is one credit too.
	missed := s.order(14900)
	f.exec("INSERT INTO remedies (order_id, type, reason) VALUES ($1, 'credit', 'timer_missed')", missed)
	if code, out := f.call("POST", "/admin/orders/"+missed+"/credit", s.support, `{"amount_paise":100,"reason":"on top of it"}`); code != 409 || out["code"] != "remedy_exists" {
		t.Errorf("%d %v", code, out)
	}
	// Amount limits: 1 and 10,000,000 are in, 0 and above are out.
	for _, c := range []struct {
		amount string
		code   int
	}{{"1", 201}, {"10000000", 201}, {"0", 400}, {"10000001", 400}, {"-1", 400}, {"1.5", 400}, {`"5"`, 400}} {
		o := s.order(14900)
		if code, out := f.call("POST", "/admin/orders/"+o+"/credit", s.support, `{"amount_paise":`+c.amount+`,"reason":"limit check"}`); code != c.code {
			t.Errorf("amount %s: %d %v", c.amount, code, out)
		}
	}
	// Not paid, unknown order, bad reason.
	unpaid := s.order(0)
	if code, out := f.call("POST", "/admin/orders/"+unpaid+"/credit", s.support, `{"amount_paise":100,"reason":"never paid"}`); code != 409 || out["code"] != "not_paid" {
		t.Errorf("%d %v", code, out)
	}
	if code, out := f.call("POST", "/admin/orders/"+unpaid+"/replacement", s.support, fmt.Sprintf(`{"items":[{"item_id":%q,"qty":1}],"reason":"never paid"}`, s.chai)); code != 409 || out["code"] != "not_paid" {
		t.Errorf("%d %v", code, out)
	}
	for _, path := range []string{"credit", "replacement"} {
		if code, out := f.call("POST", "/admin/orders/00000000-0000-0000-0000-000000000009/"+path, s.support, fmt.Sprintf(`{"amount_paise":100,"items":[{"item_id":%q,"qty":1}],"reason":"no order"}`, s.chai)); code != 404 || out["code"] != "not_found" {
			t.Errorf("%s: %d %v", path, code, out)
		}
	}
	if code, _ := f.call("POST", "/admin/orders/"+other+"/credit", s.support, `{"amount_paise":100,"reason":"x"}`); code != 400 {
		t.Errorf("a one-character reason: %d", code)
	}

	// The permission: a hub manager has no remedies.issue; ops, support and admin do.
	third := s.order(14900)
	if code, out := f.call("POST", "/admin/orders/"+third+"/credit", s.hub, `{"amount_paise":100,"reason":"not mine to do"}`); code != 403 || out["missing_permission"] != "remedies.issue" {
		t.Errorf("%d %v", code, out)
	}
	if code, out := f.call("POST", "/admin/orders/"+third+"/replacement", s.hub, fmt.Sprintf(`{"items":[{"item_id":%q,"qty":1}],"reason":"not mine"}`, s.chai)); code != 403 || out["missing_permission"] != "remedies.issue" {
		t.Errorf("%d %v", code, out)
	}
	if code, _ := f.call("POST", "/admin/orders/"+third+"/credit", s.adminA, `{"amount_paise":100,"reason":"admin may"}`); code != 201 {
		t.Errorf("admin: %d", code)
	}
}

func TestTheTimelineNamesWhoAskedForRefundsAndRemedies(t *testing.T) {
	s := newShop(t)
	f := s.f
	order := s.order(40000)
	// A reason with a phone number in it, as staff sometimes type: it stays out of the events.
	if code, out := s.refund(s.support, order, 5000, "customer asked on 9800001111, flat 4B"); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	_, ap := s.refund(s.support, order, 30000, "the big one")
	if code, _ := f.call("POST", "/admin/approvals/"+ap["approval"].(map[string]any)["id"].(string)+"/approve", s.adminB, ""); code != 200 {
		t.Fatal(code)
	}
	if code, out := f.call("POST", "/admin/orders/"+order+"/replacement", s.ops, fmt.Sprintf(`{"items":[{"item_id":%q,"qty":2}],"reason":"both cold"}`, s.chai)); code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	if code, out := f.call("POST", "/admin/orders/"+order+"/credit", s.support, `{"amount_paise":2500,"reason":"sorry"}`); code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	ticket := f.id("INSERT INTO support_tickets (customer_id, order_id, type) VALUES ($1, $2, 'cold_food') RETURNING id::text", s.customer, order)
	f.exec("INSERT INTO ticket_messages (ticket_id, sender, text) VALUES ($1, 'customer', 'cold')", ticket)
	f.call("POST", "/admin/tickets/"+ticket+"/messages", s.support, `{"text":"we are on it"}`)
	f.call("POST", "/admin/tickets/"+ticket+"/close", s.support, `{"reason":"all settled"}`)

	code, out := f.call("GET", "/admin/orders/"+order+"/timeline", s.support, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	matchesContract(t, "OrderTimeline", out)
	o := out["order"].(map[string]any)
	refunds := o["refunds"].([]any)
	if len(refunds) != 2 {
		t.Fatalf("%v", refunds)
	}
	for _, x := range refunds {
		r := x.(map[string]any)
		by := r["requested_by"].(map[string]any)
		if by["id"] != s.supportID || by["name"] != "Name desk.sup" {
			t.Errorf("requested_by must name the person who asked, also when a second admin approved: %v", r)
		}
	}
	remedies := o["remedies"].([]any)
	if len(remedies) != 2 {
		t.Fatalf("%v", remedies)
	}
	var replacement, credit map[string]any
	for _, x := range remedies {
		if m := x.(map[string]any); m["kind"] == "replacement" {
			replacement = m
		} else {
			credit = m
		}
	}
	if replacement["created_by"].(map[string]any)["name"] != "Name desk.ops" || len(replacement["items"].([]any)) != 1 || replacement["items"].([]any)[0].(map[string]any)["qty"] != float64(2) ||
		credit["created_by"].(map[string]any)["name"] != "Name desk.sup" || credit["items"] != nil {
		t.Errorf("created_by and items: %v %v", replacement, credit)
	}
	// The events name who acted, and keep staff-typed words out.
	named := map[string][]string{}
	for _, x := range out["events"].([]any) {
		e := x.(map[string]any)
		key := fmt.Sprint(e["kind"], "/", e["actor_type"])
		named[key] = append(named[key], fmt.Sprint(e["actor_name"]))
	}
	for key, want := range map[string][]string{
		"refund.requested/console": {"Name desk.sup", "Name desk.sup"}, "ticket.message/support": {"Name desk.sup"}, "ticket.resolved/support": {"Name desk.sup"},
	} {
		if fmt.Sprint(named[key]) != fmt.Sprint(want) {
			t.Errorf("%s: %v, want %v", key, named[key], want)
		}
	}
	if got := fmt.Sprint(named["remedy.owed/console"]); got != "[Name desk.ops Name desk.sup]" && got != "[Name desk.sup Name desk.ops]" {
		t.Errorf("remedy.owed: %s", got)
	}
	raw, _ := json.Marshal(out["events"])
	for _, leak := range []string{"9800001111", "flat 4B", "both cold", "sorry", "all settled", "we are on it", "the big one"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the events carry staff-typed words %q", leak)
		}
	}
	// And the customer's phone is only ever its last four digits, on the order.
	if all, _ := json.Marshal(o["customer"]); !strings.Contains(string(all), "1111") || strings.Contains(string(all), "9800001111") {
		t.Errorf("%s", all)
	}
}

func TestTheRefundApprovalLimitIsASetting(t *testing.T) {
	s := newShop(t)
	f := s.f
	_, global := f.call("GET", "/admin/settings", s.adminA, "")
	v := setting(t, global, "refund_approval_paise")
	if v["value"] != float64(20000) || v["effective"] != float64(20000) || v["unit"] != "paise" || v["required"] != false || v["min"] != float64(0) || v["max"] != float64(10000000) {
		t.Fatalf("%v", v)
	}
	base := int(global["global_version"].(float64))
	put := func(changes string, base int) (int, map[string]any) {
		return f.call("PUT", "/admin/settings", s.adminA, fmt.Sprintf(`{"changes":%s,"base_version":%d,"reason":"a tighter refund limit"}`, changes, base))
	}
	if code, out := put(`{"refund_approval_paise":10000001}`, base); code != 400 || out["code"] != "invalid_setting" {
		t.Errorf("%d %v", code, out)
	}
	code, out := put(`{"refund_approval_paise":5000}`, base)
	if code != 202 {
		t.Fatalf("%d %v", code, out)
	}
	if code, _ := f.call("POST", "/admin/approvals/"+out["approval"].(map[string]any)["id"].(string)+"/approve", s.adminB, ""); code != 200 {
		t.Fatal(code)
	}
	order := s.order(40000)
	if code, out := s.refund(s.support, order, 5001, "over the new limit"); code != 202 {
		t.Errorf("%d %v", code, out)
	}
	other := s.order(40000)
	if code, out := s.refund(s.support, other, 5000, "at the new limit"); code != 200 {
		t.Errorf("%d %v", code, out)
	}
	if code, out := s.refund(s.support, other, 1, "over it"); code != 202 {
		t.Errorf("%d %v", code, out)
	}
	// Zero means every refund waits for approval.
	_, global = f.call("GET", "/admin/settings", s.adminA, "")
	code, out = put(`{"refund_approval_paise":0}`, int(global["global_version"].(float64)))
	if code != 202 {
		t.Fatalf("%d %v", code, out)
	}
	f.call("POST", "/admin/approvals/"+out["approval"].(map[string]any)["id"].(string)+"/approve", s.adminB, "")
	if code, out := s.refund(s.support, s.order(40000), 1, "everything waits"); code != 202 {
		t.Errorf("%d %v", code, out)
	}
}
