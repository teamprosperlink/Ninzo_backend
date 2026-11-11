package mockserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Sample bodies for the rider operations added in #157 that the earlier ones do not cover.
func init() {
	for id, body := range map[string]string{
		"updateRider":      `{"name":"Ravi K","preferred_area":"Indiranagar"}`,
		"cancelRestock":    `{"reason":"changed my mind"}`,
		"submitStockCount": `{"lines":[{"item_id":"item_burger","qty":5}],"reason":"scanner_broken"}`,
	} {
		if _, taken := sampleBodies[id]; taken {
			panic("a rider sample body would replace another one: " + id)
		}
		sampleBodies[id] = body
	}
}

var keyCount int

// riderCall sends one request to the rider mock and decodes the JSON reply.
func riderCall(t *testing.T, srv *httptest.Server, method, path, scenario, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+"/rider/v1"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	if method != "GET" {
		keyCount++
		req.Header.Set("Idempotency-Key", fmt.Sprintf("ops-key-%04d", keyCount))
	}
	if scenario != "" {
		req.Header.Set(scenarioHeader, scenario)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s %s: not JSON: %v", method, path, err)
	}
	return resp.StatusCode, out
}

func TestRiderOperationErrorsFollowTheirScenarios(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	for _, tc := range []struct{ method, path, scenario, body, code string }{
		{"POST", "/riders/me/breaks/start", "no_open_shift", "", "no_open_shift"},
		{"POST", "/riders/me/breaks/start", "order_in_progress", "", "order_in_progress"},
		{"POST", "/riders/me/breaks/start", "on_break", "", "on_break"},
		{"POST", "/riders/me/breaks/end", "no_open_shift", "", "no_open_shift"},
		{"POST", "/riders/me/stock/count", "no_open_shift", sampleBodies["submitStockCount"], "no_open_shift"},
		{"POST", "/restock/rst_mock01/cancel", "restock_not_cancellable", `{}`, "restock_not_cancellable"},
		{"POST", "/orders/ord_mock0001/waiting", "not_arrived", "", "not_arrived"},
		{"GET", "/riders/me/shifts/shf_other/summary", "", "", "not_found"},
	} {
		status, out := riderCall(t, srv, tc.method, tc.path, tc.scenario, tc.body)
		want := http.StatusConflict
		if tc.code == "not_found" {
			want = http.StatusNotFound
		}
		if status != want || out["code"] != tc.code || out["retryable"] != false {
			t.Errorf("%s %s (%s): got %d %v, want %d %s", tc.method, tc.path, tc.scenario, status, out, want, tc.code)
		}
	}
}

func TestRiderProfileUpdateChecksItsFields(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	status, out := riderCall(t, srv, "PATCH", "/riders/me", "", `{"name":"Ravi K","preferred_area":"Indiranagar"}`)
	rider, _ := out["rider"].(map[string]any)
	if status != 200 || rider["name"] != "Ravi K" || rider["preferred_area"] != "Indiranagar" {
		t.Errorf("update: got %d %v", status, out)
	}
	for _, body := range []string{`{}`, `{"name":""}`, `{"name":"` + strings.Repeat("a", 81) + `"}`, `{"preferred_area":"` + strings.Repeat("a", 81) + `"}`} {
		if status, out := riderCall(t, srv, "PATCH", "/riders/me", "", body); status != 400 || out["code"] != "invalid_request" {
			t.Errorf("body %.30s: got %d %v, want 400 invalid_request", body, status, out)
		}
	}
	if _, out := riderCall(t, srv, "GET", "/riders/me", "", ""); out["rider"].(map[string]any)["preferred_area"] != nil {
		t.Error("a rider who has set no area must show preferred_area null")
	}
}

func TestRiderListsPageWithCursors(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	for _, tc := range []struct{ path, key string }{{"/riders/me/notifications", "notifications"}, {"/riders/me/shifts", "shifts"}} {
		_, first := riderCall(t, srv, "GET", tc.path, "", "")
		next, _ := first["next_cursor"].(string)
		if next == "" || len(first[tc.key].([]any)) == 0 {
			t.Fatalf("%s: the first page needs items and a next_cursor: %v", tc.path, first)
		}
		_, second := riderCall(t, srv, "GET", tc.path+"?limit=2&cursor="+next, "", "")
		if second["next_cursor"] != nil || len(second[tc.key].([]any)) == 0 {
			t.Errorf("%s: the last page needs items and a null next_cursor: %v", tc.path, second)
		}
	}
	if status, out := riderCall(t, srv, "GET", "/riders/me/shifts?from=yesterday", "", ""); status != 400 || out["code"] != "invalid_request" {
		t.Errorf("a bad date: got %d %v", status, out)
	}
}

func TestStockCountReportsMismatches(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	status, out := riderCall(t, srv, "POST", "/riders/me/stock/count", "", `{"lines":[{"item_id":"item_burger","qty":6},{"item_id":"item_chai","qty":3}],"reason":"scanner_broken"}`)
	if status != 200 || out["mismatches"] != float64(1) || len(out["counted"].([]any)) != 2 {
		t.Errorf("got %d %v, want one mismatch in two lines", status, out)
	}
	for _, body := range []string{`{"lines":[],"reason":"x"}`, `{"lines":[{"item_id":"item_burger","qty":1}]}`, `{"lines":[{"item_id":"item_burger","qty":-1}],"reason":"x"}`} {
		if status, _ := riderCall(t, srv, "POST", "/riders/me/stock/count", "", body); status != 400 {
			t.Errorf("body %s: got %d, want 400", body, status)
		}
	}
}

func TestBreaksAndTheOtherPhoneShowOnTheShift(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	shiftOf := func(out map[string]any) map[string]any { return out["shift"].(map[string]any) }
	_, started := riderCall(t, srv, "POST", "/riders/me/breaks/start", "", "")
	if s := shiftOf(started); s["break_started_at"] == nil || s["break_ends_at"] == nil {
		t.Errorf("a started break needs both times: %v", s)
	}
	_, ended := riderCall(t, srv, "POST", "/riders/me/breaks/end", "", "")
	if s := shiftOf(ended); s["break_started_at"] != nil || s["break_ends_at"] != nil {
		t.Errorf("an ended break must clear both times: %v", s)
	}
	_, state := riderCall(t, srv, "GET", "/riders/me/state", "on_break", "")
	if shiftOf(state)["break_started_at"] == nil {
		t.Error("the on_break scenario must show the break in the state")
	}
	_, shift := riderCall(t, srv, "POST", "/riders/me/shifts/start", "", `{"node_id":"node_mock01","device_id":"dev_phone_a"}`)
	if shiftOf(shift)["device_id"] != "dev_phone_a" {
		t.Errorf("startShift must keep the device_id it was sent: %v", shiftOf(shift))
	}
	_, other := riderCall(t, srv, "GET", "/riders/me/state", "shift_on_other_phone", "")
	if id := shiftOf(other)["device_id"]; id == nil || id == "dev_mock01" {
		t.Errorf("shift_on_other_phone must show another phone's device_id: %v", id)
	}
}

func TestWaitingAtTheGateAgreesAcrossTheApps(t *testing.T) {
	now := time.Now()
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	status, out := riderCall(t, srv, "POST", "/orders/ord_mock0001/waiting", "", "")
	order, _ := out["order"].(map[string]any)
	if status != 200 || order["waiting_since"] == nil || order["state"] != "ARRIVED" {
		t.Errorf("waiting: got %d %v", status, out)
	}
	if _, got := riderCall(t, srv, "GET", "/orders/ord_mock0001", "rider_waiting", ""); got["order"].(map[string]any)["waiting_since"] == nil {
		t.Error("the rider_waiting scenario must show waiting_since on the rider's order")
	}
	if _, got := riderCall(t, srv, "GET", "/orders/ord_mock0001", "", ""); got["order"].(map[string]any)["waiting_since"] != nil {
		t.Error("waiting_since must be null by default")
	}
	if p := stageProblems(currentOrder("rider_waiting", now), now); len(p) != 0 {
		t.Errorf("customer rider_waiting breaks the stage table: %v", p)
	}
	if currentOrder("rider_waiting", now)["rider_waiting_since"] == nil || currentOrder("arrived", now)["rider_waiting_since"] != nil {
		t.Error("only the rider_waiting scenario sets rider_waiting_since on the customer's order")
	}
}

func TestRejectedDocumentsCarryReasons(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	_, out := riderCall(t, srv, "GET", "/riders/me/verification", "documents_rejected", "")
	docs, _ := out["documents"].([]any)
	if out["status"] != "failed" || len(docs) == 0 {
		t.Fatalf("got %v", out)
	}
	for _, d := range docs {
		doc := d.(map[string]any)
		if (doc["status"] == "rejected") != (doc["reason"] != nil) {
			t.Errorf("only a rejected document has a reason: %v", doc)
		}
	}
	if _, plain := riderCall(t, srv, "GET", "/riders/me/verification", "", ""); len(plain["documents"].([]any)) != 0 {
		t.Error("documents must be empty until some are recorded")
	}
}

func TestReviewedRulesForShiftsBreaksAndWaiting(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	want := func(method, path, scenario, body string, status int, code string) {
		t.Helper()
		got, out := riderCall(t, srv, method, path, scenario, body)
		if got != status || (code != "" && out["code"] != code) {
			t.Errorf("%s %s (%s): got %d %v, want %d %s", method, path, scenario, got, out, status, code)
		}
	}
	want("POST", "/riders/me/breaks/start", "offer_open", "", 409, "offer_open")
	want("POST", "/orders/ord_mock0001/waiting", "order_cancelled", "", 409, "order_cancelled")
	want("POST", "/orders/ord_mock0001/waiting", "reassigned", "", 409, "order_reassigned")
	want("POST", "/riders/me/shifts/start", "unknown_device", `{"node_id":"n","device_id":"dev_x"}`, 400, "unknown_device")
	want("POST", "/riders/me/shifts/start", "shift_already_open", `{"node_id":"n","device_id":"dev_x"}`, 409, "shift_already_open")
	want("POST", "/riders/me/shifts/start", "shift_already_open", `{"node_id":"n","device_id":"dev_x","take_over":true}`, 200, "")
	want("POST", "/riders/me/shifts/start", "", `{"node_id":"n","take_over":true}`, 400, "invalid_request")
	want("POST", "/riders/me/stock/count", "", `{"lines":[{"item_id":"a","qty":1},{"item_id":"a","qty":2}],"reason":"x"}`, 400, "invalid_request")
	want("GET", "/riders/me/shifts?from=2026-11-10&to=2026-11-01", "", "", 400, "invalid_request")
	want("PATCH", "/riders/me", "", `{"name":"`+strings.Repeat("é", 80)+`"}`, 200, "")
	want("GET", "/riders/me/shifts/shf_other/summary", "", "", 404, "not_found")
	_, open := riderCall(t, srv, "GET", "/riders/me/shifts/shf_mock01/summary", "", "")
	_, ended := riderCall(t, srv, "GET", "/riders/me/shifts/shf_mock00/summary", "", "")
	if open["ended_at"] != nil || len(open["items_returned"].([]any)) != 0 || ended["ended_at"] == nil || len(ended["items_returned"].([]any)) == 0 {
		t.Errorf("open and ended summaries differ in ended_at and items_returned: %v / %v", open, ended)
	}
}
