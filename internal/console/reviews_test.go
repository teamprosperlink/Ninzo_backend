package console

import (
	"fmt"
	"testing"
	"time"
)

// review opens an arrival review on a missed-timer order and returns the review and the order.
func (f *fixture) review(phone string, due time.Duration) (string, string) {
	f.t.Helper()
	zone := f.zone2(phone)
	rider := f.rider(phone, "active", "verified")
	order := f.offer(rider, zone, f.onShift(rider, zone, "SC-"+phone[len(phone)-4:]), "accepted")
	f.exec(`UPDATE orders SET timer_started_at = now() - interval '3 minutes', timer_missed = true WHERE id = $1`, order)
	review := f.id(`INSERT INTO arrival_reviews (order_id, lat, lng, device_time, due_at) VALUES ($1, 12.95, 77.65, now() - interval '2 minutes', now() + make_interval(secs => $2))
		RETURNING id::text`, order, due.Seconds())
	return review, order
}

// zone2 adds a zone with a unique name.
func (f *fixture) zone2(tag string) string {
	f.t.Helper()
	return f.id("INSERT INTO zones (name, city, status) VALUES ($1, 'Bengaluru', 'active') RETURNING id::text", "Zone "+tag)
}

func TestDecidingArrivalReviews(t *testing.T) {
	f := newFixture(t)
	_, temp := f.user("rev.sup", "support")
	sup := f.signIn("rev.sup", temp)
	inTime, inTimeOrder := f.review("+919855500001", 10*time.Minute)
	late, lateOrder := f.review("+919855500002", 10*time.Minute)
	gone, _ := f.review("+919855500003", -time.Minute) // Its time already ran out.
	_, list := f.call("GET", "/admin/arrival-reviews", sup, "")
	reviews := list["reviews"].([]any)
	if len(reviews) != 3 || reviews[0].(map[string]any)["status"] != "open" {
		t.Fatalf("%v", list)
	}
	code, out := f.call("POST", "/admin/arrival-reviews/"+inTime+"/decision", sup, `{"decision":"arrived_in_time","reason":"pings show the gate at 1:10"}`)
	if code != 200 || out["review"].(map[string]any)["status"] != "arrived_in_time" || out["review"].(map[string]any)["decided_by"] != "Name rev.sup" {
		t.Fatalf("%d %v", code, out)
	}
	if f.count("SELECT count(*) FROM orders WHERE id = $1 AND NOT timer_missed AND timer_stopped_at = (SELECT device_time FROM arrival_reviews WHERE id = $2)", inTimeOrder, inTime) != 1 {
		t.Error("arrived_in_time must clear the miss and stop the timer at the rider's evidence")
	}
	if code, out := f.call("POST", "/admin/arrival-reviews/"+inTime+"/decision", sup, `{"decision":"late","reason":"changed my mind"}`); code != 409 || out["code"] != "review_closed" {
		t.Errorf("deciding twice: %d %v", code, out)
	}
	if code, _ := f.call("POST", "/admin/arrival-reviews/"+late+"/decision", sup, `{"decision":"late","reason":"pings show 1:40"}`); code != 200 {
		t.Fatal("late")
	}
	if f.count("SELECT count(*) FROM orders WHERE id = $1 AND timer_missed", lateOrder) != 1 {
		t.Error("late keeps the miss, so the credit is paid")
	}
	if code, out := f.call("POST", "/admin/arrival-reviews/"+gone+"/decision", sup, `{"decision":"late","reason":"too slow"}`); code != 409 || out["code"] != "review_closed" {
		t.Errorf("a review past its time: %d %v", code, out)
	}
	if code, _ := f.call("POST", "/admin/arrival-reviews/"+late+"/decision", sup, `{"decision":"maybe","reason":"not sure"}`); code != 400 {
		t.Errorf("an unknown decision: %d", code)
	}
	_, open := f.call("GET", "/admin/arrival-reviews?status=open", sup, "")
	if n := len(open["reviews"].([]any)); n != 1 {
		t.Errorf("one review is still open: %d", n)
	}
	if f.count("SELECT count(*) FROM audit_events WHERE action IN ('arrival_review.arrived_in_time', 'arrival_review.late')") != 2 {
		t.Error("every decision is audited")
	}
	_, temp = f.user("rev.hub", "ops")
	f.exec("INSERT INTO hubs (zone_id, name, lat, lng) SELECT id, 'H', 12.9, 77.6 FROM zones LIMIT 1")
	f.exec("UPDATE console_users SET role = 'hub_manager', hub_id = (SELECT id FROM hubs LIMIT 1) WHERE login_id = 'rev.hub'")
	if code, out := f.call("GET", "/admin/arrival-reviews", f.signIn("rev.hub", temp), ""); code != 403 || out["missing_permission"] != "reviews.read" {
		t.Errorf("a hub manager: %d %v", code, out)
	}
}

func TestReviewPagingKeepsOpenFirst(t *testing.T) {
	f := newFixture(t)
	_, temp := f.user("rev.page", "support")
	sup := f.signIn("rev.page", temp)
	for i := 0; i < 3; i++ {
		id, _ := f.review(fmt.Sprintf("+91985560000%d", i), time.Duration(10-i)*time.Minute)
		if i == 0 {
			f.exec("UPDATE arrival_reviews SET status = 'timed_out', decided_at = now() WHERE id = $1", id)
		}
	}
	seen := []string{}
	cursor := ""
	for page := 0; page < 5; page++ {
		_, out := f.call("GET", "/admin/arrival-reviews?limit=1"+cursor, sup, "")
		for _, r := range out["reviews"].([]any) {
			seen = append(seen, r.(map[string]any)["status"].(string))
		}
		next, _ := out["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = "&cursor=" + next
	}
	if len(seen) != 3 || seen[0] != "open" || seen[1] != "open" || seen[2] != "timed_out" {
		t.Errorf("open first, then the rest, across pages: %v", seen)
	}
}

func TestTheAuditLog(t *testing.T) {
	f := newFixture(t)
	adminID, temp := f.user("aud.admin", "admin")
	a := f.signIn("aud.admin", temp)
	f.call("POST", "/admin/users", a, `{"login_id":"aud.ops","name":"Ops","role":"ops"}`)
	f.exec(`INSERT INTO audit_events (actor_type, action, entity_type, entity_id, after) VALUES ('system', 'test.leak', 'person', 'p1',
		'{"phone":"+919800000001","password_hash":"x","totp_secret":"y","nested":{"refresh_token":"z","kept":1},"phone_last4":"0001"}')`)
	code, out := f.call("GET", "/admin/audit?action=test.leak", a, "")
	if code != 200 || len(out["events"].([]any)) != 1 {
		t.Fatalf("%d %v", code, out)
	}
	after := out["events"].([]any)[0].(map[string]any)["after"].(map[string]any)
	if after["phone"] != "(hidden)" || after["password_hash"] != "(hidden)" || after["totp_secret"] != "(hidden)" || after["phone_last4"] != "0001" ||
		after["nested"].(map[string]any)["refresh_token"] != "(hidden)" || after["nested"].(map[string]any)["kept"] != float64(1) {
		t.Errorf("secrets must never leave the log: %v", after)
	}
	_, mine := f.call("GET", "/admin/audit?actor_id="+adminID+"&entity_type=console_user", a, "")
	if len(mine["events"].([]any)) == 0 {
		t.Errorf("filter by actor and entity: %v", mine)
	}
	for _, e := range mine["events"].([]any) {
		if e.(map[string]any)["actor_id"] != adminID {
			t.Errorf("another actor: %v", e)
		}
	}
	// Newest first, paged, no repeats.
	_, first := f.call("GET", "/admin/audit?limit=2", a, "")
	next := first["next_cursor"].(string)
	_, second := f.call("GET", "/admin/audit?limit=2&cursor="+next, a, "")
	ids := []string{}
	for _, page := range []map[string]any{first, second} {
		for _, e := range page["events"].([]any) {
			ids = append(ids, e.(map[string]any)["id"].(string))
		}
	}
	for i := 1; i < len(ids); i++ {
		var prev, cur int
		fmt.Sscan(ids[i-1], &prev)
		fmt.Sscan(ids[i], &cur)
		if cur >= prev {
			t.Errorf("not newest first: %v", ids)
		}
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if _, out := f.call("GET", "/admin/audit?from="+future, a, ""); len(out["events"].([]any)) != 0 {
		t.Errorf("from the future: %v", out)
	}
	if code, _ := f.call("GET", "/admin/audit?from=yesterday", a, ""); code != 400 {
		t.Errorf("a bad time: %d", code)
	}
	_, temp = f.user("aud.ops2", "ops")
	if code, out := f.call("GET", "/admin/audit", f.signIn("aud.ops2", temp), ""); code != 403 || out["missing_permission"] != "audit.read" {
		t.Errorf("ops reading the audit log: %d %v", code, out)
	}
}

func TestSupportAlerts(t *testing.T) {
	f := newFixture(t)
	_, temp := f.user("alert.sup", "support")
	sup := f.signIn("alert.sup", temp)
	_, order := f.review("+919855500010", 10*time.Minute)
	f.exec(`INSERT INTO outbox_events (topic, payload) VALUES ('support.alert', jsonb_build_object('reason', 'handover_wrong_otp', 'order_id', $1::text, 'rider_id', 'not-an-id')),
		('push.rider', '{"kind":"offer"}'), ('support.alert', '{"reason":"signal_lost"}')`, order)
	code, out := f.call("GET", "/admin/support-alerts", sup, "")
	alerts := out["alerts"].([]any)
	if code != 200 || len(alerts) != 2 || alerts[0].(map[string]any)["kind"] != "signal_lost" || alerts[1].(map[string]any)["order_id"] != order ||
		alerts[1].(map[string]any)["rider_id"] != nil {
		t.Errorf("support alerts only, newest first, ids checked: %d %v", code, out)
	}
	_, page := f.call("GET", "/admin/support-alerts?limit=1", sup, "")
	_, more := f.call("GET", "/admin/support-alerts?limit=1&cursor="+page["next_cursor"].(string), sup, "")
	if more["alerts"].([]any)[0].(map[string]any)["kind"] != "handover_wrong_otp" || more["next_cursor"] != nil {
		t.Errorf("%v", more)
	}
}

func TestAnInTimeDecisionTakesBackTheCredit(t *testing.T) {
	f := newFixture(t)
	_, temp := f.user("rev.credit", "support")
	sup := f.signIn("rev.credit", temp)
	owed, owedOrder := f.review("+919855500021", 10*time.Minute)
	f.exec("INSERT INTO remedies (order_id, type, reason, amount_paise) VALUES ($1, 'credit', 'timer_missed', 2000)", owedOrder)
	if code, out := f.call("POST", "/admin/arrival-reviews/"+owed+"/decision", sup, `{"decision":"arrived_in_time","reason":"offline pings show the gate"}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if f.count("SELECT count(*) FROM remedies WHERE order_id = $1 AND status = 'cancelled'", owedOrder) != 1 {
		t.Error("a credit still owed must be taken back")
	}
	if f.count("SELECT count(*) FROM order_events WHERE order_id = $1 AND actor_type = 'console' AND reason = 'arrival review: arrived in time'", owedOrder) != 1 {
		t.Error("the order's history must show the decision")
	}
	paid, paidOrder := f.review("+919855500022", 10*time.Minute)
	f.exec("INSERT INTO remedies (order_id, type, reason, amount_paise, status) VALUES ($1, 'credit', 'timer_missed', 2000, 'applied')", paidOrder)
	if code, out := f.call("POST", "/admin/arrival-reviews/"+paid+"/decision", sup, `{"decision":"arrived_in_time","reason":"offline pings show the gate"}`); code != 409 || out["code"] != "credit_applied" {
		t.Errorf("a credit already given: %d %v", code, out)
	}
	if f.count("SELECT count(*) FROM arrival_reviews WHERE id = $1 AND status = 'open'", paid) != 1 || f.count("SELECT count(*) FROM orders WHERE id = $1 AND timer_missed", paidOrder) != 1 {
		t.Error("a refused decision changed something")
	}
	// The audit log keeps useful keys and hides secrets by name.
	f.exec(`INSERT INTO audit_events (actor_type, action, entity_type, entity_id, after) VALUES ('system', 'test.keys', 'order', 'o1',
		'{"otp_released_at":"2026-11-11T00:00:00Z","otp":"1234","subject":"google-123","email":"a@b.c"}')`)
	_, adminTemp := f.user("rev.admin", "admin")
	admin := f.signIn("rev.admin", adminTemp)
	_, out := f.call("GET", "/admin/audit?action=test.keys&actor_id=not-an-id", admin, "")
	code, all := f.call("GET", "/admin/audit?action=test.keys", admin, "")
	if code != 200 || len(all["events"].([]any)) != 1 {
		t.Fatalf("%d %v", code, all)
	}
	{
		after := all["events"].([]any)[0].(map[string]any)["after"].(map[string]any)
		if after["otp_released_at"] == "(hidden)" || after["otp"] != "(hidden)" || after["subject"] != "(hidden)" || after["email"] != "(hidden)" {
			t.Errorf("%v", after)
		}
	}
	if out["events"] == nil || len(out["events"].([]any)) != 0 {
		t.Errorf("a non-id actor matches nobody: %v", out)
	}
}
