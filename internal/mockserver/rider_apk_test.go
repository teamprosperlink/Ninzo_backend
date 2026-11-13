package mockserver

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Sample bodies for the operations added for the finished rider app (Issue 235).
func init() {
	for id, body := range map[string]string{
		"takeBreak":              `{"length_s":900}`,
		"setPlannedEnd":          `{"planned_end_at":"2099-01-01T12:00:00Z"}`,
		"uploadRiderDocument":    `{"document":"aadhaar","side":"front","content_type":"image/jpeg","data_base64":"/9j/4AAQ"}`,
		"sendRiderFeedback":      `{"message":"The map is hard to read at night."}`,
		"reportScooterTelemetry": `{"battery_pct":72,"at":"2026-11-12T06:00:00Z"}`,
		// quarantineLot has a body in partner_test.go that the rider operation accepts too.
		"checkGate":        `{"method":"code","value":"GATE-4821"}`,
		"replyToShiftPlan": `{"reply":"coming"}`,
	} {
		if _, taken := sampleBodies[id]; taken {
			panic("a rider app sample body would replace another one: " + id)
		}
		sampleBodies[id] = body
	}
}

// appRequest sends one request the way the rider app does: a bearer token, an Idempotency-Key only when asked, and JSON.
func appRequest(t *testing.T, srv *httptest.Server, method, path string, headers map[string]string, body string) (int, map[string]any, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+"/rider/v1"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil && resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("%s %s: not JSON: %v", method, path, err)
	}
	return resp.StatusCode, out, resp.Header
}

var appKeys int

// asApp is the app's header set for a write with a key, and for a read.
func asApp(write bool, scenario string) map[string]string {
	h := map[string]string{"Authorization": "Bearer mock-access-token"}
	if write {
		appKeys++
		h["Idempotency-Key"] = fmt.Sprintf("%032x", appKeys)
	}
	if scenario != "" {
		h[scenarioHeader] = scenario
	}
	return h
}

func field(t *testing.T, m map[string]any, path ...string) any {
	t.Helper()
	var cur any = m
	for _, p := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("%v: %q is not inside an object", m, p)
		}
		cur = obj[p]
	}
	return cur
}

// TestEveryRiderAppCallIsServed sends each call the rider app makes (rider_api.dart) as the app sends it,
// and checks the reply carries the fields the app reads.
func TestEveryRiderAppCallIsServed(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	pngBytes := base64.StdEncoding.EncodeToString([]byte("not really a photo"))
	type appCase struct {
		name, method, path, body string
		write                    bool
		status                   int
		reads                    [][]string // each is a path of fields the app reads
	}
	calls := []appCase{
		{"requestOtp", "POST", "/auth/otp/request", `{"phone":"+919876543210"}`, false, 200, [][]string{{"request_id"}}},
		{"verifyOtp", "POST", "/auth/otp/verify", `{"request_id":"otp_mock01","otp":"123456"}`, false, 200, [][]string{{"access_token"}, {"refresh_token"}, {"rider_id"}}},
		{"refreshToken", "POST", "/auth/refresh", `{"refresh_token":"mock-refresh-token"}`, false, 200, [][]string{{"access_token"}, {"refresh_token"}}},
		{"signOut", "POST", "/auth/signout", "", false, 200, nil},
		{"getRider", "GET", "/riders/me", "", false, 200, [][]string{{"rider", "id"}, {"rider", "name"}, {"rider", "status"}, {"rider", "zone_id"}, {"rider", "phone"}, {"rider", "email"}, {"rider", "area"}, {"rider", "approved_at"}}},
		{"updateRider app shape", "PATCH", "/riders/me", `{"name":"Ravi K","email":"ravi@example.com","area":{"name":"Indiranagar","city":"Bengaluru","lat":12.97,"lng":77.64}}`, true, 200, [][]string{{"rider", "email"}, {"rider", "area", "name"}}},
		{"updateRider our shape", "PATCH", "/riders/me", `{"name":"Ravi K","preferred_area":"Indiranagar"}`, true, 200, [][]string{{"rider", "preferred_area"}}},
		{"listRiderNotifications", "GET", "/riders/me/notifications", "", false, 200, [][]string{{"notifications"}}},
		{"getRiderSettings", "GET", "/riders/me/settings", "", false, 200, [][]string{{"settings", "timer_length_s"}, {"settings", "break_options_s"}, {"settings", "checklist"}, {"settings", "cancel_pay_cap", "amount_minor"}, {"settings", "problem_types"}, {"settings", "skip_reasons"}}},
		{"getRiderHelp", "GET", "/riders/me/help", "", false, 200, [][]string{{"faq"}}},
		{"sendRiderFeedback", "POST", "/riders/me/feedback", `{"message":"Nice app"}`, true, 201, [][]string{{"feedback_id"}}},
		{"checkGate (no Idempotency-Key)", "POST", "/orders/ord_mock0001/gate-check", `{"method":"code","value":"GATE-4821"}`, false, 200, [][]string{{"checked"}, {"matched"}}},
		{"getRiderState", "GET", "/riders/me/state", "", false, 200, [][]string{{"seq"}, {"shift", "break"}, {"shift", "online_at"}, {"shift", "planned_end_at"}, {"shift", "device_id"}, {"offers"}}},
		{"registerRiderDevice", "POST", "/riders/me/devices", `{"platform":"android","push_token":"t"}`, true, 201, [][]string{{"device_id"}}},
		{"removeRiderDevice", "DELETE", "/riders/me/devices/dev_mock01", "", true, 200, nil},
		{"getVerification", "GET", "/riders/me/verification", "", false, 200, [][]string{{"status"}}},
		{"startVerification", "POST", "/riders/me/verification/start", `{"method":"manual_upload"}`, true, 200, nil},
		{"getEarnings", "GET", "/riders/me/earnings?date=2026-11-12", "", false, 200, [][]string{{"order_earnings"}}},
		{"reportProblem", "POST", "/riders/me/problems", `{"type":"gate_closed","order_id":"ord_mock0001"}`, true, 201, nil},
		{"sendEventBatch", "POST", "/riders/me/events", `{"events":[{"event_id":"evt_mock0001","type":"location","payload":{"lat":1,"lng":2},"device_time":"2026-11-12T10:00:00Z"}]}`, false, 200, [][]string{{"accepted_ids"}}},
		{"locationPing", "POST", "/riders/me/location", `{"lat":12.9,"lng":77.5,"speed_mps":3,"accuracy_m":5,"device_time":"2026-11-12T10:00:00Z"}`, false, 202, [][]string{{"next_ping_s"}}},
		{"listNodes", "GET", "/nodes?zone_id=zone_mock01", "", false, 200, [][]string{{"nodes"}, {"assigned_node_id"}, {"relocation"}}},
		{"startShift with checklist", "POST", "/riders/me/shifts/start", `{"node_id":"node_mock01","device_id":"dev_mock01","checklist":["helmet","location"]}`, true, 200, [][]string{{"shift", "id"}}},
		{"endShift", "POST", "/riders/me/shifts/end", "", true, 200, nil},
		{"goOnline", "POST", "/riders/me/shifts/online", "", true, 200, [][]string{{"shift", "online_at"}}},
		{"takeBreak", "POST", "/riders/me/shifts/break", `{"length_s":600}`, true, 200, [][]string{{"shift", "break", "started_at"}, {"shift", "break", "ends_at"}, {"shift", "break_ends_at"}}},
		{"endBreak alias", "POST", "/riders/me/shifts/break/end", "", true, 200, [][]string{{"shift", "break"}}},
		{"setPlannedEnd", "POST", "/riders/me/shifts/planned-end", `{"planned_end_at":"2099-01-01T12:00:00Z"}`, true, 200, [][]string{{"shift", "planned_end_at"}}},
		{"listShifts days", "GET", "/riders/me/shifts?days=7", "", false, 200, [][]string{{"shifts"}}},
		{"listRiderDocuments", "GET", "/riders/me/documents", "", false, 200, [][]string{{"documents"}}},
		{"uploadRiderDocument", "POST", "/riders/me/documents", `{"document":"vehicleRc","side":"front","content_type":"image/png","data_base64":"` + pngBytes + `"}`, true, 201, [][]string{{"document", "size_bytes"}}},
		{"acceptRelocation", "POST", "/riders/me/relocations/rel_mock01/accept", "", true, 200, nil},
		{"getShiftPlan", "GET", "/riders/me/shift-plan", "", false, 200, [][]string{{"plan"}}},
		{"getStock", "GET", "/riders/me/stock", "", false, 200, [][]string{{"lots"}}},
		{"scanStock", "POST", "/riders/me/stock/scan", `{"lot_code":"L1","scanned_at":"2026-11-12T10:00:00Z"}`, true, 200, [][]string{{"unit", "lot_id"}}},
		{"quarantineLot", "POST", "/riders/me/stock/lot_mock01/quarantine", `{"reason":"temperature"}`, true, 200, [][]string{{"lot_id"}}},
		{"listOffers", "GET", "/riders/me/offers", "", false, 200, [][]string{{"offers"}}},
		{"acceptOffer", "POST", "/offers/off_mock01/accept", `{"fencing_token":"ft_mock01"}`, true, 200, [][]string{{"order", "total", "amount_minor"}, {"order", "payment_method"}}},
		{"declineOffer", "POST", "/offers/off_mock01/decline", `{"fencing_token":"ft_mock01"}`, true, 200, nil},
		{"markReady", "POST", "/offers/off_mock01/ready", `{"fencing_token":"ft_mock01"}`, true, 200, [][]string{{"order", "state"}}},
		{"getRiderOrder", "GET", "/orders/ord_mock0001", "", false, 200, [][]string{{"order", "total"}, {"order", "payment_method"}}},
		{"getRoute", "GET", "/orders/ord_mock0001/route", "", false, 200, [][]string{{"polyline"}}},
		{"handOver", "POST", "/orders/ord_mock0001/handover", `{"client_time":"2026-11-12T10:00:00Z","otp":"4821","quantities":[{"item_id":"item_burger","qty":1}]}`, true, 200, [][]string{{"order", "handed_over_at"}}},
		{"createRestock", "POST", "/restock", `{"hub_id":"hub_mock01","items":[{"item_id":"item_burger","qty":4}]}`, true, 201, [][]string{{"request", "id"}}},
		{"getRestockOptions", "GET", "/restock/options", "", false, 200, [][]string{{"hubs"}, {"items"}}},
		{"listRestockSuggestions", "GET", "/riders/me/restock-suggestions", "", false, 200, [][]string{{"suggestions"}}},
		{"getRestock", "GET", "/restock/rst_mock01", "", false, 200, [][]string{{"request", "state"}}},
		{"confirmRestock", "POST", "/restock/rst_mock01/confirm", `{}`, true, 200, nil},
		{"declineRestock", "POST", "/restock/rst_mock01/decline", `{}`, true, 200, nil},
		{"respondToShortfall", "POST", "/restock/rst_mock01/shortfall-response", `{"accept":true}`, true, 200, nil},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			status, out, _ := appRequest(t, srv, tc.method, tc.path, asApp(tc.write, ""), tc.body)
			if status != tc.status {
				t.Fatalf("got %d %v, want %d", status, out, tc.status)
			}
			if out["server_time"] == nil {
				t.Errorf("no server_time in %v", out)
			}
			for _, path := range tc.reads {
				if !present(out, path...) || (field(t, out, path...) == nil && !nullable[strings.Join(path, ".")]) {
					t.Errorf("the app reads %v, and it is missing (or null where null is not allowed) in %v", path, out)
				}
			}
		})
	}
}

// nullable lists fields the app reads that may be null in the default sample.
var nullable = map[string]bool{"rider.email": true, "rider.area": true, "shift.break": true, "shift.planned_end_at": true,
	"rider.approved_at": true, "assigned_node_id": true, "relocation": true, "plan": true}

func TestUpdateRiderAcceptsBothShapes(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	area := `{"name":"Indiranagar","city":"Bengaluru","lat":12.97,"lng":77.64}`
	status, out, _ := appRequest(t, srv, "PATCH", "/riders/me", asApp(true, ""), `{"area":`+area+`,"email":"ravi@example.com"}`)
	if status != 200 || field(t, out, "rider", "preferred_area") != "Indiranagar" || field(t, out, "rider", "email") != "ravi@example.com" {
		t.Errorf("area and email: %d %v", status, out)
	}
	status, out, _ = appRequest(t, srv, "PATCH", "/riders/me", asApp(true, ""), `{"email":null,"area":null}`)
	if status != 200 || field(t, out, "rider", "email") != nil || field(t, out, "rider", "area") != nil {
		t.Errorf("clearing: %d %v", status, out)
	}
	for _, bad := range []string{
		`{}`, `{"email":"not an address"}`, `{"area":{"name":"","city":"x","lat":1,"lng":2}}`, `{"area":{"name":"x","city":"x","lat":91,"lng":2}}`,
		`{"area":"Indiranagar"}`, `{"preferred_area":"x","area":` + area + `}`,
	} {
		if status, out, _ := appRequest(t, srv, "PATCH", "/riders/me", asApp(true, ""), bad); status != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: got %d %v", bad, status, out)
		}
	}
}

func TestListShiftsTakesDaysOrOurFilters(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	status, out, _ := appRequest(t, srv, "GET", "/riders/me/shifts?days=7", asApp(false, ""), "")
	shifts, _ := out["shifts"].([]any)
	if status != 200 || len(shifts) != 3 || out["next_cursor"] != nil {
		t.Fatalf("days: %d %v", status, out)
	}
	first := shifts[0].(map[string]any)
	for _, f := range []string{"id", "node_id", "started_at", "ended_at", "online_at", "planned_end_at", "orders", "portions", "avg_delivery_s"} {
		if _, ok := first[f]; !ok {
			t.Errorf("a shift entry has no %s: %v", f, first)
		}
	}
	for _, q := range []string{"days=0", "days=32", "days=x", "days=7&from=2026-11-01", "days=7&to=2026-11-01"} {
		if status, out, _ := appRequest(t, srv, "GET", "/riders/me/shifts?"+q, asApp(false, ""), ""); status != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: got %d %v", q, status, out)
		}
	}
	if status, out, _ := appRequest(t, srv, "GET", "/riders/me/shifts?from=2026-11-01", asApp(false, ""), ""); status != 200 || out["next_cursor"] != "page_2" {
		t.Errorf("our filters must still page: %d %v", status, out)
	}
}

func TestBreakIsCappedByTheConsole(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	// 3600 s asked, 900 s allowed (max_break_s in the sample settings).
	status, out, _ := appRequest(t, srv, "POST", "/riders/me/shifts/break", asApp(true, ""), `{"length_s":3600}`)
	if status != 200 {
		t.Fatalf("%d %v", status, out)
	}
	started, _ := time.Parse(time.RFC3339Nano, field(t, out, "shift", "break", "started_at").(string))
	ends, _ := time.Parse(time.RFC3339Nano, field(t, out, "shift", "break", "ends_at").(string))
	if got := ends.Sub(started); got != mockMaxBreak {
		t.Errorf("the break lasts %v, want the console's %v", got, mockMaxBreak)
	}
	if field(t, out, "shift", "break_ends_at") != field(t, out, "shift", "break", "ends_at") {
		t.Errorf("the two forms of the break disagree: %v", out)
	}
	for _, bad := range []string{`{}`, `{"length_s":59}`, `{"length_s":3601}`, `{"length_s":"900"}`, `{"length_s":90.5}`} {
		if status, _, _ := appRequest(t, srv, "POST", "/riders/me/shifts/break", asApp(true, ""), bad); status != 400 {
			t.Errorf("%s: got %d", bad, status)
		}
	}
	for _, s := range []string{"no_open_shift", "order_in_progress", "offer_open"} {
		if status, out, _ := appRequest(t, srv, "POST", "/riders/me/shifts/break", asApp(true, s), `{"length_s":600}`); status != 409 || out["code"] != s {
			t.Errorf("%s: got %d %v", s, status, out)
		}
	}
	// The aliases and our paths answer the same shift.
	_, ours, _ := appRequest(t, srv, "POST", "/riders/me/breaks/end", asApp(true, ""), "")
	_, theirs, _ := appRequest(t, srv, "POST", "/riders/me/shifts/break/end", asApp(true, ""), "")
	if fmt.Sprint(field(t, ours, "shift", "id")) != fmt.Sprint(field(t, theirs, "shift", "id")) {
		t.Errorf("the alias answers another shift: %v and %v", ours, theirs)
	}
}

func TestPlannedEndIsRecordedNotEnforced(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	soon := time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339)
	status, out, _ := appRequest(t, srv, "POST", "/riders/me/shifts/planned-end", asApp(true, ""), `{"planned_end_at":"`+soon+`"}`)
	if status != 200 || field(t, out, "shift", "ended_at") != nil || field(t, out, "shift", "planned_end_at") == nil {
		t.Errorf("the shift stays open with a planned end: %d %v", status, out)
	}
	for _, bad := range []string{`{}`, `{"planned_end_at":"2001-01-01T00:00:00Z"}`, `{"planned_end_at":"tomorrow"}`} {
		if status, _, _ := appRequest(t, srv, "POST", "/riders/me/shifts/planned-end", asApp(true, ""), bad); status != 400 {
			t.Errorf("%s: got %d", bad, status)
		}
	}
	if status, out, _ := appRequest(t, srv, "POST", "/riders/me/shifts/planned-end", asApp(true, "no_open_shift"), `{"planned_end_at":"`+soon+`"}`); status != 409 || out["code"] != "no_open_shift" {
		t.Errorf("no open shift: %d %v", status, out)
	}
}

func TestShiftPlanIsEmptyUnlessOperationsMadeOne(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	if _, out, _ := appRequest(t, srv, "GET", "/riders/me/shift-plan", asApp(false, ""), ""); out["plan"] != nil {
		t.Errorf("shifts are open-ended, so the usual day has no plan: %v", out)
	}
	_, out, _ := appRequest(t, srv, "GET", "/riders/me/shift-plan", asApp(false, "shift_plan_today"), "")
	if field(t, out, "plan", "stock_reply") != "pending" {
		t.Errorf("a plan operations made: %v", out)
	}
	status, out, _ := appRequest(t, srv, "POST", "/riders/me/shift-plan/reply", asApp(true, "shift_plan_today"), `{"reply":"not_today"}`)
	if status != 200 || field(t, out, "plan", "stock_reply") != "not_today" || field(t, out, "plan", "replied_at") == nil {
		t.Errorf("the reply is recorded: %d %v", status, out)
	}
	if status, out, _ := appRequest(t, srv, "POST", "/riders/me/shift-plan/reply", asApp(true, ""), `{"reply":"coming"}`); status != 404 || out["code"] != "not_found" {
		t.Errorf("no plan today: %d %v", status, out)
	}
	if status, out, _ := appRequest(t, srv, "POST", "/riders/me/shift-plan/reply", asApp(true, "shift_plan_closed"), `{"reply":"coming"}`); status != 409 || out["code"] != "shift_plan_closed" {
		t.Errorf("packing started: %d %v", status, out)
	}
	if status, _, _ := appRequest(t, srv, "POST", "/riders/me/shift-plan/reply", asApp(true, "shift_plan_today"), `{"reply":"maybe"}`); status != 400 {
		t.Errorf("a reply outside coming and not_today: %d", status)
	}
}

func TestTelemetryUsesAKeyForEachScooter(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	reading := `{"battery_pct":72,"at":"` + time.Now().Add(-time.Minute).UTC().Format(time.RFC3339) + `"}`
	send := func(code, key, body string) (int, map[string]any) {
		h := map[string]string{}
		if key != "" {
			h["X-Telemetry-Key"] = key
		}
		status, out, _ := appRequest(t, srv, "POST", "/scooters/"+code+"/telemetry", h, body)
		return status, out
	}
	if status, out := send("NZ-014", "scooter-key-NZ-014", reading); status != 202 || out["server_time"] == nil {
		t.Errorf("the scooter's own key: %d %v", status, out)
	}
	for name, tc := range map[string]struct{ code, key string }{
		"no key":                    {"NZ-014", ""},
		"another scooter's key":     {"NZ-014", "scooter-key-NZ-015"},
		"a key shared by the fleet": {"NZ-014", "fleet-key"},
	} {
		if status, out := send(tc.code, tc.key, reading); status != 401 || out["code"] != "unauthorized" {
			t.Errorf("%s: %d %v", name, status, out)
		}
	}
	if status, out := send("NZ-099", "scooter-key-NZ-099", reading); status != 404 || out["code"] != "not_found" {
		t.Errorf("an unknown scooter: %d %v", status, out)
	}
	for _, bad := range []string{`{}`, `{"battery_pct":101,"at":"2026-11-12T06:00:00Z"}`, `{"battery_pct":-1,"at":"2026-11-12T06:00:00Z"}`,
		`{"battery_pct":50,"at":"2999-01-01T00:00:00Z"}`, `{"battery_pct":50,"at":"soon"}`} {
		if status, _ := send("NZ-014", "scooter-key-NZ-014", bad); status != 400 {
			t.Errorf("%s: got %d", bad, status)
		}
	}
}

func TestDocumentUploadRules(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	upload := func(scenario, doc, side, contentType, data string) (int, map[string]any) {
		body := fmt.Sprintf(`{"document":%q,"side":%q,"content_type":%q,"data_base64":%q}`, doc, side, contentType, data)
		status, out, _ := appRequest(t, srv, "POST", "/riders/me/documents", asApp(true, scenario), body)
		return status, out
	}
	photo := base64.StdEncoding.EncodeToString([]byte("a photo"))
	if status, out := upload("", "selfie", "photo", "image/webp", photo); status != 201 || field(t, out, "document", "size_bytes") != float64(len("a photo")) {
		t.Errorf("a good photo: %d %v", status, out)
	}
	big := base64.StdEncoding.EncodeToString(make([]byte, maxDocumentBytes+1))
	for name, tc := range map[string]struct{ doc, side, ct, data string }{
		"too large":         {"aadhaar", "front", "image/jpeg", big},
		"not base64":        {"aadhaar", "front", "image/jpeg", "%%%%"},
		"unknown document":  {"passport", "front", "image/jpeg", photo},
		"unknown side":      {"aadhaar", "left", "image/jpeg", photo},
		"not an image type": {"aadhaar", "front", "application/pdf", photo},
	} {
		if status, out := upload("", tc.doc, tc.side, tc.ct, tc.data); status != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", name, status, out)
		}
	}
	if status, out := upload("already_verified", "aadhaar", "front", "image/jpeg", photo); status != 409 || out["code"] != "already_verified" {
		t.Errorf("already verified: %d %v", status, out)
	}
}

func TestGateCheckNeverDecidesArrival(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	check := func(scenario, body string) (int, map[string]any) {
		// No Idempotency-Key: the app sends none, and it changes nothing.
		status, out, _ := appRequest(t, srv, "POST", "/orders/ord_mock0001/gate-check", asApp(false, scenario), body)
		return status, out
	}
	if status, out := check("", `{"method":"code","value":"GATE-4821"}`); status != 200 || out["matched"] != true || out["checked"] != true {
		t.Errorf("the right code: %d %v", status, out)
	}
	if status, out := check("", `{"method":"qr","value":"GATE-0000"}`); status != 200 || out["matched"] != false {
		t.Errorf("a wrong code is an answer, not an error: %d %v", status, out)
	}
	if status, out := check("gate_code_wrong", `{"method":"code","value":"GATE-4821"}`); status != 200 || out["matched"] != false {
		t.Errorf("the scenario: %d %v", status, out)
	}
	for _, bad := range []string{`{}`, `{"method":"nfc","value":"x"}`, `{"method":"code","value":"` + strings.Repeat("x", 65) + `"}`} {
		if status, _ := check("", bad); status != 400 {
			t.Errorf("%s: got %d", bad, status)
		}
	}
	// Our own path for arrival does not exist, and the app has no way to report arrival either.
	if status, _, _ := appRequest(t, srv, "POST", "/orders/ord_mock0001/arrived", asApp(true, ""), "{}"); status != 404 && status != 405 {
		t.Errorf("there must be no arrive call: %d", status)
	}
}

func TestQuarantineRules(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	for _, s := range []string{"no_open_shift", "lot_reserved"} {
		if status, out, _ := appRequest(t, srv, "POST", "/riders/me/stock/lot_mock01/quarantine", asApp(true, s), `{"reason":"damaged"}`); status != 409 || out["code"] != s {
			t.Errorf("%s: %d %v", s, status, out)
		}
	}
	for _, bad := range []string{`{}`, `{"reason":"smelly"}`, `{"reason":"other","note":"` + strings.Repeat("n", 501) + `"}`} {
		if status, _, _ := appRequest(t, srv, "POST", "/riders/me/stock/lot_mock01/quarantine", asApp(true, ""), bad); status != 400 {
			t.Errorf("%s: got %d", bad, status)
		}
	}
}

func TestSignInPauseCarriesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	status, out, header := appRequest(t, srv, "POST", "/auth/otp/verify", map[string]string{scenarioHeader: "sign_in_paused"}, `{"request_id":"otp_mock01","otp":"000000"}`)
	if status != 429 || out["code"] != "otp_attempts_exceeded" || out["retryable"] != true || header.Get("Retry-After") != "30" {
		t.Errorf("%d %v retry-after %q", status, out, header.Get("Retry-After"))
	}
}

func TestOffersAndOrdersCarryWhatThePhoneReads(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	_, out, _ := appRequest(t, srv, "GET", "/riders/me/offers", asApp(false, ""), "")
	offer := out["offers"].([]any)[0].(map[string]any)
	if field(t, offer, "total", "amount_minor") != float64(24900) || offer["payment_method"] != "prepaid" {
		t.Errorf("offer: %v", offer)
	}
	_, out, _ = appRequest(t, srv, "GET", "/restock/options", asApp(false, ""), "")
	if hub := out["hubs"].([]any)[0].(map[string]any); !strings.HasPrefix(fmt.Sprint(hub["manager_phone"]), "+91") {
		t.Errorf("the hub needs a manager_phone for Call hub manager: %v", hub)
	}
}

// TestRiderAppNeverGetsTheCallBridge protects the owner's rule: no provider call bridge with a key in its URL.
func TestRiderAppNeverGetsTheCallBridge(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	if status, _, _ := appRequest(t, srv, "GET", "/telephony/connect?key=k&from=%2B911", map[string]string{}, ""); status != 404 && status != 405 {
		t.Errorf("the provider's call bridge must not exist here: %d", status)
	}
	status, out, _ := appRequest(t, srv, "POST", "/orders/ord_mock0001/call", asApp(true, ""), "")
	if status != 409 || out["code"] != "call_not_available" {
		t.Errorf("masked calls stay call_not_available until a provider is chosen: %d %v", status, out)
	}
}
