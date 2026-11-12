package mockserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/teamprosperlink/Ninzo_backend/contracts"
	"github.com/teamprosperlink/Ninzo_backend/internal/contractbundle"
)

// Sample bodies for the console operations.
func init() {
	for id, body := range map[string]string{
		"consoleLogin":                `{"login_id":"asha.k","password":"a-long-password"}`,
		"enrollTotp":                  `{"challenge_id":"chl_mock01"}`,
		"verifyTotp":                  `{"challenge_id":"chl_mock01","code":"123456"}`,
		"changePassword":              `{"current_password":"old-password","new_password":"a-new-long-password"}`,
		"createConsoleUser":           `{"login_id":"ravi.m","name":"Ravi M","role":"support"}`,
		"updateConsoleUser":           `{"role":"support","reason":"Moved to support"}`,
		"resetConsoleUser":            `{"reason":"Lost the authenticator"}`,
		"rejectChange":                `{"reason":"Not during rush hour"}`,
		"decideRider":                 `{"decision":"approve","zone_id":"zone_mock01","reason":"Documents checked"}`,
		"requestFlagChange":           `{"enabled":true,"reason":"Try it in one zone"}`,
		"requestGlobalSettingsChange": `{"changes":{"timer_length_s":120},"reason":"Rush hour"}`,
		"requestZoneSettingsChange":   `{"changes":{"timer_length_s":120},"reason":"Rush hour"}`,
		"requestKillSwitch":           `{"offer_enabled":false,"reason":"Heavy rain"}`,
		"createMenuItem":              `{"name":"Lemon tea","price_paise":3900}`,
		"updateMenuItem":              `{"price_paise":15900,"reason":"New price list"}`,
		"updateGate":                  `{"label":"Tower B main gate","reason":"Clearer name"}`,
		"createHub":                   `{"zone_id":"zone_mock01","name":"Hub C","lat":12.94,"lng":77.63}`,
		"updateHub":                   `{"status":"paused","reason":"Closed for cleaning"}`,
		"addHubStaff":                 `{"phone":"+919800000099","name":"Imran","role":"hub_staff"}`,
		"updateHubStaff":              `{"status":"suspended","reason":"Left the hub"}`,
		"decideArrivalReview":         `{"decision":"arrived_in_time","reason":"GPS trace shows the gate"}`,
		"replyTicket":                 `{"text":"Sorry about that. We are sending a refund."}`,
		"closeTicket":                 `{"reason":"Refund made"}`,
		"requestRefund":               `{"amount_paise":15000,"reason":"Cold food"}`,
		"issueReplacement":            `{"items":[{"item_id":"item_burger","qty":1}],"reason":"Wrong item sent"}`,
		"issueCredit":                 `{"amount_paise":5000,"reason":"Late delivery"}`,
		"assignIncident":              `{"user_id":"usr_mock01"}`,
		"addIncidentNote":             `{"text":"Called the rider; the gate was shut."}`,
		"closeIncident":               `{"reason":"Handled by phone"}`,
		"expireRestock":               `{"reason":"The hub is closed for the day"}`,
	} {
		if _, taken := sampleBodies[id]; taken {
			panic("a console sample body would replace another app's: " + id)
		}
		sampleBodies[id] = body
	}
}

func consoleContract(t *testing.T) *apiContract {
	return loadContract(t, "console", "console.openapi.yaml", "/console/v1")
}

func TestEveryConsoleOperationAndScenarioMatchesTheContract(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	c := consoleContract(t)
	names := []string{""}
	for name := range consoleScenarios {
		names = append(names, name)
	}
	sort.Strings(names)
	checked, ok := 0, 0
	for _, o := range c.operations() {
		for i, scenario := range names {
			status, body, _ := doRequest(t, srv, c, o, scenario, fmt.Sprintf("key-%s-%d", o.id, i))
			if status == http.StatusNotFound && !isDeclaredNotFound(body) {
				t.Errorf("%s: no route for this operation", o.id)
				continue
			}
			ref := c.responseRef(t, o, status)
			if ref == "" {
				continue
			}
			inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
			if err != nil {
				t.Fatalf("%s: not JSON: %v", o.id, err)
			}
			if err := c.schema(t, ref).Validate(inst); err != nil {
				t.Errorf("%s, scenario %q, status %d: %v", o.id, scenario, status, err)
			}
			checked++
			if status < 300 {
				ok++
			}
		}
	}
	if checked < 900 || ok < 700 {
		t.Errorf("only %d responses checked (%d successes), so this test proves little", checked, ok)
	}
	t.Logf("%d console responses validated against the contract", checked)
}

func TestEveryConsoleScenarioChangesSomething(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	c := consoleContract(t)
	volatile := regexp.MustCompile(`\d{4}-\d{2}-\d{2}T[0-9:.]+Z`)
	base := map[string]string{}
	ops := c.operations()
	for _, o := range ops {
		status, body, _ := doRequest(t, srv, c, o, "", "base-"+o.id)
		base[o.id] = fmt.Sprint(status) + volatile.ReplaceAllString(string(body), "T")
	}
	for name := range consoleScenarios {
		changed := false
		for _, o := range ops {
			status, body, _ := doRequest(t, srv, c, o, name, "s-"+name+"-"+o.id)
			if fmt.Sprint(status)+volatile.ReplaceAllString(string(body), "T") != base[o.id] {
				changed = true
				break
			}
		}
		if !changed {
			t.Errorf("console scenario %q changes nothing", name)
		}
	}
}

func TestAMissingPermissionIsNamed(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	c := consoleContract(t)
	refused := 0
	for _, o := range c.operations() {
		if !strings.HasPrefix(o.path, "/admin/") || o.id == "getConsoleUser" {
			continue
		}
		status, body, _ := doRequest(t, srv, c, o, "no_permission", "perm-"+o.id)
		if status != 403 || !bytes.Contains(body, []byte(`"missing_permission"`)) {
			t.Errorf("%s: %d %s", o.id, status, body)
		}
		refused++
	}
	if refused < 30 {
		t.Errorf("only %d admin operations checked", refused)
	}
}

func TestConsoleSignInIsTwoSteps(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	post := func(path, body, scenario string) (int, map[string]any) {
		req, _ := http.NewRequest("POST", srv.URL+"/console/v1"+path, strings.NewReader(body))
		if scenario != "" {
			req.Header.Set(scenarioHeader, scenario)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, out := post("/auth/login", sampleBodies["consoleLogin"], ""); code != 200 || out["access_token"] != nil || out["challenge_id"] == nil {
		t.Errorf("a password alone must never give a session: %d %v", code, out)
	}
	if code, out := post("/auth/totp/verify", sampleBodies["verifyTotp"], ""); code != 200 || out["access_token"] == nil {
		t.Errorf("%d %v", code, out)
	}
	if code, _ := post("/auth/totp/verify", sampleBodies["verifyTotp"], "invalid_totp"); code != 400 {
		t.Errorf("a wrong code: %d", code)
	}
}

func TestConsoleBrunoBodiesAreAccepted(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	c := consoleContract(t)
	raw, err := contracts.Dist.ReadFile("dist/console.openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	files, err := contractbundle.Bruno("console", raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range c.operations() {
		content, ok := files["console/"+o.id+".bru"]
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

func TestConsoleDocsAndFilesAreServed(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	for _, path := range []string{"/docs/console", "/contracts/console.openapi.yaml"} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("%s: %d", path, resp.StatusCode)
		}
	}
}

// TestEachRefusalNamesTheContractsPermission checks the mock's 403 names the permission the contract
// gives the operation, so the console can build its menus from either.
func TestEachRefusalNamesTheContractsPermission(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	c := consoleContract(t)
	named := 0
	for _, o := range c.operations() {
		want, _ := o.node["x-permission"].(string)
		if want == "" {
			continue
		}
		_, body, _ := doRequest(t, srv, c, o, "no_permission", "named-"+o.id)
		var got map[string]any
		_ = json.Unmarshal(body, &got)
		if got["missing_permission"] != want {
			t.Errorf("%s: 403 names %v, the contract says %s", o.id, got["missing_permission"], want)
		}
		named++
	}
	if named < 30 {
		t.Errorf("only %d operations have a permission", named)
	}
}

// TestARoleWithoutOrdersReadIsRefusedTheOrderSearch checks the mock's role scenarios follow the
// permission table: a hub_manager has no orders.read, an ops user does.
func TestARoleWithoutOrdersReadIsRefusedTheOrderSearch(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	c := consoleContract(t)
	for _, o := range c.operations() {
		if o.id != "listAdminOrders" {
			continue
		}
		status, body, _ := doRequest(t, srv, c, o, "role_hub_manager", "role-hub")
		var got map[string]any
		_ = json.Unmarshal(body, &got)
		if status != 403 || got["missing_permission"] != "orders.read" {
			t.Errorf("hub_manager: %d %s", status, body)
		}
		if status, body, _ := doRequest(t, srv, c, o, "role_ops", "role-ops"); status != 200 {
			t.Errorf("ops: %d %s", status, body)
		}
		return
	}
	t.Fatal("listAdminOrders is not in the contract")
}

// TestARefundCountsWhatWasRefundedBefore checks the limit is cumulative per order (#147).
func TestARefundCountsWhatWasRefundedBefore(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	c := consoleContract(t)
	for _, o := range c.operations() {
		if o.id != "requestRefund" {
			continue
		}
		for amount, want := range map[int]int{15000: 200, 15001: 202, 40000: 202} {
			saved := sampleBodies[o.id]
			sampleBodies[o.id] = fmt.Sprintf(`{"amount_paise":%d,"reason":"Cold food"}`, amount)
			status, body, _ := doRequest(t, srv, c, o, "", fmt.Sprintf("cum-%d", amount))
			sampleBodies[o.id] = saved
			if status != want {
				t.Errorf("%d paise after 5000 refunded: %d, want %d: %s", amount, status, want, body)
			}
		}
		return
	}
	t.Fatal("requestRefund is not in the contract")
}

// An amount past what an int holds, or outside the contract's range, is refused (#167): the
// fuzzer once sent one past int64 and got a 201 with a negative amount.
func TestAmountsOutsideTheRangeAreRefused(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	c := consoleContract(t)
	for _, o := range c.operations() {
		if o.id != "requestRefund" && o.id != "issueCredit" {
			continue
		}
		for i, amount := range []string{"150627966214929711275765137408", "10000001", "0", "-5", "12.5"} {
			saved := sampleBodies[o.id]
			sampleBodies[o.id] = `{"amount_paise":` + amount + `,"reason":"Cold food"}`
			status, body, _ := doRequest(t, srv, c, o, "", fmt.Sprintf("range-%s-%d", o.id, i))
			sampleBodies[o.id] = saved
			if status != 400 {
				t.Errorf("%s with %s: %d, want 400: %s", o.id, amount, status, body)
			}
		}
	}
}

// TestTheHubOpsMockFollowsTheRealCall checks what #149 adds: the zone filter, a hub_manager's own
// hub only, a stuck sample that is past the default limit, and an expiry that has just ended.
func TestTheHubOpsMockFollowsTheRealCall(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	c := consoleContract(t)
	hubsFor := func(role, query string) []any {
		for _, o := range c.operations() {
			if o.id != "listHubOps" {
				continue
			}
			saved := sampleQueries[o.id]
			sampleQueries[o.id] = query
			status, body, _ := doRequest(t, srv, c, o, role, "ops-"+role)
			sampleQueries[o.id] = saved
			var got map[string]any
			_ = json.Unmarshal(body, &got)
			if status != 200 {
				t.Fatalf("%s %s: %d %s", role, query, status, body)
			}
			return got["hubs"].([]any)
		}
		t.Fatal("listHubOps is not in the contract")
		return nil
	}
	if n := len(hubsFor("", "")); n != 2 {
		t.Errorf("an admin sees %d hubs", n)
	}
	if n := len(hubsFor("", "?zone_id=zone_mock02")); n != 1 {
		t.Errorf("zone_mock02: %d hubs", n)
	}
	if n := len(hubsFor("", "?zone_id=zone_none")); n != 0 {
		t.Errorf("an unknown zone: %d hubs", n)
	}
	if n := len(hubsFor("role_hub_manager", "")); n != 1 {
		t.Errorf("a hub_manager sees %d hubs", n)
	}
	if n := len(hubsFor("role_hub_manager", "?zone_id=zone_mock02")); n != 0 {
		t.Errorf("a hub_manager asking for another zone: %d hubs", n)
	}
	for _, o := range c.operations() {
		switch o.id {
		case "listStuckRestocks":
			_, body, _ := doRequest(t, srv, c, o, "", "stuck-1")
			var got struct {
				Requests []struct {
					WaitingS int `json:"waiting_s"`
				} `json:"requests"`
			}
			_ = json.Unmarshal(body, &got)
			if len(got.Requests) != 1 || got.Requests[0].WaitingS <= 600 {
				t.Errorf("a stuck sample waits longer than the default 600 seconds: %s", body)
			}
		case "expireRestock":
			status, body, _ := doRequest(t, srv, c, o, "role_hub_manager", "expire-1")
			var got map[string]any
			_ = json.Unmarshal(body, &got)
			if status != 403 || got["missing_permission"] != "hub_ops.manage" {
				t.Errorf("a hub_manager cannot expire: %d %s", status, body)
			}
		}
	}
}
