package console

import (
	"context"
	"fmt"
	"testing"

	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
)

// admins signs in two admins: a requester and an approver.
func (f *fixture) admins(prefix string) (string, string) {
	f.t.Helper()
	_, ta := f.user(prefix+".a", "admin")
	_, tb := f.user(prefix+".b", "admin")
	return f.signIn(prefix+".a", ta), f.signIn(prefix+".b", tb)
}

func setting(t *testing.T, view map[string]any, key string) map[string]any {
	t.Helper()
	for _, s := range view["settings"].([]any) {
		if s.(map[string]any)["key"] == key {
			return s.(map[string]any)
		}
	}
	t.Fatalf("no setting %s", key)
	return nil
}

func TestListingZonesAndReadingSettings(t *testing.T) {
	f := newFixture(t)
	a, _ := f.admins("read")
	zone := f.zone()
	f.exec("UPDATE zones SET is_test = true WHERE id = $1", zone)
	code, out := f.call("GET", "/admin/zones", a, "")
	if code != 200 || len(out["zones"].([]any)) != 1 {
		t.Fatalf("%d %v", code, out)
	}
	z := out["zones"].([]any)[0].(map[string]any)
	if z["offer_enabled"] != true || z["is_test"] != true || z["city"] != "Bengaluru" {
		t.Errorf("%v", z)
	}
	code, global := f.call("GET", "/admin/settings", a, "")
	if code != 200 || global["zone_id"] != nil || global["zone_version"] != nil || len(global["settings"].([]any)) != len(geo.Settings) {
		t.Fatalf("%d %v", code, global)
	}
	timer := setting(t, global, "timer_length_s")
	if timer["value"] != float64(90) || timer["effective"] != float64(90) || timer["min"] != float64(30) || timer["required"] != true || timer["unit"] != "seconds" {
		t.Errorf("%v", timer)
	}
	code, own := f.call("GET", "/admin/zones/"+zone+"/settings", a, "")
	if code != 200 || own["zone_version"] != float64(0) || own["zone_id"] != zone {
		t.Fatalf("%d %v", code, own)
	}
	if s := setting(t, own, "timer_length_s"); s["value"] != nil || s["effective"] != float64(90) {
		t.Errorf("a zone with no values of its own follows the global ones: %v", s)
	}
	if code, _ := f.call("GET", "/admin/zones/00000000-0000-0000-0000-000000000009/settings", a, ""); code != 404 {
		t.Errorf("an unknown zone: %d", code)
	}
	_, temp := f.user("hub.read", "support")
	if code, out := f.call("GET", "/admin/settings", f.signIn("hub.read", temp), ""); code != 403 || out["missing_permission"] != "settings.read" {
		t.Errorf("support reading settings: %d %v", code, out)
	}
}

func TestChangingSettingsThroughAnApproval(t *testing.T) {
	f := newFixture(t)
	a, b := f.admins("set")
	zone := f.zone()
	_, global := f.call("GET", "/admin/settings", a, "")
	base := int(global["global_version"].(float64))
	body := func(changes string, base int) string {
		return fmt.Sprintf(`{"changes":%s,"base_version":%d,"reason":"a longer timer for the pilot"}`, changes, base)
	}
	// Refused at once: out of range, unknown, a required value cleared, a zone left invalid, a stale base.
	for _, c := range []struct{ changes, code string }{
		{`{"timer_length_s":5}`, "invalid_setting"}, {`{"no_such_key":5}`, "invalid_setting"}, {`{"timer_length_s":null}`, "invalid_setting"},
		{`{"arrival_perimeter_m":900,"zone_radius_m":400}`, "invalid_setting"},
	} {
		if code, out := f.call("PUT", "/admin/settings", a, body(c.changes, base)); code != 400 || out["code"] != c.code {
			t.Errorf("%s: %d %v", c.changes, code, out)
		}
	}
	if code, out := f.call("PUT", "/admin/settings", a, body(`{"timer_length_s":120}`, base+5)); code != 409 || out["code"] != "stale_version" {
		t.Errorf("a stale base: %d %v", code, out)
	}
	if f.count("SELECT count(*) FROM zone_settings") != 1 || f.count("SELECT count(*) FROM approvals") != 0 {
		t.Fatal("a refused request stored something")
	}
	code, out := f.call("PUT", "/admin/settings", a, body(`{"timer_length_s":120}`, base))
	if code != 202 {
		t.Fatalf("%d %v", code, out)
	}
	ap := out["approval"].(map[string]any)
	change := ap["change"].(map[string]any)
	diff := change["diffs"].([]any)[0].(map[string]any)
	if ap["kind"] != "settings" || change["zone_id"] != nil || diff["key"] != "timer_length_s" || diff["before"] != float64(90) || diff["after"] != float64(120) {
		t.Errorf("%v", ap)
	}
	if e, _ := geo.LoadSettings(context.Background(), f.pool, ""); *e.Values["timer_length_s"] != 90 {
		t.Fatal("applied before approval")
	}
	if code, out := f.call("POST", "/admin/approvals/"+ap["id"].(string)+"/approve", b, ""); code != 200 || out["approval"].(map[string]any)["status"] != "approved" {
		t.Fatalf("%d %v", code, out)
	}
	if e, _ := geo.LoadSettings(context.Background(), f.pool, zone); *e.Values["timer_length_s"] != 120 || e.GlobalVersion != base+1 {
		t.Errorf("the change did not reach the zone: %v", e.GlobalVersion)
	}
	if f.count("SELECT count(*) FROM zone_settings WHERE created_by LIKE 'console:%'") != 1 {
		t.Error("the new version must say the console made it")
	}
	// A zone's own value, then a null that goes back to the global one.
	code, out = f.call("PUT", "/admin/zones/"+zone+"/settings", a, body(`{"timer_length_s":150}`, 0))
	if code != 202 {
		t.Fatalf("%d %v", code, out)
	}
	f.call("POST", "/admin/approvals/"+out["approval"].(map[string]any)["id"].(string)+"/approve", b, "")
	_, view := f.call("GET", "/admin/zones/"+zone+"/settings", a, "")
	if s := setting(t, view, "timer_length_s"); s["value"] != float64(150) || s["effective"] != float64(150) || view["zone_version"] != float64(1) {
		t.Errorf("%v", view)
	}
	_, out = f.call("PUT", "/admin/zones/"+zone+"/settings", a, body(`{"timer_length_s":null}`, 1))
	f.call("POST", "/admin/approvals/"+out["approval"].(map[string]any)["id"].(string)+"/approve", b, "")
	_, view = f.call("GET", "/admin/zones/"+zone+"/settings", a, "")
	if s := setting(t, view, "timer_length_s"); s["value"] != nil || s["effective"] != float64(120) {
		t.Errorf("a null goes back to the global value: %v", s)
	}
	// A change to what is already so is refused.
	_, view = f.call("GET", "/admin/zones/"+zone+"/settings", a, "")
	zv := int(view["zone_version"].(float64))
	if code, out := f.call("PUT", "/admin/zones/"+zone+"/settings", a, body(`{"timer_length_s":null}`, zv)); code != 409 || out["code"] != "no_change" {
		t.Errorf("clearing what is already clear: %d %v", code, out)
	}
	// A zone change waits; a global change approved meanwhile makes it invalid, so it fails.
	code, zoneReq := f.call("PUT", "/admin/zones/"+zone+"/settings", a, body(`{"arrival_perimeter_m":300}`, zv))
	if code != 202 {
		t.Fatalf("%d %v", code, zoneReq)
	}
	_, global = f.call("GET", "/admin/settings", a, "")
	base = int(global["global_version"].(float64))
	code, globalReq := f.call("PUT", "/admin/settings", a, body(`{"zone_radius_m":250}`, base))
	if code != 202 {
		t.Fatalf("a global change and a zone change may wait together: %d %v", code, globalReq)
	}
	if code, out := f.call("PUT", "/admin/settings", b, body(`{"timer_length_s":115}`, base)); code != 409 || out["code"] != "approval_pending" {
		t.Errorf("a second global request while one waits: %d %v", code, out)
	}
	if _, out := f.call("POST", "/admin/approvals/"+globalReq["approval"].(map[string]any)["id"].(string)+"/approve", b, ""); out["approval"].(map[string]any)["status"] != "approved" {
		t.Fatalf("%v", out)
	}
	_, out = f.call("POST", "/admin/approvals/"+zoneReq["approval"].(map[string]any)["id"].(string)+"/approve", b, "")
	if ap := out["approval"].(map[string]any); ap["status"] != "failed" || ap["failure_code"] != "invalid_setting" {
		t.Errorf("a zone change the global one made invalid: %v", out)
	}
	if e, _ := geo.LoadSettings(context.Background(), f.pool, zone); *e.Values["arrival_perimeter_m"] == 300 {
		t.Error("a failed approval changed the zone")
	}
	// The world moves past a waiting global change: another version is stored directly.
	_, global = f.call("GET", "/admin/settings", a, "")
	base = int(global["global_version"].(float64))
	_, waiting := f.call("PUT", "/admin/settings", a, body(`{"timer_length_s":110}`, base))
	global2 := waiting["approval"].(map[string]any)["id"].(string)
	tx, _ := f.pool.Begin(context.Background())
	v := 105
	if _, err := geo.ChangeSettings(context.Background(), tx, "", []geo.Change{{Name: "timer_length_s", Value: &v}}, "changed from the command line", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, out := f.call("POST", "/admin/approvals/"+global2+"/approve", b, ""); out["approval"].(map[string]any)["status"] != "failed" || out["approval"].(map[string]any)["failure_code"] != "stale_version" {
		t.Errorf("%v", out)
	}
	if e, _ := geo.LoadSettings(context.Background(), f.pool, ""); *e.Values["timer_length_s"] != 105 {
		t.Error("a failed approval changed the settings")
	}
}

func TestTheKillSwitch(t *testing.T) {
	f := newFixture(t)
	a, b := f.admins("kill")
	zone := f.zone()
	f.exec("UPDATE zones SET area = extensions.ST_Multi(extensions.ST_MakeEnvelope(77.6, 12.9, 77.7, 13.0, 4326)) WHERE id = $1", zone)
	_, temp := f.user("ops.kill", "ops")
	ops := f.signIn("ops.kill", temp)
	if code, out := f.call("POST", "/admin/zones/"+zone+"/kill-switch", ops, `{"offer_enabled":true,"reason":"turn it on"}`); code != 409 || out["code"] != "no_change" {
		t.Errorf("asking for what is already so: %d %v", code, out)
	}
	code, out := f.call("POST", "/admin/zones/"+zone+"/kill-switch", ops, `{"offer_enabled":false,"reason":"rain in the zone"}`)
	if code != 202 {
		t.Fatalf("%d %v", code, out)
	}
	ap := out["approval"].(map[string]any)
	if c := ap["change"].(map[string]any); c["kind"] != "kill_switch" || c["offer_enabled_before"] != true || c["offer_enabled_after"] != false || c["zone_id"] != zone {
		t.Errorf("%v", ap)
	}
	if code, out := f.call("POST", "/admin/zones/"+zone+"/kill-switch", a, `{"offer_enabled":false,"reason":"again"}`); code != 409 || out["code"] != "approval_pending" {
		t.Errorf("%d %v", code, out)
	}
	if code, _ := f.call("POST", "/admin/approvals/"+ap["id"].(string)+"/approve", ops, ""); code != 403 {
		t.Errorf("ops approving: %d", code)
	}
	if code, out := f.call("POST", "/admin/approvals/"+ap["id"].(string)+"/approve", b, ""); code != 200 || out["approval"].(map[string]any)["status"] != "approved" {
		t.Fatalf("%d %v", code, out)
	}
	if f.count("SELECT count(*) FROM zones WHERE id = $1 AND status = 'paused'", zone) != 1 {
		t.Error("the zone is not paused")
	}
	_, list := f.call("GET", "/admin/zones", a, "")
	if list["zones"].([]any)[0].(map[string]any)["offer_enabled"] != false {
		t.Errorf("%v", list)
	}
	if f.count("SELECT count(*) FROM audit_events WHERE action = 'zone.kill_switch' AND entity_id = $1", zone) != 1 {
		t.Error("not audited")
	}
	// A switch the zone moved past fails and changes nothing.
	_, out = f.call("POST", "/admin/zones/"+zone+"/kill-switch", ops, `{"offer_enabled":true,"reason":"rain stopped"}`)
	f.exec("UPDATE zones SET status = 'active' WHERE id = $1", zone)
	if _, out := f.call("POST", "/admin/approvals/"+out["approval"].(map[string]any)["id"].(string)+"/approve", a, ""); out["approval"].(map[string]any)["failure_code"] != "stale_version" {
		t.Errorf("%v", out)
	}
	if code, _ := f.call("POST", "/admin/zones/00000000-0000-0000-0000-000000000009/kill-switch", ops, `{"offer_enabled":false,"reason":"no zone"}`); code != 404 {
		t.Errorf("an unknown zone: %d", code)
	}
	// A zone with no area, or one overlapping an active zone, cannot be turned on.
	bare := f.id("INSERT INTO zones (name, city, status) VALUES ('Bare', 'Bengaluru', 'paused') RETURNING id::text")
	if code, out := f.call("POST", "/admin/zones/"+bare+"/kill-switch", ops, `{"offer_enabled":true,"reason":"open it"}`); code != 409 || out["code"] != "zone_has_no_area" {
		t.Errorf("%d %v", code, out)
	}
	over := f.id(`INSERT INTO zones (name, city, status, area) VALUES ('Over', 'Bengaluru', 'paused',
		extensions.ST_Multi(extensions.ST_MakeEnvelope(77.65, 12.95, 77.75, 13.05, 4326))) RETURNING id::text`)
	if code, out := f.call("POST", "/admin/zones/"+over+"/kill-switch", ops, `{"offer_enabled":true,"reason":"open it"}`); code != 409 || out["code"] != "zone_overlaps" {
		t.Errorf("%d %v", code, out)
	}
	next := f.id(`INSERT INTO zones (name, city, status, area) VALUES ('Next door', 'Bengaluru', 'paused',
		extensions.ST_Multi(extensions.ST_MakeEnvelope(77.7, 12.9, 77.8, 13.0, 4326))) RETURNING id::text`)
	code, out = f.call("POST", "/admin/zones/"+next+"/kill-switch", ops, `{"offer_enabled":true,"reason":"open it"}`)
	if code != 202 {
		t.Fatalf("a zone sharing only a border: %d %v", code, out)
	}
	// Checked again on approval: the overlapping zone goes active while the request waits.
	f.exec("UPDATE zones SET area = extensions.ST_Multi(extensions.ST_MakeEnvelope(77.65, 12.9, 77.75, 13.0, 4326)), status = 'active' WHERE id = $1", over)
	if _, out := f.call("POST", "/admin/approvals/"+out["approval"].(map[string]any)["id"].(string)+"/approve", b, ""); out["approval"].(map[string]any)["failure_code"] != "zone_overlaps" {
		t.Errorf("%v", out)
	}
}
