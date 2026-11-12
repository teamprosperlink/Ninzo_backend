package mockserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Sample body for the operation added in #178 that the earlier tests do not cover.
func init() {
	if _, taken := sampleBodies["updateNotificationSettings"]; taken {
		panic("a sample body would replace another one: updateNotificationSettings")
	}
	sampleBodies["updateNotificationSettings"] = `{"offers_and_news":true}`
}

// appCall sends one request to an app's mock (prefix such as /customer/v1) and decodes the reply.
func appCall(t *testing.T, srv *httptest.Server, prefix, method, path, scenario, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+prefix+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	if method != "GET" {
		keyCount++
		req.Header.Set("Idempotency-Key", fmt.Sprintf("add-key-%04d", keyCount))
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

func TestMenuItemsShowTheVegFlag(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	status, out := appCall(t, srv, "/customer/v1", "GET", "/menu/items", "", "")
	items, _ := out["items"].([]any)
	if status != 200 || len(items) < 2 {
		t.Fatalf("got %d %v", status, out)
	}
	veg := map[string]bool{}
	for _, raw := range items {
		item := raw.(map[string]any)
		flag, isBool := item["is_veg"].(bool)
		if !isBool {
			t.Fatalf("item %v has no boolean is_veg", item["id"])
		}
		veg[item["id"].(string)] = flag
	}
	if veg["item_chai"] != true || veg["item_burger"] != false {
		t.Errorf("the sample items should show one veg and one non-veg item: %v", veg)
	}
}

func TestOrderRiderShowsTheScooterPlate(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	_, out := appCall(t, srv, "/customer/v1", "GET", "/orders/ord_mock0001", "arrived", "")
	rider, _ := out["order"].(map[string]any)["rider"].(map[string]any)
	if rider["first_name"] != "Ravi" || rider["scooter_plate"] != "KA01AB1234" {
		t.Errorf("rider = %v", rider)
	}
}

func TestNotificationSettings(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	prefix := "/customer/v1"
	status, out := appCall(t, srv, prefix, "GET", "/me/notification-settings", "", "")
	settings, _ := out["settings"].(map[string]any)
	if status != 200 || settings["order_updates"] != true || out["server_time"] == nil {
		t.Fatalf("get: %d %v", status, out)
	}
	status, out = appCall(t, srv, prefix, "PATCH", "/me/notification-settings", "", `{"offers_and_news":true,"support_replies":false}`)
	settings, _ = out["settings"].(map[string]any)
	if status != 200 || settings["offers_and_news"] != true || settings["support_replies"] != false || settings["order_updates"] != true {
		t.Errorf("patch: %d %v", status, out)
	}
	for _, body := range []string{`{}`, `{"order_updates":true}`, `{"order_updates":false}`, `{"offers_and_news":"yes"}`, `{"unknown":true}`} {
		status, out = appCall(t, srv, prefix, "PATCH", "/me/notification-settings", "", body)
		if status != 400 || out["code"] != "invalid_request" || out["server_time"] == nil {
			t.Errorf("%s: got %d %v, want a 400 invalid_request", body, status, out)
		}
	}
}

func TestSavedGatesCarryLandmarkAndType(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	prefix := "/customer/v1"
	_, out := appCall(t, srv, prefix, "GET", "/gates", "", "")
	gate := out["gates"].([]any)[0].(map[string]any)
	if gate["landmark"] != "Opposite the pharmacy" || gate["gate_type"] != "main_gate" {
		t.Errorf("sample gate = %v", gate)
	}
	status, out := appCall(t, srv, prefix, "POST", "/gates", "", `{"label":"Tower","lat":12.9,"lng":77.5,"landmark":"Next to the lift","gate_type":"tower"}`)
	gate, _ = out["gate"].(map[string]any)
	if status != 201 || gate["landmark"] != "Next to the lift" || gate["gate_type"] != "tower" {
		t.Errorf("create: %d %v", status, out)
	}
	status, out = appCall(t, srv, prefix, "POST", "/gates", "", `{"label":"Plain","lat":12.9,"lng":77.5}`)
	gate, _ = out["gate"].(map[string]any)
	if status != 201 || gate["landmark"] != nil || gate["gate_type"] != nil {
		t.Errorf("a gate with neither sends null for both: %d %v", status, out)
	}
	tooLong := strings.Repeat("a", 81)
	for _, body := range []string{
		`{"label":"G","lat":12.9,"lng":77.5,"landmark":"` + tooLong + `"}`,
		`{"label":"G","lat":12.9,"lng":77.5,"landmark":""}`,
		`{"label":"G","lat":12.9,"lng":77.5,"gate_type":"Main Gate"}`,
		`{"label":"G","lat":12.9,"lng":77.5,"gate_type":7}`,
	} {
		status, out = appCall(t, srv, prefix, "POST", "/gates", "", body)
		if status != 400 || out["code"] != "invalid_request" {
			t.Errorf("%.80s: got %d %v, want 400", body, status, out)
		}
	}
	// 80 characters is allowed, and an unknown gate_type word is allowed (the list is open).
	ok80 := strings.Repeat("a", 80)
	status, _ = appCall(t, srv, prefix, "POST", "/gates", "", `{"label":"G","lat":12.9,"lng":77.5,"landmark":"`+ok80+`","gate_type":"podium"}`)
	if status != 201 {
		t.Errorf("an 80 character landmark and a new gate_type should be accepted, got %d", status)
	}
}

func TestConsoleMenuCarriesIsVeg(t *testing.T) {
	srv := httptest.NewServer(New().Handler())
	defer srv.Close()
	prefix := "/console/v1"
	_, out := appCall(t, srv, prefix, "GET", "/admin/menu/items", "", "")
	for _, raw := range out["items"].([]any) {
		if _, isBool := raw.(map[string]any)["is_veg"].(bool); !isBool {
			t.Errorf("an admin item has no boolean is_veg: %v", raw)
		}
	}
	status, out := appCall(t, srv, prefix, "POST", "/admin/menu/items", "", `{"name":"Paneer wrap","price_paise":9900,"is_veg":true}`)
	if status != 201 || out["item"].(map[string]any)["is_veg"] != true {
		t.Errorf("create with is_veg true: %d %v", status, out)
	}
	status, out = appCall(t, srv, prefix, "POST", "/admin/menu/items", "", `{"name":"Wrap","price_paise":9900}`)
	if status != 201 || out["item"].(map[string]any)["is_veg"] != false {
		t.Errorf("create without is_veg is false: %d %v", status, out)
	}
	status, out = appCall(t, srv, prefix, "PATCH", "/admin/menu/items/item_burger", "", `{"is_veg":true,"reason":"Checked with the kitchen"}`)
	if status != 200 || out["item"].(map[string]any)["is_veg"] != true {
		t.Errorf("update is_veg: %d %v", status, out)
	}
	status, out = appCall(t, srv, prefix, "POST", "/admin/menu/items", "", `{"name":"Wrap","price_paise":9900,"is_veg":"yes"}`)
	if status != 400 {
		t.Errorf("a non-boolean is_veg is a 400, got %d %v", status, out)
	}
}
