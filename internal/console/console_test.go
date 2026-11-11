package console

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type fixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	svc  *Service
	srv  *httptest.Server
	now  time.Time
	n    int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, pool: dbtest.Fresh(t), now: time.Now()}
	sessions := &identity.Sessions{Pool: f.pool, Log: quiet, RefreshTTL: time.Hour,
		Signer: identity.Signer{Key: []byte("a-test-key-that-is-long-enough-to-use"), TTL: 15 * time.Minute}}
	sealer, err := NewSealer([]byte("a-test-key-that-is-long-enough-to-use"))
	if err != nil {
		t.Fatal(err)
	}
	f.svc = &Service{Pool: f.pool, Log: quiet, Sessions: sessions, Sealer: sealer, now: func() time.Time { return f.now }}
	f.srv = httptest.NewServer(httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		signedIn := identity.RequireActor(sessions.Signer, identity.TypeConsole)
		f.svc.MountAuth(r, signedIn)
		f.svc.MountUsers(r, signedIn)
		f.svc.MountApprovals(r, Kinds{"flag": ApplyFlag, "settings": ApplySettings, "kill_switch": ApplyKillSwitch}, signedIn)
		f.svc.MountFlags(r, signedIn)
		f.svc.MountRiders(r, signedIn)
		f.svc.MountZones(r, signedIn)
		f.svc.MountMenu(r, signedIn)
		f.svc.MountGates(r, signedIn)
		f.svc.MountHubs(r, signedIn)
		f.svc.MountReviews(r, signedIn)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) call(method, path, token, body string) (int, map[string]any) {
	f.t.Helper()
	f.n++
	req, _ := http.NewRequest(method, f.srv.URL+"/v1"+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if method != "GET" {
		req.Header.Set("Idempotency-Key", fmt.Sprintf("console-key-%05d", f.n))
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

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// user adds a console user and returns their id and temporary password.
func (f *fixture) user(login, role string) (string, string) {
	f.t.Helper()
	var id, temp string
	if err := database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		var err error
		var hub *string
		if role == "hub_manager" {
			var h string
			if err := tx.QueryRow(context.Background(), `WITH z AS (INSERT INTO zones (name, city, status) VALUES ('Z-'||$1, 'Bengaluru', 'active') RETURNING id)
				INSERT INTO hubs (zone_id, name, lat, lng) SELECT id, 'Hub', 12.93, 77.62 FROM z RETURNING id::text`, login).Scan(&h); err != nil {
				return err
			}
			hub = &h
		}
		id, temp, err = CreateUser(context.Background(), tx, "", login, "Name "+login, role, hub)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return id, temp
}

// signIn runs the whole first sign-in: password, enrolment, code, and the password change. It
// returns a token that can do everything the role allows.
func (f *fixture) signIn(login, password string) string {
	f.t.Helper()
	code, out := f.call("POST", "/auth/login", "", `{"login_id":"`+login+`","password":"`+password+`"}`)
	if code != 200 {
		f.t.Fatalf("login: %d %v", code, out)
	}
	challenge := out["challenge_id"].(string)
	secret := f.secret(challenge, out["totp_enrolled"] == true, login)
	f.now = f.now.Add(31 * time.Second) // A fresh step, so a code is never reused.
	code, out = f.call("POST", "/auth/totp/verify", "", `{"challenge_id":"`+challenge+`","code":"`+TOTPCode(secret, StepOf(f.now))+`"}`)
	if code != 200 {
		f.t.Fatalf("verify: %d %v", code, out)
	}
	token := out["access_token"].(string)
	if out["must_change_password"] == true {
		if code, out := f.call("POST", "/me/password", token, `{"current_password":"`+password+`","new_password":"`+password+`-changed-now"}`); code != 200 {
			f.t.Fatalf("password: %d %v", code, out)
		}
	}
	return token
}

// secrets keeps each user's authenticator secret, as their phone would.
var secrets = map[string][]byte{}

func (f *fixture) secret(challenge string, enrolled bool, login string) []byte {
	f.t.Helper()
	key := fmt.Sprintf("%p-%s", f, login)
	if enrolled {
		return secrets[key]
	}
	code, out := f.call("POST", "/auth/totp/enroll", "", `{"challenge_id":"`+challenge+`"}`)
	if code != 200 {
		f.t.Fatalf("enroll: %d %v", code, out)
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(out["secret"].(string))
	if err != nil {
		f.t.Fatal(err)
	}
	secrets[key] = secret
	return secret
}

func TestTheRFC6238Vector(t *testing.T) {
	// RFC 6238, appendix B: SHA1, the secret "12345678901234567890", T = 59 seconds, 94287082.
	if got := TOTPCode([]byte("12345678901234567890"), StepOf(time.Unix(59, 0))); got != "287082" {
		t.Errorf("%s, want the last six digits of 94287082", got)
	}
	if _, ok := MatchTOTP([]byte("12345678901234567890"), "287082", time.Unix(59, 0), StepOf(time.Unix(59, 0))); ok {
		t.Error("a code was accepted twice")
	}
}

func TestPasswords(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(h, "correct horse battery") || CheckPassword(h, "correct horse batterY") || CheckPassword("plain", "plain") {
		t.Error("password check")
	}
	if strings.Contains(h, "correct") {
		t.Error("the hash holds the password")
	}
}

func TestTwoStepSignIn(t *testing.T) {
	f := newFixture(t)
	_, temp := f.user("asha.k", "admin")
	for _, body := range []string{`{"login_id":"asha.k","password":"wrong-password"}`, `{"login_id":"nobody","password":"wrong-password"}`} {
		if code, out := f.call("POST", "/auth/login", "", body); code != 401 || out["code"] != "invalid_credentials" {
			t.Errorf("%s: %d %v", body, code, out)
		}
	}
	code, out := f.call("POST", "/auth/login", "", `{"login_id":"Asha.K","password":"`+temp+`"}`)
	if code != 200 || out["access_token"] != nil || out["totp_enrolled"] != false || out["must_change_password"] != true {
		t.Fatalf("a password alone must never give a session: %d %v", code, out)
	}
	challenge := out["challenge_id"].(string)
	if code, out := f.call("POST", "/auth/totp/verify", "", `{"challenge_id":"`+challenge+`","code":"123456"}`); code != 409 || out["code"] != "challenge_expired" {
		t.Errorf("verify before enrolling: %d %v", code, out)
	}
	secret := f.secret(challenge, false, "asha.k")
	code, out = f.call("POST", "/auth/totp/verify", "", `{"challenge_id":"`+challenge+`","code":"`+TOTPCode(secret, StepOf(f.now))+`"}`)
	if code != 200 || out["role"] != "admin" {
		t.Fatalf("%d %v", code, out)
	}
	token := out["access_token"].(string)
	// The temporary password must go first.
	if code, out := f.call("GET", "/admin/users", token, ""); code != 403 || out["code"] != "password_change_required" {
		t.Errorf("%d %v", code, out)
	}
	if code, out := f.call("GET", "/admin/me", token, ""); code != 200 || len(out["permissions"].([]any)) < 10 {
		t.Errorf("getConsoleUser works before the change: %d %v", code, out)
	}
	if code, _ := f.call("POST", "/me/password", token, `{"current_password":"`+temp+`","new_password":"short"}`); code != 400 {
		t.Errorf("a weak password: %d", code)
	}
	if code, _ := f.call("POST", "/me/password", token, `{"current_password":"wrong-one","new_password":"a-long-enough-password"}`); code != 401 {
		t.Errorf("a wrong current password: %d", code)
	}
	if code, _ := f.call("POST", "/me/password", token, `{"current_password":"`+temp+`","new_password":"a-long-enough-password"}`); code != 200 {
		t.Fatal("the password change")
	}
	if code, _ := f.call("GET", "/admin/users", token, ""); code != 200 {
		t.Errorf("after the change: %d", code)
	}
	// The same code never works twice, even on a new challenge.
	_, out = f.call("POST", "/auth/login", "", `{"login_id":"asha.k","password":"a-long-enough-password"}`)
	if out["totp_enrolled"] != true || out["must_change_password"] != false {
		t.Errorf("%v", out)
	}
	if code, out := f.call("POST", "/auth/totp/verify", "", `{"challenge_id":"`+out["challenge_id"].(string)+`","code":"`+TOTPCode(secret, StepOf(f.now))+`"}`); code != 400 || out["code"] != "invalid_totp" {
		t.Errorf("a replayed code: %d %v", code, out)
	}
	if code, _ := f.call("POST", "/auth/totp/enroll", "", `{"challenge_id":"`+challenge+`"}`); code != 409 {
		t.Errorf("a used challenge: %d", code)
	}
}

func TestWrongCodesEndTheChallenge(t *testing.T) {
	f := newFixture(t)
	_, temp := f.user("ravi.m", "support")
	_, out := f.call("POST", "/auth/login", "", `{"login_id":"ravi.m","password":"`+temp+`"}`)
	challenge := out["challenge_id"].(string)
	f.secret(challenge, false, "ravi.m")
	for i := 0; i < 4; i++ {
		if code, out := f.call("POST", "/auth/totp/verify", "", `{"challenge_id":"`+challenge+`","code":"000000"}`); code != 400 {
			t.Fatalf("try %d: %d %v", i, code, out)
		}
	}
	if code, out := f.call("POST", "/auth/totp/verify", "", `{"challenge_id":"`+challenge+`","code":"000000"}`); code != 429 || out["code"] != "totp_attempts_exceeded" {
		t.Errorf("%d %v", code, out)
	}
	if code, _ := f.call("POST", "/auth/totp/verify", "", `{"challenge_id":"`+challenge+`","code":"`+TOTPCode(secrets[fmt.Sprintf("%p-ravi.m", f)], StepOf(f.now))+`"}`); code != 409 {
		t.Errorf("the right code after the limit: %d", code)
	}
	// Five wrong codes also lock the account, as five wrong passwords do.
	if code, _ := f.call("POST", "/auth/login", "", `{"login_id":"ravi.m","password":"`+temp+`"}`); code != 401 {
		t.Errorf("wrong codes must count toward the lockout: %d", code)
	}
	f.now = f.now.Add(16 * time.Minute)
	// A challenge also ends after 5 minutes.
	_, out = f.call("POST", "/auth/login", "", `{"login_id":"ravi.m","password":"`+temp+`"}`)
	f.now = f.now.Add(6 * time.Minute)
	if code, _ := f.call("POST", "/auth/totp/enroll", "", `{"challenge_id":"`+out["challenge_id"].(string)+`"}`); code != 409 {
		t.Errorf("an expired challenge: %d", code)
	}
}

func TestATimedLockout(t *testing.T) {
	f := newFixture(t)
	id, temp := f.user("lock.me", "ops")
	for i := 0; i < 5; i++ {
		f.call("POST", "/auth/login", "", `{"login_id":"lock.me","password":"guess-number-`+fmt.Sprint(i)+`"}`)
	}
	if code, out := f.call("POST", "/auth/login", "", `{"login_id":"lock.me","password":"another-guess"}`); code != 401 || out["code"] != "invalid_credentials" {
		t.Errorf("a wrong password says nothing about the lock: %d %v", code, out)
	}
	if code, out := f.call("POST", "/auth/login", "", `{"login_id":"lock.me","password":"`+temp+`"}`); code != 401 || out["code"] != "invalid_credentials" {
		t.Errorf("a locked account answers like a wrong password, even to the right one: %d %v", code, out)
	}
	if f.count("SELECT count(*) FROM console_users WHERE id = $1 AND locked_until IS NOT NULL", id) != 1 {
		t.Error("no lock recorded")
	}
	f.now = f.now.Add(16 * time.Minute)
	if code, _ := f.call("POST", "/auth/login", "", `{"login_id":"lock.me","password":"`+temp+`"}`); code != 200 {
		t.Errorf("the lock must end on its own: %d", code)
	}
}

func TestPermissionsAreCheckedOnEveryCall(t *testing.T) {
	f := newFixture(t)
	_, temp := f.user("sup.one", "support")
	token := f.signIn("sup.one", temp)
	if code, out := f.call("GET", "/admin/users", token, ""); code != 403 || out["missing_permission"] != "users.manage" {
		t.Errorf("%d %v", code, out)
	}
	if code, out := f.call("GET", "/admin/flags", token, ""); code != 403 || out["missing_permission"] != "flags.manage" {
		t.Errorf("%d %v", code, out)
	}
	if code, _ := f.call("GET", "/admin/users", "", ""); code != 401 {
		t.Errorf("no token: %d", code)
	}
}

func TestManagingUsers(t *testing.T) {
	f := newFixture(t)
	adminID, temp := f.user("boss.one", "admin")
	token := f.signIn("boss.one", temp)
	code, out := f.call("POST", "/admin/users", token, `{"login_id":"new.ops","name":"New Ops","role":"ops"}`)
	if code != 201 || out["temporary_password"] == nil || out["user"].(map[string]any)["role"] != "ops" {
		t.Fatalf("%d %v", code, out)
	}
	newID := out["user"].(map[string]any)["id"].(string)
	if code, out := f.call("POST", "/admin/users", token, `{"login_id":"new.ops","name":"Again","role":"ops"}`); code != 409 || out["code"] != "login_id_taken" {
		t.Errorf("%d %v", code, out)
	}
	if code, _ := f.call("POST", "/admin/users", token, `{"login_id":"hub.mgr","name":"Hub","role":"hub_manager"}`); code != 400 {
		t.Errorf("a hub_manager with no hub: %d", code)
	}
	if code, _ := f.call("GET", "/admin/users?limit=1", token, ""); code != 200 {
		t.Errorf("paging: %d", code)
	}
	_, page := f.call("GET", "/admin/users?limit=1", token, "")
	next, _ := page["next_cursor"].(string)
	if next == "" {
		t.Error("a second page is missing")
	} else if _, more := f.call("GET", "/admin/users?limit=1&cursor="+next, token, ""); len(more["users"].([]any)) != 1 || more["users"].([]any)[0].(map[string]any)["id"] == page["users"].([]any)[0].(map[string]any)["id"] {
		t.Errorf("the second page repeats the first: %v", more)
	}
	if code, out := f.call("PATCH", "/admin/users/"+adminID, token, `{"role":"ops","reason":"stepping down"}`); code != 409 || out["code"] != "last_admin" {
		t.Errorf("demoting yourself: %d %v", code, out)
	}
	// The new user signs in, then is deactivated: their session ends.
	opsTemp := out["temporary_password"]
	_ = opsTemp
	_, created := f.call("POST", "/admin/users/"+newID+"/reset", token, `{"reason":"set a known password"}`)
	opsToken := f.signIn("new.ops", created["temporary_password"].(string))
	if code, _ := f.call("PATCH", "/admin/users/"+newID, token, `{"active":false,"reason":"left the team"}`); code != 200 {
		t.Fatalf("%d", code)
	}
	if code, _ := f.call("GET", "/admin/me", opsToken, ""); code != 401 {
		t.Errorf("a deactivated user still works: %d", code)
	}
	// A reset clears the authenticator and gives a new temporary password.
	code, out = f.call("POST", "/admin/users/"+newID+"/reset", token, `{"reason":"lost the phone"}`)
	if code != 200 || out["user"].(map[string]any)["totp_enrolled"] != false {
		t.Errorf("%d %v", code, out)
	}
}

func TestApprovalsOnAFlag(t *testing.T) {
	f := newFixture(t)
	_, tempA := f.user("admin.a", "admin")
	_, tempB := f.user("admin.b", "admin")
	a := f.signIn("admin.a", tempA)
	b := f.signIn("admin.b", tempB)
	code, out := f.call("PUT", "/admin/flags/eta_offers", a, `{"enabled":true,"reason":"try it out"}`)
	if code != 202 || out["approval"].(map[string]any)["status"] != "pending" {
		t.Fatalf("%d %v", code, out)
	}
	ap := out["approval"].(map[string]any)
	change := ap["change"].(map[string]any)
	if change["kind"] != "flag" || change["enabled_before"] != false || change["enabled_after"] != true || ap["request_reason"] != "try it out" {
		t.Errorf("%v", ap)
	}
	id := ap["id"].(string)
	if code, out := f.call("PUT", "/admin/flags/eta_offers", b, `{"enabled":true,"reason":"again"}`); code != 409 || out["code"] != "approval_pending" {
		t.Errorf("a second request: %d %v", code, out)
	}
	if code, out := f.call("POST", "/admin/approvals/"+id+"/approve", a, ""); code != 409 || out["code"] != "own_change" {
		t.Errorf("approving your own: %d %v", code, out)
	}
	if on, _ := FlagOn(context.Background(), f.pool, "eta_offers"); on {
		t.Fatal("applied before approval")
	}
	code, out = f.call("POST", "/admin/approvals/"+id+"/approve", b, "")
	if code != 200 || out["approval"].(map[string]any)["status"] != "approved" || out["approval"].(map[string]any)["decided_by"].(map[string]any)["name"] != "Name admin.b" {
		t.Fatalf("%d %v", code, out)
	}
	if on, _ := FlagOn(context.Background(), f.pool, "eta_offers"); !on {
		t.Error("not applied")
	}
	if code, _ := f.call("POST", "/admin/approvals/"+id+"/approve", b, ""); code != 409 {
		t.Errorf("approving twice: %d", code)
	}
	// A change the world moved past fails, and nothing changes.
	_, out = f.call("PUT", "/admin/flags/eta_offers", a, `{"enabled":false,"reason":"turn it off"}`)
	stale := out["approval"].(map[string]any)["id"].(string)
	f.exec("UPDATE feature_flags SET enabled = false WHERE key = 'eta_offers'")
	if _, out := f.call("POST", "/admin/approvals/"+stale+"/approve", b, ""); out["approval"].(map[string]any)["status"] != "failed" || out["approval"].(map[string]any)["failure_code"] != "stale_version" {
		t.Errorf("%v", out)
	}
	// Rejected with a reason, cancelled by the requester, lapsed after a day.
	_, out = f.call("PUT", "/admin/flags/eta_offers", a, `{"enabled":true,"reason":"once more"}`)
	rej := out["approval"].(map[string]any)["id"].(string)
	if _, out := f.call("POST", "/admin/approvals/"+rej+"/reject", b, `{"reason":"not now"}`); out["approval"].(map[string]any)["decision_reason"] != "not now" {
		t.Errorf("%v", out)
	}
	_, out = f.call("PUT", "/admin/flags/eta_offers", a, `{"enabled":true,"reason":"and again"}`)
	can := out["approval"].(map[string]any)["id"].(string)
	if code, _ := f.call("POST", "/admin/approvals/"+can+"/cancel", b, ""); code != 404 {
		t.Errorf("someone else cancelling: %d", code)
	}
	if _, out := f.call("POST", "/admin/approvals/"+can+"/cancel", a, ""); out["approval"].(map[string]any)["status"] != "cancelled" {
		t.Errorf("%v", out)
	}
	_, out = f.call("PUT", "/admin/flags/eta_offers", a, `{"enabled":true,"reason":"last try"}`)
	old := out["approval"].(map[string]any)["id"].(string)
	f.now = f.now.Add(25 * time.Hour)
	if code, out := f.call("POST", "/admin/approvals/"+old+"/approve", b, ""); code != 409 || out["code"] != "not_pending" {
		t.Errorf("a lapsed change: %d %v", code, out)
	}
	if code, out := f.call("GET", "/admin/approvals?status=lapsed", a, ""); code != 200 || len(out["approvals"].([]any)) != 1 {
		t.Errorf("%d %v", code, out)
	}
	if code, _ := f.call("GET", "/admin/approvals?status=nope", a, ""); code != 400 {
		t.Errorf("a bad status: %d", code)
	}
}

func TestParallelWrongCodesCannotPassTheLimit(t *testing.T) {
	f := newFixture(t)
	_, temp := f.user("race.me", "support")
	_, out := f.call("POST", "/auth/login", "", `{"login_id":"race.me","password":"`+temp+`"}`)
	challenge := out["challenge_id"].(string)
	f.secret(challenge, false, "race.me")
	done := make(chan int, 12)
	for i := 0; i < 12; i++ {
		go func() {
			req, _ := http.NewRequest("POST", f.srv.URL+"/v1/auth/totp/verify", strings.NewReader(`{"challenge_id":"`+challenge+`","code":"000000"}`))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				done <- 0
				return
			}
			resp.Body.Close()
			done <- resp.StatusCode
		}()
	}
	checked := 0
	for i := 0; i < 12; i++ {
		if c := <-done; c == 400 || c == 429 {
			checked++
		}
	}
	if checked > 5 {
		t.Errorf("%d codes were checked on one challenge, at most 5 may be", checked)
	}
}

func TestAResetEndsOpenChallengesAndDeactivationCancelsChanges(t *testing.T) {
	f := newFixture(t)
	_, tempA := f.user("boss.two", "admin")
	victim, tempV := f.user("victim.one", "admin")
	a := f.signIn("boss.two", tempA)
	v := f.signIn("victim.one", tempV)
	// An attacker holds a challenge from the victim's password; the admin resets the victim.
	_, out := f.call("POST", "/auth/login", "", `{"login_id":"victim.one","password":"`+tempV+`-changed-now"}`)
	held := out["challenge_id"].(string)
	f.call("POST", "/admin/users/"+victim+"/reset", a, `{"reason":"phone stolen"}`)
	if code, _ := f.call("POST", "/auth/totp/enroll", "", `{"challenge_id":"`+held+`"}`); code != 409 {
		t.Errorf("a challenge from before the reset still works: %d", code)
	}
	// The victim had asked for a change; deactivating them cancels it.
	_, tempC := f.user("other.admin", "admin")
	c := f.signIn("other.admin", tempC)
	_, out = f.call("PUT", "/admin/flags/eta_offers", c, `{"enabled":true,"reason":"try it"}`)
	pending := out["approval"].(map[string]any)["id"].(string)
	_ = v
	other := f.count("SELECT count(*) FROM console_users WHERE login_id = 'other.admin'")
	_ = other
	var otherID string
	f.pool.QueryRow(context.Background(), "SELECT id::text FROM console_users WHERE login_id = 'other.admin'").Scan(&otherID)
	f.call("PATCH", "/admin/users/"+otherID, a, `{"active":false,"reason":"left"}`)
	if f.count("SELECT count(*) FROM approvals WHERE id = $1 AND status = 'cancelled'", pending) != 1 {
		t.Error("a deactivated user's pending change is still waiting")
	}
}

func TestTwoAdminsCannotRemoveEachOther(t *testing.T) {
	f := newFixture(t)
	idA, tempA := f.user("pair.a", "admin")
	idB, tempB := f.user("pair.b", "admin")
	a := f.signIn("pair.a", tempA)
	b := f.signIn("pair.b", tempB)
	// Removing yourself is refused even when another admin remains.
	if code, out := f.call("PATCH", "/admin/users/"+idA, a, `{"active":false,"reason":"leaving now"}`); code != 409 || out["code"] != "last_admin" {
		t.Errorf("deactivating yourself: %d %v", code, out)
	}
	codes := make(chan int, 2)
	for _, p := range [][2]string{{a, idB}, {b, idA}} {
		go func(token, target string) {
			req, _ := http.NewRequest("PATCH", f.srv.URL+"/v1/admin/users/"+target, strings.NewReader(`{"active":false,"reason":"remove the other"}`))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Idempotency-Key", "race-key-"+target)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				codes <- 0
				return
			}
			resp.Body.Close()
			codes <- resp.StatusCode
		}(p[0], p[1])
	}
	<-codes
	<-codes
	if n := f.count("SELECT count(*) FROM console_users WHERE role = 'admin' AND active"); n != 1 {
		t.Errorf("%d active admins remain, want exactly 1", n)
	}
}

func TestDeactivationRevokesRefreshTokens(t *testing.T) {
	f := newFixture(t)
	_, tempA := f.user("boss.three", "admin")
	opsID, tempO := f.user("ops.three", "ops")
	a := f.signIn("boss.three", tempA)
	f.signIn("ops.three", tempO)
	if f.count("SELECT count(*) FROM refresh_tokens WHERE actor_type = 'console' AND actor_id = $1 AND revoked_at IS NULL", opsID) == 0 {
		t.Fatal("no session to end")
	}
	f.call("PATCH", "/admin/users/"+opsID, a, `{"active":false,"reason":"left the team"}`)
	if n := f.count("SELECT count(*) FROM refresh_tokens WHERE actor_type = 'console' AND actor_id = $1 AND revoked_at IS NULL", opsID); n != 0 {
		t.Errorf("%d refresh tokens survive deactivation", n)
	}
}

func TestSigningInWithAnEmail(t *testing.T) {
	f := newFixture(t)
	_, temp := f.user("asha.k@ninzo.in", "admin")
	token := f.signIn("asha.k@ninzo.in", temp)
	// The email is not case sensitive at sign-in.
	if code, out := f.call("POST", "/auth/login", "", `{"login_id":"Asha.K@Ninzo.IN","password":"`+temp+`-changed-now"}`); code != 200 || out["totp_enrolled"] != true {
		t.Errorf("%d %v", code, out)
	}
	code, out := f.call("POST", "/admin/users", token, `{"login_id":"ravi@ninzo.in","name":"Ravi","role":"ops"}`)
	if code != 201 || out["user"].(map[string]any)["login_id"] != "ravi@ninzo.in" {
		t.Fatalf("%d %v", code, out)
	}
	if code, out := f.call("POST", "/admin/users", token, `{"login_id":"RAVI@NINZO.IN","name":"Again","role":"ops"}`); code != 409 || out["code"] != "login_id_taken" {
		t.Errorf("the same email in capitals is taken: %d %v", code, out)
	}
	for _, bad := range []string{"ravi@", "@ninzo.in", "ravi@ninzo", "ravi k@ninzo.in", "ravi@" + strings.Repeat("a", 250) + ".in"} {
		if code, _ := f.call("POST", "/admin/users", token, `{"login_id":"`+bad+`","name":"Bad","role":"ops"}`); code != 400 {
			t.Errorf("%q: %d", bad, code)
		}
	}
	// The database holds the same rule, whoever writes.
	if _, err := f.pool.Exec(context.Background(), "INSERT INTO console_users (login_id, name, role, password_hash) VALUES ('not an email@', 'X', 'ops', 'x')"); err == nil {
		t.Error("the database took a bad login")
	}
}

func TestTheCodeIsOptionalUntilPreLaunch(t *testing.T) {
	f := newFixture(t)
	f.svc.TOTPOptional = true
	_, temp := f.user("team@ninzo.in", "admin")
	code, out := f.call("POST", "/auth/login", "", `{"login_id":"team@ninzo.in","password":"`+temp+`"}`)
	session, _ := out["session"].(map[string]any)
	if code != 200 || out["totp_required"] != false || session == nil || session["role"] != "admin" || session["must_change_password"] != true {
		t.Fatalf("the password alone signs in: %d %v", code, out)
	}
	token := session["access_token"].(string)
	if code, out := f.call("GET", "/admin/users", token, ""); code != 403 || out["code"] != "password_change_required" {
		t.Errorf("the temporary password must still go first: %d %v", code, out)
	}
	if code, _ := f.call("POST", "/me/password", token, `{"current_password":"`+temp+`","new_password":"a-long-enough-password"}`); code != 200 {
		t.Fatal("the password change")
	}
	if code, _ := f.call("GET", "/admin/users", token, ""); code != 200 {
		t.Errorf("after the change: %d", code)
	}
	if f.count("SELECT count(*) FROM audit_events WHERE action = 'session.started' AND reason LIKE 'password only%'") != 1 {
		t.Error("a password-only sign-in must be audited as one")
	}
	// A user who set up an authenticator is still asked for it.
	_, out = f.call("POST", "/auth/login", "", `{"login_id":"team@ninzo.in","password":"a-long-enough-password"}`)
	secret := f.secret(out["challenge_id"].(string), false, "team@ninzo.in")
	f.now = f.now.Add(31 * time.Second)
	f.call("POST", "/auth/totp/verify", "", `{"challenge_id":"`+out["challenge_id"].(string)+`","code":"`+TOTPCode(secret, StepOf(f.now))+`"}`)
	code, out = f.call("POST", "/auth/login", "", `{"login_id":"team@ninzo.in","password":"a-long-enough-password"}`)
	if code != 200 || out["totp_required"] != true || out["session"] != nil || out["totp_enrolled"] != true {
		t.Errorf("once set up, the code is asked for: %d %v", code, out)
	}
	// With the switch off (the default, and production), the password alone never signs in.
	f.svc.TOTPOptional = false
	_, other := f.user("ops@ninzo.in", "ops")
	if _, out := f.call("POST", "/auth/login", "", `{"login_id":"ops@ninzo.in","password":"`+other+`"}`); out["totp_required"] != true || out["session"] != nil {
		t.Errorf("%v", out)
	}
}
