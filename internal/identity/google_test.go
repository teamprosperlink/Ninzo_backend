package identity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/config"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

const webClient = "web-client.apps.googleusercontent.com"

// fakeGoogle signs tokens the way Google does, and serves its public keys.
type fakeGoogle struct {
	key     *rsa.PrivateKey
	kid     string
	fetches atomic.Int32
	down    atomic.Bool
	hang    chan struct{} // While set, every fetch waits for it to close.
	srv     *httptest.Server
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGoogle{key: key, kid: "key-1"}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		g.fetches.Add(1)
		if g.hang != nil {
			<-g.hang
		}
		if g.down.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": g.kid, "kty": "RSA",
			"n": base64.RawURLEncoding.EncodeToString(g.key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(g.key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGoogle) verifier() *GoogleVerifier {
	return &GoogleVerifier{ClientIDs: []string{webClient}, KeysURL: g.srv.URL}
}

func (g *fakeGoogle) claims(subject string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss": "https://accounts.google.com", "aud": webClient, "sub": subject,
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"email": subject + "@gmail.test", "email_verified": true,
	}
}

func (g *fakeGoogle) sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = g.kid
	signed, err := token.SignedString(g.key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func (g *fakeGoogle) token(t *testing.T, subject string) string {
	t.Helper()
	return g.sign(t, g.claims(subject))
}

func TestGoogleTokenChecks(t *testing.T) {
	g := newFakeGoogle(t)
	ctx := context.Background()
	v := g.verifier()
	profile, err := v.Verify(ctx, g.token(t, "sub-1"))
	if err != nil || profile.Subject != "sub-1" || profile.Email != "sub-1@gmail.test" {
		t.Fatalf("a good token: %+v, %v", profile, err)
	}
	change := func(field string, value any) string {
		c := g.claims("sub-1")
		if value == nil {
			delete(c, field)
		} else {
			c[field] = value
		}
		return g.sign(t, c)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	forged := jwt.NewWithClaims(jwt.SigningMethodRS256, g.claims("sub-1"))
	forged.Header["kid"] = g.kid
	forgedToken, err := forged.SignedString(other)
	if err != nil {
		t.Fatal(err)
	}
	// Signing with the public key as an HMAC secret is the classic way to forge RS256.
	publicAsSecret := jwt.NewWithClaims(jwt.SigningMethodHS256, g.claims("sub-1"))
	publicAsSecret.Header["kid"] = g.kid
	confused, err := publicAsSecret.SignedString(g.key.N.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	none, err := jwt.NewWithClaims(jwt.SigningMethodNone, g.claims("sub-1")).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	unknownKid := jwt.NewWithClaims(jwt.SigningMethodRS256, g.claims("sub-1"))
	unknownKid.Header["kid"] = "not-a-key"
	unknownToken, err := unknownKid.SignedString(g.key)
	if err != nil {
		t.Fatal(err)
	}
	for name, token := range map[string]string{
		"another app's audience":    change("aud", "another-app.apps.googleusercontent.com"),
		"no audience":               change("aud", nil),
		"another issuer":            change("iss", "https://evil.example.com"),
		"no issuer":                 change("iss", nil),
		"expired":                   change("exp", time.Now().Add(-time.Minute).Unix()),
		"no expiry":                 change("exp", nil),
		"no subject":                change("sub", nil),
		"signed by someone else":    forgedToken,
		"HS256 with the public key": confused,
		"alg none":                  none,
		"an unknown key":            unknownToken,
		"not a token":               "garbage",
		"empty":                     "",
	} {
		if _, err := v.Verify(ctx, token); !errors.Is(err, ErrGoogleToken) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, iss := range []string{"accounts.google.com", "https://accounts.google.com"} {
		if _, err := v.Verify(ctx, change("iss", iss)); err != nil {
			t.Errorf("issuer %s: %v", iss, err)
		}
	}
}

func TestFailedFetchesAreLimited(t *testing.T) {
	g := newFakeGoogle(t)
	ctx := context.Background()
	v := g.verifier()
	g.down.Store(true)
	for i := 0; i < 5; i++ {
		if _, err := v.Verify(ctx, g.token(t, "s")); !errors.Is(err, ErrGoogleUnavailable) {
			t.Errorf("attempt %d: %v", i, err)
		}
	}
	if n := g.fetches.Load(); n != 1 {
		t.Errorf("%d fetches during an outage, so bad traffic can hammer Google", n)
	}
}

func TestAHangingGoogleHoldsUpNobodyWithAKnownKey(t *testing.T) {
	g := newFakeGoogle(t)
	ctx := context.Background()
	now := time.Now()
	v := g.verifier()
	v.now = func() time.Time { return now }
	if _, err := v.Verify(ctx, g.token(t, "s")); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	g.hang = release
	t.Cleanup(func() { close(release) })
	now = now.Add(2 * googleKeysLife)
	long := g.claims("s")
	long["exp"] = now.Add(time.Hour).Unix()
	token := g.sign(t, long)
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := v.Verify(ctx, token); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("four sign-ins waited %s on a hanging Google", took)
	}
}

func TestOldKeysDuringAnOutageDoNotHammerGoogle(t *testing.T) {
	g := newFakeGoogle(t)
	ctx := context.Background()
	now := time.Now()
	v := g.verifier()
	v.now = func() time.Time { return now }
	if _, err := v.Verify(ctx, g.token(t, "s")); err != nil {
		t.Fatal(err)
	}
	g.down.Store(true)
	now = now.Add(2 * googleKeysLife)
	long := g.claims("s")
	long["exp"] = now.Add(time.Hour).Unix()
	token := g.sign(t, long)
	if _, err := v.Verify(ctx, token); err != nil {
		t.Fatal(err)
	}
	// The background refresh records its attempt, and then the limit holds.
	for i := 0; i < 200 && g.fetches.Load() < 2; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	v.fetching.Lock()
	v.fetching.Unlock() //nolint:staticcheck // Waits for that refresh to finish.
	for i := 0; i < 20; i++ {
		if _, err := v.Verify(ctx, token); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if n := g.fetches.Load(); n != 2 {
		t.Errorf("%d fetches: 20 requests during an outage must not each call Google", n)
	}
}

func TestOneCancelledRequestHarmsNobody(t *testing.T) {
	g := newFakeGoogle(t)
	v := g.verifier()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = v.Verify(cancelled, g.token(t, "s"))
	if _, err := v.Verify(context.Background(), g.token(t, "s")); err != nil {
		t.Errorf("a client hanging up must not fail the next sign-in: %v", err)
	}
}

func TestANewKeyDuringAnOutageIsNotARefusal(t *testing.T) {
	g := newFakeGoogle(t)
	ctx := context.Background()
	now := time.Now()
	v := g.verifier()
	v.now = func() time.Time { return now }
	if _, err := v.Verify(ctx, g.token(t, "s")); err != nil {
		t.Fatal(err)
	}
	g.kid = "key-2"
	g.down.Store(true)
	now = now.Add(2 * googleRefetchEvery)
	for i := 0; i < 2; i++ { // The second attempt is the limit holding, with the same answer.
		if _, err := v.Verify(ctx, g.token(t, "s")); !errors.Is(err, ErrGoogleUnavailable) {
			t.Errorf("attempt %d: %v", i, err)
		}
	}
}

func TestGoogleEmailIsKeptOnlyIfVerified(t *testing.T) {
	g := newFakeGoogle(t)
	v := g.verifier()
	for value, want := range map[any]string{true: "e@x.test", "true": "e@x.test", false: "", "false": ""} {
		c := g.claims("s")
		c["email"], c["email_verified"] = "e@x.test", value
		profile, err := v.Verify(context.Background(), g.sign(t, c))
		if err != nil || profile.Email != want {
			t.Errorf("email_verified %v: %q, %v", value, profile.Email, err)
		}
	}
}

func TestGoogleKeysAreCachedAndRotate(t *testing.T) {
	g := newFakeGoogle(t)
	ctx := context.Background()
	now := time.Now()
	v := g.verifier()
	v.now = func() time.Time { return now }
	// Expiry follows the wall clock in these tokens, so moving v.now back and forth stays valid.
	for i := 0; i < 3; i++ {
		if _, err := v.Verify(ctx, g.token(t, "s")); err != nil {
			t.Fatal(err)
		}
	}
	if n := g.fetches.Load(); n != 1 {
		t.Errorf("%d fetches for three tokens", n)
	}

	// An unknown key cannot make us hammer Google.
	bad := jwt.NewWithClaims(jwt.SigningMethodRS256, g.claims("s"))
	bad.Header["kid"] = "unknown"
	badToken, err := bad.SignedString(g.key)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := v.Verify(ctx, badToken); !errors.Is(err, ErrGoogleToken) {
			t.Fatalf("%v", err)
		}
	}
	if n := g.fetches.Load(); n != 1 {
		t.Errorf("%d fetches after bad tokens", n)
	}

	// Google rotates: a new key is found once the wait has passed.
	g.kid = "key-2"
	now = now.Add(2 * time.Minute)
	if _, err := v.Verify(ctx, g.token(t, "s")); err != nil {
		t.Errorf("a rotated key: %v", err)
	}
}

func TestAGoogleOutageIsNotARefusal(t *testing.T) {
	g := newFakeGoogle(t)
	ctx := context.Background()
	now := time.Now()
	v := g.verifier()
	v.now = func() time.Time { return now }
	g.down.Store(true)
	if _, err := v.Verify(ctx, g.token(t, "s")); !errors.Is(err, ErrGoogleUnavailable) {
		t.Errorf("no keys yet: %v", err)
	}
	g.down.Store(false)
	now = now.Add(2 * googleRefetchEvery) // Fetches are limited to one a minute, even after a failure.
	if _, err := v.Verify(ctx, g.token(t, "s")); err != nil {
		t.Fatal(err)
	}
	// Keys already held keep working through an outage.
	g.down.Store(true)
	now = now.Add(2 * googleKeysLife)
	long := g.claims("s")
	long["exp"] = now.Add(time.Hour).Unix() // Valid at the moved clock.
	if _, err := v.Verify(ctx, g.sign(t, long)); err != nil {
		t.Errorf("old keys during an outage: %v", err)
	}
}

// ---------- Sign-in and merging

type googleFixture struct {
	*otpFixture
	g      *fakeGoogle
	google *GoogleSignIn
}

func newGoogleFixture(t *testing.T) *googleFixture {
	t.Helper()
	f := newOTPFixture(t)
	g := newFakeGoogle(t)
	return &googleFixture{otpFixture: f, g: g, google: &GoogleSignIn{Pool: f.s.Pool, Sessions: f.s, Verifier: g.verifier(), Log: quiet}}
}

func (f *googleFixture) googleSignIn(subject string) GoogleResult {
	f.t.Helper()
	r, err := f.google.SignIn(context.Background(), f.g.token(f.t, subject))
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

// verify proves a phone for an actor, and returns the result.
func (f *googleFixture) verify(a Actor, phone string) (Verified, error) {
	f.t.Helper()
	ctx := context.Background()
	f.allowResend()
	req, err := f.o.RequestPhone(ctx, a, phone)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.o.VerifyPhone(ctx, a, req.ID, testCode)
}

func TestFirstGoogleSignInCreatesAnAccountWithNoPhone(t *testing.T) {
	f := newGoogleFixture(t)
	r := f.googleSignIn("sub-1")
	if r.PhoneVerified {
		t.Error("a new Google account has no verified phone")
	}
	if got, err := f.s.Signer.Verify(r.Pair.AccessToken, TypeCustomer); err != nil || got.ID != r.Actor.ID {
		t.Errorf("session: %+v, %v", got, err)
	}
	f.mustRefresh(r.Pair.RefreshToken, TypeCustomer)
	if n := f.count("SELECT count(*) FROM people WHERE phone IS NULL"); n != 1 {
		t.Errorf("%d people", n)
	}
	if n := f.count("SELECT count(*) FROM person_identities WHERE provider = 'google' AND subject = 'sub-1' AND email = 'sub-1@gmail.test'"); n != 1 {
		t.Errorf("%d identities", n)
	}
}

func TestGoogleSigningInAgainIsTheSameCustomer(t *testing.T) {
	f := newGoogleFixture(t)
	a, b := f.googleSignIn("sub-1"), f.googleSignIn("sub-1")
	if a.Actor.ID != b.Actor.ID || a.Actor.ChainID == b.Actor.ChainID {
		t.Errorf("%+v, %+v", a.Actor, b.Actor)
	}
}

func TestADoubleTapMakesOnePerson(t *testing.T) {
	f := newGoogleFixture(t)
	token := f.g.token(t, "sub-1")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.google.SignIn(context.Background(), token); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := f.count("SELECT count(*) FROM people"); n != 1 {
		t.Errorf("%d people", n)
	}
}

func TestMatchingEmailNeverMerges(t *testing.T) {
	f := newGoogleFixture(t)
	first := f.googleSignIn("sub-1")
	c := f.g.claims("sub-2")
	c["email"] = "sub-1@gmail.test"
	second, err := f.google.SignIn(context.Background(), f.g.sign(t, c))
	if err != nil {
		t.Fatal(err)
	}
	if first.Actor.ID == second.Actor.ID {
		t.Error("two Google accounts merged on a matching email, which allows account takeover")
	}
}

func TestBlockedGoogleAccountIsRefused(t *testing.T) {
	f := newGoogleFixture(t)
	f.googleSignIn("sub-1")
	f.exec("UPDATE people SET status = 'blocked'")
	if _, err := f.google.SignIn(context.Background(), f.g.token(t, "sub-1")); !errors.Is(err, ErrAccountBlocked) {
		t.Errorf("got %v", err)
	}
}

func TestVerifyingANewPhone(t *testing.T) {
	f := newGoogleFixture(t)
	r := f.googleSignIn("sub-1")
	v, err := f.verify(r.Actor, phoneA)
	if err != nil {
		t.Fatal(err)
	}
	if v.Actor.ID != r.Actor.ID || v.Actor.ChainID == r.Actor.ChainID {
		t.Errorf("%+v", v.Actor)
	}
	f.refused(r.Pair.RefreshToken, TypeCustomer) // The old tokens stop working.
	f.mustRefresh(v.Pair.RefreshToken, TypeCustomer)
	if again := f.googleSignIn("sub-1"); !again.PhoneVerified {
		t.Error("the next sign-in must know the phone is verified")
	}
	if n := f.count("SELECT count(*) FROM people WHERE phone = $1", phoneA); n != 1 {
		t.Errorf("%d people", n)
	}
}

func TestVerifyingTheSamePhoneAgainWorks(t *testing.T) {
	f := newGoogleFixture(t)
	r := f.googleSignIn("sub-1")
	v, err := f.verify(r.Actor, phoneA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.verify(v.Actor, phoneA); err != nil {
		t.Errorf("got %v", err)
	}
}

func TestADifferentPhoneIsRefused(t *testing.T) {
	f := newGoogleFixture(t)
	r := f.googleSignIn("sub-1")
	v, err := f.verify(r.Actor, phoneA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.verify(v.Actor, phoneB); !errors.Is(err, ErrPhoneAlreadySet) {
		t.Errorf("got %v", err)
	}
	if n := f.count("SELECT count(*) FROM people WHERE phone = $1", phoneB); n != 0 {
		t.Error("the phone changed")
	}
}

func TestAPhoneWithADifferentGoogleAccountIsRefused(t *testing.T) {
	f := newGoogleFixture(t)
	first := f.googleSignIn("sub-1")
	if _, err := f.verify(first.Actor, phoneA); err != nil {
		t.Fatal(err)
	}
	second := f.googleSignIn("sub-2")
	if _, err := f.verify(second.Actor, phoneA); !errors.Is(err, ErrPhoneAlreadyRegistered) {
		t.Errorf("got %v", err)
	}
	if n := f.count("SELECT count(*) FROM person_identities i JOIN people p ON p.id = i.person_id WHERE p.phone = $1", phoneA); n != 1 {
		t.Errorf("the phone's Google account was swapped: %d", n)
	}
	f.mustRefresh(second.Pair.RefreshToken, TypeCustomer) // The second account carries on, with no phone.
}

func TestBlockedPhoneOwnerStopsAMerge(t *testing.T) {
	f := newGoogleFixture(t)
	f.signIn(TypeCustomer, phoneA)
	r := f.googleSignIn("sub-1")
	f.exec("UPDATE people SET status = 'blocked' WHERE phone = $1", phoneA)
	if _, err := f.verify(r.Actor, phoneA); !errors.Is(err, ErrAccountBlocked) {
		t.Errorf("got %v", err)
	}
	if n := f.count("SELECT count(*) FROM person_identities i JOIN people p ON p.id = i.person_id WHERE p.phone = $1", phoneA); n != 0 {
		t.Error("the Google account moved onto a blocked person")
	}
}

func TestACodeOnlyWorksInTheSessionThatAskedForIt(t *testing.T) {
	f := newGoogleFixture(t)
	ctx := context.Background()
	asker := f.googleSignIn("sub-1")
	thief := f.googleSignIn("sub-2")
	req, err := f.o.RequestPhone(ctx, asker.Actor, phoneA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.o.VerifyPhone(ctx, thief.Actor, req.ID, testCode); !errors.Is(err, ErrInvalidOTP) {
		t.Errorf("another customer: %v", err)
	}
	if _, err := f.o.Verify(ctx, TypeCustomer, req.ID, testCode); !errors.Is(err, ErrInvalidOTP) {
		t.Errorf("a verification code used to sign in: %v", err)
	}
	f.allowResend()
	signIn, err := f.o.Request(ctx, TypeCustomer, phoneA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.o.VerifyPhone(ctx, asker.Actor, signIn.ID, testCode); !errors.Is(err, ErrInvalidOTP) {
		t.Errorf("a sign-in code used to verify: %v", err)
	}
	if _, err := f.o.VerifyPhone(ctx, asker.Actor, req.ID, testCode); err != nil {
		t.Errorf("the right session: %v", err)
	}
}

func TestPhoneVerificationSharesTheLimits(t *testing.T) {
	f := newGoogleFixture(t)
	ctx := context.Background()
	r := f.googleSignIn("sub-1")
	if _, err := f.o.RequestPhone(ctx, r.Actor, phoneA); err != nil {
		t.Fatal(err)
	}
	if _, err := f.o.RequestPhone(ctx, r.Actor, phoneA); !errors.Is(err, ErrResendLimit) {
		t.Errorf("got %v", err)
	}
	if _, err := f.o.RequestPhone(ctx, r.Actor, "98765"); !errors.Is(err, ErrBadPhone) {
		t.Errorf("got %v", err)
	}
}

func TestAPhoneCodeSentToANumberAlwaysGoesOut(t *testing.T) {
	f := newGoogleFixture(t)
	r := f.googleSignIn("sub-1")
	if _, err := f.o.RequestPhone(context.Background(), r.Actor, phoneB); err != nil {
		t.Fatal(err)
	}
	if _, sent := f.sender.sent[phoneB]; !sent {
		t.Error("no code went out")
	}
}

func TestTwoAccountsRacingForOnePhoneStayConsistent(t *testing.T) {
	f := newGoogleFixture(t)
	ctx := context.Background()
	a, b := f.googleSignIn("sub-1"), f.googleSignIn("sub-2")
	reqA, err := f.o.RequestPhone(ctx, a.Actor, phoneA)
	if err != nil {
		t.Fatal(err)
	}
	f.allowResend()
	reqB, err := f.o.RequestPhone(ctx, b.Actor, phoneA)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var worked atomic.Int32
	run := func(actor Actor, id string) {
		defer wg.Done()
		_, err := f.o.VerifyPhone(ctx, actor, id, testCode)
		switch {
		case err == nil:
			worked.Add(1)
		case !errors.Is(err, ErrPhoneAlreadyRegistered):
			t.Errorf("a race must not fail like this: %v", err)
		}
	}
	wg.Add(2)
	go run(a.Actor, reqA.ID)
	go run(b.Actor, reqB.ID)
	wg.Wait()
	if worked.Load() != 1 {
		t.Errorf("%d accounts claimed one phone", worked.Load())
	}
	if n := f.count("SELECT count(*) FROM people WHERE phone = $1", phoneA); n != 1 {
		t.Errorf("%d people hold the phone", n)
	}
}

func TestAPhoneNewToEveryoneIsToldAboutTheLink(t *testing.T) {
	f := newGoogleFixture(t)
	r := f.googleSignIn("sub-1")
	if _, err := f.verify(r.Actor, phoneA); err != nil {
		t.Fatal(err)
	}
	if n := f.count("SELECT count(*) FROM outbox_events WHERE topic = 'identity.account_linked'"); n != 1 {
		t.Errorf("%d events: the phone's real owner must be told a Google account is on it", n)
	}
}

func TestAPhoneOnlyCustomerIsNotToldAboutALink(t *testing.T) {
	f := newGoogleFixture(t)
	v := f.signIn(TypeCustomer, phoneA)
	f.allowResend()
	if _, err := f.verify(v.Actor, phoneA); err != nil {
		t.Fatal(err)
	}
	if n := f.count("SELECT count(*) FROM outbox_events"); n != 0 {
		t.Errorf("%d events, with nothing linked", n)
	}
}

func TestABlockedTokenCannotSendACode(t *testing.T) {
	f := newGoogleFixture(t)
	blocked := f.googleSignIn("sub-2")
	f.exec("UPDATE people SET status = 'blocked' WHERE id = (SELECT person_id FROM customers WHERE id = $1)", blocked.Actor.ID)
	if _, err := f.o.RequestPhone(context.Background(), blocked.Actor, phoneB); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("blocked: %v", err)
	}
	if len(f.sender.sent) != 0 {
		t.Error("a blocked token sent an SMS")
	}
}

func TestABlockWhileASignInWaitsStopsIt(t *testing.T) {
	f := newGoogleFixture(t)
	ctx := context.Background()
	first := f.googleSignIn("sub-1")
	tx, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if err := lockActor(ctx, tx, TypeCustomer, first.Actor.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.google.SignIn(ctx, f.g.token(t, "sub-1"))
		done <- err
	}()
	f.waitForBlockedQuery()
	if _, err := tx.Exec(ctx, "UPDATE people SET status = 'blocked'"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrAccountBlocked) {
		t.Errorf("got %v", err)
	}
	if n := f.count("SELECT count(*) FROM refresh_tokens"); n != 1 {
		t.Errorf("%d sessions, want only the first", n)
	}
}

// One account holds the phone lock while it takes the number. The other must wait, then be told
// properly, and not hit a database error.
func TestTheSecondAccountForAPhoneWaitsAndIsToldProperly(t *testing.T) {
	f := newGoogleFixture(t)
	ctx := context.Background()
	a, b := f.googleSignIn("sub-1"), f.googleSignIn("sub-2")
	req, err := f.o.RequestPhone(ctx, b.Actor, phoneA)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 2))", phoneA); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "UPDATE people SET phone = $1 WHERE id = (SELECT person_id FROM customers WHERE id = $2)", phoneA, a.Actor.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.o.VerifyPhone(ctx, b.Actor, req.ID, testCode)
		done <- err
	}()
	f.waitForBlockedQuery()
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrPhoneAlreadyRegistered) {
		t.Errorf("got %v", err)
	}
}

// ---------- HTTP

func (f *googleFixture) server() *httptest.Server {
	f.t.Helper()
	handler := httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		MountGoogle(r, f.google)
		MountPhoneVerification(r, f.o, f.s.Signer)
	})
	srv := httptest.NewServer(handler)
	f.t.Cleanup(srv.Close)
	return srv
}

func TestGoogleEndpoint(t *testing.T) {
	f := newGoogleFixture(t)
	srv := f.server()
	code, body := call(t, "POST", srv.URL+"/v1/auth/google", "", `{"id_token":"`+f.g.token(t, "sub-1")+`"}`)
	if code != 200 || body["phone_verified"] != false {
		t.Fatalf("%d %v", code, body)
	}
	for _, field := range contractRequired(t, "customer", "CustomerSession") {
		if _, ok := body[field]; !ok {
			t.Errorf("the reply has no %s: %v", field, body)
		}
	}
	if code, out := call(t, "POST", srv.URL+"/v1/auth/google", "", `{"id_token":"bad"}`); code != 401 || out["code"] != "invalid_google_token" {
		t.Errorf("a bad token: %d %v", code, out)
	}
	if code, out := call(t, "POST", srv.URL+"/v1/auth/google", "", `{}`); code != 400 || out["code"] != "invalid_request" {
		t.Errorf("no token: %d %v", code, out)
	}
	f.exec("UPDATE people SET status = 'blocked'")
	if code, out := call(t, "POST", srv.URL+"/v1/auth/google", "", `{"id_token":"`+f.g.token(t, "sub-1")+`"}`); code != 403 || out["code"] != "account_blocked" {
		t.Errorf("blocked: %d %v", code, out)
	}
}

func TestAGoogleOutageOverHTTPIsRetryable(t *testing.T) {
	f := newGoogleFixture(t)
	srv := f.server()
	f.g.down.Store(true)
	code, out := call(t, "POST", srv.URL+"/v1/auth/google", "", `{"id_token":"`+f.g.token(t, "sub-1")+`"}`)
	if code != 503 || out["retryable"] != true {
		t.Errorf("%d %v", code, out)
	}
}

func TestPhoneVerificationEndpoints(t *testing.T) {
	f := newGoogleFixture(t)
	srv := f.server()
	r := f.googleSignIn("sub-1")
	bearer := r.Pair.AccessToken
	if code, _ := call(t, "POST", srv.URL+"/v1/me/phone/otp", "", `{"phone":"`+phoneA+`"}`); code != 401 {
		t.Errorf("no sign-in: %d", code)
	}
	if code, out := call(t, "POST", srv.URL+"/v1/me/phone/otp", bearer, `{"phone":"98765"}`); code != 400 || out["code"] != "invalid_request" {
		t.Errorf("a bad phone: %d %v", code, out)
	}
	code, req := call(t, "POST", srv.URL+"/v1/me/phone/otp", bearer, `{"phone":"`+phoneA+`"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, req)
	}
	for _, field := range contractRequired(t, "customer", "OtpRequestResult") {
		if _, ok := req[field]; !ok {
			t.Errorf("the reply has no %s", field)
		}
	}
	if code, out := call(t, "POST", srv.URL+"/v1/me/phone/otp", bearer, `{"phone":"`+phoneA+`"}`); code != 429 || out["code"] != "otp_resend_limit" {
		t.Errorf("resend: %d %v", code, out)
	}
	id := req["request_id"].(string)
	if code, out := call(t, "POST", srv.URL+"/v1/me/phone/verify", bearer, `{"request_id":"`+id+`","otp":"000000"}`); code != 400 || out["code"] != "invalid_otp" {
		t.Errorf("wrong: %d %v", code, out)
	}
	if code, out := call(t, "POST", srv.URL+"/v1/me/phone/verify", bearer, `{"request_id":"`+id+`","otp":"abc"}`); code != 400 || out["code"] != "invalid_request" {
		t.Errorf("malformed: %d %v", code, out)
	}
	code, session := call(t, "POST", srv.URL+"/v1/me/phone/verify", bearer, `{"request_id":"`+id+`","otp":"`+testCode+`"}`)
	if code != 200 || session["phone_verified"] != true || session["customer_id"] != r.Actor.ID {
		t.Fatalf("%d %v", code, session)
	}
	for _, field := range contractRequired(t, "customer", "CustomerSession") {
		if _, ok := session[field]; !ok {
			t.Errorf("the session has no %s", field)
		}
	}

	second := f.googleSignIn("sub-2")
	f.allowResend()
	_, other := call(t, "POST", srv.URL+"/v1/me/phone/otp", second.Pair.AccessToken, `{"phone":"`+phoneA+`"}`)
	code, out := call(t, "POST", srv.URL+"/v1/me/phone/verify", second.Pair.AccessToken, `{"request_id":"`+other["request_id"].(string)+`","otp":"`+testCode+`"}`)
	if code != 409 || out["code"] != "phone_already_registered" {
		t.Errorf("already registered: %d %v", code, out)
	}
	f.allowResend()
	_, change := call(t, "POST", srv.URL+"/v1/me/phone/otp", session["access_token"].(string), `{"phone":"`+phoneB+`"}`)
	code, out = call(t, "POST", srv.URL+"/v1/me/phone/verify", session["access_token"].(string), `{"request_id":"`+change["request_id"].(string)+`","otp":"`+testCode+`"}`)
	if code != 409 || out["code"] != "phone_already_set" {
		t.Errorf("already set: %d %v", code, out)
	}
}

func TestSettings(t *testing.T) {
	if err := RequireGoogle(config.Config{}); err == nil {
		t.Error("no client IDs must stop the customer API")
	}
	if err := RequireGoogle(config.Config{GoogleClientIDs: []string{webClient}}); err != nil {
		t.Error(err)
	}
	if v := (&GoogleVerifier{}); v.KeysURL != "" {
		t.Error("the real key URL must be the default")
	}
	if !strings.HasPrefix(googleKeysURL, "https://www.googleapis.com/") {
		t.Errorf("key URL: %s", googleKeysURL)
	}
}
