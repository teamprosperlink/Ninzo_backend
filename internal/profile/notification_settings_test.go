package profile

import (
	"context"
	"testing"
)

func settingsOf(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	s, ok := out["settings"].(map[string]any)
	if !ok {
		t.Fatalf("no settings in %v", out)
	}
	return s
}

func TestNotificationSettingsDefaultsAndChanges(t *testing.T) {
	f := newFixture(t)
	id, token := f.newCustomer("+919800000001")
	// No row yet: offers and news off, support replies on, order updates always on.
	code, out := f.send(f.customer, "GET", "/me/notification-settings", token, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	checkFields(t, out, "customer", "NotificationSettingsResponse")
	checkFields(t, settingsOf(t, out), "customer", "NotificationSettings")
	if s := settingsOf(t, out); s["order_updates"] != true || s["offers_and_news"] != false || s["support_replies"] != true {
		t.Errorf("defaults: %v", s)
	}
	if n := f.count("SELECT count(*) FROM customer_notification_settings"); n != 0 {
		t.Error("reading must not create a row")
	}
	// One kind alone: the other keeps its default.
	code, out = f.send(f.customer, "PATCH", "/me/notification-settings", token, `{"offers_and_news":true}`)
	if s := settingsOf(t, out); code != 200 || s["offers_and_news"] != true || s["support_replies"] != true || s["order_updates"] != true {
		t.Fatalf("%d %v", code, out)
	}
	checkFields(t, out, "customer", "NotificationSettingsResponse")
	// The other kind alone: the first stays as set.
	code, out = f.send(f.customer, "PATCH", "/me/notification-settings", token, `{"support_replies":false}`)
	if s := settingsOf(t, out); code != 200 || s["offers_and_news"] != true || s["support_replies"] != false {
		t.Fatalf("%d %v", code, out)
	}
	_, out = f.send(f.customer, "GET", "/me/notification-settings", token, "")
	if s := settingsOf(t, out); s["offers_and_news"] != true || s["support_replies"] != false || s["order_updates"] != true {
		t.Errorf("stored: %v", s)
	}
	// Changing the first again leaves a switched-off second as it is.
	_, out = f.send(f.customer, "PATCH", "/me/notification-settings", token, `{"offers_and_news":true}`)
	if s := settingsOf(t, out); s["support_replies"] != false {
		t.Errorf("support replies came back on: %v", s)
	}
	// Both together.
	_, out = f.send(f.customer, "PATCH", "/me/notification-settings", token, `{"offers_and_news":false,"support_replies":true}`)
	if s := settingsOf(t, out); s["offers_and_news"] != false || s["support_replies"] != true {
		t.Errorf("%v", s)
	}
	if n := f.count("SELECT count(*) FROM customer_notification_settings WHERE customer_id = $1", id); n != 1 {
		t.Errorf("%d rows for one customer", n)
	}
	// Each change is audited, with the switches and not anything personal.
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'customer.notification_settings_updated' AND actor_id = $1", id); n != 4 {
		t.Errorf("%d audit rows", n)
	}
	if n := f.count(`SELECT count(*) FROM audit_events WHERE action = 'customer.notification_settings_updated' AND after @> '{"support_replies": false}'`); n != 1 {
		t.Error("the audit row does not say what was switched")
	}
}

func TestBadNotificationSettingsAreRefused(t *testing.T) {
	f := newFixture(t)
	_, token := f.newCustomer("+919800000001")
	for _, body := range []string{
		`{"order_updates":true}`, `{"order_updates":false}`, `{"order_updates":true,"offers_and_news":true}`, // order_updates is never accepted.
		`{}`, `[]`, `null`, `not json`, ``, // Nothing to change.
		`{"offers_and_news":"yes"}`, `{"support_replies":1}`, `{"offers_and_news":null}`, // Wrong types.
		`{"marketing":true}`, `{"offers_and_news":true,"marketing":true}`, // Unknown kinds.
	} {
		code, out := f.send(f.customer, "PATCH", "/me/notification-settings", token, body)
		if code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%q: %d %v", body, code, out)
		}
	}
	// None of it changed anything.
	if n := f.count("SELECT count(*) FROM customer_notification_settings"); n != 0 {
		t.Error("a refused request stored something")
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'customer.notification_settings_updated'"); n != 0 {
		t.Error("a refused request was audited")
	}
	// A refused request with order_updates in a valid body also leaves the other kind alone.
	f.send(f.customer, "PATCH", "/me/notification-settings", token, `{"offers_and_news":true}`)
	f.send(f.customer, "PATCH", "/me/notification-settings", token, `{"offers_and_news":false,"order_updates":true}`)
	if _, out := f.send(f.customer, "GET", "/me/notification-settings", token, ""); settingsOf(t, out)["offers_and_news"] != true {
		t.Error("a refused request changed a setting")
	}
}

func TestNotificationSettingsReplayAndSeparation(t *testing.T) {
	f := newFixture(t)
	_, a := f.newCustomer("+919800000001")
	_, b := f.newCustomer("+919800000002")
	code, first := f.sendKey(f.customer, "PATCH", "/me/notification-settings", a, `{"offers_and_news":true}`, "settings-key-0001")
	if code != 200 {
		t.Fatal(code)
	}
	code, second := f.sendKey(f.customer, "PATCH", "/me/notification-settings", a, `{"offers_and_news":true}`, "settings-key-0001")
	if code != 200 || second["server_time"] != first["server_time"] {
		t.Errorf("not the stored reply: %d %v", code, second)
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'customer.notification_settings_updated'"); n != 1 {
		t.Errorf("a replay was applied again: %d audit rows", n)
	}
	if code, out := f.sendKey(f.customer, "PATCH", "/me/notification-settings", a, `{"offers_and_news":false}`, "settings-key-0001"); code != 422 || out["code"] != "idempotency_key_reused" {
		t.Errorf("%d %v", code, out)
	}
	if code, out := f.sendKey(f.customer, "PATCH", "/me/notification-settings", a, `{"offers_and_news":false}`, ""); code != 400 || out["code"] != "idempotency_key_required" {
		t.Errorf("%d %v", code, out)
	}
	// Another customer is unaffected, and cannot read or change this one's.
	_, out := f.send(f.customer, "GET", "/me/notification-settings", b, "")
	if s := settingsOf(t, out); s["offers_and_news"] != false || s["support_replies"] != true {
		t.Errorf("another customer's settings leaked: %v", s)
	}
	f.send(f.customer, "PATCH", "/me/notification-settings", b, `{"support_replies":false}`)
	_, out = f.send(f.customer, "GET", "/me/notification-settings", a, "")
	if s := settingsOf(t, out); s["support_replies"] != true || s["offers_and_news"] != true {
		t.Errorf("one customer changed another's: %v", s)
	}
	// Without a token, and from a rider, there is no access.
	if code, _ := f.send(f.customer, "GET", "/me/notification-settings", "", ""); code != 401 {
		t.Errorf("signed out: %d", code)
	}
	_, rider := f.newRider("+919800000003")
	if code, _ := f.send(f.customer, "GET", "/me/notification-settings", rider, ""); code != 401 {
		t.Errorf("a rider: %d", code)
	}
}

func TestBlockedAndEndedSessionsCannotUseNotificationSettings(t *testing.T) {
	f := newFixture(t)
	_, token := f.newCustomer("+919800000001")
	f.exec("UPDATE people SET status = 'blocked' WHERE phone = '+919800000001'")
	for _, m := range []string{"GET", "PATCH"} {
		body := ""
		if m == "PATCH" {
			body = `{"offers_and_news":true}`
		}
		if code, out := f.send(f.customer, m, "/me/notification-settings", token, body); code != 403 || out["code"] != "account_blocked" {
			t.Errorf("%s while blocked: %d %v", m, code, out)
		}
	}
	f.exec("UPDATE people SET status = 'active'")
	f.exec("UPDATE refresh_tokens SET revoked_at = now()")
	for _, m := range []string{"GET", "PATCH"} {
		body := ""
		if m == "PATCH" {
			body = `{"offers_and_news":true}`
		}
		if code, _ := f.send(f.customer, m, "/me/notification-settings", token, body); code != 401 {
			t.Errorf("%s after sign-out: %d", m, code)
		}
	}
	if n := f.count("SELECT count(*) FROM customer_notification_settings"); n != 0 {
		t.Error("a refused customer stored settings")
	}
}

// An empty account is removed when its Google sign-in moves to another customer
// (internal/identity), so a settings row must never block deleting its customer (#178 review).
func TestSettingsNeverBlockRemovingACustomer(t *testing.T) {
	f := newFixture(t)
	id, token := f.newCustomer("+919800000001")
	if code, out := f.send(f.customer, "PATCH", "/me/notification-settings", token, `{"support_replies":false}`); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	f.exec("DELETE FROM devices WHERE actor_id = $1", id)
	if _, err := f.s.Pool.Exec(context.Background(), "DELETE FROM customers WHERE id = $1", id); err != nil {
		t.Fatalf("the settings row blocked removing the customer: %v", err)
	}
	if n := f.count("SELECT count(*) FROM customer_notification_settings"); n != 0 {
		t.Errorf("%d settings rows left", n)
	}
}
