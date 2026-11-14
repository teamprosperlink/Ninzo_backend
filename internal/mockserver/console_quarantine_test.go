package mockserver

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// listQuarantinedStock needs orders.read, which admin, ops and support have and a hub manager does not.
func TestQuarantinedStockFollowsOrdersRead(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	for scenario, allowed := range map[string]bool{"": true, "role_ops": true, "role_support": true, "role_hub_manager": false, "no_permission": false} {
		status, out := consoleCall(t, srv, "GET", "/admin/quarantined-stock", "", scenario)
		if allowed && status != 200 {
			t.Errorf("as %q: %d %v", scenario, status, out)
		}
		if !allowed && (status != 403 || out["missing_permission"] != "orders.read") {
			t.Errorf("as %q: %d %v, want 403 missing_permission orders.read", scenario, status, out)
		}
	}
	// The sample shows one lot from a rider and one from a hub, and no typed words.
	_, out := consoleCall(t, srv, "GET", "/admin/quarantined-stock", "", "")
	lots := out["lots"].([]any)
	if len(lots) != 2 || lots[0].(map[string]any)["source"] != "rider" || lots[1].(map[string]any)["source"] != "hub" {
		t.Errorf("the sample: %v", out)
	}
	if _, empty := consoleCall(t, srv, "GET", "/admin/quarantined-stock", "", "quarantines_empty"); len(empty["lots"].([]any)) != 0 {
		t.Errorf("quarantines_empty: %v", empty)
	}
	if status, out := consoleCall(t, srv, "GET", "/admin/quarantined-stock?from=yesterday", "", ""); status != 400 || out["code"] != "invalid_request" {
		t.Errorf("a bad time: %d %v", status, out)
	}
	// The order timeline in the mock shows the new source too.
	_, tl := consoleCall(t, srv, "GET", "/admin/orders/ord_mock0001/timeline", "", "")
	found := false
	for _, e := range tl["events"].([]any) {
		found = found || (e.(map[string]any)["source"] == "stock" && strings.HasPrefix(e.(map[string]any)["kind"].(string), "stock."))
	}
	if !found {
		t.Errorf("the mock timeline has no stock event: %v", tl["events"])
	}
}
