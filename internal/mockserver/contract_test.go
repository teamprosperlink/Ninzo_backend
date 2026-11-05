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
	"registerDevice": `{"platform":"android","push_token":"t"}`, "registerRiderDevice": `{"platform":"android","push_token":"t"}`,
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

// TestEveryScenarioChangesSomething stops a listed scenario from silently doing nothing.
func TestEveryScenarioChangesSomething(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	apps := []struct {
		c         *apiContract
		scenarios map[string]string
	}{
		{loadContract(t, "customer", "customer.openapi.yaml", "/customer/v1"), customerScenarios},
		{loadContract(t, "rider", "rider.openapi.yaml", "/rider/v1"), riderScenarios},
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
			if name == "socket_drop" || name == "partial_quantity" || name == "rider_already_near" || name == "payment_captured" {
				continue // These differ on the stream, or by what the client sends.
			}
			changed := false
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

func checkStage(t *testing.T, where string, order map[string]any) {
	t.Helper()
	stage, _ := order["customer_stage"].(string)
	allowed, ok := stageRules[stage]
	if !ok {
		t.Errorf("%s: unknown stage %q", where, stage)
		return
	}
	permitted := map[string]bool{}
	for _, f := range allowed {
		permitted[f] = true
	}
	for _, f := range stageFields {
		if order[f] != nil && !permitted[f] {
			t.Errorf("%s: stage %s must not set %s", where, stage, f)
		}
	}
	// Nothing may reveal a rider before rider_accepted.
	if stage == "looking_for_rider" || stage == "no_scooter" || stage == "payment" || stage == "payment_failed" {
		if order["rider"] != nil || order["eta_s"] != nil || order["eta_source"] != "placeholder" {
			t.Errorf("%s: stage %s reveals a rider", where, stage)
		}
	}
	required := map[string][]string{"payment": {"payment_request"}, "looking_for_rider": {"wait_deadline", "cancel_until"}, "no_scooter": {"wait_deadline"},
		"rider_accepted": {"rider", "wait_deadline"}, "arrived": {"rider", "handover_otp"}, "ended": {"ended_reason"}}[stage]
	for _, f := range required {
		if order[f] == nil {
			t.Errorf("%s: stage %s must set %s", where, stage, f)
		}
	}
	timer, _ := order["timer"].(map[string]any)
	if (stage == "payment" || stage == "looking_for_rider" || stage == "no_scooter" || stage == "rider_accepted") && timer["timer_started_at"] != nil {
		t.Errorf("%s: no timer can have started at %s", where, stage)
	}
	if order["cancellable"] == true && (stage == "timer_running" || stage == "arrived" || stage == "handed_over" || stage == "ended") {
		t.Errorf("%s: stage %s cannot be cancellable", where, stage)
	}
	if stage != "payment" && stage != "payment_failed" && order["payment_status"] == "pending" {
		t.Errorf("%s: stage %s cannot have a pending payment", where, stage)
	}
}

func TestEveryCustomerOrderFollowsTheStageTable(t *testing.T) {
	now := time.Now()
	checked := 0
	for name := range customerScenarios {
		var raw map[string]any
		data, _ := json.Marshal(orderForScenario(name, now))
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		checkStage(t, "scenario "+name, raw)
		checked++
		events, _ := customerTimeline(name, now)
		for i, e := range events {
			if e.kind != "order_update" {
				continue
			}
			var o map[string]any
			data, _ := json.Marshal(e.data)
			if err := json.Unmarshal(data, &o); err != nil {
				t.Fatal(err)
			}
			checkStage(t, fmt.Sprintf("stream %s event %d", name, i), o)
			checked++
		}
	}
	t.Logf("%d orders checked against the stage table", checked)
}

func TestStageCheckCatchesBadData(t *testing.T) {
	bad := customerOrder("looking_for_rider", 2, time.Now(), func(o map[string]any) { o["rider"] = map[string]any{"first_name": "Ravi"} })
	inner := &testing.T{}
	checkStage(inner, "bad", bad)
	if !inner.Failed() {
		t.Error("a rider at looking_for_rider must fail the stage check")
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
	fail[scenarioHeader] = ""
	if status, _, _ := post(fail, body); status != 201 {
		t.Errorf("retry after a rejection must run again: got %d", status)
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
	conn := dial(t, srv, "/customer/v1/orders/ord_mock0001/stream?step_ms=5")
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
			checkStage(t, "live stream", order)
		}
		if e["type"] == "rider_position" && (len(stages) == 0 || stages[len(stages)-1] == "looking_for_rider") {
			t.Error("a rider position must not appear during looking_for_rider")
		}
	}
	want := "looking_for_rider,rider_accepted,timer_running,arrived,handed_over"
	if got := strings.Join(stages, ","); got != want {
		t.Errorf("stages: got %s", got)
	}
}

func TestStreamRefusesBadAuthAndCanDrop(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	conn := dial(t, srv, "/customer/v1/orders/x/stream")
	defer conn.CloseNow() //nolint:errcheck
	_ = conn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"auth","token":"`+ExpiredToken+`"}`))
	if _, code := readAll(t, conn); code != closeUnauthorized {
		t.Errorf("expired token: close code %d", code)
	}

	drop := dial(t, srv, "/customer/v1/orders/x/stream?scenario=socket_drop&step_ms=5")
	defer drop.CloseNow() //nolint:errcheck
	_ = drop.Write(context.Background(), websocket.MessageText, []byte(`{"type":"auth","token":"t"}`))
	events, code := readAll(t, drop)
	if code != -1 {
		t.Errorf("a drop must have no close frame: code %d", code)
	}
	if len(events) < 3 {
		t.Errorf("expected auth_ok and two events before the drop, got %d", len(events))
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
	data, _ := json.Marshal(riderTimeline(time.Now())[1].data)
	for _, banned := range []string{"timer_started_at", "timer_duration_s", "timer_stopped_at", "handover_otp"} {
		if strings.Contains(string(data), banned) {
			t.Errorf("rider stream exposes %s", banned)
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
