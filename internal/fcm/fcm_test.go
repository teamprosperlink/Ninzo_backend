package fcm

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

var at = time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)

type fakeDevices struct {
	mu      sync.Mutex
	deleted []string
}

func (d *fakeDevices) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if strings.HasPrefix(sql, "DELETE FROM devices WHERE push_token = $1") {
		d.deleted = append(d.deleted, args[0].(string))
	}
	return pgconn.CommandTag{}, nil
}

// fake is Google's token endpoint and FCM in one server.
type fake struct {
	t       *testing.T
	key     *rsa.PublicKey
	srv     *httptest.Server
	mu      sync.Mutex
	tokens  int
	sent    []map[string]any
	auth    []string
	reply   func() (int, string) // FCM's answer; nil is 200
	account []byte
}

func newFake(t *testing.T) *fake {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fake{t: t, key: &priv.PublicKey}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	f.account, _ = json.Marshal(map[string]string{
		"type": "service_account", "project_id": "ninzo-test", "client_email": "push@ninzo-test.iam.gserviceaccount.com",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "token_uri": f.srv.URL + "/token",
	})
	return f
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/token":
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || !f.validJWT(r.Form.Get("assertion")) {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		f.tokens++
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-" + string(rune('0'+f.tokens)), "expires_in": 3600, "token_type": "Bearer"})
	case "/v1/projects/ninzo-test/messages:send":
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		if json.Unmarshal(b, &body) != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		f.sent = append(f.sent, body["message"].(map[string]any))
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		if f.reply != nil {
			code, text := f.reply()
			w.WriteHeader(code)
			_, _ = w.Write([]byte(text))
			return
		}
		_, _ = w.Write([]byte(`{"name":"projects/ninzo-test/messages/1"}`))
	default:
		http.NotFound(w, r)
	}
}

// validJWT checks the assertion is signed by the account's key and claims what Google needs.
func (f *fake) validJWT(jwt string) bool {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(f.key, crypto.SHA256, sum[:], sig) != nil {
		return false
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Iss, Scope, Aud string
		Iat, Exp        int64
	}
	if json.Unmarshal(raw, &c) != nil {
		return false
	}
	return c.Iss == "push@ninzo-test.iam.gserviceaccount.com" && c.Scope == Scope && c.Aud == f.srv.URL+"/token" && c.Exp-c.Iat == 3600
}

func (f *fake) sender(t *testing.T) (*Sender, *fakeDevices) {
	t.Helper()
	s, err := New(f.account)
	if err != nil {
		t.Fatal(err)
	}
	s.BaseURL = f.srv.URL
	d := &fakeDevices{}
	s.Devices = d
	return s, d
}

func TestAPushIsSignedAndSent(t *testing.T) {
	f := newFake(t)
	s, _ := f.sender(t)
	if s.Project() != "ninzo-test" {
		t.Fatal(s.Project())
	}
	err := s.Push(context.Background(), "android", "tok-1", "push.customer", map[string]any{"customer_id": "c1", "order_id": "o1", "stage": "rider_accepted", "seq": 7})
	if err != nil {
		t.Fatal(err)
	}
	if f.tokens != 1 || len(f.sent) != 1 || f.auth[0] != "Bearer at-1" {
		t.Fatalf("tokens %d sent %d auth %v", f.tokens, len(f.sent), f.auth)
	}
	m := f.sent[0]
	data := m["data"].(map[string]any)
	if m["token"] != "tok-1" || data["order_id"] != "o1" || data["stage"] != "rider_accepted" || data["seq"] != "7" || data["topic"] != "push.customer" {
		t.Fatalf("%v", m)
	}
	if n := m["notification"].(map[string]any); n["title"] != "Your order is on its way" {
		t.Fatalf("%v", n)
	}
	if a := m["android"].(map[string]any); a["priority"] != "normal" || a["collapse_key"] != "order:o1" {
		t.Fatalf("%v", a)
	}
	if _, ok := m["apns"]; ok {
		t.Fatal("an android push with an apns block")
	}
}

func TestTheAccessTokenIsKeptUntilNearItsEnd(t *testing.T) {
	f := newFake(t)
	s, _ := f.sender(t)
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	ctx := context.Background()
	for range 3 {
		if err := s.Push(ctx, "ios", "t", "push.rider", map[string]any{"rider_id": "r", "kind": "restock"}); err != nil {
			t.Fatal(err)
		}
	}
	if f.tokens != 1 {
		t.Fatalf("a token per push: %d", f.tokens)
	}
	now = now.Add(58 * time.Minute) // 2 minutes left: kept
	if err := s.Push(ctx, "ios", "t", "push.rider", map[string]any{"rider_id": "r", "kind": "restock"}); err != nil || f.tokens != 1 {
		t.Fatalf("renewed too early: %d %v", f.tokens, err)
	}
	now = now.Add(90 * time.Second) // 30 s left: renewed
	if err := s.Push(ctx, "ios", "t", "push.rider", map[string]any{"rider_id": "r", "kind": "restock"}); err != nil {
		t.Fatal(err)
	}
	if f.tokens != 2 || f.auth[4] != "Bearer at-2" {
		t.Fatalf("the token was not renewed: %d %v", f.tokens, f.auth)
	}
}

func TestAnUnregisteredTokenIsDeleted(t *testing.T) {
	for name, reply := range map[string]string{
		"as 404": `{"error":{"code":404,"status":"NOT_FOUND","details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"UNREGISTERED"}]}}`,
		"as 400": `{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"errorCode":"UNREGISTERED"}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.reply = func() (int, string) { return 404, reply }
			s, d := f.sender(t)
			if err := s.Push(context.Background(), "android", "dead", "push.customer", map[string]any{"customer_id": "c", "stage": "ended"}); err != nil {
				t.Fatalf("a dead token is not a failure to retry: %v", err)
			}
			if len(d.deleted) != 1 || d.deleted[0] != "dead" {
				t.Fatalf("%v", d.deleted)
			}
		})
	}
}

func TestOtherFailuresAreRetriedAndKeepTheToken(t *testing.T) {
	// A bare 404 is a wrong project or address, not a dead token: deleting on it would delete every
	// token on the first pass.
	for name, r := range map[string]struct {
		code int
		body string
	}{
		"bare 404":    {404, `<html>Not Found</html>`},
		"project 404": {404, `{"error":{"code":404,"status":"NOT_FOUND","message":"Requested entity was not found."}}`},
		"quota":       {429, `{"error":{"code":429,"details":[{"errorCode":"QUOTA_EXCEEDED"}]}}`},
		"internal":    {500, `{"error":{"code":500,"details":[{"errorCode":"INTERNAL"}]}}`},
		"unavailable": {503, `{"error":{"code":503,"details":[{"errorCode":"UNAVAILABLE"}]}}`},
		"apns auth":   {401, `{"error":{"code":401,"details":[{"errorCode":"THIRD_PARTY_AUTH_ERROR"}]}}`},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.reply = func() (int, string) { return r.code, r.body }
			s, d := f.sender(t)
			if err := s.Push(context.Background(), "android", "live", "push.customer", map[string]any{"customer_id": "c", "stage": "ended"}); err == nil {
				t.Fatal("a refused push must return an error, so the outbox retries")
			}
			if len(d.deleted) != 0 {
				t.Fatal("a live token was deleted")
			}
		})
	}
}

func TestAPushThatCanNeverWorkIsDroppedAndTheTokenKept(t *testing.T) {
	for name, r := range map[string]struct {
		code int
		body string
	}{
		"another project": {403, `{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"errorCode":"SENDER_ID_MISMATCH"}]}}`},
		"invalid":         {400, `{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"errorCode":"INVALID_ARGUMENT"}]}}`},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.reply = func() (int, string) { return r.code, r.body }
			s, d := f.sender(t)
			var logged bytes.Buffer
			s.Log = slog.New(slog.NewTextHandler(&logged, nil))
			if err := s.Push(context.Background(), "android", "foreign", "push.customer", map[string]any{"customer_id": "c", "stage": "ended"}); err != nil {
				t.Fatalf("retrying can never help, and repeats the push to the other devices: %v", err)
			}
			if len(d.deleted) != 0 {
				t.Fatal("a wrong service account would make every token look foreign: never delete on this")
			}
			if !strings.Contains(logged.String(), "fcm: push dropped") || strings.Contains(logged.String(), "foreign") {
				t.Fatalf("logged without the token: %q", logged.String())
			}
		})
	}
}

func TestA401GetsANewAccessToken(t *testing.T) {
	f := newFake(t)
	first := true
	f.reply = func() (int, string) {
		if first {
			first = false
			return 401, `{"error":{"code":401}}`
		}
		return 200, `{}`
	}
	s, _ := f.sender(t)
	ctx := context.Background()
	if err := s.Push(ctx, "android", "t", "push.customer", map[string]any{"customer_id": "c", "stage": "ended"}); err == nil {
		t.Fatal("a 401 is a failure")
	}
	if err := s.Push(ctx, "android", "t", "push.customer", map[string]any{"customer_id": "c", "stage": "ended"}); err != nil {
		t.Fatal(err)
	}
	if f.tokens != 2 {
		t.Fatalf("the refused token was used again: %d", f.tokens)
	}
}

func TestARefusedTokenRequestSendsNothing(t *testing.T) {
	f := newFake(t)
	var a map[string]string
	_ = json.Unmarshal(f.account, &a)
	other, _ := rsa.GenerateKey(rand.Reader, 2048) // not the key the server knows
	der, _ := x509.MarshalPKCS8PrivateKey(other)
	a["private_key"] = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	raw, _ := json.Marshal(a)
	s, err := New(raw)
	if err != nil {
		t.Fatal(err)
	}
	s.BaseURL, s.Devices = f.srv.URL, &fakeDevices{}
	if err := s.Push(context.Background(), "android", "t", "push.customer", map[string]any{"stage": "ended"}); err == nil {
		t.Fatal("pushed without a token")
	}
	if len(f.sent) != 0 {
		t.Fatal("a message went out")
	}
}

func TestTheHandoverIsUrgentAndShortLived(t *testing.T) {
	cases := []struct {
		topic   string
		payload map[string]any
		ttl     time.Duration
	}{
		{"push.rider", map[string]any{"rider_id": "r", "kind": "offer"}, time.Minute},
		{"push.customer", map[string]any{"order_id": "o", "stage": "timer_running"}, 5 * time.Minute},
		{"push.customer", map[string]any{"order_id": "o", "stage": "arrived"}, 5 * time.Minute},
		{"push.customer", map[string]any{"order_id": "o", "stage": "arrived", "kind": "rider_waiting"}, 5 * time.Minute},
	}
	for _, c := range cases {
		ios := Message("ios", "t", c.topic, c.payload, at)
		apns := ios["apns"].(map[string]any)
		h := apns["headers"].(map[string]string)
		aps := apns["payload"].(map[string]any)["aps"].(map[string]any)
		if h["apns-priority"] != "10" || aps["interruption-level"] != "time-sensitive" || h["apns-expiration"] != fmt.Sprint(at.Add(c.ttl).Unix()) {
			t.Fatalf("%v: %v", c.payload, apns)
		}
		a := Message("android", "t", c.topic, c.payload, at)["android"].(map[string]any)
		if a["priority"] != "high" || a["ttl"] != fmt.Sprintf("%ds", int(c.ttl.Seconds())) {
			t.Fatalf("%v: %v", c.payload, a)
		}
	}
	if h := Message("ios", "t", "push.rider", map[string]any{"kind": "offer"}, at)["apns"].(map[string]any)["headers"].(map[string]string); h["apns-collapse-id"] != "push.rider:offer" {
		t.Fatalf("%v", h)
	}
}

func TestTheRestAreNormalAndOrderPushesExpireInAnHour(t *testing.T) {
	for _, c := range []struct {
		topic   string
		payload map[string]any
		ttl     string // "" is FCM's default
	}{
		{"push.rider", map[string]any{"kind": "relocation"}, ""},
		{"push.rider", map[string]any{"kind": "restock"}, ""},
		{"push.customer", map[string]any{"order_id": "o", "stage": "rider_accepted"}, "3600s"},
		{"push.customer", map[string]any{"order_id": "o", "stage": "refund_processed"}, "3600s"},
		{"push.customer", map[string]any{"kind": "offer"}, ""}, // only a rider's offer is urgent
	} {
		m := Message("ios", "t", c.topic, c.payload, at)
		apns := m["apns"].(map[string]any)
		h := apns["headers"].(map[string]string)
		if h["apns-priority"] != "5" || len(apns["payload"].(map[string]any)["aps"].(map[string]any)) != 0 {
			t.Fatalf("%v: %v", c.payload, apns)
		}
		a := Message("android", "t", c.topic, c.payload, at)["android"].(map[string]any)
		if a["priority"] != "normal" {
			t.Fatalf("%v is urgent", c.payload)
		}
		got, _ := a["ttl"].(string)
		if got != c.ttl || (c.ttl == "") != (h["apns-expiration"] == "") {
			t.Fatalf("%v: ttl %q expiration %q", c.payload, got, h["apns-expiration"])
		}
	}
}

func TestANewerPushReplacesTheShownOneOnAndroid(t *testing.T) {
	a := Message("android", "t", "push.customer", map[string]any{"order_id": "o1", "stage": "arrived"}, at)["android"].(map[string]any)
	if a["collapse_key"] != "order:o1" || a["notification"].(map[string]string)["tag"] != "order:o1" {
		t.Fatalf("%v", a)
	}
}

func TestATokenRefusalSaysWhy(t *testing.T) {
	f := newFake(t)
	var a map[string]string
	_ = json.Unmarshal(f.account, &a)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(other)
	a["private_key"] = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	raw, _ := json.Marshal(a)
	s, _ := New(raw)
	s.BaseURL = f.srv.URL
	err := s.Push(context.Background(), "android", "t", "push.customer", map[string]any{"stage": "ended"})
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("%v", err)
	}
}

func TestEveryPushWeSendHasItsOwnText(t *testing.T) {
	// Every stage and kind the backend queues today (orders.pushed, waiting, notify-me, support,
	// refunds; offers, relocation, restock, break end).
	customer := []string{"payment_failed", "no_scooter", "rider_accepted", "timer_running", "arrived", "handed_over", "ended", "available", "ticket_reply", "refund_processed"}
	for _, st := range customer {
		if n := Message("android", "t", "push.customer", map[string]any{"stage": st}, at)["notification"].(map[string]string); n["title"] == fallback.title {
			t.Errorf("customer %s has no text", st)
		}
	}
	if n := Message("android", "t", "push.customer", map[string]any{"stage": "arrived", "kind": "rider_waiting"}, at)["notification"].(map[string]string); n["title"] != "Your rider is waiting at the gate" {
		t.Errorf("%v", n)
	}
	for _, k := range []string{"offer", "relocation", "restock", "shift"} {
		if n := Message("android", "t", "push.rider", map[string]any{"kind": k}, at)["notification"].(map[string]string); n["title"] == fallback.title {
			t.Errorf("rider %s has no text", k)
		}
	}
	if n := Message("android", "t", "push.rider", map[string]any{"kind": "something_new"}, at)["notification"].(map[string]string); n["title"] != fallback.title || n["body"] != fallback.body {
		t.Errorf("an unknown kind should fall back: %v", n)
	}
}

func TestALongCollapseKeyIsShortened(t *testing.T) {
	m := Message("ios", "t", "push.customer", map[string]any{"order_id": strings.Repeat("x", 80), "stage": "ended"}, at)
	if id := m["apns"].(map[string]any)["headers"].(map[string]string)["apns-collapse-id"]; len(id) > 64 || id == "" {
		t.Fatalf("%q", id)
	}
}

func TestBadAccountsAreRefused(t *testing.T) {
	f := newFake(t)
	var good map[string]string
	_ = json.Unmarshal(f.account, &good)
	with := func(k, v string) []byte {
		a := map[string]string{}
		for kk, vv := range good {
			a[kk] = vv
		}
		if v == "" {
			delete(a, k)
		} else {
			a[k] = v
		}
		b, _ := json.Marshal(a)
		return b
	}
	for name, raw := range map[string][]byte{
		"not json":         []byte("{"),
		"not a service":    with("type", "authorized_user"),
		"no project":       with("project_id", ""),
		"no email":         with("client_email", ""),
		"no key":           with("private_key", ""),
		"key not pem":      with("private_key", "abc"),
		"no token uri":     with("token_uri", ""),
		"plain http token": with("token_uri", "http://oauth2.example.com/token"),
	} {
		if _, err := New(raw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if s, err := FromEnv(func(string) string { return " " }); s != nil || err != nil {
		t.Fatalf("unset must keep the stand-in: %v %v", s, err)
	}
	if _, err := FromEnv(func(string) string { return "nonsense" }); err == nil {
		t.Fatal("a broken account must stop the API from starting")
	}
	if s, err := FromEnv(func(string) string { return string(f.account) }); err != nil || s == nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(good["token_uri"])
	if u.Scheme != "http" {
		t.Fatal("the test server should be http on 127.0.0.1")
	}
}

func TestABadAPNsKeyKeepsOurAccessToken(t *testing.T) {
	f := newFake(t)
	f.reply = func() (int, string) {
		return 401, `{"error":{"code":401,"details":[{"errorCode":"THIRD_PARTY_AUTH_ERROR"}]}}`
	}
	s, _ := f.sender(t)
	for range 2 {
		if err := s.Push(context.Background(), "ios", "t", "push.customer", map[string]any{"stage": "ended"}); err == nil {
			t.Fatal("an APNs auth failure is still a failure")
		}
	}
	if f.tokens != 1 {
		t.Fatalf("the APNs key was wrong, not our token: %d tokens", f.tokens)
	}
}

// Each Firebase project has its own variable (issue 257): FromEnvKey reads only the one it names.
func TestFromEnvKeyReadsOnlyItsOwnVariable(t *testing.T) {
	f := newFake(t)
	env := func(set map[string]string) func(string) string { return func(k string) string { return set[k] } }
	if s, err := FromEnvKey(env(map[string]string{"FCM_SERVICE_ACCOUNT_JSON": string(f.account)}), "FCM_CUSTOMER_SERVICE_ACCOUNT_JSON"); s != nil || err != nil {
		t.Errorf("another variable counted: %v %v", s, err)
	}
	if _, err := FromEnvKey(env(map[string]string{"FCM_CUSTOMER_SERVICE_ACCOUNT_JSON": "nonsense"}), "FCM_CUSTOMER_SERVICE_ACCOUNT_JSON"); err == nil {
		t.Error("a broken customer key was accepted")
	}
	s, err := FromEnvKey(env(map[string]string{"FCM_CUSTOMER_SERVICE_ACCOUNT_JSON": string(f.account)}), "FCM_CUSTOMER_SERVICE_ACCOUNT_JSON")
	if err != nil || s == nil || s.Project() != "ninzo-test" {
		t.Errorf("%v %v", s, err)
	}
}
