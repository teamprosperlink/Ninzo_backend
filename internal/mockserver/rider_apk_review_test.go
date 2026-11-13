package mockserver

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// present is true when every key along the path exists, even if its value is null.
func present(m map[string]any, path ...string) bool {
	var cur any = m
	for _, p := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		if cur, ok = obj[p]; !ok {
			return false
		}
	}
	return true
}

func TestPresentSeesAMissingFieldAsMissing(t *testing.T) {
	m := map[string]any{"a": map[string]any{"b": nil}}
	if !present(m, "a", "b") || present(m, "a", "c") || present(m, "x") {
		t.Error("present must tell a null field from a missing one")
	}
}

// Wrong-typed values must be answered with 400, never drop the connection.
func TestWrongTypedFieldsAreRefusedNotCrashed(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/riders/me/shifts/planned-end", `{"planned_end_at":12345}`},
		{"POST", "/riders/me/shifts/planned-end", `{"planned_end_at":{"a":1}}`},
		{"POST", "/riders/me/shifts/break", `{"length_s":"900"}`},
		{"POST", "/riders/me/shifts/start", `{"node_id":"node_mock01","checklist":"helmet"}`},
		{"POST", "/riders/me/shifts/start", `{"node_id":"node_mock01","checklist":[1,2]}`},
		{"POST", "/riders/me/documents", `{"document":1,"side":2,"content_type":3,"data_base64":4}`},
		{"POST", "/riders/me/feedback", `{"message":123}`},
		{"POST", "/riders/me/stock/lot_mock01/quarantine", `{"reason":5}`},
		{"POST", "/riders/me/stock/lot_mock01/quarantine", `{"reason":"other","note":7}`},
		{"POST", "/orders/ord_mock0001/gate-check", `{"method":1,"value":2}`},
		{"POST", "/riders/me/shift-plan/reply", `{"reply":3}`},
		{"POST", "/riders/me/problems", `{"type":9}`},
		{"PATCH", "/riders/me", `{"name":5}`},
		{"PATCH", "/riders/me", `{"email":5}`},
		{"PATCH", "/riders/me", `{"area":{"name":1,"city":2,"lat":"x","lng":[]}}`},
	} {
		status, out, _ := appRequest(t, srv, tc.method, tc.path, asApp(true, "shift_plan_today"), tc.body)
		if status != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s %s %s: got %d %v", tc.method, tc.path, tc.body, status, out)
		}
	}
	for _, body := range []string{`{"battery_pct":50,"at":12345}`, `{"battery_pct":"50","at":"2026-11-12T06:00:00Z"}`, `{"battery_pct":50,"at":null}`} {
		status, out, _ := appRequest(t, srv, "POST", "/scooters/NZ-014/telemetry", map[string]string{"X-Telemetry-Key": "scooter-key-NZ-014"}, body)
		if status != 400 || out["code"] != "invalid_request" {
			t.Errorf("telemetry %s: got %d %v", body, status, out)
		}
	}
}

func TestEverySettingsProblemTypeIsAValidReportProblemType(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	_, out, _ := appRequest(t, srv, "GET", "/riders/me/settings", asApp(false, ""), "")
	types, _ := field(t, out, "settings", "problem_types").([]any)
	if len(types) == 0 {
		t.Fatal("no problem types offered")
	}
	for _, item := range types {
		code := item.(map[string]any)["code"].(string)
		status, reply, _ := appRequest(t, srv, "POST", "/riders/me/problems", asApp(true, ""), fmt.Sprintf(`{"type":%q}`, code))
		if status != 201 {
			t.Errorf("the settings offer %q, and reportProblem refuses it: %d %v", code, status, reply)
		}
	}
	if status, out, _ := appRequest(t, srv, "POST", "/riders/me/problems", asApp(true, ""), `{"type":"cold_food"}`); status != 400 || out["code"] != "invalid_request" {
		t.Errorf("a type outside the enum must be refused: %d %v", status, out)
	}
}

func TestMaskedCallIsNotAvailableUntilAProviderIsChosen(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	status, out, _ := appRequest(t, srv, "POST", "/orders/ord_mock0001/call", asApp(true, ""), "")
	if status != 409 || out["code"] != "call_not_available" || out["retryable"] != false {
		t.Errorf("the default: %d %v", status, out)
	}
	status, out, _ = appRequest(t, srv, "POST", "/orders/ord_mock0001/call", asApp(true, "masked_call_works"), "")
	if status != 200 || out["phone"] == nil || out["expires_at"] == nil {
		t.Errorf("the scenario shows the success shape: %d %v", status, out)
	}
}

func TestDocumentSizeLimitIsTheContractsLimit(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	send := func(n int) int {
		data := base64.StdEncoding.EncodeToString(make([]byte, n))
		body := `{"document":"aadhaar","side":"front","content_type":"image/png","data_base64":"` + data + `"}`
		status, _, _ := appRequest(t, srv, "POST", "/riders/me/documents", asApp(true, ""), body)
		return status
	}
	if got := len(base64.StdEncoding.EncodeToString(make([]byte, 700000))); got != 933336 {
		t.Fatalf("the contract's maxLength 933336 is not the base64 of 700000 bytes: %d", got)
	}
	if status := send(700000); status != 201 {
		t.Errorf("exactly 700000 bytes: %d", status)
	}
	if status := send(700001); status != 400 {
		t.Errorf("1 byte over: %d", status)
	}
}

func TestQuarantineRefusesWhatTheContractDeclares(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	status, out, _ := appRequest(t, srv, "POST", "/riders/me/stock/lot_mock01/quarantine", asApp(true, "lot_not_on_scooter"), `{"reason":"damaged"}`)
	if status != http.StatusConflict || out["code"] != "lot_not_on_scooter" {
		t.Errorf("lot_not_on_scooter: %d %v", status, out)
	}
	status, out, _ = appRequest(t, srv, "POST", "/riders/me/stock/lot_other/quarantine", asApp(true, "lot_of_another_rider"), `{"reason":"damaged"}`)
	if status != http.StatusNotFound || out["code"] != "not_found" {
		t.Errorf("another rider's lot: %d %v", status, out)
	}
}

func TestRiderSignInRefusesAMalformedPhoneAsInvalidPhone(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	for body, want := range map[string]string{
		`{"phone":"12345"}`: "invalid_phone", `{"phone":"+14155550123"}`: "invalid_phone", `{"phone":"+915876543210"}`: "invalid_phone",
		`{"phone":"9876543210"}`: "invalid_phone", `{"phone":42}`: "invalid_phone", `{"phone":""}`: "invalid_request", `{}`: "invalid_request",
	} {
		status, out, _ := appRequest(t, srv, "POST", "/auth/otp/request", nil, body)
		if status != 400 || out["code"] != want || out["retryable"] != false {
			t.Errorf("%s: got %d %v, want %s", body, status, out, want)
		}
	}
	if status, _, _ := appRequest(t, srv, "POST", "/auth/otp/request", nil, `{"phone":"+919876543210"}`); status != 200 {
		t.Errorf("a good phone: %d", status)
	}
	// The customer contract and mock keep what they had: no invalid_phone there.
	req, _ := http.NewRequest("POST", srv.URL+"/customer/v1/auth/otp/request", strings.NewReader(`{"phone":"12345"}`))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if strings.Contains(string(data), "invalid_phone") {
		t.Errorf("the customer mock must not change: %s", data)
	}
}
