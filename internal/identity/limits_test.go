package identity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/config"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/ratelimit"
)

// limitedServer mounts the customer API with limits on, trusting X-Test-IP for the address.
func (f *googleFixture) limitedServer() *httptest.Server {
	f.t.Helper()
	limits := NewLimits(ratelimit.NewMemory(), config.Config{ClientIPHeader: "X-Test-IP"}, quiet)
	f.s.Limits, f.o.Limits, f.google.Limits = limits, limits, limits
	f.t.Cleanup(func() { f.s.Limits, f.o.Limits, f.google.Limits = nil, nil, nil })
	enabled := []string{MethodPhone, MethodGoogle}
	handler := httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		MountSessions(r, f.s, TypeCustomer)
		MountMethods(r, enabled, PhoneMethod{OTPs: f.o, ActorType: TypeCustomer}, f.google)
		MountPhoneVerification(r, f.o, f.s.Signer)
		MountMethodChange(r, enabled, f.o, f.google, f.s.Signer)
	})
	srv := httptest.NewServer(handler)
	f.t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url, ip, bearer, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Test-IP", ip)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// reachLimit sends requests until one is limited, and reports how many it took.
func reachLimit(t *testing.T, url, ip, bearer string, body func(int) string, most int) (*http.Response, int) {
	t.Helper()
	for i := 1; i <= most; i++ {
		resp := post(t, url, ip, bearer, body(i))
		if resp.StatusCode == http.StatusTooManyRequests {
			return resp, i
		}
	}
	t.Fatalf("%s was not limited after %d requests", url, most)
	return nil, 0
}

func checkLimited(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"code":"rate_limited"`) || !strings.Contains(buf.String(), `"retryable":true`) {
		t.Errorf("reply: %s", buf.String())
	}
}

func TestSignInEndpointsAreLimitedByAddress(t *testing.T) {
	f := newGoogleFixture(t)
	srv := f.limitedServer()
	before := f.count("SELECT count(*) FROM otp_requests")
	for _, c := range []struct {
		path string
		rule ratelimit.Rule
		body func(int) string
	}{
		// Malformed bodies, so only the limit is under test and no SMS goes out.
		{"/v1/auth/otp/request", ruleCodeRequestMinute, func(int) string { return `{"phone":"bad"}` }},
		{"/v1/auth/otp/verify", ruleCodeVerifyMinute, func(int) string { return `{}` }},
		{"/v1/auth/google", ruleGoogleMinute, func(int) string { return `{}` }},
		{"/v1/auth/refresh", ruleRefreshMinute, func(int) string { return `{}` }},
	} {
		resp, n := reachLimit(t, srv.URL+c.path, "1.1.1.1", "", c.body, c.rule.Limit+1)
		if n != c.rule.Limit+1 {
			t.Errorf("%s: limited at %d, want %d", c.path, n, c.rule.Limit+1)
		}
		checkLimited(t, resp)
		if got := post(t, srv.URL+c.path, "2.2.2.2", "", c.body(0)); got.StatusCode == http.StatusTooManyRequests {
			t.Errorf("%s: another address was limited", c.path)
		}
	}
	if n := f.count("SELECT count(*) FROM otp_requests"); n != before {
		t.Errorf("%d new code rows", n-before)
	}
}

func TestALimitedRequestDoesNotReachTheDatabase(t *testing.T) {
	f := newGoogleFixture(t)
	srv := f.limitedServer()
	reachLimit(t, srv.URL+"/v1/auth/refresh", "1.1.1.1", "", func(int) string { return `{}` }, ruleRefreshMinute.Limit+1)
	f.s.Pool.Close() // Anything that touches the database now answers 503, and not 429.
	if resp := post(t, srv.URL+"/v1/auth/refresh", "1.1.1.1", "", `{"refresh_token":"x"}`); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("got %d", resp.StatusCode)
	}
}

func TestOneRefreshTokenRepeatedInALoopIsLimited(t *testing.T) {
	f := newGoogleFixture(t)
	srv := f.limitedServer()
	first := f.signIn(TypeCustomer, phoneA)
	body := `{"refresh_token":"` + first.Pair.RefreshToken + `"}`
	resp, n := reachLimit(t, srv.URL+"/v1/auth/refresh", "1.1.1.1", "", func(int) string { return body }, ruleRefreshToken.Limit+1)
	if n != ruleRefreshToken.Limit+1 {
		t.Errorf("limited at %d", n)
	}
	checkLimited(t, resp)
	if rows := f.count("SELECT count(*) FROM refresh_tokens"); rows > ruleRefreshToken.Limit+1 {
		t.Errorf("%d rows from a loop", rows)
	}
}

func TestAStrangerCannotLockAPhoneOwnerOut(t *testing.T) {
	f := newGoogleFixture(t)
	srv := f.limitedServer()
	url := srv.URL + "/v1/auth/otp/request"
	body := `{"phone":"` + phoneA + `"}`
	// A real stranger waits out the 30 seconds between codes each time.
	for i := 0; i < rulePhoneAndAddress.Limit; i++ {
		f.allowResend()
		if resp := post(t, url, "6.6.6.6", "", body); resp.StatusCode != 200 {
			t.Fatalf("request %d: %d", i+1, resp.StatusCode)
		}
	}
	f.allowResend()
	resp := post(t, url, "6.6.6.6", "", body)
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode != 429 || !strings.Contains(buf.String(), "otp_resend_limit") || resp.Header.Get("Retry-After") == "" {
		t.Errorf("the stranger: %d %s", resp.StatusCode, buf.String())
	}
	f.allowResend()
	if resp := post(t, url, "7.7.7.7", "", body); resp.StatusCode != 200 {
		t.Errorf("the owner, from their own address, was locked out: %d", resp.StatusCode)
	}
}

func TestTheHourlyPhoneLimitStillHoldsAcrossAddresses(t *testing.T) {
	f := newGoogleFixture(t)
	srv := f.limitedServer()
	url := srv.URL + "/v1/auth/otp/request"
	body := `{"phone":"` + phoneA + `"}`
	for i := 0; i < otpPerHour; i++ {
		f.allowResend()
		if resp := post(t, url, fmt.Sprintf("10.0.0.%d", i), "", body); resp.StatusCode != 200 {
			t.Fatalf("request %d: %d", i+1, resp.StatusCode)
		}
	}
	f.allowResend()
	if resp := post(t, url, "10.0.1.1", "", body); resp.StatusCode != 429 {
		t.Errorf("past the hourly limit: %d", resp.StatusCode)
	}
	if n := f.count("SELECT count(*) FROM otp_requests"); n != otpPerHour {
		t.Errorf("%d codes made", n)
	}
}

func TestAPhoneVerificationCodeHasTheSameLimit(t *testing.T) {
	f := newGoogleFixture(t)
	srv := f.limitedServer()
	r := f.googleSignIn("sub-1")
	for i := 0; i < rulePhoneAndAddress.Limit; i++ {
		f.allowResend()
		if resp := post(t, srv.URL+"/v1/me/phone/otp", "6.6.6.6", r.Pair.AccessToken, `{"phone":"`+phoneA+`"}`); resp.StatusCode != 200 {
			t.Fatalf("request %d: %d", i+1, resp.StatusCode)
		}
	}
	f.allowResend()
	if resp := post(t, srv.URL+"/v1/me/phone/otp", "6.6.6.6", r.Pair.AccessToken, `{"phone":"`+phoneA+`"}`); resp.StatusCode != 429 {
		t.Errorf("got %d", resp.StatusCode)
	}
}

func TestOneSessionIsLimitedWhateverTheAddress(t *testing.T) {
	f := newGoogleFixture(t)
	srv := f.limitedServer()
	r := f.googleSignIn("sub-1")
	other := f.googleSignIn("sub-2")
	for _, path := range []string{"/v1/me/phone/verify", "/v1/me/sign-in-method"} {
		f.o.Limits.Limiter = ratelimit.NewMemory()
		resp, n := reachLimit(t, srv.URL+path, "", r.Pair.AccessToken, func(i int) string { return `{}` }, ruleSession.Limit+1)
		if n != ruleSession.Limit+1 {
			t.Errorf("%s: limited at %d", path, n)
		}
		checkLimited(t, resp)
		if got := post(t, srv.URL+path, "1.1.1.1", other.Pair.AccessToken, `{}`); got.StatusCode == 429 {
			t.Errorf("%s: another session was limited", path)
		}
	}
}

type brokenLimiter struct{}

func (brokenLimiter) Allow(context.Context, string, ratelimit.Rule) (bool, time.Duration, error) {
	return false, 0, errors.New("store down")
}

func TestABrokenLimiterDoesNotStopSignIn(t *testing.T) {
	f := newGoogleFixture(t)
	srv := f.limitedServer()
	f.o.Limits.Limiter = brokenLimiter{}
	if resp := post(t, srv.URL+"/v1/auth/otp/request", "1.1.1.1", "", `{"phone":"`+phoneA+`"}`); resp.StatusCode != 200 {
		t.Errorf("got %d", resp.StatusCode)
	}
	if n := f.count("SELECT count(*) FROM otp_requests"); n != 1 {
		t.Errorf("the request did not go through: %d codes", n)
	}
}

func TestLimitLogsHoldNoNumberOrAddress(t *testing.T) {
	var logs bytes.Buffer
	limits := NewLimits(ratelimit.NewMemory(), config.Config{ClientIPHeader: "X-Test-IP"}, slog.New(slog.NewJSONHandler(&logs, nil)))
	for i := 0; i <= rulePhoneAndAddress.Limit; i++ {
		req := httptest.NewRequest("POST", "/", nil)
		req.Header.Set("X-Test-IP", "9.9.9.9")
		limits.allowPhone(httptest.NewRecorder(), req, phoneA)
	}
	if !strings.Contains(logs.String(), rulePhoneAndAddress.Name) {
		t.Errorf("the limit was not logged: %s", logs.String())
	}
	if strings.Contains(logs.String(), phoneA) || strings.Contains(logs.String(), "9.9.9.9") {
		t.Errorf("private data in logs: %s", logs.String())
	}
}

func TestAProxiedAPIMustKnowItsCallerHeader(t *testing.T) {
	if err := RequireClientIP(config.Config{Env: config.EnvLocal}); err != nil {
		t.Error(err)
	}
	if err := RequireClientIP(config.Config{Env: "staging"}); err == nil {
		t.Error("everyone would share one limit")
	}
	if err := RequireClientIP(config.Config{Env: "staging", ClientIPHeader: "X-Real-IP"}); err != nil {
		t.Error(err)
	}
}
