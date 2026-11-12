package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/catalog"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

// customerMenu reads the customer's menu through the real catalog handler, without sign-in.
func (f *fixture) customerMenu() map[string]map[string]any {
	f.t.Helper()
	svc := &catalog.Service{Pool: f.pool, Log: quiet}
	srv := httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) { svc.MountCustomer(r) }))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/menu/items")
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != 200 {
		f.t.Fatalf("%d %v", resp.StatusCode, err)
	}
	byName := map[string]map[string]any{}
	for _, it := range out.Items {
		byName[it["name"].(string)] = it
	}
	return byName
}

// TestTheVegFlagGoesFromTheConsoleToTheCustomerMenu follows is_veg end to end (issue 178).
func TestTheVegFlagGoesFromTheConsoleToTheCustomerMenu(t *testing.T) {
	f := newFixture(t)
	_, temp := f.user("veg.ops", "ops")
	ops := f.signIn("veg.ops", temp)
	// Left out on create: false.
	code, out := f.call("POST", "/admin/menu/items", ops, `{"name":"Chicken Roll","price_paise":12900}`)
	if code != 201 || out["item"].(map[string]any)["is_veg"] != false {
		t.Fatalf("a new item is not vegetarian until set: %d %v", code, out)
	}
	chicken := out["item"].(map[string]any)["id"].(string)
	// Sent true on create.
	code, out = f.call("POST", "/admin/menu/items", ops, `{"name":"Paneer Roll","price_paise":11900,"is_veg":true}`)
	if code != 201 || out["item"].(map[string]any)["is_veg"] != true {
		t.Fatalf("%d %v", code, out)
	}
	paneer := out["item"].(map[string]any)["id"].(string)
	if n := f.count("SELECT count(*) FROM audit_events WHERE entity_id = $1 AND action = 'menu.created' AND after @> '{\"is_veg\": true}'", paneer); n != 1 {
		t.Error("the create audit row does not say the item is vegetarian")
	}
	// The admin list returns it.
	_, list := f.call("GET", "/admin/menu/items", ops, "")
	flags := map[string]any{}
	for _, it := range list["items"].([]any) {
		flags[it.(map[string]any)["name"].(string)] = it.(map[string]any)["is_veg"]
	}
	if flags["Chicken Roll"] != false || flags["Paneer Roll"] != true {
		t.Errorf("the admin list: %v", flags)
	}
	// Show both, then the customer menu carries the flags.
	for _, id := range []string{chicken, paneer} {
		if code, _ := f.call("PATCH", "/admin/menu/items/"+id, ops, `{"status":"active","reason":"show it"}`); code != 200 {
			t.Fatal(code)
		}
	}
	menu := f.customerMenu()
	if menu["Chicken Roll"]["is_veg"] != false || menu["Paneer Roll"]["is_veg"] != true {
		t.Errorf("the customer menu: %v", menu)
	}
	// A change to the flag alone keeps everything else, is returned, and is audited with before and after.
	code, out = f.call("PATCH", "/admin/menu/items/"+chicken, ops, `{"is_veg":true,"reason":"it is a soya roll"}`)
	item := out["item"].(map[string]any)
	if code != 200 || item["is_veg"] != true || item["status"] != "active" || item["price"].(map[string]any)["amount_minor"] != float64(12900) {
		t.Fatalf("%d %v", code, out)
	}
	if n := f.count(`SELECT count(*) FROM audit_events WHERE entity_id = $1 AND action = 'menu.updated' AND reason = 'it is a soya roll'
		AND before @> '{"is_veg": false}' AND after @> '{"is_veg": true}'`, chicken); n != 1 {
		t.Error("the flag change is not audited with its old and new value")
	}
	if f.customerMenu()["Chicken Roll"]["is_veg"] != true {
		t.Error("the customer menu did not follow the change")
	}
	// Another change leaves the flag alone.
	if code, out := f.call("PATCH", "/admin/menu/items/"+chicken, ops, `{"price_paise":13900,"reason":"price"}`); code != 200 || out["item"].(map[string]any)["is_veg"] != true {
		t.Errorf("a price change reset the flag: %d %v", code, out)
	}
	// And back to false.
	if code, out := f.call("PATCH", "/admin/menu/items/"+chicken, ops, `{"is_veg":false,"reason":"checked again"}`); code != 200 || out["item"].(map[string]any)["is_veg"] != false {
		t.Errorf("%d %v", code, out)
	}
	// Wrong types, and null, are refused.
	for _, bad := range []string{`{"is_veg":null,"reason":"x1x"}`, `{"is_veg":"yes","reason":"x1x"}`, `{"is_veg":1,"reason":"x1x"}`} {
		if code, _ := f.call("PATCH", "/admin/menu/items/"+chicken, ops, bad); code != 400 {
			t.Errorf("%s: %d", bad, code)
		}
	}
	if code, _ := f.call("POST", "/admin/menu/items", ops, `{"name":"Bad Roll","price_paise":9900,"is_veg":"yes"}`); code != 400 {
		t.Errorf("a text flag on create: %d", code)
	}
}
