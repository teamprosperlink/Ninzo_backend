package profile

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type fixture struct {
	t        *testing.T
	s        *identity.Sessions
	customer *httptest.Server
	rider    *httptest.Server
	keys     int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, s: &identity.Sessions{
		Pool: dbtest.Fresh(t), Log: quiet, RefreshTTL: 90 * 24 * time.Hour,
		Signer: identity.Signer{Key: []byte("a-test-key-that-is-long-enough-to-use"), TTL: 15 * time.Minute},
	}}
	svc := &Service{Pool: f.s.Pool, Log: quiet}
	f.customer = httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		svc.MountCustomer(r, identity.RequireActor(f.s.Signer, identity.TypeCustomer))
	}))
	f.rider = httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		svc.MountRider(r, identity.RequireActor(f.s.Signer, identity.TypeRider))
	}))
	t.Cleanup(f.customer.Close)
	t.Cleanup(f.rider.Close)
	return f
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.s.Pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.s.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// session signs an actor in, with no phone sign-in needed.
func (f *fixture) session(actorType, id string) (string, identity.Actor) {
	f.t.Helper()
	var pair identity.Pair
	var actor identity.Actor
	err := database.InTx(context.Background(), f.s.Pool, func(tx pgx.Tx) error {
		var err error
		pair, actor, err = f.s.Start(context.Background(), tx, actorType, id)
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return pair.AccessToken, actor
}

func (f *fixture) newCustomer(phone any) (id, token string) {
	f.t.Helper()
	ctx := context.Background()
	var person string
	if err := f.s.Pool.QueryRow(ctx, "INSERT INTO people (phone) VALUES ($1) RETURNING id::text", phone).Scan(&person); err != nil {
		f.t.Fatal(err)
	}
	if err := f.s.Pool.QueryRow(ctx, "INSERT INTO customers (person_id) VALUES ($1) RETURNING id::text", person).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	token, _ = f.session(identity.TypeCustomer, id)
	return id, token
}

func (f *fixture) newRider(phone string) (id, token string) {
	f.t.Helper()
	ctx := context.Background()
	var person string
	if err := f.s.Pool.QueryRow(ctx, "INSERT INTO people (phone) VALUES ($1) RETURNING id::text", phone).Scan(&person); err != nil {
		f.t.Fatal(err)
	}
	if err := f.s.Pool.QueryRow(ctx, "INSERT INTO riders (person_id) VALUES ($1) RETURNING id::text", person).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	token, _ = f.session(identity.TypeRider, id)
	return id, token
}

// send uses a new Idempotency-Key each time.
func (f *fixture) send(srv *httptest.Server, method, path, token, body string) (int, map[string]any) {
	f.t.Helper()
	f.keys++
	return f.sendKey(srv, method, path, token, body, fmt.Sprintf("test-key-%04d", f.keys))
}

func (f *fixture) sendKey(srv *httptest.Server, method, path, token, body, key string) (int, map[string]any) {
	f.t.Helper()
	req, err := http.NewRequest(method, srv.URL+"/v1"+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func contractRequired(t *testing.T, app, schema string) []string {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/" + app + "/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Required []string `yaml:"required"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	fields := doc.Components.Schemas[schema].Required
	if len(fields) == 0 {
		t.Fatalf("no required fields for %s, so this check would prove nothing", schema)
	}
	return fields
}

func checkFields(t *testing.T, got map[string]any, app, schema string) {
	t.Helper()
	for _, field := range contractRequired(t, app, schema) {
		if _, ok := got[field]; !ok {
			t.Errorf("%s has no %s: %v", schema, field, got)
		}
	}
}

func TestGettingACustomer(t *testing.T) {
	f := newFixture(t)
	id, token := f.newCustomer("+919800000001")
	code, out := f.send(f.customer, "GET", "/me", token, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	checkFields(t, out, "customer", "CustomerResponse")
	c := out["customer"].(map[string]any)
	checkFields(t, c, "customer", "Customer")
	if c["id"] != id || c["phone"] != "+919800000001" || c["sign_in_method"] != "phone" || c["email"] != nil {
		t.Errorf("%v", c)
	}
}

func TestAGoogleCustomerHasNoPhoneYet(t *testing.T) {
	f := newFixture(t)
	_, token := f.newCustomer(nil)
	_, out := f.send(f.customer, "GET", "/me", token, "")
	if c := out["customer"].(map[string]any); c["phone"] != nil {
		t.Errorf("%v", c)
	}
}

func TestSignedOutAndBlockedCustomers(t *testing.T) {
	f := newFixture(t)
	if code, _ := f.send(f.customer, "GET", "/me", "", ""); code != 401 {
		t.Errorf("no token: %d", code)
	}
	_, rider := f.newRider("+919800000002")
	if code, _ := f.send(f.customer, "GET", "/me", rider, ""); code != 401 {
		t.Errorf("a rider's token: %d", code)
	}
	id, token := f.newCustomer("+919800000001")
	f.exec("UPDATE people SET status = 'blocked' WHERE phone = '+919800000001'")
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/me", ""},
		{"PATCH", "/me", `{"name":"A"}`},
		{"POST", "/me/devices", `{"platform":"android","push_token":"t"}`},
	} {
		code, out := f.send(f.customer, c.method, c.path, token, c.body)
		if code != 403 || out["code"] != "account_blocked" {
			t.Errorf("%s %s: %d %v", c.method, c.path, code, out)
		}
	}
	f.exec("UPDATE people SET status = 'active'")
	f.exec("UPDATE refresh_tokens SET revoked_at = now()")
	f.exec("DELETE FROM customers WHERE id = $1", id)
	if code, _ := f.send(f.customer, "GET", "/me", token, ""); code != 401 {
		t.Errorf("a removed customer: %d", code)
	}
}

func TestUpdatingACustomer(t *testing.T) {
	f := newFixture(t)
	id, token := f.newCustomer("+919800000001")
	code, out := f.send(f.customer, "PATCH", "/me", token, `{"name":"  Asha  ","email":"asha@example.com"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	c := out["customer"].(map[string]any)
	if c["name"] != "Asha" || c["email"] != "asha@example.com" || c["id"] != id {
		t.Errorf("%v", c)
	}
	// Each can change alone, and the other stays.
	_, out = f.send(f.customer, "PATCH", "/me", token, `{"name":"Asha R"}`)
	if c := out["customer"].(map[string]any); c["name"] != "Asha R" || c["email"] != "asha@example.com" {
		t.Errorf("%v", c)
	}
	_, out = f.send(f.customer, "PATCH", "/me", token, `{"email":null}`)
	if c := out["customer"].(map[string]any); c["name"] != "Asha R" || c["email"] != nil {
		t.Errorf("clearing the email: %v", c)
	}
	var after string
	if err := f.s.Pool.QueryRow(context.Background(), "SELECT after::text FROM audit_events WHERE action = 'customer.updated' ORDER BY created_at LIMIT 1").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after, "name") || !strings.Contains(after, "email") {
		t.Errorf("the audit row must say what changed: %s", after)
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE after::text LIKE '%Asha%' OR after::text LIKE '%example.com%'"); n != 0 {
		t.Error("personal data was kept in the audit log for good")
	}
}

func TestBadProfileValuesAreRefused(t *testing.T) {
	f := newFixture(t)
	_, token := f.newCustomer("+919800000001")
	long := strings.Repeat("a", 81)
	longEmail := strings.Repeat("a", 250) + "@b.co"
	for _, body := range []string{
		`{}`, `not json`, `{"other":"x"}`,
		`{"name":""}`, `{"name":"   "}`, `{"name":null}`, `{"name":5}`, `{"name":"` + long + `"}`, `{"name":"a\u0000b"}`, `{"name":"a\nb"}`, `{"name":"\u200b"}`, `{"name":"a\u202eb"}`, `{"name":"..."}`,
		`{"email":""}`, `{"email":"nope"}`, `{"email":5}`, `{"email":"A <a@b.co>"}`, `{"email":"a\n@b.co"}`, `{"email":"` + longEmail + `"}`,
	} {
		code, out := f.send(f.customer, "PATCH", "/me", token, body)
		if code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", body[:min(len(body), 30)], code, out)
		}
	}
	if code, _ := f.send(f.customer, "PATCH", "/me", token, `{"name":"क्\u200dष"}`); code != 200 {
		t.Errorf("a joiner in an Indian script: %d", code)
	}
	if code, _ := f.send(f.customer, "PATCH", "/me", token, `{"name":"`+strings.Repeat("क", 80)+`"}`); code != 200 {
		t.Errorf("80 Hindi letters fit: %d", code)
	}
}

func TestAProfileChangeIsSafeToRetry(t *testing.T) {
	f := newFixture(t)
	_, token := f.newCustomer("+919800000001")
	code, first := f.sendKey(f.customer, "PATCH", "/me", token, `{"name":"A"}`, "retry-key-0001")
	if code != 200 {
		t.Fatal(code)
	}
	code, second := f.sendKey(f.customer, "PATCH", "/me", token, `{"name":"A"}`, "retry-key-0001")
	if code != 200 || second["server_time"] != first["server_time"] {
		t.Errorf("not the stored reply: %d %v", code, second)
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'customer.updated'"); n != 1 {
		t.Errorf("%d changes recorded", n)
	}
	if code, out := f.sendKey(f.customer, "PATCH", "/me", token, `{"name":"B"}`, "retry-key-0001"); code != 422 || out["code"] != "idempotency_key_reused" {
		t.Errorf("%d %v", code, out)
	}
	if code, out := f.sendKey(f.customer, "PATCH", "/me", token, `{"name":"B"}`, ""); code != 400 || out["code"] != "idempotency_key_required" {
		t.Errorf("%d %v", code, out)
	}
}

func TestAnEndedSessionCannotChangeAnything(t *testing.T) {
	f := newFixture(t)
	_, token := f.newCustomer("+919800000001")
	f.exec("UPDATE refresh_tokens SET revoked_at = now()")
	if code, _ := f.send(f.customer, "PATCH", "/me", token, `{"name":"A"}`); code != 401 {
		t.Errorf("profile: %d", code)
	}
	if code, _ := f.send(f.customer, "GET", "/me", token, ""); code != 401 {
		t.Errorf("reading a profile: %d", code)
	}
	_, rider := f.newRider("+919800000002")
	f.exec("UPDATE refresh_tokens SET revoked_at = now()")
	if code, _ := f.send(f.rider, "GET", "/riders/me", rider, ""); code != 401 {
		t.Errorf("reading a rider: %d", code)
	}
	if code, _ := f.send(f.customer, "POST", "/me/devices", token, `{"platform":"android","push_token":"stranger"}`); code != 401 {
		t.Errorf("device: %d", code)
	}
	if n := f.count("SELECT count(*) FROM devices"); n != 0 {
		t.Error("an ended session added a device, so it would get someone's notifications")
	}
}

func TestDevices(t *testing.T) {
	f := newFixture(t)
	a, tokenA := f.newCustomer("+919800000001")
	b, tokenB := f.newCustomer("+919800000002")
	code, out := f.send(f.customer, "POST", "/me/devices", tokenA, `{"platform":"android","push_token":"phone-1"}`)
	if code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	checkFields(t, out, "customer", "DeviceResponse")
	id := out["device_id"].(string)
	// The same token again updates it.
	code, out = f.send(f.customer, "POST", "/me/devices", tokenA, `{"platform":"ios","push_token":"phone-1"}`)
	if code != 201 || out["device_id"] != id {
		t.Errorf("%d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM devices WHERE platform = 'ios'"); n != 1 {
		t.Errorf("%d", n)
	}
	// Someone else cannot remove it.
	if code, _ := f.send(f.customer, "DELETE", "/me/devices/"+id, tokenB, ""); code != 404 {
		t.Errorf("another customer: %d", code)
	}
	// Whoever signs in on that phone next owns its notifications.
	if code, out := f.send(f.customer, "POST", "/me/devices", tokenB, `{"platform":"android","push_token":"phone-1"}`); code != 201 || out["device_id"] != id {
		t.Errorf("%d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM devices WHERE actor_id = $1", a); n != 0 {
		t.Errorf("the old owner kept it: %d", n)
	}
	if n := f.count("SELECT count(*) FROM devices WHERE actor_id = $1", b); n != 1 {
		t.Errorf("%d", n)
	}
	if code, out := f.send(f.customer, "DELETE", "/me/devices/"+id, tokenB, ""); code != 200 {
		t.Errorf("%d %v", code, out)
	}
	if code, _ := f.send(f.customer, "DELETE", "/me/devices/"+id, tokenB, ""); code != 404 {
		t.Errorf("already gone: %d", code)
	}
	if code, _ := f.send(f.customer, "DELETE", "/me/devices/not-an-id", tokenB, ""); code != 404 {
		t.Errorf("not an ID: %d", code)
	}
}

func TestBadDevicesAreRefused(t *testing.T) {
	f := newFixture(t)
	_, token := f.newCustomer("+919800000001")
	for _, body := range []string{
		`{}`, `not json`, `{"platform":"android"}`, `{"push_token":"t"}`, `{"platform":"windows","push_token":"t"}`,
		`{"platform":"android","push_token":""}`, `{"platform":"android","push_token":"a\u0000b"}`,
		`{"platform":"android","push_token":"` + strings.Repeat("a", maxPushToken+1) + `"}`,
	} {
		if code, out := f.send(f.customer, "POST", "/me/devices", token, body); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", body[:min(len(body), 30)], code, out)
		}
	}
}

func TestDevicesAreBounded(t *testing.T) {
	f := newFixture(t)
	_, token := f.newCustomer("+919800000001")
	var first string
	for i := 0; i <= maxDevices; i++ {
		code, out := f.send(f.customer, "POST", "/me/devices", token, fmt.Sprintf(`{"platform":"android","push_token":"phone-%d"}`, i))
		if code != 201 {
			t.Fatalf("%d %v", code, out)
		}
		if i == 0 {
			first = out["device_id"].(string)
		}
		f.exec("UPDATE devices SET updated_at = now() - make_interval(secs => $1) WHERE push_token = $2", maxDevices-i, fmt.Sprintf("phone-%d", i))
	}
	if n := f.count("SELECT count(*) FROM devices"); n != maxDevices {
		t.Errorf("%d devices", n)
	}
	if n := f.count("SELECT count(*) FROM devices WHERE id = $1", first); n != 0 {
		t.Error("the device unused for longest should go")
	}
}

func TestADeviceCanBeRemovedAfterSignOut(t *testing.T) {
	f := newFixture(t)
	_, token := f.newCustomer("+919800000001")
	_, out := f.send(f.customer, "POST", "/me/devices", token, `{"platform":"android","push_token":"phone"}`)
	f.exec("UPDATE refresh_tokens SET revoked_at = now()")
	if code, _ := f.send(f.customer, "DELETE", "/me/devices/"+out["device_id"].(string), token, ""); code != 200 {
		t.Errorf("got %d", code)
	}
}

func TestGettingARider(t *testing.T) {
	f := newFixture(t)
	id, token := f.newRider("+919800000002")
	code, out := f.send(f.rider, "GET", "/riders/me", token, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	checkFields(t, out, "rider", "RiderResponse")
	r := out["rider"].(map[string]any)
	checkFields(t, r, "rider", "Rider")
	if r["id"] != id || r["status"] != "applicant" || r["name"] != nil || r["zone_id"] != nil {
		t.Errorf("%v", r)
	}
	if code, _ := f.send(f.rider, "GET", "/riders/me", "", ""); code != 401 {
		t.Errorf("no token: %d", code)
	}
	_, customer := f.newCustomer("+919800000003")
	if code, _ := f.send(f.rider, "GET", "/riders/me", customer, ""); code != 401 {
		t.Errorf("a customer's token: %d", code)
	}
}

func TestSuspendedAndBlockedRiders(t *testing.T) {
	f := newFixture(t)
	_, token := f.newRider("+919800000002")
	f.exec("UPDATE riders SET status = 'suspended'")
	code, out := f.send(f.rider, "GET", "/riders/me", token, "")
	if code != 200 || out["rider"].(map[string]any)["status"] != "suspended" {
		t.Errorf("a suspended rider must see why: %d %v", code, out)
	}
	if code, out := f.send(f.rider, "POST", "/riders/me/devices", token, `{"platform":"android","push_token":"t"}`); code != 403 || out["code"] != "account_blocked" {
		t.Errorf("%d %v", code, out)
	}
	f.exec("UPDATE riders SET status = 'active'")
	f.exec("UPDATE people SET status = 'blocked'")
	if code, out := f.send(f.rider, "GET", "/riders/me", token, ""); code != 403 || out["code"] != "account_blocked" {
		t.Errorf("%d %v", code, out)
	}
}

func TestRiderDevicesAreSeparateFromTheirCustomerProfile(t *testing.T) {
	f := newFixture(t)
	riderID, riderToken := f.newRider("+919800000002")
	_, customerToken := f.newCustomer("+919800000003")
	code, out := f.send(f.rider, "POST", "/riders/me/devices", riderToken, `{"platform":"android","push_token":"rider-app"}`)
	if code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	checkFields(t, out, "rider", "DeviceResponse")
	id := out["device_id"].(string)
	if n := f.count("SELECT count(*) FROM devices WHERE actor_type = 'rider' AND actor_id = $1", riderID); n != 1 {
		t.Errorf("%d", n)
	}
	if code, _ := f.send(f.customer, "DELETE", "/me/devices/"+id, customerToken, ""); code != 404 {
		t.Errorf("a customer removed a rider's device: %d", code)
	}
	if code, _ := f.send(f.rider, "DELETE", "/riders/me/devices/"+id, riderToken, ""); code != 200 {
		t.Errorf("%d", code)
	}
}

func TestKeysBelongToOneUser(t *testing.T) {
	f := newFixture(t)
	_, a := f.newCustomer("+919800000001")
	_, b := f.newCustomer("+919800000002")
	_, first := f.sendKey(f.customer, "PATCH", "/me", a, `{"name":"A"}`, "shared-key-0001")
	code, second := f.sendKey(f.customer, "PATCH", "/me", b, `{"name":"A"}`, "shared-key-0001")
	if code != 200 {
		t.Fatalf("%d %v", code, second)
	}
	if first["customer"].(map[string]any)["id"] == second["customer"].(map[string]any)["id"] {
		t.Error("one customer got another's stored reply")
	}
}

// Verification is the contract's stand-in until DigiLocker (#156): start records a pending request.
func TestVerificationStatusAndStart(t *testing.T) {
	f := newFixture(t)
	id, token := f.newRider("+919811200001")
	code, out := f.send(f.rider, "GET", "/riders/me/verification", token, "")
	if code != 200 || out["status"] != "pending" || out["reason"] != "not_started" {
		t.Fatalf("nothing started: %d %v", code, out)
	}
	checkFields(t, out, "rider", "VerificationResponse")
	if code, _ := f.send(f.rider, "POST", "/riders/me/verification/start", token, `{"method":"post"}`); code != 400 {
		t.Errorf("an unknown method: %d", code)
	}
	code, out = f.sendKey(f.rider, "POST", "/riders/me/verification/start", token, `{"method":"manual_upload"}`, "verify-start-0001")
	if code != 200 || out["redirect_url"] != nil || out["upload_url"] != nil {
		t.Fatalf("%d %v", code, out)
	}
	checkFields(t, out, "rider", "VerificationStart")
	if again, replay := f.sendKey(f.rider, "POST", "/riders/me/verification/start", token, `{"method":"manual_upload"}`, "verify-start-0001"); again != 200 || replay["server_time"] != out["server_time"] {
		t.Errorf("a replay must give the same answer: %d %v", again, replay)
	}
	if f.count("SELECT count(*) FROM rider_verifications WHERE rider_id = $1 AND status = 'pending'", id) != 1 {
		t.Error("one pending request")
	}
	if code, out := f.sendKey(f.rider, "POST", "/riders/me/verification/start", token, `{"method":"digilocker"}`, "verify-start-0002"); code != 409 || out["code"] != "verification_in_progress" {
		t.Errorf("a second start while checked: %d %v", code, out)
	}
	if _, out := f.send(f.rider, "GET", "/riders/me/verification", token, ""); out["status"] != "pending" || out["reason"] != nil {
		t.Errorf("being checked: %v", out)
	}
	// Ops mark it verified; then expired once past its date; a failed one can start again.
	f.exec("INSERT INTO rider_verifications (rider_id, status, method) VALUES ($1, 'verified', 'manual_upload')", id)
	if code, out := f.sendKey(f.rider, "POST", "/riders/me/verification/start", token, `{"method":"digilocker"}`, "verify-start-0003"); code != 409 || out["code"] != "already_verified" {
		t.Errorf("%d %v", code, out)
	}
	f.exec("UPDATE rider_verifications SET expires_at = now() - interval '1 day' WHERE rider_id = $1 AND status = 'verified'", id)
	if _, out := f.send(f.rider, "GET", "/riders/me/verification", token, ""); out["status"] != "expired" {
		t.Errorf("past its date: %v", out)
	}
	if code, _ := f.sendKey(f.rider, "POST", "/riders/me/verification/start", token, `{"method":"digilocker"}`, "verify-start-0004"); code != 200 {
		t.Errorf("an expired verification can start again: %d", code)
	}
	if code, _ := f.send(f.rider, "GET", "/riders/me/verification", "", ""); code != 401 {
		t.Errorf("signed out: %d", code)
	}
}
