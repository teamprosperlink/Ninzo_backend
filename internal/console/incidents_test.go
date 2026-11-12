package console

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func t0() context.Context { return context.Background() }

// queue is a support desk with one source of each kind: a ticket, an arrival review, a problem
// report, a handover support alert and a lost signal (its report and its alert).
type queue struct {
	*shop
	rider, riderPhone                 string
	ticket, review, problem, alert    string
	lost                              string
	alertOrder, problemOrder, lostOrd string
	reviewOrder, ticketOrder          string
}

const (
	ticketWords  = "my flat is 12B Palm Residency, MG Road, call me on 9876512345"
	problemWords = "gate guard shouted at me, phone 9123456780"
)

func newQueue(t *testing.T) *queue {
	s := newShop(t)
	f := s.f
	q := &queue{shop: s, riderPhone: "+919811100001"}
	q.rider = f.rider(q.riderPhone, "active", "verified")

	q.ticketOrder = s.order(0)
	q.ticket = f.id("INSERT INTO support_tickets (customer_id, order_id, type) VALUES ($1, $2, 'cold_food') RETURNING id::text", s.customer, q.ticketOrder)
	f.exec("INSERT INTO ticket_messages (ticket_id, sender, text) VALUES ($1, 'customer', $2)", q.ticket, ticketWords)

	q.review, q.reviewOrder = f.review("+919855500777", 10*time.Minute)

	q.problemOrder = s.order(0)
	q.problem = f.id("INSERT INTO problem_reports (rider_id, order_id, type, source, note, outcome) VALUES ($1, $2, 'gate_closed', 'rider', $3, 'none') RETURNING id::text",
		q.rider, q.problemOrder, problemWords)

	q.alertOrder = s.order(0)
	q.alert = f.id(`INSERT INTO outbox_events (topic, payload) VALUES ('support.alert', jsonb_build_object('reason', 'handover_wrong_otp', 'order_id', $1::text, 'rider_id', $2::text))
		RETURNING id::text`, q.alertOrder, q.rider)

	q.lostOrd = s.order(0)
	q.lost = f.id("INSERT INTO problem_reports (rider_id, order_id, type, source, outcome) VALUES ($1, $2, 'signal_lost', 'system', 'support_alerted') RETURNING id::text", q.rider, q.lostOrd)
	f.exec(`INSERT INTO outbox_events (topic, payload) VALUES ('support.alert', jsonb_build_object('reason', 'signal_lost', 'order_id', $1::text, 'rider_id', $2::text))`, q.lostOrd, q.rider)
	return q
}

// inc returns the id of the incident for a source.
func (q *queue) inc(kind, source string) string {
	q.f.t.Helper()
	return q.f.id("SELECT id::text FROM incidents WHERE kind = $1 AND source_id = $2", kind, source)
}

// pairs lists every incident as kind/source, sorted.
func (f *fixture) pairs() []string {
	f.t.Helper()
	rows, err := f.pool.Query(t0(), "SELECT kind || '/' || source_id FROM incidents ORDER BY 1")
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestEachSourceMakesExactlyOneIncident(t *testing.T) {
	q := newQueue(t)
	f := q.f
	want := []string{"arrival_review/" + q.review, "problem_report/" + q.problem, "signal_lost/" + q.lost, "support_alert/" + q.alert, "ticket/" + q.ticket}
	slices.Sort(want)
	if got := f.pairs(); !slices.Equal(got, want) {
		t.Fatalf("one incident per source, and a lost signal's alert is not a sixth:\n got %v\nwant %v", got, want)
	}
	// What each incident carries.
	var order, rider *string
	f.mustScan(&order, &rider, "SELECT order_id::text, rider_id::text FROM incidents WHERE kind = 'ticket'")
	if order == nil || *order != q.ticketOrder || rider != nil {
		t.Errorf("ticket: %v %v", order, rider)
	}
	f.mustScan(&order, &rider, "SELECT order_id::text, rider_id::text FROM incidents WHERE kind = 'arrival_review'")
	if order == nil || *order != q.reviewOrder {
		t.Errorf("review has its order: %v %v", order, rider)
	}
	// A review for an order that has a rider carries that rider.
	withRider := q.shop.order(0)
	f.exec("UPDATE orders SET rider_id = $2 WHERE id = $1", withRider, q.rider)
	review := f.id("INSERT INTO arrival_reviews (order_id, lat, lng, device_time, due_at) VALUES ($1, 12.9, 77.6, now(), now() + interval '10 minutes') RETURNING id::text", withRider)
	f.mustScan(&order, &rider, "SELECT order_id::text, rider_id::text FROM incidents WHERE kind = 'arrival_review' AND source_id = $1", review)
	if order == nil || *order != withRider || rider == nil || *rider != q.rider {
		t.Errorf("review incident has the order and its rider: %v %v", order, rider)
	}
	f.exec("DELETE FROM incidents WHERE source_id = $1", review)
	f.exec("DELETE FROM arrival_reviews WHERE id = $1", review)
	f.mustScan(&order, &rider, "SELECT order_id::text, rider_id::text FROM incidents WHERE kind = 'support_alert'")
	if order == nil || *order != q.alertOrder || rider == nil || *rider != q.rider {
		t.Errorf("alert: %v %v", order, rider)
	}
	if f.count("SELECT count(*) FROM incidents WHERE status = 'open' AND assigned_to IS NULL AND closed_at IS NULL") != 5 {
		t.Error("every new incident is open")
	}

	// A second lost-signal report on the same order (another rider after a rematch) joins the open
	// incident, and a push or an unrelated outbox event makes nothing.
	other := f.rider("+919811100002", "active", "verified")
	f.exec("INSERT INTO problem_reports (rider_id, order_id, type, source, outcome) VALUES ($1, $2, 'signal_lost', 'system', 'support_alerted')", other, q.lostOrd)
	f.exec("INSERT INTO outbox_events (topic, payload) VALUES ('push.rider', '{\"kind\":\"offer\"}')")
	if got := f.pairs(); !slices.Equal(got, want) {
		t.Errorf("a second lost signal on one order, and a push, made incidents: %v", got)
	}

	// A stray id in an alert's payload cannot stop the alert from being written.
	f.exec(`INSERT INTO outbox_events (topic, payload) VALUES ('support.alert', '{"reason":"handover_wrong_otp","order_id":"not-an-id","rider_id":"00000000-0000-4000-8000-000000000000"}'),
		('support.alert', '[1,2]'), ('support.alert', '{"reason":"signal_lost"}')`)
	if f.count("SELECT count(*) FROM incidents WHERE kind = 'support_alert' AND order_id IS NULL AND rider_id IS NULL") != 3 {
		t.Error("alerts with stray or no ids get an incident with null ids")
	}
	// A resolved ticket or a decided review, inserted that way, needs nobody.
	before := len(f.pairs())
	f.exec("INSERT INTO support_tickets (customer_id, type, status, closed_at, close_reason) VALUES ($1, 'other', 'resolved', now(), 'old and done')", q.customer)
	if len(f.pairs()) != before {
		t.Error("a ticket that is not open made an incident")
	}
}

// mustScan runs a one-row query into two nullable strings.
func (f *fixture) mustScan(a, b **string, sql string, args ...any) {
	f.t.Helper()
	if err := f.pool.QueryRow(t0(), sql, args...).Scan(a, b); err != nil {
		f.t.Fatal(err)
	}
}

func TestIncidentListFiltersAndPaging(t *testing.T) {
	q := newQueue(t)
	f := q.f
	// Spread the times, so "newest first" is exact: ticket newest, then alert, lost, problem, review.
	for i, id := range []string{q.inc("ticket", q.ticket), q.inc("support_alert", q.alert), q.inc("signal_lost", q.lost), q.inc("problem_report", q.problem), q.inc("arrival_review", q.review)} {
		f.exec("UPDATE incidents SET opened_at = now() - make_interval(mins => $2) WHERE id = $1", id, (i+1)*10)
	}
	_, temp := f.user("lst.ops", "ops")
	ops := f.signIn("lst.ops", temp)
	code, out := f.call("GET", "/admin/incidents", ops, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	matchesContract(t, "IncidentList", out)
	var kinds []string
	for _, x := range out["incidents"].([]any) {
		kinds = append(kinds, x.(map[string]any)["kind"].(string))
	}
	if !slices.Equal(kinds, []string{"ticket", "support_alert", "signal_lost", "problem_report", "arrival_review"}) || out["next_cursor"] != nil {
		t.Errorf("newest first: %v %v", kinds, out["next_cursor"])
	}

	// Paging by two, to the end, with no repeats and nothing missed.
	var seen []string
	path := "/admin/incidents?limit=2"
	for pages := 0; pages < 5; pages++ {
		_, page := f.call("GET", path, ops, "")
		for _, x := range page["incidents"].([]any) {
			seen = append(seen, x.(map[string]any)["kind"].(string))
		}
		if page["next_cursor"] == nil {
			break
		}
		path = "/admin/incidents?limit=2&cursor=" + page["next_cursor"].(string)
	}
	if !slices.Equal(seen, kinds) {
		t.Errorf("paged: %v want %v", seen, kinds)
	}

	// Filters.
	_, byKind := f.call("GET", "/admin/incidents?kind=signal_lost", ops, "")
	if l := byKind["incidents"].([]any); len(l) != 1 || l[0].(map[string]any)["source_id"] != q.lost {
		t.Errorf("kind filter: %v", byKind)
	}
	assign := f.id("SELECT id::text FROM console_users WHERE login_id = 'lst.ops'")
	if code, out := f.call("POST", "/admin/incidents/"+q.inc("ticket", q.ticket)+"/assign", ops, fmt.Sprintf(`{"user_id":%q}`, assign)); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if code, out := f.call("POST", "/admin/incidents/"+q.inc("problem_report", q.problem)+"/close", ops, `{"reason":"the guard apologised"}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	for status, n := range map[string]int{"open": 3, "assigned": 1, "closed": 1} {
		_, l := f.call("GET", "/admin/incidents?status="+status, ops, "")
		if got := len(l["incidents"].([]any)); got != n {
			t.Errorf("status %s: %d want %d", status, got, n)
		}
	}
	_, both := f.call("GET", "/admin/incidents?status=open&kind=ticket", ops, "")
	if len(both["incidents"].([]any)) != 0 {
		t.Errorf("an assigned ticket is not open: %v", both)
	}
	for _, bad := range []string{"status=done", "kind=other", "limit=0", "limit=101", "limit=x", "cursor=%21%21", "cursor=" + strings.Repeat("a", 40)} {
		if code, _ := f.call("GET", "/admin/incidents?"+bad, ops, ""); code != 400 {
			t.Errorf("%s: %d", bad, code)
		}
	}
	// The count of notes is in the list.
	if code, _ := f.call("POST", "/admin/incidents/"+q.inc("ticket", q.ticket)+"/notes", ops, `{"text":"first"}`); code != 201 {
		t.Fatal(code)
	}
	_, l := f.call("GET", "/admin/incidents?kind=ticket", ops, "")
	if l["incidents"].([]any)[0].(map[string]any)["notes_count"] != float64(1) {
		t.Errorf("notes_count: %v", l)
	}
}

func TestIncidentDetailAndPrivacy(t *testing.T) {
	q := newQueue(t)
	f := q.f
	_, temp := f.user("prv.sup", "support")
	sup := f.signIn("prv.sup", temp)
	var everything strings.Builder
	for _, id := range f.ids("SELECT id::text FROM incidents") {
		code, out := f.call("GET", "/admin/incidents/"+id, sup, "")
		if code != 200 {
			t.Fatalf("%d %v", code, out)
		}
		matchesContract(t, "IncidentResponse", out)
		raw, _ := json.Marshal(out)
		everything.Write(raw)
	}
	_, list := f.call("GET", "/admin/incidents", sup, "")
	raw, _ := json.Marshal(list)
	everything.Write(raw)
	for _, leak := range []string{"9876512345", "Palm Residency", "MG Road", "12B", "9123456780", "guard", "9800001111", "9811100001", "9855500777", "call me"} {
		if strings.Contains(everything.String(), leak) {
			t.Errorf("the incident views must not hold %q", leak)
		}
	}
	// A note is staff-written and shown in the incident, oldest first, and not in the list.
	inc := q.inc("ticket", q.ticket)
	for _, text := range []string{"rang the customer at the desk", "offered a replacement"} {
		if code, out := f.call("POST", "/admin/incidents/"+inc+"/notes", sup, fmt.Sprintf(`{"text":%q}`, text)); code != 201 {
			t.Fatalf("%d %v", code, out)
		}
	}
	_, got := f.call("GET", "/admin/incidents/"+inc, sup, "")
	notes := got["notes"].([]any)
	if len(notes) != 2 || notes[0].(map[string]any)["text"] != "rang the customer at the desk" || notes[1].(map[string]any)["text"] != "offered a replacement" ||
		notes[0].(map[string]any)["author"].(map[string]any)["name"] == "" {
		t.Errorf("notes oldest first with their author: %v", notes)
	}
	_, list = f.call("GET", "/admin/incidents?kind=ticket", sup, "")
	raw, _ = json.Marshal(list)
	if strings.Contains(string(raw), "rang the customer") {
		t.Error("note text must not be in the list")
	}
	if code, _ := f.call("GET", "/admin/incidents/"+q.rider, sup, ""); code != 404 {
		t.Errorf("unknown id: %d", code)
	}
	if code, _ := f.call("GET", "/admin/incidents/not-an-id", sup, ""); code != 404 {
		t.Errorf("malformed id: %d", code)
	}
}

func (f *fixture) ids(sql string, args ...any) []string {
	f.t.Helper()
	rows, err := f.pool.Query(t0(), sql, args...)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestAssigningAnIncident(t *testing.T) {
	q := newQueue(t)
	f := q.f
	supID, supTemp := f.user("asg.sup", "support")
	sup := f.signIn("asg.sup", supTemp)
	opsID, _ := f.user("asg.ops", "ops")
	hubID, _ := f.user("asg.hub", "hub_manager")
	goneID, _ := f.user("asg.gone", "support")
	f.exec("UPDATE console_users SET active = false WHERE id = $1", goneID)
	inc := q.inc("signal_lost", q.lost)

	code, out := f.call("POST", "/admin/incidents/"+inc+"/assign", sup, fmt.Sprintf(`{"user_id":%q}`, opsID))
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	matchesContract(t, "IncidentResponse", out)
	got := out["incident"].(map[string]any)
	if got["status"] != "assigned" || got["assigned_to"].(map[string]any)["id"] != opsID {
		t.Errorf("assigned: %v", got)
	}
	// To themselves, and again to the same person.
	if code, out := f.call("POST", "/admin/incidents/"+inc+"/assign", sup, fmt.Sprintf(`{"user_id":%q}`, supID)); code != 200 || out["incident"].(map[string]any)["assigned_to"].(map[string]any)["id"] != supID {
		t.Errorf("reassigned: %d %v", code, out)
	}
	// Refused: inactive, without the permission, unknown, malformed, missing.
	for name, c := range map[string]struct {
		body string
		code int
		err  string
	}{
		"inactive":    {fmt.Sprintf(`{"user_id":%q}`, goneID), 409, "user_cannot_take"},
		"hub manager": {fmt.Sprintf(`{"user_id":%q}`, hubID), 409, "user_cannot_take"},
		"unknown":     {`{"user_id":"00000000-0000-4000-8000-0000000000aa"}`, 404, "not_found"},
		"malformed":   {`{"user_id":"abc"}`, 400, "invalid_request"},
		"number":      {`{"user_id":7}`, 400, "invalid_request"},
		"missing":     {`{}`, 400, "invalid_request"},
		"no body":     {``, 400, "invalid_request"},
	} {
		if code, out := f.call("POST", "/admin/incidents/"+inc+"/assign", sup, c.body); code != c.code || out["code"] != c.err {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if f.count("SELECT count(*) FROM incidents WHERE id = $1 AND assigned_to = $2 AND status = 'assigned'", inc, supID) != 1 {
		t.Error("a refused assignment changed the incident")
	}
	// Taking it away puts it back to open.
	code, out = f.call("POST", "/admin/incidents/"+inc+"/assign", sup, `{"user_id":null}`)
	got = out["incident"].(map[string]any)
	if code != 200 || got["status"] != "open" || got["assigned_to"] != nil {
		t.Errorf("unassigned: %d %v", code, out)
	}
	if code, out := f.call("POST", "/admin/incidents/"+inc+"/assign", sup, `{"user_id":null}`); code != 200 || out["incident"].(map[string]any)["status"] != "open" {
		t.Errorf("unassigning an open incident: %d %v", code, out)
	}
	if f.count("SELECT count(*) FROM audit_events WHERE action = 'incident.assigned' AND entity_id = $1", inc) != 4 {
		t.Error("each assignment is audited (4 were made)")
	}
	// A person who loses the permission or is deactivated after being assigned does not undo the
	// assignment, but nobody can be given the incident then.
	// A closed incident is not assigned.
	if code, out := f.call("POST", "/admin/incidents/"+inc+"/close", sup, `{"reason":"the order was handed over"}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	for _, body := range []string{fmt.Sprintf(`{"user_id":%q}`, supID), `{"user_id":null}`} {
		if code, out := f.call("POST", "/admin/incidents/"+inc+"/assign", sup, body); code != 409 || out["code"] != "incident_closed" {
			t.Errorf("assign when closed: %d %v", code, out)
		}
	}
	if code, out := f.call("POST", "/admin/incidents/00000000-0000-4000-8000-0000000000aa/assign", sup, `{"user_id":null}`); code != 404 {
		t.Errorf("unknown incident: %d %v", code, out)
	}
}

func TestIncidentNotes(t *testing.T) {
	q := newQueue(t)
	f := q.f
	_, temp := f.user("nt.sup", "support")
	sup := f.signIn("nt.sup", temp)
	inc := q.inc("support_alert", q.alert)
	code, out := f.call("POST", "/admin/incidents/"+inc+"/notes", sup, `{"text":"  Spoke to the customer; the food was handed over.  "}`)
	if code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	matchesContract(t, "IncidentResponse", out)
	if out["incident"].(map[string]any)["notes_count"] != float64(1) || out["notes"].([]any)[0].(map[string]any)["text"] != "Spoke to the customer; the food was handed over." {
		t.Errorf("%v", out)
	}
	if code, _ := f.call("POST", "/admin/incidents/"+inc+"/notes", sup, `{"text":"`+strings.Repeat("é", 2000)+`"}`); code != 201 {
		t.Errorf("2000 characters are allowed: %d", code)
	}
	for name, body := range map[string]string{"empty": `{"text":""}`, "blank": `{"text":"   "}`, "too long": `{"text":"` + strings.Repeat("a", 2001) + `"}`, "missing": `{}`,
		"number": `{"text":5}`, "nul": `{"text":"a\u0000b"}`} {
		if code, out := f.call("POST", "/admin/incidents/"+inc+"/notes", sup, body); code != 400 {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if f.count("SELECT count(*) FROM incident_notes WHERE incident_id = $1", inc) != 2 {
		t.Error("refused notes were stored")
	}
	// The audit row keeps the note's id and not its words.
	if f.count("SELECT count(*) FROM audit_events WHERE action = 'incident.noted' AND entity_id = $1", inc) != 2 ||
		f.count("SELECT count(*) FROM audit_events WHERE action = 'incident.noted' AND after::text LIKE '%handed over%'") != 0 {
		t.Error("audit")
	}
	if code, _ := f.call("POST", "/admin/incidents/"+inc+"/close", sup, `{"reason":"all sorted out"}`); code != 200 {
		t.Fatal(code)
	}
	if code, out := f.call("POST", "/admin/incidents/"+inc+"/notes", sup, `{"text":"too late"}`); code != 409 || out["code"] != "incident_closed" {
		t.Errorf("a note on a closed incident: %d %v", code, out)
	}
	// The notes stay readable on a closed incident.
	_, got := f.call("GET", "/admin/incidents/"+inc, sup, "")
	if len(got["notes"].([]any)) != 2 {
		t.Errorf("%v", got)
	}
	if code, _ := f.call("POST", "/admin/incidents/00000000-0000-4000-8000-0000000000aa/notes", sup, `{"text":"hi"}`); code != 404 {
		t.Error(code)
	}
}

func TestClosingAnIncidentMarksItsSourceHandled(t *testing.T) {
	q := newQueue(t)
	f := q.f
	_, temp := f.user("cl.sup", "support")
	sup := f.signIn("cl.sup", temp)
	other := f.id(`INSERT INTO outbox_events (topic, payload) VALUES ('support.alert', jsonb_build_object('reason', 'handover_order_cancelled', 'order_id', $1::text)) RETURNING id::text`, q.alertOrder)
	loneSignal := f.id(`INSERT INTO outbox_events (topic, payload) VALUES ('support.alert', jsonb_build_object('reason', 'signal_lost', 'order_id', $1::text)) RETURNING id::text`, q.problemOrder)

	// A support alert: it is delivered (handled), so it leaves the outbox's due index. Another alert is not.
	inc := q.inc("support_alert", q.alert)
	code, out := f.call("POST", "/admin/incidents/"+inc+"/close", sup, `{"reason":"the customer got the food"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	matchesContract(t, "IncidentResponse", out)
	got := out["incident"].(map[string]any)
	if got["status"] != "closed" || got["closed_at"] == nil {
		t.Errorf("%v", got)
	}
	if f.count("SELECT count(*) FROM outbox_events WHERE id = $1 AND delivered_at IS NOT NULL", q.alert) != 1 {
		t.Error("the alert must be marked handled")
	}
	if f.count("SELECT count(*) FROM outbox_events WHERE id IN ($1, $2) AND delivered_at IS NULL", other, loneSignal) != 2 ||
		f.count("SELECT count(*) FROM outbox_events WHERE topic = 'support.alert' AND delivered_at IS NULL") != 3 {
		t.Error("only this alert is handled (the lost-signal alert, the second alert and a stray one are not)")
	}
	if f.count("SELECT count(*) FROM audit_events WHERE action = 'incident.closed' AND entity_id = $1 AND reason = 'the customer got the food'", inc) != 1 {
		t.Error("audit with the reason")
	}
	// Closing it twice.
	if code, out := f.call("POST", "/admin/incidents/"+inc+"/close", sup, `{"reason":"once more please"}`); code != 409 || out["code"] != "incident_closed" {
		t.Errorf("%d %v", code, out)
	}

	// A problem report.
	pinc := q.inc("problem_report", q.problem)
	if code, out := f.call("POST", "/admin/incidents/"+pinc+"/close", sup, `{"reason":"rider is back on the road"}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if f.count("SELECT count(*) FROM problem_reports WHERE id = $1 AND handled_at IS NOT NULL", q.problem) != 1 ||
		f.count("SELECT count(*) FROM problem_reports WHERE id = $1 AND handled_at IS NULL", q.lost) != 1 {
		t.Error("only the report of the closed incident is handled")
	}

	// A lost signal: its report and every lost-signal alert for the order.
	other2 := f.rider("+919811100003", "active", "verified")
	f.exec("INSERT INTO problem_reports (rider_id, order_id, type, source, outcome) VALUES ($1, $2, 'signal_lost', 'system', 'support_alerted')", other2, q.lostOrd)
	sinc := q.inc("signal_lost", q.lost)
	if code, out := f.call("POST", "/admin/incidents/"+sinc+"/close", sup, `{"reason":"signal came back"}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if f.count("SELECT count(*) FROM problem_reports WHERE order_id = $1 AND handled_at IS NULL", q.lostOrd) != 0 ||
		f.count("SELECT count(*) FROM outbox_events WHERE payload->>'order_id' = $1 AND delivered_at IS NULL", q.lostOrd) != 0 {
		t.Error("the lost signal's reports and alerts are handled")
	}
	if f.count("SELECT count(*) FROM outbox_events WHERE id = $1 AND delivered_at IS NULL", loneSignal) != 1 {
		t.Error("another order's lost-signal alert is not handled")
	}
	// A new lost signal on that order after the close is a new incident.
	f.exec("INSERT INTO problem_reports (rider_id, order_id, type, source, outcome) VALUES ($1, $2, 'signal_lost', 'system', 'support_alerted')", q.rider, q.problemOrder)
	f.exec("INSERT INTO problem_reports (rider_id, order_id, type, source, outcome) VALUES ($1, $2, 'signal_lost', 'system', 'support_alerted')", f.rider("+919811100004", "active", "verified"), q.lostOrd)
	if f.count("SELECT count(*) FROM incidents WHERE kind = 'signal_lost' AND status = 'open'") != 2 {
		t.Error("a lost signal after the close opens a new incident")
	}

	// Bad reasons.
	next := q.inc("ticket", q.ticket)
	for _, body := range []string{`{}`, `{"reason":"no"}`, `{"reason":"   "}`, `{"reason":"` + strings.Repeat("a", 501) + `"}`, `{"reason":5}`} {
		if code, _ := f.call("POST", "/admin/incidents/"+next+"/close", sup, body); code != 400 {
			t.Errorf("%s: %d", body, code)
		}
	}
}

func TestATicketOrReviewIncidentClosesOnlyWithItsSource(t *testing.T) {
	q := newQueue(t)
	f := q.f
	_, temp := f.user("src.sup", "support")
	sup := f.signIn("src.sup", temp)
	ticketInc, reviewInc := q.inc("ticket", q.ticket), q.inc("arrival_review", q.review)

	// Closing the incident by hand is refused, and changes neither the incident nor the source.
	for _, inc := range []string{ticketInc, reviewInc} {
		if code, out := f.call("POST", "/admin/incidents/"+inc+"/close", sup, `{"reason":"I think it is fine"}`); code != 409 || out["code"] != "source_open" {
			t.Errorf("%d %v", code, out)
		}
	}
	if f.count("SELECT count(*) FROM support_tickets WHERE id = $1 AND status = 'open'", q.ticket) != 1 ||
		f.count("SELECT count(*) FROM arrival_reviews WHERE id = $1 AND status = 'open'", q.review) != 1 ||
		f.count("SELECT count(*) FROM incidents WHERE status = 'closed'") != 0 {
		t.Fatal("a refused close changed something")
	}
	// Notes and assigning still work while the source is open.
	if code, _ := f.call("POST", "/admin/incidents/"+ticketInc+"/notes", sup, `{"text":"waiting on the customer"}`); code != 201 {
		t.Error(code)
	}

	// Closing the ticket through its own call closes its incident.
	if code, out := f.call("POST", "/admin/tickets/"+q.ticket+"/close", sup, `{"reason":"refunded the cold food"}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	var status string
	var by *string
	if err := f.pool.QueryRow(t0(), "SELECT status, closed_by::text FROM incidents WHERE id = $1", ticketInc).Scan(&status, &by); err != nil || status != "closed" || by != nil {
		t.Errorf("the ticket's incident closes by itself: %v %v %v", status, by, err)
	}
	if code, out := f.call("POST", "/admin/incidents/"+ticketInc+"/close", sup, `{"reason":"already closed one"}`); code != 409 || out["code"] != "incident_closed" {
		t.Errorf("%d %v", code, out)
	}
	if code, out := f.call("POST", "/admin/incidents/"+ticketInc+"/notes", sup, `{"text":"late"}`); code != 409 || out["code"] != "incident_closed" {
		t.Errorf("%d %v", code, out)
	}

	// Deciding the review through its own call closes its incident.
	if code, out := f.call("POST", "/admin/arrival-reviews/"+q.review+"/decision", sup, `{"decision":"late","reason":"pings show it was late"}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if f.count("SELECT count(*) FROM incidents WHERE id = $1 AND status = 'closed'", reviewInc) != 1 {
		t.Error("the review's incident must close with the decision")
	}
	// A review that times out on its own (the system, not a person) closes it too.
	r2, _ := f.review("+919855500778", 10*time.Minute)
	if f.count("SELECT count(*) FROM incidents WHERE kind = 'arrival_review' AND source_id = $1 AND status = 'open'", r2) != 1 {
		t.Fatal("the second review has an incident")
	}
	f.exec("UPDATE arrival_reviews SET status = 'timed_out', decided_at = now() WHERE id = $1", r2)
	if f.count("SELECT count(*) FROM incidents WHERE kind = 'arrival_review' AND source_id = $1 AND status = 'closed'", r2) != 1 {
		t.Error("a timed-out review closes its incident")
	}
	// An assigned incident closes with its source too, and keeps who had it.
	t2 := f.id("INSERT INTO support_tickets (customer_id, type) VALUES ($1, 'other') RETURNING id::text", q.customer)
	i2 := q.inc("ticket", t2)
	supID := f.id("SELECT id::text FROM console_users WHERE login_id = 'src.sup'")
	if code, _ := f.call("POST", "/admin/incidents/"+i2+"/assign", sup, fmt.Sprintf(`{"user_id":%q}`, supID)); code != 200 {
		t.Fatal(code)
	}
	if code, _ := f.call("POST", "/admin/tickets/"+t2+"/close", sup, `{"reason":"customer withdrew it"}`); code != 200 {
		t.Fatal(code)
	}
	if f.count("SELECT count(*) FROM incidents WHERE id = $1 AND status = 'closed' AND assigned_to = $2", i2, supID) != 1 {
		t.Error("closed, still showing who had it")
	}
}

func TestIncidentCallsNeedTheirPermission(t *testing.T) {
	q := newQueue(t)
	f := q.f
	inc := q.inc("support_alert", q.alert)
	for _, c := range []struct{ method, path, body, perm string }{
		{"GET", "/admin/incidents", "", "incidents.read"},
		{"GET", "/admin/incidents/" + inc, "", "incidents.read"},
		{"POST", "/admin/incidents/" + inc + "/assign", `{"user_id":null}`, "incidents.manage"},
		{"POST", "/admin/incidents/" + inc + "/notes", `{"text":"hello"}`, "incidents.manage"},
		{"POST", "/admin/incidents/" + inc + "/close", `{"reason":"nothing to do"}`, "incidents.manage"},
	} {
		if code, out := f.call(c.method, c.path, q.hub, c.body); code != 403 || out["missing_permission"] != c.perm {
			t.Errorf("hub manager %s %s: %d %v", c.method, c.path, code, out)
		}
		if code, _ := f.call(c.method, c.path, "", c.body); code != 401 {
			t.Errorf("no token %s %s: %d", c.method, c.path, code)
		}
		// Support, ops and admin all pass (the close is last, as it ends the incident).
		for name, token := range map[string]string{"support": q.support, "ops": q.ops, "admin": q.adminA} {
			if c.perm == "incidents.manage" && strings.HasSuffix(c.path, "/close") {
				continue
			}
			if code, out := f.call(c.method, c.path, token, c.body); code/100 != 2 {
				t.Errorf("%s %s %s: %d %v", name, c.method, c.path, code, out)
			}
		}
	}
	if f.count("SELECT count(*) FROM incident_notes") != 3 || f.count("SELECT count(*) FROM incidents WHERE status = 'closed'") != 0 {
		t.Error("a refused call changed an incident")
	}
	if code, out := f.call("POST", "/admin/incidents/"+inc+"/close", q.adminA, `{"reason":"admin closes it"}`); code != 200 {
		t.Errorf("%d %v", code, out)
	}
}

func TestIncidentWritesReplay(t *testing.T) {
	q := newQueue(t)
	f := q.f
	_, temp := f.user("rp.sup", "support")
	sup := f.signIn("rp.sup", temp)
	inc := q.inc("problem_report", q.problem)
	path, body := "/admin/incidents/"+inc+"/notes", `{"text":"called the rider"}`
	code, first := f.callKey("POST", path, sup, "incident-replay-key-1", body)
	code2, again := f.callKey("POST", path, sup, "incident-replay-key-1", body)
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(again)
	if code != 201 || code2 != 201 || string(a) != string(b) {
		t.Errorf("a replay answers the same: %d %d\n%s\n%s", code, code2, a, b)
	}
	if f.count("SELECT count(*) FROM incident_notes WHERE incident_id = $1", inc) != 1 || f.count("SELECT count(*) FROM audit_events WHERE action = 'incident.noted' AND entity_id = $1", inc) != 1 {
		t.Error("a replay must not add a second note or audit row")
	}
	if code, out := f.callKey("POST", path, sup, "incident-replay-key-1", `{"text":"something else"}`); code != 422 {
		t.Errorf("same key, other body: %d %v", code, out)
	}
	if code, _ := f.callKey("POST", path, sup, "", body); code != 400 {
		t.Errorf("no Idempotency-Key: %d", code)
	}
	// Close and assign replay too; the replay of a close is the first answer, not incident_closed.
	cpath, cbody := "/admin/incidents/"+inc+"/close", `{"reason":"rider is fine now"}`
	c1, r1 := f.callKey("POST", cpath, sup, "incident-replay-key-2", cbody)
	c2, r2 := f.callKey("POST", cpath, sup, "incident-replay-key-2", cbody)
	a, _ = json.Marshal(r1)
	b, _ = json.Marshal(r2)
	if c1 != 200 || c2 != 200 || string(a) != string(b) {
		t.Errorf("close replay: %d %d %s %s", c1, c2, a, b)
	}
	if f.count("SELECT count(*) FROM audit_events WHERE action = 'incident.closed' AND entity_id = $1", inc) != 1 {
		t.Error("one close audit row")
	}
}

func TestOnlyOneOfManyConcurrentClosesWins(t *testing.T) {
	q := newQueue(t)
	f := q.f
	var tokens []string
	for i := 0; i < 6; i++ {
		login := fmt.Sprintf("race.sup%d", i)
		_, temp := f.user(login, "support")
		tokens = append(tokens, f.signIn(login, temp))
	}
	inc := q.inc("signal_lost", q.lost)
	var wg sync.WaitGroup
	codes := make([]int, len(tokens))
	for i, token := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = doCall(t, f.srv.URL, "POST", "/admin/incidents/"+inc+"/close", token, fmt.Sprintf("race-close-key-%02d", i), `{"reason":"closing it for the desk"}`)
		}()
	}
	wg.Wait()
	won, closed := 0, 0
	for _, c := range codes {
		switch c {
		case 200:
			won++
		case 409:
			closed++
		}
	}
	if won != 1 || closed != len(tokens)-1 {
		t.Errorf("one close wins and the rest are incident_closed: %v", codes)
	}
	if f.count("SELECT count(*) FROM audit_events WHERE action = 'incident.closed' AND entity_id = $1", inc) != 1 {
		t.Error("one audit row")
	}
}

// TestBackfillFindsWhatNeedsAPersonAndNothingElse runs the migration's own backfill statements
// again on a database where the triggers made the incidents. Wiping the incidents first stands
// for a database from before the migration.
func TestBackfillFindsWhatNeedsAPersonAndNothingElse(t *testing.T) {
	q := newQueue(t)
	f := q.f
	_, temp := f.user("bf.sup", "support")
	sup := f.signIn("bf.sup", temp)
	// Sources that were already dealt with, which must not come back: a handled alert and report
	// (closed through their incidents), a closed ticket and a decided review.
	doneAlert := f.id(`INSERT INTO outbox_events (topic, payload) VALUES ('support.alert', jsonb_build_object('reason', 'handover_wrong_otp', 'order_id', $1::text)) RETURNING id::text`, q.alertOrder)
	doneProblem := f.id("INSERT INTO problem_reports (rider_id, order_id, type, source, outcome) VALUES ($1, $2, 'breakdown', 'rider', 'none') RETURNING id::text", q.rider, q.problemOrder)
	doneTicket := f.id("INSERT INTO support_tickets (customer_id, type) VALUES ($1, 'other') RETURNING id::text", q.customer)
	doneReview, _ := f.review("+919855500779", 10*time.Minute)
	for _, inc := range []string{q.inc("support_alert", doneAlert), q.inc("problem_report", doneProblem)} {
		if code, out := f.call("POST", "/admin/incidents/"+inc+"/close", sup, `{"reason":"dealt with already"}`); code != 200 {
			t.Fatalf("%d %v", code, out)
		}
	}
	if code, _ := f.call("POST", "/admin/tickets/"+doneTicket+"/close", sup, `{"reason":"dealt with already"}`); code != 200 {
		t.Fatal(code)
	}
	if code, out := f.call("POST", "/admin/arrival-reviews/"+doneReview+"/decision", sup, `{"decision":"late","reason":"dealt with already"}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	// And an alert that arrived before any incident existed, for an order with a lost-signal report.
	f.exec(`INSERT INTO outbox_events (topic, payload) VALUES ('support.alert', '{"reason":"handover_wrong_otp","order_id":"not-an-id"}')`)

	open := f.ids("SELECT kind || '/' || source_id FROM incidents WHERE status <> 'closed' ORDER BY 1")
	if len(open) != 6 {
		t.Fatalf("five sources and the stray alert: %v", open)
	}
	data, err := os.ReadFile("../../migrations/0044_incidents.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	start, end := strings.Index(text, "INSERT INTO incidents (kind, source_id, order_id, rider_id, opened_at)"), strings.Index(text, "-- +goose Down")
	if start < 0 || end < start {
		t.Fatal("cannot find the backfill in the migration")
	}
	backfill := text[start:end]
	for run := 1; run <= 2; run++ {
		f.exec("DELETE FROM incident_notes")
		f.exec("DELETE FROM incidents")
		f.exec(backfill)
		if run == 2 {
			f.exec(backfill) // Twice in a row: nothing doubles.
		}
		got := f.ids("SELECT kind || '/' || source_id FROM incidents ORDER BY 1")
		if !slices.Equal(got, open) {
			t.Errorf("run %d: the backfill makes the open incidents and nothing handled:\n got %v\nwant %v", run, got, open)
		}
		if f.count("SELECT count(*) FROM incidents WHERE status <> 'open'") != 0 {
			t.Error("backfilled incidents are open")
		}
	}
	var order, rider *string
	f.mustScan(&order, &rider, "SELECT order_id::text, rider_id::text FROM incidents WHERE kind = 'support_alert' AND source_id = $1", q.alert)
	if order == nil || *order != q.alertOrder || rider == nil || *rider != q.rider {
		t.Errorf("the backfill keeps order and rider: %v %v", order, rider)
	}
	var opened time.Time
	var created time.Time
	if err := f.pool.QueryRow(t0(), "SELECT i.opened_at, p.created_at FROM incidents i JOIN problem_reports p ON p.id::text = i.source_id WHERE i.kind = 'problem_report' AND p.id = $1", q.problem).Scan(&opened, &created); err != nil || !opened.Equal(created) {
		t.Errorf("opened_at is the source's time: %v %v %v", opened, created, err)
	}
}

// TestIncidentWritesWaitForTheRowLock holds the incident's row in another transaction, closes it
// there, and checks that an assign, a note and a close wait, then see it closed.
func TestIncidentWritesWaitForTheRowLock(t *testing.T) {
	q := newQueue(t)
	f := q.f
	var tokens []string
	for i := 0; i < 3; i++ {
		login := fmt.Sprintf("lock.sup%d", i)
		_, temp := f.user(login, "support")
		tokens = append(tokens, f.signIn(login, temp))
	}
	inc := q.inc("support_alert", q.alert)
	holder, err := f.pool.Begin(t0())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(t0())
	if _, err := holder.Exec(t0(), "SELECT 1 FROM incidents WHERE id = $1 FOR UPDATE", inc); err != nil {
		t.Fatal(err)
	}
	type call struct{ path, body string }
	calls := []call{{"/assign", `{"user_id":null}`}, {"/notes", `{"text":"waiting for the lock"}`}, {"/close", `{"reason":"waiting for the lock"}`}}
	codes := make([]int, len(calls))
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = doCall(t, f.srv.URL, "POST", "/admin/incidents/"+inc+c.path, tokens[i], fmt.Sprintf("lock-key-%02d", i), c.body)
		}()
	}
	time.Sleep(1500 * time.Millisecond)
	for i, c := range codes {
		if c != 0 {
			t.Errorf("call %d answered %d while another transaction held the incident", i, c)
		}
	}
	if _, err := holder.Exec(t0(), "UPDATE incidents SET status = 'closed', closed_at = now() WHERE id = $1", inc); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(t0()); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 409 {
			t.Errorf("call %d after the lock was released: %d, want incident_closed", i, c)
		}
	}
	if f.count("SELECT count(*) FROM incident_notes") != 0 {
		t.Error("a note was added to a closed incident")
	}
}
