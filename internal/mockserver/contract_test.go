package mockserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"github.com/teamprosperlink/Ninzo_backend/contracts"
	"github.com/teamprosperlink/Ninzo_backend/internal/contractbundle"
)

type apiContract struct {
	name     string
	prefix   string
	doc      map[string]any
	compiler *jsonschema.Compiler
	cached   map[string]*jsonschema.Schema
}

func loadContract(t *testing.T, name, file, prefix string) *apiContract {
	t.Helper()
	raw, err := contracts.Dist.ReadFile("dist/" + file)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	normal, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(normal))
	if err != nil {
		t.Fatal(err)
	}
	if err := compiler.AddResource("mem://"+file, inst); err != nil {
		t.Fatal(err)
	}
	return &apiContract{name: file, prefix: prefix, doc: doc, compiler: compiler, cached: map[string]*jsonschema.Schema{}}
}

// schema compiles a contract schema, given its $ref or a schema name.
func (a *apiContract) schema(t *testing.T, ref string) *jsonschema.Schema {
	t.Helper()
	if s, ok := a.cached[ref]; ok {
		return s
	}
	s, err := a.compiler.Compile("mem://" + a.name + ref)
	if err != nil {
		t.Fatalf("compile %s: %v", ref, err)
	}
	a.cached[ref] = s
	return s
}

func (a *apiContract) resolve(node map[string]any) map[string]any {
	if ref, ok := node["$ref"].(string); ok {
		var cur any = a.doc
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			cur = cur.(map[string]any)[part]
		}
		return cur.(map[string]any)
	}
	return node
}

type op struct {
	method, path, id string
	node             map[string]any
}

func (a *apiContract) operations() []op {
	var ops []op
	for path, item := range a.doc["paths"].(map[string]any) {
		for method, raw := range item.(map[string]any) {
			switch method {
			case "get", "post", "put", "patch", "delete":
				node := raw.(map[string]any)
				ops = append(ops, op{strings.ToUpper(method), path, node["operationId"].(string), node})
			}
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].id < ops[j].id })
	return ops
}

func (o op) hasIdempotency(a *apiContract) bool {
	params, _ := o.node["parameters"].([]any)
	for _, p := range params {
		if a.resolve(p.(map[string]any))["name"] == "Idempotency-Key" {
			return true
		}
	}
	return false
}

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// sampleBodies has a valid request for each operation with a body.
var sampleBodies = map[string]string{
	"requestOtp": `{"phone":"+919876543210"}`, "verifyOtp": `{"request_id":"otp_mock01","otp":"123456"}`,
	"refreshToken": `{"refresh_token":"mock-refresh-token"}`, "updateMe": `{"name":"Test"}`,
	"signInWithGoogle": `{"id_token":"t"}`, "requestPhoneOtp": `{"phone":"+919876543210"}`, "verifyPhone": `{"request_id":"otp_mock01","otp":"123456"}`,
	"changeSignInMethod": `{"method":"phone","request_id":"otp_mock01","otp":"123456"}`,
	"confirmTestPayment": `{"result":"paid"}`,
	"registerDevice":     `{"platform":"android","push_token":"t"}`, "registerRiderDevice": `{"platform":"android","push_token":"t"}`,
	"createGate": `{"label":"Back gate","lat":12.9,"lng":77.5}`, "notifyWhenAvailable": `{"gate_id":"gate_mock01"}`,
	"createOrder": `{"gate_id":"gate_mock01","items":[{"item_id":"item_burger","qty":1}]}`, "cancelOrder": `{"reason":"changed my mind"}`,
	"changeOrderGate": `{"gate_id":"gate_mock01"}`, "acceptLongerEta": `{"eta_offer_id":"eta_mock01"}`,
	"createTicket": `{"type":"cold_food","text":"Cold"}`, "razorpayWebhook": `{"event":"payment.captured"}`,
	"startVerification": `{"method":"manual_upload"}`, "startShift": `{"node_id":"node_mock01"}`,
	"scanStock": `{"lot_code":"L1","scanned_at":"2026-11-04T10:00:00Z"}`, "acceptOffer": `{"fencing_token":"ft_mock01"}`,
	"declineOffer": `{"fencing_token":"ft_mock01"}`, "markReady": `{"fencing_token":"ft_mock01"}`,
	"handOver":       `{"otp":"4821","quantities":[{"item_id":"item_burger","qty":1}],"client_time":"2026-11-04T10:00:00Z"}`,
	"locationPing":   `{"lat":12.9,"lng":77.5,"speed_mps":3,"accuracy_m":5,"device_time":"2026-11-04T10:00:00Z"}`,
	"sendEventBatch": `{"events":[{"event_id":"evt_mock0001","type":"problem","payload":{"problem_type":"gate_closed"},"device_time":"2026-11-04T10:00:00Z"}]}`,
	"confirmRestock": `{}`, "declineRestock": `{}`, "respondToShortfall": `{"accept":true}`,
	"createRestock": `{"hub_id":"hub_mock01","items":[{"item_id":"item_burger","qty":4}]}`, "reportProblem": `{"type":"gate_closed"}`,
}

var sampleQueries = map[string]string{"getAvailability": "?gate_id=gate_mock01", "listNodes": "?zone_id=zone_mock01", "getEarnings": "?date=2026-11-04"}

func doRequest(t *testing.T, srv *httptest.Server, a *apiContract, o op, scenario, key string) (int, []byte, http.Header) {
	t.Helper()
	url := srv.URL + a.prefix + pathParam.ReplaceAllString(o.path, "mock_id") + sampleQueries[o.id]
	if o.id == "getMenuItem" {
		url = srv.URL + a.prefix + "/menu/items/item_burger"
	}
	req, err := http.NewRequest(o.method, url, strings.NewReader(sampleBodies[o.id]))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer mock-access-token")
	req.Header.Set("X-Razorpay-Signature", "abc123")
	if scenario != "" {
		req.Header.Set(scenarioHeader, scenario)
	}
	if o.hasIdempotency(a) {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body, resp.Header
}

// responseRef finds the schema the contract declares for this status.
func (a *apiContract) responseRef(t *testing.T, o op, status int) string {
	t.Helper()
	responses := o.node["responses"].(map[string]any)
	raw, ok := responses[fmt.Sprint(status)]
	if !ok {
		t.Errorf("%s returned %d, which the contract does not declare", o.id, status)
		return ""
	}
	resp := a.resolve(raw.(map[string]any))
	schema := resp["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	ref, ok := schema["$ref"].(string)
	if !ok {
		t.Fatalf("%s %d: response schema must be a reference", o.id, status)
	}
	return ref
}

func TestEveryOperationAndScenarioMatchesTheContract(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	apps := []struct {
		c         *apiContract
		scenarios map[string]string
	}{
		{loadContract(t, "customer", "customer.openapi.yaml", "/customer/v1"), customerScenarios},
		{loadContract(t, "rider", "rider.openapi.yaml", "/rider/v1"), riderScenarios},
	}
	checked := 0
	for _, app := range apps {
		names := []string{""}
		for name := range app.scenarios {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, o := range app.c.operations() {
			for i, scenario := range names {
				status, body, _ := doRequest(t, srv, app.c, o, scenario, fmt.Sprintf("key-%s-%d", o.id, i))
				if status == http.StatusNotFound && !isDeclaredNotFound(body) {
					t.Errorf("%s: no route for this operation", o.id)
					continue
				}
				ref := app.c.responseRef(t, o, status)
				if ref == "" {
					continue
				}
				inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
				if err != nil {
					t.Fatalf("%s: not JSON: %v", o.id, err)
				}
				if err := app.c.schema(t, ref).Validate(inst); err != nil {
					t.Errorf("%s, scenario %q, status %d: %v", o.id, scenario, status, err)
				}
				checked++
			}
		}
	}
	if checked < 1000 {
		t.Errorf("only %d responses checked, so this test proves little", checked)
	}
	t.Logf("%d responses validated against the contracts", checked)
}

func TestBadDatesAreRefused(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/rider/v1/riders/me/earnings?date=null", nil)
	req.Header.Set("Authorization", "Bearer t")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("got %d", resp.StatusCode)
	}
}

func isDeclaredNotFound(body []byte) bool { return bytes.Contains(body, []byte(`"not_found"`)) }

// scenariosWithNoVisibleChange are the only ones allowed to look like the default.
var scenariosWithNoVisibleChange = map[string]string{
	"payment_captured": "documented as the same as no scenario",
	"offer_arrives":    "documented as the same as no scenario",
	"partial_quantity": "the difference is what the client sends",
	"socket_drop":      "checked by TestStreamClosesAsDocumented",
	"token_expiring":   "checked by TestStreamClosesAsDocumented",
}

// TestEveryScenarioChangesSomething stops a listed scenario from silently doing nothing.
func TestEveryScenarioChangesSomething(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	apps := []struct {
		c         *apiContract
		scenarios map[string]string
		stream    func(string) string
	}{
		{loadContract(t, "customer", "customer.openapi.yaml", "/customer/v1"), customerScenarios, func(name string) string {
			events, ended := customerEvents(name, time.Unix(0, 0), time.Second)
			return fmt.Sprintf("%d%v", len(events), ended)
		}},
		{loadContract(t, "rider", "rider.openapi.yaml", "/rider/v1"), riderScenarios, func(name string) string {
			return fmt.Sprint(len(riderStoryFor(name, time.Unix(0, 0)).events))
		}},
	}
	volatile := regexp.MustCompile(`\d{4}-\d{2}-\d{2}T[0-9:.]+Z`)
	for _, app := range apps {
		base := map[string]string{}
		ops := app.c.operations()
		for _, o := range ops {
			status, body, _ := doRequest(t, srv, app.c, o, "", "base-"+o.id)
			base[o.id] = fmt.Sprint(status) + volatile.ReplaceAllString(string(body), "T")
		}
		for name := range app.scenarios {
			if _, ok := scenariosWithNoVisibleChange[name]; ok {
				continue
			}
			changed := app.stream(name) != app.stream("")
			for _, o := range ops {
				status, body, _ := doRequest(t, srv, app.c, o, name, "s-"+name+"-"+o.id)
				if fmt.Sprint(status)+volatile.ReplaceAllString(string(body), "T") != base[o.id] {
					changed = true
					break
				}
			}
			if !changed {
				t.Errorf("%s: scenario %q changes nothing", app.c.name, name)
			}
		}
	}
}

// stageRules is the stage table in contracts/README.md. A field not listed must be null.
var stageRules = map[string][]string{
	"payment":           {"payment_request"},
	"payment_failed":    {},
	"looking_for_rider": {"wait_deadline", "cancel_until"},
	"no_scooter":        {"wait_deadline", "eta_offer", "cancel_until"},
	"rider_accepted":    {"rider", "wait_deadline", "eta_s", "cancel_until"},
	"timer_running":     {"rider", "eta_s", "handover_otp", "remedy", "cancel_until"},
	"arrived":           {"rider", "handover_otp", "cancel_until"},
	"handed_over":       {"rider", "remedy", "cancel_until"},
	"ended":             {"ended_reason", "rider", "remedy", "cancel_until"},
}

var stageFields = []string{"payment_request", "wait_deadline", "eta_offer", "rider", "eta_s", "handover_otp", "ended_reason", "remedy", "cancel_until"}

// stageProblems checks one order against the stage table, and returns what is wrong.
func stageProblems(order map[string]any, at time.Time) []string {
	var problems []string
	stage, _ := order["customer_stage"].(string)
	allowed, ok := stageRules[stage]
	if !ok {
		return []string{"unknown stage " + stage}
	}
	permitted := map[string]bool{}
	for _, f := range allowed {
		permitted[f] = true
	}
	for _, f := range stageFields {
		if order[f] != nil && !permitted[f] {
			problems = append(problems, stage+" must not set "+f)
		}
	}
	// Nothing may reveal a rider before rider_accepted.
	switch stage {
	case "looking_for_rider", "no_scooter", "payment", "payment_failed":
		if order["rider"] != nil || order["eta_s"] != nil || order["eta_source"] != "placeholder" {
			problems = append(problems, stage+" reveals a rider")
		}
	}
	required := map[string][]string{
		"payment": {"payment_request"}, "looking_for_rider": {"wait_deadline", "cancel_until"}, "no_scooter": {"wait_deadline", "cancel_until"},
		"rider_accepted": {"rider", "wait_deadline", "cancel_until"}, "timer_running": {"rider", "cancel_until"},
		"arrived": {"rider", "handover_otp", "cancel_until"}, "handed_over": {"rider", "cancel_until"}, "ended": {"ended_reason"},
	}[stage]
	for _, f := range required {
		if order[f] == nil {
			problems = append(problems, stage+" must set "+f)
		}
	}
	timer, _ := order["timer"].(map[string]any)
	switch stage {
	case "payment", "payment_failed", "looking_for_rider", "no_scooter", "rider_accepted":
		if timer["timer_started_at"] != nil {
			problems = append(problems, "no timer can have started at "+stage)
		}
	}
	cancellable := order["cancellable"] == true
	switch stage {
	case "payment", "payment_failed", "no_scooter":
		if !cancellable {
			problems = append(problems, stage+" must be cancellable")
		}
	case "timer_running", "arrived", "handed_over", "ended":
		if cancellable {
			problems = append(problems, stage+" cannot be cancellable")
		}
	}
	if stage == "looking_for_rider" || stage == "rider_accepted" {
		if !at.IsZero() {
			until, err1 := time.Parse(time.RFC3339Nano, fmt.Sprint(order["cancel_until"]))
			deadline, err2 := time.Parse(time.RFC3339Nano, fmt.Sprint(order["wait_deadline"]))
			if err1 == nil && err2 == nil {
				want := at.After(deadline) || (stage == "looking_for_rider" && at.Before(until))
				if cancellable != want {
					problems = append(problems, fmt.Sprintf("%s has cancellable %v, and the times say %v", stage, cancellable, want))
				}
			}
		}
	}
	status := order["payment_status"]
	want := map[string][]any{
		"payment": {"pending"}, "payment_failed": {"failed"}, "looking_for_rider": {"paid"}, "no_scooter": {"paid"},
		"rider_accepted": {"paid"}, "timer_running": {"paid"}, "arrived": {"paid"}, "handed_over": {"paid", "refunded"},
		"ended": {"refunded", "not_paid"},
	}[stage]
	matched := false
	for _, w := range want {
		matched = matched || w == status
	}
	if !matched {
		problems = append(problems, fmt.Sprintf("%s cannot have payment_status %v", stage, status))
	}
	return problems
}

func normal(t *testing.T, v any) map[string]any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEveryCustomerOrderFollowsTheStageTable(t *testing.T) {
	now := time.Now()
	checked := 0
	names := []string{""}
	for name := range customerScenarios {
		names = append(names, name)
	}
	for _, name := range names {
		for _, p := range stageProblems(normal(t, currentOrder(name, now)), now) {
			t.Errorf("GET, scenario %q: %s", name, p)
		}
		checked++
		for _, step := range []time.Duration{time.Second, 500 * time.Millisecond, 30 * time.Second} {
			events, _ := customerEvents(name, now, step)
			index := 0
			for _, e := range events {
				if e.kind != "order_update" {
					continue
				}
				for _, p := range stageProblems(normal(t, e.data), now.Add(time.Duration(index)*step)) {
					t.Errorf("stream, scenario %q, step %v, event %d: %s", name, step, index, p)
				}
				index++
				checked++
			}
		}
	}
	t.Logf("%d orders checked against the stage table", checked)
}

func TestStageCheckCatchesBadData(t *testing.T) {
	now := time.Now()
	bad := []struct {
		name  string
		start string
		key   string
		value any
	}{
		{"a rider during the wait", "", "rider", map[string]any{"first_name": "Ravi"}},
		{"no rider while the timer runs", "timer_running", "rider", nil},
		{"no rider after handover", "handed_over", "rider", nil},
		{"cancellable while the timer runs", "timer_running", "cancellable", true},
		{"no OTP at arrived", "arrived", "handover_otp", nil},
		{"paid before payment", "awaiting_payment", "payment_status", "paid"},
		{"pending while looking", "", "payment_status", "pending"},
		{"not cancellable at payment", "awaiting_payment", "cancellable", false},
		{"cancellable after the window", "after_cancel_window", "cancellable", true},
		{"cancellable once the rider is shown", "offer_accepted", "cancellable", true},
		{"not cancellable inside the window", "", "cancellable", false},
		{"not cancellable after the deadline", "max_wait_passed", "cancellable", false},
	}
	for _, b := range bad {
		order := normal(t, currentOrder(b.start, now))
		order[b.key] = b.value
		if len(stageProblems(order, now)) == 0 {
			t.Errorf("%s must fail the stage check", b.name)
		}
	}
}

// TestStreamAndGetTellTheSameStory is the rule that stops the two drifting apart.
func TestStreamAndGetTellTheSameStory(t *testing.T) {
	now := time.Now()
	names := []string{""}
	for name := range customerScenarios {
		names = append(names, name)
	}
	for _, name := range names {
		story := storyFor(name)
		events, _ := customerEvents(name, now, time.Second)
		var orders []map[string]any
		for _, e := range events {
			if e.kind == "order_update" {
				orders = append(orders, normal(t, e.data))
			}
		}
		get := normal(t, currentOrder(name, now))
		at := story.getAt
		if at < 0 {
			at = len(orders) - 1
		}
		if orders[at]["customer_stage"] != get["customer_stage"] || orders[at]["seq"] != get["seq"] {
			t.Errorf("%q: GET is %v at seq %v, the stream has %v at seq %v", name, get["customer_stage"], get["seq"], orders[at]["customer_stage"], orders[at]["seq"])
		}
		var deadline, started, stopped any
		for i, o := range orders {
			if o["seq"] != float64(i+1) {
				t.Errorf("%q: event %d has seq %v", name, i, o["seq"])
			}
			timer := o["timer"].(map[string]any)
			for _, pair := range []struct {
				field string
				held  *any
			}{{"timer_started_at", &started}, {"timer_stopped_at", &stopped}} {
				v := timer[pair.field]
				if *pair.held != nil && v != *pair.held {
					t.Errorf("%q: %s changed from %v to %v", name, pair.field, *pair.held, v)
				}
				if v != nil {
					*pair.held = v
				}
			}
			if o["customer_stage"] == "looking_for_rider" {
				deadline = o["wait_deadline"]
			}
			if o["customer_stage"] == "rider_accepted" && deadline != nil && o["wait_deadline"] != deadline {
				t.Errorf("%q: wait_deadline changed when the rider accepted", name)
			}
		}
	}
}

func TestRiderGetAndStreamTellTheSameStory(t *testing.T) {
	now := time.Now()
	names := []string{""}
	for name := range riderScenarios {
		names = append(names, name)
	}
	for _, name := range names {
		story := riderStoryFor(name, now)
		last := 0
		for _, e := range story.events {
			if e.seq != last+1 {
				t.Errorf("%q: seq jumped from %d to %d", name, last, e.seq)
			}
			last = e.seq
		}
		if story.getAt == 0 && story.seq >= last {
			t.Errorf("%q: GET at seq %d would make the app ignore the stream, which ends at %d", name, story.seq, last)
		}
		if story.getAt != 0 && story.seq != last {
			t.Errorf("%q: GET is at seq %d, and the stream ends at %d", name, story.seq, last)
		}
	}
}

// TestActionsFollowTheOrder checks each action against the same scenario's order.
func TestActionsFollowTheOrder(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	act := func(method, path, scenario, body string) int {
		req, _ := http.NewRequest(method, srv.URL+"/customer/v1/orders/ord_mock0001"+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer t")
		req.Header.Set("Idempotency-Key", "action-"+scenario+path)
		req.Header.Set(scenarioHeader, scenario)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	now := time.Now()
	for name := range customerScenarios {
		if name == "idempotency_in_progress" {
			continue
		}
		order := currentOrder(name, now)
		want := 409
		if order["cancellable"] == true || order["ended_reason"] == "cancelled" {
			want = 200
		}
		if got := act("POST", "/cancel", name, `{}`); got != want {
			t.Errorf("%q at %v: cancel returned %d, cancellable is %v", name, order["customer_stage"], got, order["cancellable"])
		}
		want = 409
		if order["customer_stage"] == "payment" {
			want = 200
		}
		if got := act("PATCH", "/gate", name, `{"gate_id":"g"}`); got != want {
			t.Errorf("%q at %v: gate change returned %d", name, order["customer_stage"], got)
		}
		want = 409
		if order["rider"] != nil {
			want = 200
		}
		if got := act("POST", "/call", name, ""); got != want {
			t.Errorf("%q at %v: call returned %d", name, order["customer_stage"], got)
		}
	}
	cancelled := cancelledFrom(currentOrder("payment_declined", now))
	if cancelled["payment_status"] != "not_paid" {
		t.Errorf("cancelling before payment must be not_paid, got %v", cancelled["payment_status"])
	}
	if cancelledFrom(currentOrder("", now))["payment_status"] != "refunded" {
		t.Error("cancelling after payment must be refunded")
	}
}

func TestRefundsAreOnlyForRefundedOrders(t *testing.T) {
	now := time.Now()
	for name := range customerScenarios {
		story := storyFor(name)
		refunded := currentOrder(name, now)["payment_status"] == "refunded"
		if refunded != (story.refund != "") {
			t.Errorf("%q: payment_status refunded is %v, and refund reason is %q", name, refunded, story.refund)
		}
	}
}

func TestSharedRules(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	post := func(headers map[string]string, body string) (int, string, http.Header) {
		req, _ := http.NewRequest("POST", srv.URL+"/customer/v1/orders", strings.NewReader(body))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data), resp.Header
	}
	body := sampleBodies["createOrder"]
	auth := map[string]string{"Authorization": "Bearer t"}
	if status, _, _ := post(nil, body); status != 401 {
		t.Errorf("no token: got %d", status)
	}
	if status, data, _ := post(map[string]string{"Authorization": "Bearer " + ExpiredToken}, body); status != 401 || !strings.Contains(data, "token_expired") {
		t.Errorf("expired token: got %d %s", status, data)
	}
	if status, data, _ := post(auth, body); status != 400 || !strings.Contains(data, "idempotency_key_required") {
		t.Errorf("no key: got %d %s", status, data)
	}
	withKey := map[string]string{"Authorization": "Bearer t", "Idempotency-Key": "order-key-1"}
	first, firstBody, _ := post(withKey, body)
	second, secondBody, secondHeader := post(withKey, body)
	if first != 201 || second != 201 || firstBody != secondBody || secondHeader.Get(replayHeader) != "true" {
		t.Errorf("replay: %d %d, same body %v, header %q", first, second, firstBody == secondBody, secondHeader.Get(replayHeader))
	}
	if status, data, _ := post(withKey, `{"gate_id":"other","items":[{"item_id":"x","qty":1}]}`); status != 422 || !strings.Contains(data, "idempotency_key_reused") {
		t.Errorf("reused key: got %d %s", status, data)
	}
	// A rejection is not stored, so a retry with the same key is checked again.
	fail := map[string]string{"Authorization": "Bearer t", "Idempotency-Key": "order-key-2", scenarioHeader: "last_unit_taken"}
	if status, _, _ := post(fail, body); status != 409 {
		t.Errorf("rejection: got %d", status)
	}
	fail[scenarioHeader] = "payment_captured"
	if status, _, _ := post(fail, body); status != 201 {
		t.Errorf("a different scenario must run again: got %d", status)
	}
	// A very large body is refused, not stored.
	big := map[string]string{"Authorization": "Bearer t", "Idempotency-Key": "order-key-3"}
	if status, _, _ := post(big, `{"gate_id":"`+strings.Repeat("x", maxBodyBytes+1)+`"}`); status != 400 {
		t.Errorf("large body: got %d", status)
	}
	progress := map[string]string{"Authorization": "Bearer t", "Idempotency-Key": "order-key-4", scenarioHeader: "idempotency_in_progress"}
	if status, data, header := post(progress, body); status != 409 || !strings.Contains(data, "idempotency_in_progress") || header.Get("Retry-After") == "" {
		t.Errorf("in progress: got %d %s", status, data)
	}
}

func dial(t *testing.T, srv *httptest.Server, path string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func readAll(t *testing.T, conn *websocket.Conn) ([]map[string]any, websocket.StatusCode) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var events []map[string]any
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return events, websocket.CloseStatus(err)
		}
		var e map[string]any
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
}

func TestCustomerStreamReplaysAnOrder(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	conn := dial(t, srv, "/customer/v1/orders/ord_mock0001/stream?step_ms=60")
	defer conn.CloseNow() //nolint:errcheck
	if err := conn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"auth","token":"t"}`)); err != nil {
		t.Fatal(err)
	}
	events, code := readAll(t, conn)
	if code != closeOrderEnded {
		t.Errorf("close code: got %d", code)
	}
	if len(events) == 0 || events[0]["type"] != "auth_ok" {
		t.Fatalf("first event must be auth_ok: %v", events)
	}
	var stages []string
	last := 0.0
	for _, e := range events {
		if e["server_time"] == nil {
			t.Errorf("event %v has no server_time", e["type"])
		}
		if e["type"] == "order_update" {
			seq := e["seq"].(float64)
			if seq != last+1 {
				t.Errorf("seq jumped from %v to %v", last, seq)
			}
			last = seq
			order := e["data"].(map[string]any)
			stages = append(stages, order["customer_stage"].(string))
			for _, p := range stageProblems(order, time.Time{}) {
				t.Errorf("live stream: %s", p)
			}
		}
		if e["type"] == "rider_position" && (len(stages) == 0 || stages[len(stages)-1] == "looking_for_rider") {
			t.Error("a rider position must not appear during looking_for_rider")
		}
	}
	want := "looking_for_rider,looking_for_rider,rider_accepted,timer_running,arrived,handed_over"
	if got := strings.Join(stages, ","); got != want {
		t.Errorf("stages: got %s", got)
	}
}

func TestStreamClosesAsDocumented(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	auth := `{"type":"auth","token":"t"}`
	cases := []struct {
		name, path, first string
		code              websocket.StatusCode
	}{
		{"expired token", "/customer/v1/orders/x/stream", `{"type":"auth","token":"` + ExpiredToken + `"}`, closeUnauthorized},
		{"not an auth frame", "/customer/v1/orders/x/stream", `{"type":"hello"}`, closeUnauthorized},
		{"missing order", "/customer/v1/orders/ord_missing/stream", auth, closeNotFound},
		{"another customer's order", "/customer/v1/orders/ord_forbidden/stream", auth, closeForbidden},
		{"token not renewed", "/customer/v1/orders/x/stream?scenario=token_expiring&step_ms=60", auth, closeUnauthorized},
		{"a drop", "/customer/v1/orders/x/stream?scenario=socket_drop&step_ms=60", auth, -1},
		{"ended order", "/customer/v1/orders/x/stream?scenario=cancelled&step_ms=60", auth, closeOrderEnded},
	}
	for _, tc := range cases {
		conn := dial(t, srv, tc.path)
		_ = conn.Write(context.Background(), websocket.MessageText, []byte(tc.first))
		if _, code := readAll(t, conn); code != tc.code {
			t.Errorf("%s: close code %d, want %d", tc.name, code, tc.code)
		}
		_ = conn.CloseNow()
	}
}

func TestNoAuthFrameClosesWith4401(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the 5-second limit")
	}
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	conn := dial(t, srv, "/customer/v1/orders/x/stream")
	defer conn.CloseNow() //nolint:errcheck
	ctx, cancel := context.WithTimeout(context.Background(), authWait+3*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if code := websocket.CloseStatus(err); code != closeUnauthorized {
		t.Errorf("close code %d, error %v", code, err)
	}
}

// TestReconnectingWorks follows what an app does: reconnect with the same token, or a refreshed one.
func TestReconnectingWorks(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	for _, tc := range []struct {
		scenario string
		first    websocket.StatusCode
	}{{"socket_drop", -1}, {"token_expiring", closeUnauthorized}} {
		for attempt, want := range []websocket.StatusCode{tc.first, closeOrderEnded} {
			conn := dial(t, srv, "/customer/v1/orders/x/stream?step_ms=60&scenario="+tc.scenario)
			_ = conn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"auth","token":"mock-access-token"}`))
			if _, code := readAll(t, conn); code != want {
				t.Errorf("%s, attempt %d: close code %d, want %d", tc.scenario, attempt+1, code, want)
			}
			_ = conn.CloseNow()
		}
	}
}

func TestOneFailurePerMinute(t *testing.T) {
	s := New()
	now := time.Unix(1000, 0)
	if !s.firstTime("k", now) || s.firstTime("k", now.Add(time.Second)) {
		t.Error("the failure must happen once")
	}
	if !s.firstTime("k", now.Add(oneShotReset+time.Second)) {
		t.Error("the scenario must be repeatable later")
	}
}

func TestCancellingTwiceSucceeds(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/customer/v1/orders/x/cancel", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Idempotency-Key", "a-new-key-1")
	req.Header.Set(scenarioHeader, "cancelled")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("got %d", resp.StatusCode)
	}
}

func TestRenewingATokenKeepsTheStreamOpen(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	conn := dial(t, srv, "/customer/v1/orders/x/stream?scenario=token_expiring&step_ms=200")
	defer conn.CloseNow() //nolint:errcheck
	ctx := context.Background()
	_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"auth","token":"t"}`))
	_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"auth","token":"t2"}`))
	events, code := readAll(t, conn)
	if code != closeOrderEnded {
		t.Errorf("a renewed stream must run to its end: close code %d", code)
	}
	if len(events) < 6 {
		t.Errorf("got only %d events", len(events))
	}
}

// TestStreamsOutliveTheServerReadTimeout matches the timeouts in cmd/mock-server.
func TestStreamsOutliveTheServerReadTimeout(t *testing.T) {
	srv := httptest.NewUnstartedServer(New().Handler())
	srv.Config.ReadTimeout = 100 * time.Millisecond
	srv.Start()
	defer srv.Close()
	conn := dial(t, srv, "/customer/v1/orders/x/stream?step_ms=60")
	defer conn.CloseNow() //nolint:errcheck
	_ = conn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"auth","token":"t"}`))
	if _, code := readAll(t, conn); code != closeOrderEnded {
		t.Errorf("the stream must run to its end: close code %d", code)
	}
}

// TestFirstEventIsTrueWhenItArrives reads the real stream, so it covers event timing too.
func TestFirstEventIsTrueWhenItArrives(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	conn := dial(t, srv, "/customer/v1/orders/x/stream")
	defer conn.CloseNow() //nolint:errcheck
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"auth","token":"t"}`))
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var e map[string]any
		_ = json.Unmarshal(raw, &e)
		if e["type"] != "order_update" {
			continue
		}
		order := e["data"].(map[string]any)
		until, err := time.Parse(time.RFC3339Nano, order["cancel_until"].(string))
		if err != nil {
			t.Fatal(err)
		}
		if order["cancellable"] != true || !time.Now().Before(until) {
			t.Errorf("cancellable %v, with cancel_until %v already at %v", order["cancellable"], until, time.Now())
		}
		return
	}
}

func TestHeartbeatCarriesSeq(t *testing.T) {
	s := New()
	s.heartbeat = 5 * time.Millisecond
	s.step = time.Hour
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	conn := dial(t, srv, "/rider/v1/riders/me/stream")
	defer conn.CloseNow() //nolint:errcheck
	_ = conn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"auth","token":"t"}`))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for i := 0; i < 3; i++ {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var e map[string]any
		_ = json.Unmarshal(raw, &e)
		if e["type"] == "heartbeat" && e["seq"] != nil {
			return
		}
	}
	t.Error("no heartbeat with a seq")
}

func TestRiderStreamNeverSendsACountdown(t *testing.T) {
	names := []string{""}
	for name := range riderScenarios {
		names = append(names, name)
	}
	for _, name := range names {
		for _, e := range riderStoryFor(name, time.Now()).events {
			data, _ := json.Marshal(e.data)
			for _, banned := range []string{"timer_started_at", "timer_duration_s", "timer_stopped_at", "timer_missed", "handover_otp", "customer_phone"} {
				if strings.Contains(string(data), banned) {
					t.Errorf("%q, %s event: exposes %s", name, e.kind, banned)
				}
			}
		}
	}
}

func TestBrunoBodiesAreAccepted(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	for _, app := range []struct{ name, file string }{{"customer", "customer.openapi.yaml"}, {"rider", "rider.openapi.yaml"}} {
		c := loadContract(t, app.name, app.file, "/"+app.name+"/v1")
		raw, err := contracts.Dist.ReadFile("dist/" + app.file)
		if err != nil {
			t.Fatal(err)
		}
		files, err := contractbundle.Bruno(app.name, raw)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range c.operations() {
			content, ok := files[app.name+"/"+o.id+".bru"]
			if !ok {
				t.Errorf("no Bruno request for %s", o.id)
				continue
			}
			if o.node["requestBody"] == nil {
				continue
			}
			_, after, found := strings.Cut(content, "body:json {\n")
			if !found {
				t.Errorf("%s has no body", o.id)
				continue
			}
			body := strings.TrimSuffix(strings.TrimSpace(after), "}")
			if !json.Valid([]byte(body)) {
				t.Errorf("%s: body is not JSON: %s", o.id, body)
				continue
			}
			saved := sampleBodies[o.id]
			sampleBodies[o.id] = body
			status, data, _ := doRequest(t, srv, c, o, "", "bruno-"+o.id)
			sampleBodies[o.id] = saved
			if status == 400 {
				t.Errorf("%s: mock refused the Bruno body: %s", o.id, data)
			}
		}
	}
}

func TestDocsAndContractFilesAreServed(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	for _, path := range []string{"/", "/healthz", "/scenarios", "/docs/customer", "/docs/rider", "/contracts/customer.openapi.yaml", "/contracts/rider.openapi.yaml", "/contracts/customer.stream.asyncapi.yaml", "/contracts/rider.stream.asyncapi.yaml"} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("%s: %d", path, resp.StatusCode)
		}
	}
	resp, err := srv.Client().Get(srv.URL + "/contracts/..%2f..%2fgo.mod")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Error("contract files must not serve other paths")
	}
}

func TestRiderSignInUsesAPhoneOnly(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	for _, c := range []struct {
		path, body string
		want       int
	}{
		{"/auth/otp/request", `{"phone":"+919876543210"}`, 200},
		{"/auth/otp/request", `{}`, 400},
		{"/auth/password/login", `{"phone":"+919876543210","password":"long-enough"}`, 404},
		{"/auth/password/reset", `{"request_id":"r","otp":"123456","new_password":"long-enough"}`, 404},
		{"/riders/me/password", `{"new_password":"long-enough"}`, 404},
	} {
		resp, err := http.Post(srv.URL+"/rider/v1"+c.path, "application/json", strings.NewReader(c.body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s %s: got %d, want %d", c.path, c.body, resp.StatusCode, c.want)
		}
	}
}
