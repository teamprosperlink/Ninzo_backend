package profile

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// The rider's profile changes, notifications and document reasons (issue 159).

func TestUpdatingARider(t *testing.T) {
	f := newFixture(t)
	id, token := f.newRider("+919800000071")
	patch := func(body string) (int, map[string]any) {
		return f.send(f.rider, "PATCH", "/riders/me", token, body)
	}
	code, out := patch(`{"name":"  Ravi Kumar ","preferred_area":"Indiranagar"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	checkFields(t, out, "rider", "RiderResponse")
	r := out["rider"].(map[string]any)
	checkFields(t, r, "rider", "Rider")
	if r["name"] != "Ravi Kumar" || r["preferred_area"] != "Indiranagar" || r["id"] != id || r["status"] != "applicant" || r["zone_id"] != nil {
		t.Errorf("%v", r)
	}
	// Send only the fields to change.
	if _, out := patch(`{"preferred_area":"Koramangala"}`); out["rider"].(map[string]any)["name"] != "Ravi Kumar" || out["rider"].(map[string]any)["preferred_area"] != "Koramangala" {
		t.Errorf("a one-field change: %v", out)
	}
	if _, out := patch(`{"name":"Ravi K"}`); out["rider"].(map[string]any)["preferred_area"] != "Koramangala" {
		t.Errorf("the area was lost: %v", out)
	}
	// An empty area clears it. The zone is never touched.
	if _, out := patch(`{"preferred_area":""}`); out["rider"].(map[string]any)["preferred_area"] != nil {
		t.Errorf("clearing the area: %v", out)
	}
	if _, out := f.send(f.rider, "GET", "/riders/me", token, ""); out["rider"].(map[string]any)["name"] != "Ravi K" {
		t.Errorf("getRider: %v", out)
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'rider.updated' AND entity_id = $1", id); n != 4 {
		t.Errorf("%d audit rows, want 4", n)
	}
	var zone *string
	if err := f.s.Pool.QueryRow(context.Background(), "SELECT zone_id::text FROM riders WHERE id = $1", id).Scan(&zone); err != nil || zone != nil {
		t.Errorf("the zone changed: %v %v", zone, err)
	}
}

func TestBadRiderChangesAreRefused(t *testing.T) {
	f := newFixture(t)
	_, token := f.newRider("+919800000072")
	for name, body := range map[string]string{
		"nothing":                 `{}`,
		"only unknown fields":     `{"zone_id":"x"}`,
		"an empty name":           `{"name":""}`,
		"a blank name":            `{"name":"   "}`,
		"a long name":             `{"name":"` + strings.Repeat("a", 81) + `"}`,
		"a long area":             `{"preferred_area":"` + strings.Repeat("a", 81) + `"}`,
		"a null name":             `{"name":null}`,
		"a number for the name":   `{"name":7}`,
		"a control character":     `{"name":"a\u0000b"}`,
		"not json":                `[`,
		"a good name, a bad area": `{"name":"Ravi","preferred_area":` + `5}`,
	} {
		if code, out := f.send(f.rider, "PATCH", "/riders/me", token, body); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if f.count("SELECT count(*) FROM riders WHERE name IS NOT NULL") != 0 {
		t.Error("a refused change was applied")
	}
	// 80 characters is the limit, counted in characters, not bytes.
	if code, out := f.send(f.rider, "PATCH", "/riders/me", token, `{"name":"`+strings.Repeat("é", 80)+`"}`); code != 200 {
		t.Errorf("80 characters: %d %v", code, out)
	}
	if code, _ := f.send(f.rider, "PATCH", "/riders/me", "", `{"name":"Ravi"}`); code != 401 {
		t.Errorf("no session: %d", code)
	}
	_, customer := f.newCustomer("+919800000073")
	if code, _ := f.send(f.rider, "PATCH", "/riders/me", customer, `{"name":"Ravi"}`); code != 401 {
		t.Errorf("a customer: %d", code)
	}
}

func TestARiderChangeIsSafeToRetryAndBlockedRidersCannotMakeOne(t *testing.T) {
	f := newFixture(t)
	id, token := f.newRider("+919800000074")
	body := `{"name":"Ravi","preferred_area":"HSR"}`
	code, first := f.sendKey(f.rider, "PATCH", "/riders/me", token, body, "rider-patch-0001")
	code2, second := f.sendKey(f.rider, "PATCH", "/riders/me", token, body, "rider-patch-0001")
	if code != 200 || code2 != 200 || first["server_time"] != second["server_time"] {
		t.Errorf("a replay must give the same answer: %d %v / %d %v", code, first, code2, second)
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'rider.updated'"); n != 1 {
		t.Errorf("%d audit rows after a replay", n)
	}
	if code, out := f.sendKey(f.rider, "PATCH", "/riders/me", token, `{"name":"Other"}`, "rider-patch-0001"); code != 422 {
		t.Errorf("the same key with another body: %d %v", code, out)
	}
	f.exec("UPDATE riders SET status = 'suspended' WHERE id = $1", id)
	if code, out := f.send(f.rider, "PATCH", "/riders/me", token, `{"name":"Sneaky"}`); code != 403 || out["code"] != "account_blocked" {
		t.Errorf("a suspended rider: %d %v", code, out)
	}
	f.exec("UPDATE riders SET status = 'active'")
	f.exec("UPDATE people SET status = 'blocked'")
	if code, out := f.send(f.rider, "PATCH", "/riders/me", token, `{"name":"Sneaky"}`); code != 403 || out["code"] != "account_blocked" {
		t.Errorf("a blocked person: %d %v", code, out)
	}
}

func TestListingNotifications(t *testing.T) {
	f := newFixture(t)
	id, token := f.newRider("+919800000075")
	_, other := f.newRider("+919800000076")
	var otherRider string
	if err := f.s.Pool.QueryRow(context.Background(), "SELECT id::text FROM riders WHERE id <> $1", id).Scan(&otherRider); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ {
		f.exec(`INSERT INTO rider_notifications (rider_id, kind, title, body, created_at) VALUES ($1, 'restock', $2, 'body', now() - make_interval(mins => $3))`,
			id, fmt.Sprintf("Note %d", i), 10-i)
	}
	f.exec(`INSERT INTO rider_notifications (rider_id, kind, title, body) VALUES ($1, 'shift', 'Not mine', 'x')`, otherRider)
	f.exec(`INSERT INTO rider_notifications (rider_id, kind, title, body, created_at) VALUES ($1, 'shift', 'Too old', 'x', now() - interval '31 days')`, id)

	var titles []string
	cursor := ""
	for page := 0; page < 5; page++ {
		path := "/riders/me/notifications?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		code, out := f.send(f.rider, "GET", path, token, "")
		if code != 200 {
			t.Fatalf("%d %v", code, out)
		}
		checkFields(t, out, "rider", "RiderNotificationList")
		for _, raw := range out["notifications"].([]any) {
			n := raw.(map[string]any)
			checkFields(t, n, "rider", "RiderNotification")
			titles = append(titles, n["title"].(string))
		}
		if out["next_cursor"] == nil {
			break
		}
		cursor = out["next_cursor"].(string)
	}
	if strings.Join(titles, ",") != "Note 5,Note 4,Note 3,Note 2,Note 1" {
		t.Errorf("got %v: newest first, only this rider's, and nothing older than 30 days", titles)
	}
	_, mine := f.send(f.rider, "GET", "/riders/me/notifications", other, "")
	if got := mine["notifications"].([]any); len(got) != 1 || got[0].(map[string]any)["title"] != "Not mine" {
		t.Errorf("the other rider's list: %v", mine)
	}
	for _, q := range []string{"limit=0", "limit=101", "limit=x", "cursor=nonsense"} {
		if code, out := f.send(f.rider, "GET", "/riders/me/notifications?"+q, token, ""); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", q, code, out)
		}
	}
	if code, _ := f.send(f.rider, "GET", "/riders/me/notifications", "", ""); code != 401 {
		t.Errorf("no session: %d", code)
	}
	// An order notification keeps its order id, and it is the only link to the order.
	if code, out := f.send(f.rider, "GET", "/riders/me/notifications", token, ""); code != 200 || len(out["notifications"].([]any)) != 5 {
		t.Errorf("%d %v", code, out)
	}
}

func TestVerificationDocumentsShowTheReasonPerDocument(t *testing.T) {
	f := newFixture(t)
	id, token := f.newRider("+919800000077")
	_, out := f.send(f.rider, "GET", "/riders/me/verification", token, "")
	if docs, ok := out["documents"].([]any); !ok || len(docs) != 0 {
		t.Fatalf("documents before any are recorded: %v", out)
	}
	// Documents belong to the newest verification, so an earlier one's do not show.
	var older, newest string
	if err := f.s.Pool.QueryRow(context.Background(), "INSERT INTO rider_verifications (rider_id, status, method, created_at) VALUES ($1, 'failed', 'manual_upload', now() - interval '2 days') RETURNING id::text", id).Scan(&older); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Pool.QueryRow(context.Background(), "INSERT INTO rider_verifications (rider_id, status, method) VALUES ($1, 'failed', 'manual_upload') RETURNING id::text", id).Scan(&newest); err != nil {
		t.Fatal(err)
	}
	f.exec("INSERT INTO verification_documents (verification_id, type, status, reason) VALUES ($1, 'rc', 'rejected', 'expired')", older)
	f.exec("INSERT INTO verification_documents (verification_id, type, status, reason) VALUES ($1, 'aadhaar', 'verified', NULL)", newest)
	f.exec("INSERT INTO verification_documents (verification_id, type, status, reason) VALUES ($1, 'driving_licence', 'rejected', 'name_mismatch')", newest)
	f.exec("INSERT INTO verification_documents (verification_id, type, status) VALUES ($1, 'photo', 'pending')", newest)
	_, out = f.send(f.rider, "GET", "/riders/me/verification", token, "")
	checkFields(t, out, "rider", "VerificationResponse")
	docs := out["documents"].([]any)
	if len(docs) != 3 {
		t.Fatalf("%d documents, want the 3 of the newest verification: %v", len(docs), docs)
	}
	got := map[string]string{}
	for _, raw := range docs {
		d := raw.(map[string]any)
		checkFields(t, d, "rider", "VerificationDocument")
		reason, _ := d["reason"].(string)
		got[d["type"].(string)] = d["status"].(string) + "/" + reason
	}
	want := map[string]string{"aadhaar": "verified/", "driving_licence": "rejected/name_mismatch", "photo": "pending/"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s is %q, want %q", k, got[k], v)
		}
	}
	// A reason belongs to a rejection only, and a document is recorded once per verification.
	if _, err := f.s.Pool.Exec(context.Background(), "INSERT INTO verification_documents (verification_id, type, status, reason) VALUES ($1, 'insurance', 'verified', 'expired')", newest); err == nil {
		t.Error("a verified document with a reason was accepted")
	}
	if _, err := f.s.Pool.Exec(context.Background(), "INSERT INTO verification_documents (verification_id, type, status) VALUES ($1, 'photo', 'verified')", newest); err == nil {
		t.Error("a document was recorded twice for one verification")
	}
}
