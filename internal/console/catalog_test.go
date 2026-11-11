package console

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
)

func TestTheMenuAndItsPrices(t *testing.T) {
	f := newFixture(t)
	_, temp := f.user("menu.ops", "ops")
	ops := f.signIn("menu.ops", temp)
	code, out := f.call("POST", "/admin/menu/items", ops, `{"name":"Masala Dosa","price_paise":14900,"allergens":["gluten"],"reason":"launch price"}`)
	if code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	item := out["item"].(map[string]any)
	id := item["id"].(string)
	if item["status"] != "hidden" || item["price"].(map[string]any)["amount_minor"] != float64(14900) || item["sort_order"] != float64(100) {
		t.Errorf("a new item is hidden: %v", item)
	}
	if code, out := f.call("POST", "/admin/menu/items", ops, `{"name":"Masala Dosa","price_paise":9900}`); code != 409 || out["code"] != "name_taken" {
		t.Errorf("%d %v", code, out)
	}
	for _, bad := range []string{`{"name":"","price_paise":9900}`, `{"name":"Idli","price_paise":50}`, `{"name":"Idli","price_paise":9900,"photo_url":"http://x"}`} {
		if code, _ := f.call("POST", "/admin/menu/items", ops, bad); code != 400 {
			t.Errorf("%s: %d", bad, code)
		}
	}
	code, out = f.call("PATCH", "/admin/menu/items/"+id, ops, `{"price_paise":15900,"status":"active","description":"Crisp","reason":"ghee costs more"}`)
	if code != 200 || out["item"].(map[string]any)["price"].(map[string]any)["amount_minor"] != float64(15900) || out["item"].(map[string]any)["status"] != "active" {
		t.Fatalf("%d %v", code, out)
	}
	if code, out := f.call("PATCH", "/admin/menu/items/"+id, ops, `{"description":null,"reason":"no description"}`); code != 200 || out["item"].(map[string]any)["description"] != nil ||
		out["item"].(map[string]any)["allergens"].([]any)[0] != "gluten" {
		t.Errorf("a null clears only what it names: %d %v", code, out)
	}
	if code, _ := f.call("PATCH", "/admin/menu/items/"+id, ops, `{"price_paise":15900}`); code != 400 {
		t.Errorf("no reason: %d", code)
	}
	for _, bad := range []string{`{"price_paise":"16900","reason":"as text"}`, `{"name":null,"reason":"no name"}`, `{"status":null,"reason":"no status"}`} {
		if code, _ := f.call("PATCH", "/admin/menu/items/"+id, ops, bad); code != 400 {
			t.Errorf("%s: %d", bad, code)
		}
	}
	// 30 Kannada characters are 90 bytes: the limit is in characters.
	if code, out := f.call("POST", "/admin/menu/items", ops, `{"name":"ಮಸಾಲೆ ದೋಸೆ ಮಸಾಲೆ ದೋಸೆ ಮಸಾಲೆ ದೋಸೆ","price_paise":9900}`); code != 201 {
		t.Errorf("a Kannada name: %d %v", code, out)
	}
	_, hist := f.call("GET", "/admin/menu/items/"+id+"/prices", ops, "")
	prices := hist["prices"].([]any)
	if len(prices) != 2 || prices[0].(map[string]any)["reason"] != "ghee costs more" || prices[1].(map[string]any)["reason"] != "launch price" {
		t.Errorf("newest first, with reasons: %v", hist)
	}
	if code, _ := f.call("GET", "/admin/menu/items/00000000-0000-0000-0000-000000000009/prices", ops, ""); code != 404 {
		t.Errorf("an unknown item: %d", code)
	}
	if f.count("SELECT count(*) FROM audit_events WHERE entity_id = $1 AND action IN ('menu.created', 'menu.updated')", id) != 3 {
		t.Error("every change is audited")
	}
	_, s := f.user("menu.sup", "support")
	if code, out := f.call("GET", "/admin/menu/items", f.signIn("menu.sup", s), ""); code != 403 || out["missing_permission"] != "menu.manage" {
		t.Errorf("%d %v", code, out)
	}
}

// gate adds a customer's gate in a zone with an area, and returns it.
func (f *fixture) gate(zone, phone string) string {
	f.t.Helper()
	person := f.id("INSERT INTO people (phone) VALUES ($1) RETURNING id::text", phone)
	customer := f.id("INSERT INTO customers (person_id) VALUES ($1) RETURNING id::text", person)
	g := f.id("INSERT INTO gates (customer_id, zone_id, label, lat, lng, verified) VALUES ($1, $2, 'Home', 12.95, 77.65, true) RETURNING id::text", customer, zone)
	f.exec("INSERT INTO gate_history (gate_id, version, label, lat, lng, verified, reason) VALUES ($1, 1, 'Home', 12.95, 77.65, true, 'saved by the customer')", g)
	return g
}

func TestEditingGates(t *testing.T) {
	f := newFixture(t)
	_, temp := f.user("gate.ops", "ops")
	ops := f.signIn("gate.ops", temp)
	zone := f.zone()
	f.exec("UPDATE zones SET area = extensions.ST_Multi(extensions.ST_MakeEnvelope(77.6, 12.9, 77.7, 13.0, 4326)) WHERE id = $1", zone)
	first := f.gate(zone, "+919833300001")
	second := f.gate(zone, "+919833300002")
	_, page := f.call("GET", "/admin/gates?limit=1&zone_id="+zone, ops, "")
	next, _ := page["next_cursor"].(string)
	_, more := f.call("GET", "/admin/gates?limit=1&cursor="+next, ops, "")
	if page["gates"].([]any)[0].(map[string]any)["id"] != second || more["gates"].([]any)[0].(map[string]any)["id"] != first || more["next_cursor"] != nil {
		t.Errorf("newest first, paged: %v then %v", page, more)
	}
	if code, out := f.call("PATCH", "/admin/gates/"+first, ops, `{"lat":13.5,"lng":77.65,"reason":"moved the pin"}`); code != 409 || out["code"] != "outside_zone" {
		t.Errorf("%d %v", code, out)
	}
	if code, _ := f.call("PATCH", "/admin/gates/"+first, ops, `{"lat":12.96,"reason":"only one"}`); code != 400 {
		t.Errorf("lat without lng: %d", code)
	}
	code, out := f.call("PATCH", "/admin/gates/"+first, ops, `{"lat":12.96,"lng":77.66,"entry_notes":"Blue gate","verified":false,"reason":"the pin was off"}`)
	g := out["gate"].(map[string]any)
	if code != 200 || g["version"] != float64(2) || g["lat"] != 12.96 || g["entry_notes"] != "Blue gate" || g["verified"] != false || g["label"] != "Home" {
		t.Fatalf("%d %v", code, out)
	}
	_, hist := f.call("GET", "/admin/gates/"+first+"/history", ops, "")
	versions := hist["versions"].([]any)
	if len(versions) != 2 || versions[0].(map[string]any)["changed_by"] != nil || versions[1].(map[string]any)["changed_by"] != "Name gate.ops" ||
		versions[1].(map[string]any)["reason"] != "the pin was off" {
		t.Errorf("oldest first, with who: %v", hist)
	}
	// The gate row always matches its newest version.
	if f.count(`SELECT count(*) FROM gates g JOIN gate_history h ON h.gate_id = g.id AND h.version = (SELECT max(version) FROM gate_history WHERE gate_id = g.id)
		WHERE g.id = $1 AND h.lat = g.lat AND h.lng = g.lng AND h.label = g.label AND h.entry_notes IS NOT DISTINCT FROM g.entry_notes AND h.verified = g.verified`, first) != 1 {
		t.Error("the gate and its newest version differ")
	}
	// A wrong type or a null where none is allowed is refused, never half applied.
	for _, bad := range []string{`{"verified":"false","reason":"bad pin"}`, `{"label":null,"reason":"no label"}`, `{"label":"Home\u202e","reason":"hidden text"}`} {
		if code, _ := f.call("PATCH", "/admin/gates/"+first, ops, bad); code != 400 {
			t.Errorf("%s: %d", bad, code)
		}
	}
	if code, out := f.call("PATCH", "/admin/gates/"+first, ops, `{"entry_notes":null,"reason":"notes were wrong"}`); code != 200 || out["gate"].(map[string]any)["entry_notes"] != nil {
		t.Errorf("a null clears the notes: %d %v", code, out)
	}
	// A redaction may blur a version, but never change who made it.
	if _, err := f.pool.Exec(context.Background(), `UPDATE gate_history SET label = '(removed)', entry_notes = NULL, lat = round(lat::numeric, 2)::double precision,
		lng = round(lng::numeric, 2)::double precision, redacted_at = now(), changed_by = NULL WHERE gate_id = $1 AND version = 2`, first); err == nil {
		t.Error("a redaction rewrote who made a version")
	}
	// Erasing the customer keeps who made each version; nothing may rewrite it.
	if err := database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		return geo.RedactGates(context.Background(), tx, f.id("SELECT customer_id::text FROM gates WHERE id = $1", first))
	}); err != nil {
		t.Fatal(err)
	}
	if f.count("SELECT count(*) FROM gate_history WHERE gate_id = $1 AND changed_by IS NOT NULL AND redacted_at IS NOT NULL", first) != 2 {
		t.Error("a redaction lost who made a version")
	}
	if _, err := f.pool.Exec(context.Background(), "UPDATE gate_history SET changed_by = NULL WHERE gate_id = $1 AND version = 2", first); err == nil {
		t.Error("who made a version was rewritten")
	}
	// A gate whose customer was erased is left alone.
	if err := database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		return geo.RedactGates(context.Background(), tx, f.id("SELECT customer_id::text FROM gates WHERE id = $1", second))
	}); err != nil {
		t.Fatal(err)
	}
	if code, out := f.call("PATCH", "/admin/gates/"+second, ops, `{"label":"Office","reason":"rename it"}`); code != 409 || out["code"] != "not_allowed" {
		t.Errorf("%d %v", code, out)
	}
	_, s := f.user("gate.sup", "support")
	if code, out := f.call("PATCH", "/admin/gates/"+first, f.signIn("gate.sup", s), `{"label":"X","reason":"try it"}`); code != 403 || out["missing_permission"] != "gates.manage" {
		t.Errorf("%d %v", code, out)
	}
}

func TestHubsAndTheirStaff(t *testing.T) {
	f := newFixture(t)
	_, temp := f.user("hub.ops", "ops")
	ops := f.signIn("hub.ops", temp)
	zone := f.zone()
	f.exec("UPDATE zones SET area = extensions.ST_Multi(extensions.ST_MakeEnvelope(77.6, 12.9, 77.7, 13.0, 4326)) WHERE id = $1", zone)
	code, out := f.call("POST", "/admin/hubs", ops, `{"zone_id":"`+zone+`","name":"Koramangala Hub","lat":12.93,"lng":77.62}`)
	if code != 201 || out["hub"].(map[string]any)["status"] != "active" || out["hub"].(map[string]any)["staff_count"] != float64(0) {
		t.Fatalf("%d %v", code, out)
	}
	hub := out["hub"].(map[string]any)["id"].(string)
	if code, out := f.call("POST", "/admin/hubs", ops, `{"zone_id":"`+zone+`","name":"Koramangala Hub","lat":12.93,"lng":77.62}`); code != 409 || out["code"] != "name_taken" {
		t.Errorf("%d %v", code, out)
	}
	code, out = f.call("POST", "/admin/hubs/"+hub+"/staff", ops, `{"phone":"+919844400001","name":"Lakshmi","role":"hub_lead"}`)
	if code != 201 || out["staff"].(map[string]any)["phone_last4"] != "0001" {
		t.Fatalf("%d %v", code, out)
	}
	lead := out["staff"].(map[string]any)["id"].(string)
	if code, out := f.call("POST", "/admin/hubs/"+hub+"/staff", ops, `{"phone":"+919844400001","name":"Again","role":"hub_staff"}`); code != 409 || out["code"] != "phone_taken" {
		t.Errorf("%d %v", code, out)
	}
	if code, _ := f.call("POST", "/admin/hubs/"+hub+"/staff", ops, `{"phone":"+911234","name":"Bad","role":"hub_staff"}`); code != 400 {
		t.Errorf("a bad phone: %d", code)
	}
	// A hub manager sees and changes only their own hub.
	other := f.id("INSERT INTO hubs (zone_id, name, lat, lng) VALUES ($1, 'Other Hub', 12.94, 77.63) RETURNING id::text", zone)
	mgrID, mgrTemp := f.user("hub.mgr", "ops")
	f.exec("UPDATE console_users SET role = 'hub_manager', hub_id = $2 WHERE id = $1", mgrID, hub)
	mgr := f.signIn("hub.mgr", mgrTemp)
	_, list := f.call("GET", "/admin/hubs", mgr, "")
	if hubs := list["hubs"].([]any); len(hubs) != 1 || hubs[0].(map[string]any)["id"] != hub || hubs[0].(map[string]any)["staff_count"] != float64(1) {
		t.Errorf("%v", list)
	}
	if code, _ := f.call("GET", "/admin/hubs/"+other+"/staff", mgr, ""); code != 404 {
		t.Errorf("another hub's staff: %d", code)
	}
	if code, _ := f.call("POST", "/admin/hubs/"+other+"/staff", mgr, `{"phone":"+919844400002","name":"Ravi","role":"hub_staff"}`); code != 404 {
		t.Errorf("adding to another hub: %d", code)
	}
	if code, _ := f.call("PATCH", "/admin/hub-staff/"+lead, mgr, `{"hub_id":"`+other+`","reason":"move them"}`); code != 404 {
		t.Errorf("moving staff out of the hub: %d", code)
	}
	if code, _ := f.call("PATCH", "/admin/hubs/"+hub, mgr, `{"status":"paused","reason":"closed today"}`); code != 403 {
		t.Errorf("a hub manager pausing the hub: %d", code)
	}
	// Suspending ends the staff member's sessions.
	if err := database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		_, _, err := f.svc.Sessions.Start(context.Background(), tx, "staff", lead)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	code, out = f.call("PATCH", "/admin/hub-staff/"+lead, mgr, `{"status":"suspended","reason":"left the job"}`)
	if code != 200 || out["staff"].(map[string]any)["status"] != "suspended" {
		t.Fatalf("%d %v", code, out)
	}
	if f.count("SELECT count(*) FROM refresh_tokens WHERE actor_type = 'staff' AND actor_id = $1 AND revoked_at IS NULL", lead) != 0 {
		t.Error("the sessions survive")
	}
	if code, out := f.call("PATCH", "/admin/hubs/"+hub, ops, `{"status":"paused","name":"Kora Hub","reason":"closed today"}`); code != 200 ||
		out["hub"].(map[string]any)["status"] != "paused" || out["hub"].(map[string]any)["name"] != "Kora Hub" {
		t.Errorf("%d %v", code, out)
	}
	if code, out := f.call("PATCH", "/admin/hubs/"+hub, ops, `{"lat":13.5,"lng":77.62,"reason":"moved"}`); code != 409 || out["code"] != "outside_zone" {
		t.Errorf("a hub moved out of its zone: %d %v", code, out)
	}
	// Moving someone to another hub ends their sessions.
	code, out = f.call("POST", "/admin/hubs/"+hub+"/staff", ops, `{"phone":"+919844400003","name":"Meena","role":"hub_staff"}`)
	if code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	meena := out["staff"].(map[string]any)["id"].(string)
	if err := database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		_, _, err := f.svc.Sessions.Start(context.Background(), tx, "staff", meena)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if code, out := f.call("PATCH", "/admin/hub-staff/"+meena, ops, `{"hub_id":"`+other+`","reason":"needed there"}`); code != 200 || out["staff"].(map[string]any)["hub_id"] != other {
		t.Fatalf("%d %v", code, out)
	}
	if f.count("SELECT count(*) FROM refresh_tokens WHERE actor_type = 'staff' AND actor_id = $1 AND revoked_at IS NULL", meena) != 0 {
		t.Error("a move must end the sessions")
	}
	if f.count("SELECT count(*) FROM audit_events WHERE action IN ('hub.created', 'hub.updated', 'hub_staff.added', 'hub_staff.updated')") != 6 {
		t.Error("every change is audited")
	}
}
