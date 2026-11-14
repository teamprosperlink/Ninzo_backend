package mockserver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Sample bodies for the console's rider app operations (issue 235, part B2).
func init() {
	for id, body := range map[string]string{
		"createHelpArticle":     `{"slug":"gate-closed","question":"What if the gate is closed?","answer":"Tap Report a problem, then Gate is closed."}`,
		"updateHelpArticle":     `{"answer":"Tap Report a problem. Support will help.","reason":"Clearer words"}`,
		"deactivateHelpArticle": `{"reason":"No longer true"}`,
		"createShiftPlan":       `{"rider_id":"rdr_mock0001","date":"2099-01-01","node_id":"node_mock01","starts_at":"2099-01-01T04:00:00Z","ends_at":"2099-01-01T10:00:00Z"}`,
	} {
		if _, taken := sampleBodies[id]; taken {
			panic("a console sample body would replace another one: " + id)
		}
		sampleBodies[id] = body
	}
}

// consoleCall sends one console request with a scenario and returns the status and body.
func consoleCall(t *testing.T, srv *httptest.Server, method, path, body, scenario string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+"/console/v1"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer mock-access-token")
	req.Header.Set("Content-Type", "application/json")
	if method != "GET" {
		req.Header.Set("Idempotency-Key", fmt.Sprintf("key-%s-%s-%s", method, path, scenario))
	}
	if scenario != "" {
		req.Header.Set(scenarioHeader, scenario)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// Every one of the new calls names its permission, ops may write help and plans but not read the feedback
// inbox, and a hub manager may do none of them.
func TestTheRiderAppConsoleCallsFollowTheirPermissions(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	calls := []struct {
		method, path, body, permission string
		opsMay                         bool
	}{
		{"GET", "/admin/help-articles", "", "help.manage", true},
		{"POST", "/admin/help-articles", sampleBodies["createHelpArticle"], "help.manage", true},
		{"PATCH", "/admin/help-articles/hlp_mock01", sampleBodies["updateHelpArticle"], "help.manage", true},
		{"POST", "/admin/help-articles/hlp_mock01/deactivate", sampleBodies["deactivateHelpArticle"], "help.manage", true},
		{"GET", "/admin/rider-feedback", "", "tickets.manage", false},
		{"GET", "/admin/shift-plans", "", "shift_plans.manage", true},
		{"POST", "/admin/shift-plans", sampleBodies["createShiftPlan"], "shift_plans.manage", true},
	}
	for _, c := range calls {
		for scenario, allowed := range map[string]bool{"": true, "role_ops": c.opsMay, "role_hub_manager": false, "no_permission": false} {
			status, out := consoleCall(t, srv, c.method, c.path, c.body, scenario)
			if allowed && status >= 400 {
				t.Errorf("%s %s as %q: got %d %v, want success", c.method, c.path, scenario, status, out)
			}
			if !allowed && (status != 403 || out["missing_permission"] != c.permission) {
				t.Errorf("%s %s as %q: got %d %v, want 403 missing_permission %s", c.method, c.path, scenario, status, out, c.permission)
			}
		}
	}
}

// help.manage and shift_plans.manage belong to admin and ops only.
func TestTheNewPermissionsBelongToAdminAndOps(t *testing.T) {
	for _, perm := range []string{"help.manage", "shift_plans.manage"} {
		for role, want := range map[string]bool{"admin": true, "ops": true, "support": false, "hub_manager": false} {
			got := false
			for _, p := range rolePermissions[role] {
				got = got || p == perm
			}
			if got != want {
				t.Errorf("%s has %s: %v, want %v", role, perm, got, want)
			}
		}
	}
}

// The mock refuses what the real call refuses.
func TestTheRiderAppConsoleMockRefusesBadBodies(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	bad := []struct{ method, path, body string }{
		{"POST", "/admin/help-articles", `{"slug":"Bad Slug","question":"q","answer":"a"}`},
		{"POST", "/admin/help-articles", `{"slug":"ok","question":"","answer":"a"}`},
		{"POST", "/admin/help-articles", `{"slug":"ok","question":"q","answer":"a","sort":10001}`},
		{"PATCH", "/admin/help-articles/hlp_mock01", `{"reason":"only a reason"}`},
		{"POST", "/admin/help-articles/hlp_mock01/deactivate", `{}`},
		{"POST", "/admin/shift-plans", `{"rider_id":"r","date":"not a date","starts_at":"2099-01-01T04:00:00Z","ends_at":"2099-01-01T10:00:00Z"}`},
		{"POST", "/admin/shift-plans", `{"rider_id":"r","date":"2099-01-01","starts_at":"2099-01-01T10:00:00Z","ends_at":"2099-01-01T04:00:00Z"}`},
		{"POST", "/admin/shift-plans", `{"rider_id":"r","date":"2099-01-01","starts_at":"2099-01-01T00:00:00Z","ends_at":"2099-01-02T01:00:00Z"}`},
	}
	for _, b := range bad {
		if status, out := consoleCall(t, srv, b.method, b.path, b.body, ""); status != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s %s %s: got %d %v, want 400 invalid_request", b.method, b.path, b.body, status, out)
		}
	}
	if status, out := consoleCall(t, srv, "POST", "/admin/help-articles", sampleBodies["createHelpArticle"], "slug_taken"); status != 409 || out["code"] != "slug_taken" {
		t.Errorf("slug_taken: got %d %v", status, out)
	}
	if status, out := consoleCall(t, srv, "POST", "/admin/shift-plans", sampleBodies["createShiftPlan"], "shift_plan_exists"); status != 409 || out["code"] != "shift_plan_exists" {
		t.Errorf("shift_plan_exists: got %d %v", status, out)
	}
	// The empty help centre is shown on both sides.
	if _, out := consoleCall(t, srv, "GET", "/admin/help-articles", "", "help_empty"); len(out["articles"].([]any)) != 0 {
		t.Errorf("help_empty: console list %v", out)
	}
}
