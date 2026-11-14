package fcm

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

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
	err := s.Push(context.Background(), "android", "tok-1", "push.customer", map[string]any{"customer_id": "c1", "order_id": "o1", "stage": "arrived", "seq": 7})
	if err != nil {
		t.Fatal(err)
	}
	if f.tokens != 1 || len(f.sent) != 1 || f.auth[0] != "Bearer at-1" {
		t.Fatalf("tokens %d sent %d auth %v", f.tokens, len(f.sent), f.auth)
	}
	m := f.sent[0]
	data := m["data"].(map[string]any)
	if m["token"] != "tok-1" || data["order_id"] != "o1" || data["stage"] != "arrived" || data["seq"] != "7" || data["topic"] != "push.customer" {
		t.Fatalf("%v", m)
	}
	if n := m["notification"].(map[string]any); n["title"] != "Your rider has arrived" {
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
	for name, reply := range map[string]func() (int, string){
		"unregistered": func() (int, string) {
			return 404, `{"error":{"code":404,"status":"NOT_FOUND","details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"UNREGISTERED"}]}}`
		},
		"unregistered as 400": func() (int, string) {
			return 400, `{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"errorCode":"UNREGISTERED"}]}}`
		},
		"plain 404": func() (int, string) { return 404, `not found` },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.reply = reply
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
	for name, code := range map[string]int{"invalid argument": 400, "quota": 429, "server": 500, "unavailable": 503, "forbidden": 403} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.reply = func() (int, string) {
				return code, `{"error":{"code":` + http.StatusText(code) + `,"details":[{"errorCode":"INVALID_ARGUMENT"}]}}`
			}
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

func TestOffersAreUrgentAndTheRestAreNot(t *testing.T) {
	ios := Message("ios", "t", "push.rider", map[string]any{"rider_id": "r", "kind": "offer"})
	apns := ios["apns"].(map[string]any)
	h := apns["headers"].(map[string]string)
	aps := apns["payload"].(map[string]any)["aps"].(map[string]any)
	if h["apns-priority"] != "10" || aps["interruption-level"] != "time-sensitive" || h["apns-collapse-id"] != "push.rider:offer" {
		t.Fatalf("%v", apns)
	}
	if a := Message("android", "t", "push.rider", map[string]any{"kind": "offer"})["android"].(map[string]any); a["priority"] != "high" {
		t.Fatalf("%v", a)
	}
	for _, p := range []map[string]any{{"kind": "relocation"}, {"kind": "restock"}, {"stage": "arrived", "order_id": "o"}} {
		topic := "push.rider"
		if p["stage"] != nil {
			topic = "push.customer"
		}
		m := Message("ios", "t", topic, p)
		apns := m["apns"].(map[string]any)
		if apns["headers"].(map[string]string)["apns-priority"] != "5" || len(apns["payload"].(map[string]any)["aps"].(map[string]any)) != 0 {
			t.Fatalf("%v: %v", p, apns)
		}
		if Message("android", "t", topic, p)["android"].(map[string]any)["priority"] != "normal" {
			t.Fatalf("%v is urgent", p)
		}
	}
	// A customer push about an "offer" stage is not a rider offer.
	if Message("android", "t", "push.customer", map[string]any{"kind": "offer"})["android"].(map[string]any)["priority"] != "normal" {
		t.Fatal("only rider offers are urgent")
	}
}

func TestEveryPushWeSendHasItsOwnText(t *testing.T) {
	// Every stage and kind the backend queues today (orders.pushed, waiting, notify-me, support,
	// refunds; offers, relocation, restock, break end).
	customer := []string{"payment_failed", "no_scooter", "rider_accepted", "timer_running", "arrived", "handed_over", "ended", "available", "ticket_reply", "refund_processed"}
	for _, st := range customer {
		if n := Message("android", "t", "push.customer", map[string]any{"stage": st})["notification"].(map[string]string); n["title"] == fallback.title {
			t.Errorf("customer %s has no text", st)
		}
	}
	if n := Message("android", "t", "push.customer", map[string]any{"stage": "arrived", "kind": "rider_waiting"})["notification"].(map[string]string); n["title"] != "Your rider is waiting at the gate" {
		t.Errorf("%v", n)
	}
	for _, k := range []string{"offer", "relocation", "restock", "shift"} {
		if n := Message("android", "t", "push.rider", map[string]any{"kind": k})["notification"].(map[string]string); n["title"] == fallback.title {
			t.Errorf("rider %s has no text", k)
		}
	}
	if n := Message("android", "t", "push.rider", map[string]any{"kind": "something_new"})["notification"].(map[string]string); n["title"] != fallback.title || n["body"] != fallback.body {
		t.Errorf("an unknown kind should fall back: %v", n)
	}
}

func TestALongCollapseKeyIsShortened(t *testing.T) {
	m := Message("ios", "t", "push.customer", map[string]any{"order_id": strings.Repeat("x", 80), "stage": "ended"})
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
