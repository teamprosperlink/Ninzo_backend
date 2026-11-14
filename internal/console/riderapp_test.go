package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/contracttest"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/riderapp"
)

// Issue 235, part B2: what operations write for the rider app, and what riders send back. Each test
// uses the console's real handlers and the rider API's real handlers on one database, and checks
// replies against the bundled contracts.

const riderContract = "rider.openapi.yaml"

// deskUsers are signed-in console users of every role.
type deskUsers struct{ admin, ops, support, hub string }

type riderSide struct {
	f   *fixture
	srv *httptest.Server
	n   int
}

func newRiderSide(t *testing.T) (*fixture, deskUsers, *riderSide) {
	t.Helper()
	f := newFixture(t)
	var u deskUsers
	for login, dst := range map[string]*string{"ra.admin": &u.admin, "ra.ops": &u.ops, "ra.support": &u.support, "ra.hub": &u.hub} {
		role := strings.TrimPrefix(login, "ra.")
		if role == "hub" {
			role = "hub_manager"
		}
		_, temp := f.user(login, role)
		*dst = f.signIn(login, temp)
	}
	svc := &riderapp.Service{Pool: f.pool, Log: quiet}
	rs := &riderSide{f: f}
	rs.srv = httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		svc.MountRider(r, identity.RequireActor(f.svc.Sessions.Signer, identity.TypeRider))
	}))
	t.Cleanup(rs.srv.Close)
	return f, u, rs
}

// rider makes an active rider in the zone and signs them in.
func (rs *riderSide) rider(zone, phone, name string) (string, string) {
	rs.f.t.Helper()
	person := rs.f.id("INSERT INTO people (phone) VALUES ($1) RETURNING id::text", phone)
	id := rs.f.id("INSERT INTO riders (person_id, name, status, zone_id) VALUES ($1, $2, 'active', $3) RETURNING id::text", person, name, zone)
	var pair identity.Pair
	if err := database.InTx(context.Background(), rs.f.pool, func(tx pgx.Tx) error {
		var err error
		pair, _, err = rs.f.svc.Sessions.Start(context.Background(), tx, identity.TypeRider, id)
		return err
	}); err != nil {
		rs.f.t.Fatal(err)
	}
	return id, pair.AccessToken
}

func (rs *riderSide) call(method, path, token, body string) (int, map[string]any) {
	rs.f.t.Helper()
	rs.n++
	return doCall(rs.f.t, rs.srv.URL, method, path, token, fmt.Sprintf("rider-key-%05d", rs.n), body)
}

func TestHelpIsEmptyThenWrittenInTheConsoleThenSeenByRiders(t *testing.T) {
	f, u, rs := newRiderSide(t)
	zone := f.zone2("Help")
	_, rider := rs.rider(zone, "+919822200001", "Ravi")

	// Nothing is written to start with, on either side.
	code, out := f.call("GET", "/admin/help-articles", u.ops, "")
	if code != 200 || len(out["articles"].([]any)) != 0 {
		t.Fatalf("%d %v", code, out)
	}
	matchesContract(t, "HelpArticleList", out)
	_, out = rs.call("GET", "/riders/me/help", rider, "")
	contracttest.Check(t, riderContract, "RiderHelp", out)
	if len(out["faq"].([]any)) != 0 {
		t.Fatalf("faq must be empty: %v", out)
	}

	// Ops write two articles. They are active at once, and ordered by sort.
	code, out = f.call("POST", "/admin/help-articles", u.ops, `{"slug":"gate-closed","question":"What if the gate is closed?","answer":"Tap Report a problem, then Gate is closed.","sort":20}`)
	if code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	matchesContract(t, "HelpArticleResponse", out)
	first := out["article"].(map[string]any)
	if first["active"] != true || first["audience"] != "rider" || first["sort"] != float64(20) || first["updated_by"].(map[string]any)["name"] != "Name ra.ops" {
		t.Errorf("%v", first)
	}
	_, out = f.call("POST", "/admin/help-articles", u.admin, `{"slug":"when-paid","question":"When am I paid?","answer":"Your earnings show under Earnings.\nThey are paid daily.","sort":10}`)
	second := out["article"].(map[string]any)
	_, out = rs.call("GET", "/riders/me/help", rider, "")
	contracttest.Check(t, riderContract, "RiderHelp", out)
	faq := out["faq"].([]any)
	if len(faq) != 2 || faq[0].(map[string]any)["id"] != second["id"] || faq[1].(map[string]any)["id"] != first["id"] {
		t.Fatalf("riders see both, lower sort first: %v", faq)
	}
	if faq[1].(map[string]any)["question"] != "What if the gate is closed?" || !strings.Contains(faq[0].(map[string]any)["answer"].(string), "\n") {
		t.Errorf("%v", faq)
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'help.created' AND actor_type = 'console'"); n != 2 {
		t.Errorf("%d audit rows for the two articles", n)
	}

	// A taken slug is a conflict. Nobody but ops and admin may write.
	if code, out := f.call("POST", "/admin/help-articles", u.ops, `{"slug":"gate-closed","question":"Again","answer":"Again"}`); code != 409 || out["code"] != "slug_taken" {
		t.Errorf("a taken slug: %d %v", code, out)
	}
	for name, token := range map[string]string{"support": u.support, "hub manager": u.hub} {
		for _, c := range []struct{ method, path, body string }{
			{"GET", "/admin/help-articles", ""},
			{"POST", "/admin/help-articles", `{"slug":"x-1","question":"q","answer":"a"}`},
			{"PATCH", "/admin/help-articles/" + first["id"].(string), `{"answer":"b","reason":"Because"}`},
			{"POST", "/admin/help-articles/" + first["id"].(string) + "/deactivate", `{"reason":"Because"}`},
		} {
			if code, out := f.call(c.method, c.path, token, c.body); code != 403 || out["missing_permission"] != "help.manage" {
				t.Errorf("%s %s %s: %d %v", name, c.method, c.path, code, out)
			}
		}
	}
	if code, _ := f.call("GET", "/admin/help-articles", "", ""); code != 401 {
		t.Errorf("no token: %d", code)
	}
	if code, _ := f.call("GET", "/admin/help-articles", rider, ""); code != 401 {
		t.Errorf("a rider's token on the console: %d", code)
	}
	if n := f.count("SELECT count(*) FROM help_articles"); n != 2 {
		t.Errorf("refused calls left %d articles", n)
	}

	// Update: the words change for riders at once, and the audit row has before, after and the reason.
	id := first["id"].(string)
	code, out = f.call("PATCH", "/admin/help-articles/"+id, u.ops, `{"answer":"Tap Report a problem. Support will help.","sort":5,"reason":"Clearer words"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	matchesContract(t, "HelpArticleResponse", out)
	if a := out["article"].(map[string]any); a["answer"] != "Tap Report a problem. Support will help." || a["sort"] != float64(5) || a["slug"] != "gate-closed" {
		t.Errorf("%v", a)
	}
	_, out = rs.call("GET", "/riders/me/help", rider, "")
	faq = out["faq"].([]any)
	if faq[0].(map[string]any)["id"] != id || faq[0].(map[string]any)["answer"] != "Tap Report a problem. Support will help." {
		t.Errorf("after the update: %v", faq)
	}
	var reason, after string
	if err := f.pool.QueryRow(context.Background(), "SELECT reason, after::text FROM audit_events WHERE action = 'help.updated'").Scan(&reason, &after); err != nil || reason != "Clearer words" || !strings.Contains(after, `"sort": 5`) {
		t.Errorf("audit %q %s %v", reason, after, err)
	}
	// Nothing to change is not a change: no new audit row.
	if code, _ := f.call("PATCH", "/admin/help-articles/"+id, u.ops, `{"sort":5,"reason":"Same again"}`); code != 200 {
		t.Errorf("the same values: %d", code)
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'help.updated'"); n != 1 {
		t.Errorf("%d update audit rows, want 1", n)
	}
	for name, c := range map[string]struct{ path, body string }{
		"no reason": {id, `{"answer":"b"}`}, "a short reason": {id, `{"answer":"b","reason":"no"}`}, "no field": {id, `{"reason":"Only a reason"}`},
		"the slug": {id, `{"slug":"new-slug","reason":"Rename it"}`}, "an empty answer": {id, `{"answer":"","reason":"Empty it"}`},
		"a null question": {id, `{"question":null,"reason":"Null it"}`}, "sort 10001": {id, `{"sort":10001,"reason":"Too far"}`},
		"a long question": {id, `{"question":"` + strings.Repeat("q", 201) + `","reason":"Too long"}`}, "active as text": {id, `{"active":"no","reason":"Wrong type"}`},
		"not json": {id, `x`},
	} {
		if code, out := f.call("PATCH", "/admin/help-articles/"+c.path, u.ops, c.body); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	for _, missing := range []string{"00000000-0000-0000-0000-000000000000", "not-a-uuid"} {
		if code, out := f.call("PATCH", "/admin/help-articles/"+missing, u.ops, `{"answer":"b","reason":"Missing one"}`); code != 404 || out["code"] != "not_found" {
			t.Errorf("%s: %d %v", missing, code, out)
		}
		if code, _ := f.call("POST", "/admin/help-articles/"+missing+"/deactivate", u.ops, `{"reason":"Missing one"}`); code != 404 {
			t.Errorf("deactivate %s: %d", missing, code)
		}
	}

	// Deactivate hides it from riders, keeps it for ops, and a second time changes nothing. Active again shows it.
	code, out = f.call("POST", "/admin/help-articles/"+id+"/deactivate", u.ops, `{"reason":"No longer true"}`)
	if code != 200 || out["article"].(map[string]any)["active"] != false {
		t.Fatalf("%d %v", code, out)
	}
	matchesContract(t, "HelpArticleResponse", out)
	_, out = rs.call("GET", "/riders/me/help", rider, "")
	if faq := out["faq"].([]any); len(faq) != 1 || faq[0].(map[string]any)["id"] != second["id"] {
		t.Errorf("a deactivated article is still shown: %v", faq)
	}
	_, out = f.call("GET", "/admin/help-articles", u.ops, "")
	if len(out["articles"].([]any)) != 2 {
		t.Errorf("ops still see both: %v", out)
	}
	f.call("POST", "/admin/help-articles/"+id+"/deactivate", u.ops, `{"reason":"Again please"}`)
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'help.deactivated'"); n != 1 {
		t.Errorf("%d deactivate audit rows, want 1", n)
	}
	if code, _ := f.call("POST", "/admin/help-articles/"+id+"/deactivate", u.ops, `{"reason":""}`); code != 400 {
		t.Errorf("deactivate with no reason: %d", code)
	}
	f.call("PATCH", "/admin/help-articles/"+id, u.ops, `{"active":true,"reason":"True again"}`)
	if _, out = rs.call("GET", "/riders/me/help", rider, ""); len(out["faq"].([]any)) != 2 {
		t.Errorf("an article made active again is not shown: %v", out)
	}

	// Bad new articles.
	long := strings.Repeat("a", 4001)
	for name, body := range map[string]string{
		"an upper-case slug": `{"slug":"Gate","question":"q","answer":"a"}`, "a slug with a space": `{"slug":"a b","question":"q","answer":"a"}`,
		"a 61-character slug": `{"slug":"` + strings.Repeat("a", 61) + `","question":"q","answer":"a"}`, "no slug": `{"question":"q","answer":"a"}`,
		"no question": `{"slug":"ok-1","answer":"a"}`, "spaces for a question": `{"slug":"ok-1","question":"   ","answer":"a"}`,
		"a long question": `{"slug":"ok-1","question":"` + strings.Repeat("q", 201) + `","answer":"a"}`, "a long answer": `{"slug":"ok-1","question":"q","answer":"` + long + `"}`,
		"a control character": `{"slug":"ok-1","question":"q\u0007","answer":"a"}`, "sort -1": `{"slug":"ok-1","question":"q","answer":"a","sort":-1}`,
		"another audience": `{"slug":"ok-1","question":"q","answer":"a","audience":"customer"}`, "an extra field": `{"slug":"ok-1","question":"q","answer":"a","active":false}`,
		"not json": `x`,
	} {
		if code, out := f.call("POST", "/admin/help-articles", u.ops, body); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if code, _ := f.call("POST", "/admin/help-articles", u.ops, `{"slug":"ok-1","question":"`+strings.Repeat("q", 200)+`","answer":"`+strings.Repeat("a", 4000)+`","audience":"rider","sort":0}`); code != 201 {
		t.Errorf("the longest article: %d", code)
	}

	// Replay: the same key and body gives the same reply and one row; another body is refused.
	body := `{"slug":"replayed","question":"q","answer":"a"}`
	c1, a := f.callKey("POST", "/admin/help-articles", u.ops, "help-replay-0001", body)
	c2, b := f.callKey("POST", "/admin/help-articles", u.ops, "help-replay-0001", body)
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if c1 != 201 || c2 != 201 || string(aj) != string(bj) {
		t.Errorf("replay: %d %d\n%s\n%s", c1, c2, aj, bj)
	}
	if n := f.count("SELECT count(*) FROM help_articles WHERE slug = 'replayed'"); n != 1 {
		t.Errorf("%d replayed articles", n)
	}
	if code, out := f.callKey("POST", "/admin/help-articles", u.ops, "help-replay-0001", `{"slug":"replayed","question":"other","answer":"a"}`); code != 422 {
		t.Errorf("the same key with another body: %d %v", code, out)
	}
}

func TestRiderFeedbackIsStoredAndListedInTheConsole(t *testing.T) {
	f, u, rs := newRiderSide(t)
	zone := f.zone2("Feedback")
	ravi, raviToken := rs.rider(zone, "+919822200011", "Ravi")
	meena, meenaToken := rs.rider(zone, "+919822200012", "Meena")

	// Nothing yet.
	code, out := f.call("GET", "/admin/rider-feedback", u.support, "")
	if code != 200 || len(out["feedback"].([]any)) != 0 || out["next_cursor"] != nil {
		t.Fatalf("%d %v", code, out)
	}
	matchesContract(t, "RiderFeedbackList", out)

	for i, m := range []struct{ token, text string }{{raviToken, "The map is hard to read at night."}, {meenaToken, "Add a dark mode.\nThanks."}, {raviToken, "The break button is too small."}} {
		if code, out := rs.call("POST", "/riders/me/feedback", m.token, fmt.Sprintf(`{"message":%q}`, m.text)); code != 201 {
			t.Fatalf("%d %v", code, out)
		}
		f.exec("UPDATE rider_feedback SET created_at = now() - make_interval(mins => $1) WHERE message = $2", 30-i*10, m.text)
	}
	code, out = f.call("GET", "/admin/rider-feedback", u.support, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	matchesContract(t, "RiderFeedbackList", out)
	list := out["feedback"].([]any)
	if len(list) != 3 || list[0].(map[string]any)["message"] != "The break button is too small." || list[2].(map[string]any)["message"] != "The map is hard to read at night." {
		t.Fatalf("newest first: %v", list)
	}
	if r := list[1].(map[string]any)["rider"].(map[string]any); r["id"] != meena || r["name"] != "Meena" || list[1].(map[string]any)["message"] != "Add a dark mode.\nThanks." {
		t.Errorf("%v", list[1])
	}
	// Privacy: the rider's id and name, never a phone number; and no customer data exists in the reply.
	raw, _ := json.Marshal(out)
	for _, bad := range []string{"9822200011", "9822200012", "+91", "phone", "customer", "address"} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("the feedback list holds %q: %s", bad, raw)
		}
	}
	// Admin may read too. Ops and a hub manager may not: the permission is tickets.manage.
	if code, _ := f.call("GET", "/admin/rider-feedback", u.admin, ""); code != 200 {
		t.Errorf("admin: %d", code)
	}
	for name, token := range map[string]string{"ops": u.ops, "hub manager": u.hub} {
		if code, out := f.call("GET", "/admin/rider-feedback", token, ""); code != 403 || out["missing_permission"] != "tickets.manage" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if code, _ := f.call("GET", "/admin/rider-feedback", raviToken, ""); code != 401 {
		t.Errorf("a rider's token on the console: %d", code)
	}
	// Filter by rider, and page.
	_, out = f.call("GET", "/admin/rider-feedback?rider_id="+ravi, u.support, "")
	if got := out["feedback"].([]any); len(got) != 2 {
		t.Errorf("Ravi's feedback: %v", got)
	}
	_, page := f.call("GET", "/admin/rider-feedback?limit=2", u.support, "")
	if len(page["feedback"].([]any)) != 2 || page["next_cursor"] == nil {
		t.Fatalf("%v", page)
	}
	_, page2 := f.call("GET", "/admin/rider-feedback?limit=2&cursor="+page["next_cursor"].(string), u.support, "")
	if got := page2["feedback"].([]any); len(got) != 1 || got[0].(map[string]any)["message"] != "The map is hard to read at night." || page2["next_cursor"] != nil {
		t.Errorf("page 2: %v", page2)
	}
	for _, bad := range []string{"?limit=0", "?limit=101", "?cursor=!!", "?rider_id=nope"} {
		if code, out := f.call("GET", "/admin/rider-feedback"+bad, u.support, ""); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", bad, code, out)
		}
	}
	// The console cannot change feedback: it is read only.
	for _, method := range []string{"POST", "PATCH", "DELETE"} {
		if code, _ := f.call(method, "/admin/rider-feedback", u.admin, `{}`); code != 405 && code != 404 {
			t.Errorf("%s on the feedback list: %d", method, code)
		}
	}
}

// today is the zone's local day.
func today() string { return time.Now().In(india).Format("2006-01-02") }

func TestShiftPlansAreMadeByOpsAndAnsweredByRiders(t *testing.T) {
	f, u, rs := newRiderSide(t)
	zone, other := f.zone2("Plans"), f.zone2("Elsewhere")
	ravi, raviToken := rs.rider(zone, "+919822200021", "Ravi")
	_, meenaToken := rs.rider(zone, "+919822200022", "Meena")
	node := f.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, 'Node A', 12.93, 77.62, 4) RETURNING id::text", zone)
	farNode := f.id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, 'Node Far', 12.93, 77.62, 4) RETURNING id::text", other)

	// With no plan, the rider sees null, and ops see an empty list.
	_, out := rs.call("GET", "/riders/me/shift-plan", raviToken, "")
	contracttest.Check(t, riderContract, "ShiftPlanResponse", out)
	if out["plan"] != nil {
		t.Fatalf("no plan yet: %v", out)
	}
	code, out := f.call("GET", "/admin/shift-plans", u.ops, "")
	if code != 200 || len(out["plans"].([]any)) != 0 {
		t.Fatalf("%d %v", code, out)
	}
	matchesContract(t, "ShiftPlanList", out)

	starts := time.Now().UTC().Truncate(time.Second)
	body := func(rider, date, nodeID string, from, to time.Time) string {
		n := "null"
		if nodeID != "" {
			n = `"` + nodeID + `"`
		}
		return fmt.Sprintf(`{"rider_id":%q,"date":%q,"node_id":%s,"starts_at":%q,"ends_at":%q}`, rider, date, n, from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	code, out = f.call("POST", "/admin/shift-plans", u.ops, body(ravi, today(), node, starts.Add(time.Hour), starts.Add(7*time.Hour)))
	if code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	matchesContract(t, "AdminShiftPlanResponse", out)
	made := out["plan"].(map[string]any)
	if made["stock_reply"] != "pending" || made["packing"] != "not_started" || made["node_id"] != node || made["rider"].(map[string]any)["name"] != "Ravi" || made["date"] != today() {
		t.Errorf("%v", made)
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'shift_plan.created' AND actor_type = 'console'"); n != 1 {
		t.Errorf("%d audit rows", n)
	}
	// One plan per rider and day.
	if code, out := f.call("POST", "/admin/shift-plans", u.admin, body(ravi, today(), "", starts.Add(time.Hour), starts.Add(3*time.Hour))); code != 409 || out["code"] != "shift_plan_exists" {
		t.Errorf("a second plan for the day: %d %v", code, out)
	}
	// Ops may make another day's plan, with no node yet, up to 60 days ahead.
	tomorrow := time.Now().In(india).AddDate(0, 0, 1)
	day := tomorrow.Format("2006-01-02")
	if code, out := f.call("POST", "/admin/shift-plans", u.ops, body(ravi, day, "", tomorrow, tomorrow.Add(5*time.Hour))); code != 201 || out["plan"].(map[string]any)["node_id"] != nil {
		t.Errorf("tomorrow: %d %v", code, out)
	}

	// Meena has a plan too, two days on, so the rider and date filters have something to leave out.
	dayAfter := time.Now().In(india).AddDate(0, 0, 2)
	if code, out := f.call("POST", "/admin/shift-plans", u.ops, body(meenaID(t, f, "+919822200022"), dayAfter.Format("2006-01-02"), "", dayAfter, dayAfter.Add(time.Hour))); code != 201 {
		t.Fatalf("%d %v", code, out)
	}

	// The rider sees today's plan, in the contract's shape, and not tomorrow's.
	_, out = rs.call("GET", "/riders/me/shift-plan", raviToken, "")
	contracttest.Check(t, riderContract, "ShiftPlanResponse", out)
	seen := out["plan"].(map[string]any)
	if seen["id"] != made["id"] || seen["date"] != today() || seen["node_id"] != node || seen["stock_reply"] != "pending" {
		t.Errorf("the rider's plan: %v", seen)
	}
	raw, _ := json.Marshal(seen)
	if strings.Contains(string(raw), "Ravi") || strings.Contains(string(raw), "rider") {
		t.Errorf("the rider's plan holds more than the contract: %s", raw)
	}
	// Another rider has none.
	if _, out := rs.call("GET", "/riders/me/shift-plan", meenaToken, ""); out["plan"] != nil {
		t.Errorf("Meena sees %v", out["plan"])
	}

	// The rider answers, and ops see the answer.
	if code, out := rs.call("POST", "/riders/me/shift-plan/reply", raviToken, `{"reply":"coming"}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	_, out = f.call("GET", "/admin/shift-plans?rider_id="+ravi+"&date="+today(), u.ops, "")
	matchesContract(t, "ShiftPlanList", out)
	plans := out["plans"].([]any)
	if len(plans) != 1 || plans[0].(map[string]any)["stock_reply"] != "coming" || plans[0].(map[string]any)["replied_at"] == nil {
		t.Fatalf("ops see the answer: %v", plans)
	}
	rs.call("POST", "/riders/me/shift-plan/reply", raviToken, `{"reply":"not_today"}`)
	_, out = f.call("GET", "/admin/shift-plans", u.admin, "")
	if all := out["plans"].([]any); len(all) != 3 || all[2].(map[string]any)["stock_reply"] != "not_today" || all[0].(map[string]any)["rider"].(map[string]any)["name"] != "Meena" {
		t.Errorf("all plans, newest first: %v", all)
	}
	// Paging and filters.
	if _, out := f.call("GET", "/admin/shift-plans?rider_id="+ravi, u.ops, ""); len(out["plans"].([]any)) != 2 {
		t.Errorf("Ravi's plans: %v", out)
	}
	_, page := f.call("GET", "/admin/shift-plans?limit=1", u.ops, "")
	if len(page["plans"].([]any)) != 1 || page["next_cursor"] == nil {
		t.Fatalf("%v", page)
	}
	_, page2 := f.call("GET", "/admin/shift-plans?limit=1&cursor="+page["next_cursor"].(string), u.ops, "")
	if got := page2["plans"].([]any); len(got) != 1 || got[0].(map[string]any)["id"] == page["plans"].([]any)[0].(map[string]any)["id"] || page2["next_cursor"] == nil {
		t.Errorf("page 2: %v", page2)
	}
	if _, out := f.call("GET", "/admin/shift-plans?date="+day, u.ops, ""); len(out["plans"].([]any)) != 1 {
		t.Errorf("the date filter: %v", out)
	}
	for _, bad := range []string{"?limit=0", "?cursor=!!", "?rider_id=nope", "?date=2026-13-45", "?date=today"} {
		if code, out := f.call("GET", "/admin/shift-plans"+bad, u.ops, ""); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", bad, code, out)
		}
	}

	// Refused: a rider who does not exist (404), and bad plans (400).
	if code, out := f.call("POST", "/admin/shift-plans", u.ops, body("00000000-0000-0000-0000-000000000000", today(), "", starts, starts.Add(time.Hour))); code != 404 || out["code"] != "not_found" {
		t.Errorf("unknown rider: %d %v", code, out)
	}
	longDay := time.Now().In(india).AddDate(0, 0, 61)
	pastDay := time.Now().In(india).AddDate(0, 0, -1)
	for name, b := range map[string]string{
		"a node in another zone":  body(meenaID(t, f, "+919822200022"), today(), farNode, starts, starts.Add(time.Hour)),
		"an unknown node":         body(meenaID(t, f, "+919822200022"), today(), "00000000-0000-0000-0000-000000000000", starts, starts.Add(time.Hour)),
		"yesterday":               body(ravi, pastDay.Format("2006-01-02"), "", pastDay, pastDay.Add(time.Hour)),
		"61 days ahead":           body(ravi, longDay.Format("2006-01-02"), "", longDay, longDay.Add(time.Hour)),
		"an end before the start": body(ravi, day, "", tomorrow.Add(2*time.Hour), tomorrow),
		"longer than 24 hours":    body(ravi, day, "", tomorrow, tomorrow.Add(25*time.Hour)),
		"a start on another day":  body(ravi, day, "", tomorrow.Add(48*time.Hour), tomorrow.Add(50*time.Hour)),
		"a bad date":              `{"rider_id":"` + ravi + `","date":"31-12-2099","starts_at":"2099-12-31T04:00:00Z","ends_at":"2099-12-31T06:00:00Z"}`,
		"no rider":                `{"date":"2099-12-31","starts_at":"2099-12-31T04:00:00Z","ends_at":"2099-12-31T06:00:00Z"}`,
		"an extra field":          `{"rider_id":"` + ravi + `","date":"` + day + `","starts_at":"` + tomorrow.Format(time.RFC3339) + `","ends_at":"` + tomorrow.Add(time.Hour).Format(time.RFC3339) + `","packing":"ready"}`,
		"not json":                `x`,
	} {
		if code, out := f.call("POST", "/admin/shift-plans", u.ops, b); (code != 400 && code != 404) || (out["code"] != "invalid_request" && out["code"] != "not_found") {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	// Support and hub managers may do none of it.
	for name, token := range map[string]string{"support": u.support, "hub manager": u.hub} {
		if code, out := f.call("GET", "/admin/shift-plans", token, ""); code != 403 || out["missing_permission"] != "shift_plans.manage" {
			t.Errorf("%s list: %d %v", name, code, out)
		}
		if code, out := f.call("POST", "/admin/shift-plans", token, body(ravi, day, "", tomorrow, tomorrow.Add(time.Hour))); code != 403 || out["missing_permission"] != "shift_plans.manage" {
			t.Errorf("%s create: %d %v", name, code, out)
		}
	}
	if code, _ := f.call("GET", "/admin/shift-plans", raviToken, ""); code != 401 {
		t.Errorf("a rider's token on the console: %d", code)
	}
	// Replay.
	replayBody := body(meenaID(t, f, "+919822200022"), today(), "", starts, starts.Add(2*time.Hour))
	c1, a := f.callKey("POST", "/admin/shift-plans", u.ops, "plan-replay-0001", replayBody)
	c2, b := f.callKey("POST", "/admin/shift-plans", u.ops, "plan-replay-0001", replayBody)
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if c1 != 201 || c2 != 201 || string(aj) != string(bj) {
		t.Errorf("replay: %d %d\n%s\n%s", c1, c2, aj, bj)
	}
	if n := f.count("SELECT count(*) FROM shift_plans WHERE rider_id = $1 AND plan_date = $2::date", meenaID(t, f, "+919822200022"), today()); n != 1 {
		t.Errorf("%d plans after a replay", n)
	}
	// Plans are a note: no shift was made, ended or scheduled by any of this.
	for _, table := range []string{"shifts", "outbox_events", "scheduled_tasks"} {
		if n := f.count("SELECT count(*) FROM " + table); n != 0 {
			t.Errorf("%s has %d rows after shift plans", table, n)
		}
	}
}

func meenaID(t *testing.T, f *fixture, phone string) string {
	t.Helper()
	return f.id("SELECT r.id::text FROM riders r JOIN people p ON p.id = r.person_id WHERE p.phone = $1", phone)
}

// An applicant can sign in before giving a name (riders.name may be null). The console lists must
// still answer, with an empty name, and the replies must match the contract.
func TestConsoleListsWorkForARiderWithNoName(t *testing.T) {
	f, u, rs := newRiderSide(t)
	zone := f.zone2("Nameless")
	id, token := rs.rider(zone, "+919822200031", "x")
	f.exec("UPDATE riders SET name = NULL WHERE id = $1", id)
	if code, out := rs.call("POST", "/riders/me/feedback", token, `{"message":"No name yet."}`); code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	now := time.Now().UTC().Truncate(time.Second)
	body := fmt.Sprintf(`{"rider_id":%q,"date":%q,"starts_at":%q,"ends_at":%q}`, id, today(), now.Format(time.RFC3339), now.Add(time.Hour).Format(time.RFC3339))
	code, out := f.call("POST", "/admin/shift-plans", u.ops, body)
	if code != 201 {
		t.Fatalf("createShiftPlan: %d %v", code, out)
	}
	matchesContract(t, "AdminShiftPlanResponse", out)
	code, out = f.call("GET", "/admin/shift-plans", u.ops, "")
	if code != 200 {
		t.Fatalf("listShiftPlans: %d %v", code, out)
	}
	matchesContract(t, "ShiftPlanList", out)
	if name := out["plans"].([]any)[0].(map[string]any)["rider"].(map[string]any)["name"]; name != "" {
		t.Errorf("plan rider name %v", name)
	}
	code, out = f.call("GET", "/admin/rider-feedback", u.support, "")
	if code != 200 {
		t.Fatalf("listRiderFeedback: %d %v", code, out)
	}
	matchesContract(t, "RiderFeedbackList", out)
	if name := out["feedback"].([]any)[0].(map[string]any)["rider"].(map[string]any)["name"]; name != "" {
		t.Errorf("feedback rider name %v", name)
	}
}
