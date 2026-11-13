package profile

import (
	"context"
	"strings"
	"testing"

	"github.com/teamprosperlink/Ninzo_backend/internal/contracttest"
)

// Issue 235, part B1: the rider's phone, email and picked area, as the finished rider app reads and
// sends them. Every test goes through the real handlers and checks the reply against the contract.

const riderContract = "rider.openapi.yaml"

const indiranagar = `{"name":"Indiranagar","city":"Bengaluru","lat":12.9784,"lng":77.6408}`

func (f *fixture) rider235(token string) map[string]any {
	f.t.Helper()
	code, out := f.send(f.rider, "GET", "/riders/me", token, "")
	if code != 200 {
		f.t.Fatalf("getRider: %d %v", code, out)
	}
	contracttest.Check(f.t, riderContract, "RiderResponse", out)
	return out["rider"].(map[string]any)
}

func TestGetRiderShowsTheRidersOwnPhoneAndNullEmailAndArea(t *testing.T) {
	f := newFixture(t)
	_, token := f.newRider("+919800000235")
	f.newRider("+919800000999")
	r := f.rider235(token)
	if r["phone"] != "+919800000235" {
		t.Errorf("phone %v: it is the rider's own number", r["phone"])
	}
	for _, field := range []string{"email", "area"} {
		if v, ok := r[field]; !ok || v != nil {
			t.Errorf("%s must be sent, as null, until set: %v", field, r)
		}
	}
	// A rider whose person has no number never gets a made-up one.
	f.exec("UPDATE people SET phone = NULL WHERE phone = '+919800000235'")
	if r := f.rider235(token); r["phone"] != nil {
		if _, present := r["phone"]; present {
			t.Errorf("no number must mean no phone key: %v", r)
		}
	}
}

func TestUpdateRiderTakesEmailAndAreaAsTheAppSendsThem(t *testing.T) {
	f := newFixture(t)
	id, token := f.newRider("+919800000236")
	patch := func(body string) (int, map[string]any) {
		return f.send(f.rider, "PATCH", "/riders/me", token, body)
	}
	code, out := patch(`{"name":"Ravi K","email":"ravi@example.com","area":` + indiranagar + `}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	contracttest.Check(t, riderContract, "RiderResponse", out)
	r := out["rider"].(map[string]any)
	area := r["area"].(map[string]any)
	if r["email"] != "ravi@example.com" || area["name"] != "Indiranagar" || area["city"] != "Bengaluru" || area["lat"] != 12.9784 || area["lng"] != 77.6408 {
		t.Errorf("%v", r)
	}
	// area.name keeps preferred_area in step.
	if r["preferred_area"] != "Indiranagar" || r["name"] != "Ravi K" || r["phone"] != "+919800000236" {
		t.Errorf("%v", r)
	}
	if got := f.rider235(token); got["email"] != "ravi@example.com" || got["area"].(map[string]any)["city"] != "Bengaluru" {
		t.Errorf("getRider after: %v", got)
	}
	// The email is contact only. It never reaches the audit trail, only whether there is one.
	var audit string
	if err := f.s.Pool.QueryRow(context.Background(), "SELECT string_agg(before::text || after::text, ' ') FROM audit_events WHERE action = 'rider.updated' AND entity_id = $1", id).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(audit, "ravi@example.com") || !strings.Contains(audit, `"has_email": true`) {
		t.Errorf("audit: %s", audit)
	}
	// Changing only the name keeps the email and the area.
	if _, out := patch(`{"name":"Ravi Kumar"}`); out["rider"].(map[string]any)["email"] != "ravi@example.com" || out["rider"].(map[string]any)["area"] == nil {
		t.Errorf("a name change lost the email or the area: %v", out)
	}
	// email null clears the email only.
	code, out = patch(`{"email":null}`)
	r = out["rider"].(map[string]any)
	if code != 200 || r["email"] != nil || r["area"] == nil || r["preferred_area"] != "Indiranagar" {
		t.Errorf("clearing the email: %d %v", code, out)
	}
	// A new area replaces the old, and the preferred_area follows it.
	code, out = patch(`{"area":{"name":"Koramangala","city":"Bengaluru","lat":12.93,"lng":77.62}}`)
	r = out["rider"].(map[string]any)
	if code != 200 || r["area"].(map[string]any)["name"] != "Koramangala" || r["preferred_area"] != "Koramangala" {
		t.Errorf("a new area: %d %v", code, out)
	}
	// area null clears the area, and the name it gave preferred_area.
	code, out = patch(`{"area":null}`)
	contracttest.Check(t, riderContract, "RiderResponse", out)
	r = out["rider"].(map[string]any)
	if code != 200 || r["area"] != nil || r["preferred_area"] != nil {
		t.Errorf("clearing the area: %d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM riders WHERE id = $1 AND area_name IS NULL AND area_city IS NULL AND area_lat IS NULL AND area_lng IS NULL", id); n != 1 {
		t.Error("the area columns were not all cleared")
	}
}

func TestOurPreferredAreaAndThePickedAreaStayInStep(t *testing.T) {
	f := newFixture(t)
	_, token := f.newRider("+919800000237")
	patch := func(body string) map[string]any {
		_, out := f.send(f.rider, "PATCH", "/riders/me", token, body)
		return out["rider"].(map[string]any)
	}
	patch(`{"area":` + indiranagar + `}`)
	// The same text again keeps the picked place.
	if r := patch(`{"preferred_area":"Indiranagar"}`); r["area"] == nil || r["preferred_area"] != "Indiranagar" {
		t.Errorf("the same text lost the place: %v", r)
	}
	// Other text is not that place any more, so the place goes with it.
	if r := patch(`{"preferred_area":"Whitefield"}`); r["area"] != nil || r["preferred_area"] != "Whitefield" {
		t.Errorf("other text kept a stale place: %v", r)
	}
	// With no place picked, area stays null and the free text is kept as it always was.
	if r := patch(`{"name":"Ravi"}`); r["area"] != nil || r["preferred_area"] != "Whitefield" {
		t.Errorf("%v", r)
	}
	patch(`{"area":` + indiranagar + `}`)
	if r := patch(`{"preferred_area":""}`); r["area"] != nil || r["preferred_area"] != nil {
		t.Errorf("clearing the text must clear the place: %v", r)
	}
}

func TestUpdateRiderRefusesBadEmailsAndAreas(t *testing.T) {
	f := newFixture(t)
	_, token := f.newRider("+919800000238")
	long := strings.Repeat("a", 250) + "@example.com"
	for name, body := range map[string]string{
		"both preferred_area and area":  `{"preferred_area":"x","area":` + indiranagar + `}`,
		"preferred_area and null area":  `{"preferred_area":"x","area":null}`,
		"not an address":                `{"email":"not an address"}`,
		"a name with the address":       `{"email":"Ravi <ravi@example.com>"}`,
		"an empty email":                `{"email":""}`,
		"an address in angle brackets":  `{"email":"<ravi@example.com>"}`,
		"an email with a space":         `{"email":"ravi @example.com"}`,
		"an email over 254":             `{"email":"` + long + `"}`,
		"a number for the email":        `{"email":5}`,
		"an object for the email":       `{"email":{}}`,
		"an area as text":               `{"area":"Indiranagar"}`,
		"an area with no name":          `{"area":{"city":"x","lat":1,"lng":2}}`,
		"an area with an empty name":    `{"area":{"name":"","city":"x","lat":1,"lng":2}}`,
		"an area with a blank name":     `{"area":{"name":"  ","city":"x","lat":1,"lng":2}}`,
		"an area name of 81":            `{"area":{"name":"` + strings.Repeat("a", 81) + `","city":"x","lat":1,"lng":2}}`,
		"an area with no city":          `{"area":{"name":"x","lat":1,"lng":2}}`,
		"a city of 81":                  `{"area":{"name":"x","city":"` + strings.Repeat("a", 81) + `","lat":1,"lng":2}}`,
		"a lat over 90":                 `{"area":{"name":"x","city":"x","lat":90.1,"lng":2}}`,
		"a lat under -90":               `{"area":{"name":"x","city":"x","lat":-90.1,"lng":2}}`,
		"a lng over 180":                `{"area":{"name":"x","city":"x","lat":1,"lng":180.1}}`,
		"a lng under -180":              `{"area":{"name":"x","city":"x","lat":1,"lng":-180.1}}`,
		"a text lat":                    `{"area":{"name":"x","city":"x","lat":"1","lng":2}}`,
		"a control character in a name": `{"area":{"name":"a\u0000b","city":"x","lat":1,"lng":2}}`,
		"nothing":                       `{}`,
		"only unknown fields":           `{"zone_id":"x"}`,
	} {
		if code, out := f.send(f.rider, "PATCH", "/riders/me", token, body); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if n := f.count("SELECT count(*) FROM riders WHERE email IS NOT NULL OR area_name IS NOT NULL OR preferred_area IS NOT NULL"); n != 0 {
		t.Errorf("a refused change was applied to %d riders", n)
	}
	// The limits themselves are allowed: 80 characters, the poles, an empty city.
	for name, body := range map[string]string{
		"a name of 80":        `{"area":{"name":"` + strings.Repeat("é", 80) + `","city":"x","lat":1,"lng":2}}`,
		"the extreme corners": `{"area":{"name":"x","city":"","lat":-90,"lng":180}}`,
		"an email of 254":     `{"email":"` + strings.Repeat("a", 64) + "@" + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 57) + `.com"}`,
	} {
		if code, out := f.send(f.rider, "PATCH", "/riders/me", token, body); code != 200 {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
}

func TestARiderAreaIsAllOrNothingAndFollowsPreferredAreaInTheDatabase(t *testing.T) {
	f := newFixture(t)
	id, _ := f.newRider("+919800000239")
	exec := func(sql string, args ...any) error {
		_, err := f.s.Pool.Exec(context.Background(), sql, args...)
		return err
	}
	for name, sql := range map[string]string{
		"a name with no city":                 "UPDATE riders SET preferred_area = 'x', area_name = 'x', area_lat = 1, area_lng = 2 WHERE id = $1",
		"an area whose name is not preferred": "UPDATE riders SET preferred_area = 'y', area_name = 'x', area_city = 'c', area_lat = 1, area_lng = 2 WHERE id = $1",
		"an area with no preferred_area":      "UPDATE riders SET area_name = 'x', area_city = 'c', area_lat = 1, area_lng = 2 WHERE id = $1",
		"a latitude of 91":                    "UPDATE riders SET preferred_area = 'x', area_name = 'x', area_city = 'c', area_lat = 91, area_lng = 2 WHERE id = $1",
		"an email of two characters":          "UPDATE riders SET email = 'ab' WHERE id = $1",
	} {
		if err := exec(sql, id); err == nil {
			t.Errorf("the database accepted %s", name)
		}
	}
	if err := exec("UPDATE riders SET preferred_area = 'x', area_name = 'x', area_city = 'c', area_lat = 1, area_lng = 2 WHERE id = $1", id); err != nil {
		t.Errorf("a whole area: %v", err)
	}
}

func TestUpdateRiderWithEmailIsAuditedReplaysAndIsRefusedForASuspendedRider(t *testing.T) {
	f := newFixture(t)
	id, token := f.newRider("+919800000240")
	body := `{"email":"ravi@example.com","area":` + indiranagar + `}`
	code, first := f.sendKey(f.rider, "PATCH", "/riders/me", token, body, "rider-apk-0001")
	code2, second := f.sendKey(f.rider, "PATCH", "/riders/me", token, body, "rider-apk-0001")
	if code != 200 || code2 != 200 || first["server_time"] != second["server_time"] {
		t.Errorf("a replay must give the same answer: %d %v / %d %v", code, first, code2, second)
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'rider.updated' AND entity_id = $1", id); n != 1 {
		t.Errorf("%d audit rows after a replay, want 1", n)
	}
	if code, _ := f.sendKey(f.rider, "PATCH", "/riders/me", token, `{"email":"other@example.com"}`, "rider-apk-0001"); code != 422 {
		t.Errorf("the same key with another body: %d", code)
	}
	f.exec("UPDATE riders SET status = 'suspended' WHERE id = $1", id)
	if code, out := f.send(f.rider, "PATCH", "/riders/me", token, `{"email":"sneaky@example.com"}`); code != 403 || out["code"] != "account_blocked" {
		t.Errorf("a suspended rider: %d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM riders WHERE email = 'sneaky@example.com'"); n != 0 {
		t.Error("a suspended rider changed their email")
	}
}
