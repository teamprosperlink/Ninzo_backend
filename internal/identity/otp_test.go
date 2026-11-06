package identity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/config"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

const (
	testCode = "123456"
	phoneA   = "+919876543210"
	phoneB   = "+919876543211"
)

type recordingSender struct {
	mu   sync.Mutex
	sent map[string]string
	fail error
}

func (s *recordingSender) SendOTP(_ context.Context, phone, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	if s.sent == nil {
		s.sent = map[string]string{}
	}
	s.sent[phone] = code
	return nil
}

func (s *recordingSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

type otpFixture struct {
	*fixture
	o      *OTPs
	sender *recordingSender
}

func newOTPFixture(t *testing.T) *otpFixture {
	t.Helper()
	f := newFixture(t)
	sender := &recordingSender{}
	return &otpFixture{fixture: f, sender: sender, o: &OTPs{
		Pool: f.s.Pool, Sessions: f.s, Sender: sender, HashKey: []byte("otp-test-key"), TestCode: testCode, Log: quiet,
	}}
}

func (f *otpFixture) signIn(actorType, phone string) Verified {
	f.t.Helper()
	ctx := context.Background()
	req, err := f.o.Request(ctx, actorType, phone)
	if err != nil {
		f.t.Fatal(err)
	}
	v, err := f.o.Verify(ctx, actorType, req.ID, testCode)
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

func (f *otpFixture) rider(phone string) string {
	f.t.Helper()
	var person, id string
	ctx := context.Background()
	if err := f.s.Pool.QueryRow(ctx, "INSERT INTO people (phone) VALUES ($1) RETURNING id::text", phone).Scan(&person); err != nil {
		f.t.Fatal(err)
	}
	if err := f.s.Pool.QueryRow(ctx, "INSERT INTO riders (person_id, status) VALUES ($1, 'active') RETURNING id::text", person).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

// allowResend moves earlier requests back in time, so the 30-second rule does not stop a test.
func (f *otpFixture) allowResend() {
	f.exec("UPDATE otp_requests SET created_at = created_at - interval '31 seconds'")
}

func (f *otpFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.s.Pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func TestCustomerFirstSignInCreatesTheAccount(t *testing.T) {
	f := newOTPFixture(t)
	v := f.signIn(TypeCustomer, phoneA)
	if v.Actor.Type != TypeCustomer || v.Status != "" {
		t.Errorf("%+v", v)
	}
	if got, err := f.s.Signer.Verify(v.Pair.AccessToken, TypeCustomer); err != nil || got.ID != v.Actor.ID {
		t.Errorf("the session: %+v, %v", got, err)
	}
	f.mustRefresh(v.Pair.RefreshToken, TypeCustomer)
	if n := f.count("SELECT count(*) FROM people WHERE phone = $1", phoneA); n != 1 {
		t.Errorf("%d people", n)
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action IN ('customer.created', 'session.started')"); n != 2 {
		t.Errorf("%d audit rows", n)
	}
}

func TestSigningInAgainIsTheSameCustomer(t *testing.T) {
	f := newOTPFixture(t)
	first := f.signIn(TypeCustomer, phoneA)
	f.allowResend()
	second := f.signIn(TypeCustomer, phoneA)
	if first.Actor.ID != second.Actor.ID {
		t.Errorf("two customers for one phone: %s, %s", first.Actor.ID, second.Actor.ID)
	}
	if first.Actor.ChainID == second.Actor.ChainID {
		t.Error("a new sign-in must be a new session")
	}
}

func TestARiderWhoOrdersFoodIsOnePerson(t *testing.T) {
	f := newOTPFixture(t)
	rider := f.rider(phoneA)
	customer := f.signIn(TypeCustomer, phoneA)
	if n := f.count("SELECT count(*) FROM people"); n != 1 {
		t.Errorf("%d people", n)
	}
	if n := f.count("SELECT count(*) FROM customers c JOIN riders r ON r.person_id = c.person_id WHERE r.id = $1 AND c.id = $2", rider, customer.Actor.ID); n != 1 {
		t.Error("the customer and rider are not one person")
	}
}

func TestRealCodesAreRandomSixDigitsSentOnlyByTheSender(t *testing.T) {
	f := newOTPFixture(t)
	f.o.TestCode = ""
	ctx := context.Background()
	req, err := f.o.Request(ctx, TypeCustomer, phoneA)
	if err != nil {
		t.Fatal(err)
	}
	code := f.sender.sent[phoneA]
	if len(code) != 6 || strings.Trim(code, "0123456789") != "" {
		t.Errorf("code %q", code)
	}
	var stored string
	if err := f.s.Pool.QueryRow(ctx, "SELECT code_hash FROM otp_requests").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, code) {
		t.Error("the code is in the stored row")
	}
	if len(stored) != 64 {
		t.Errorf("hash length %d", len(stored))
	}
	if req.ExpiresInS != 300 || req.ResendAfterS != 30 {
		t.Errorf("%+v", req)
	}
	if _, err := f.o.Verify(ctx, TypeCustomer, req.ID, code); err != nil {
		t.Errorf("the sent code: %v", err)
	}
}

func TestACodeIsKeyedSoALeakDoesNotRevealIt(t *testing.T) {
	a := &OTPs{HashKey: []byte("one")}
	b := &OTPs{HashKey: []byte("two")}
	if a.hash(phoneA, testCode) == b.hash(phoneA, testCode) {
		t.Error("the hash ignores the key")
	}
	if a.hash(phoneA, testCode) == a.hash(phoneB, testCode) {
		t.Error("the hash ignores the phone")
	}
}

func TestWrongExpiredAndUsedCodes(t *testing.T) {
	f := newOTPFixture(t)
	ctx := context.Background()
	req, err := f.o.Request(ctx, TypeCustomer, phoneA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.o.Verify(ctx, TypeCustomer, req.ID, "000000"); !errors.Is(err, ErrInvalidOTP) {
		t.Errorf("wrong: %v", err)
	}
	if n := f.count("SELECT attempts FROM otp_requests"); n != 1 {
		t.Errorf("a wrong code was not counted: %d", n)
	}
	for _, bad := range []string{"not-a-request", "11111111-1111-1111-1111-111111111111", ""} {
		if _, err := f.o.Verify(ctx, TypeCustomer, bad, testCode); !errors.Is(err, ErrInvalidOTP) {
			t.Errorf("request %q: %v", bad, err)
		}
	}
	if _, err := f.o.Verify(ctx, TypeRider, req.ID, testCode); !errors.Is(err, ErrInvalidOTP) {
		t.Errorf("a customer code at the rider API: %v", err)
	}
	if _, err := f.o.Verify(ctx, TypeCustomer, req.ID, testCode); err != nil {
		t.Errorf("right: %v", err)
	}
	if _, err := f.o.Verify(ctx, TypeCustomer, req.ID, testCode); !errors.Is(err, ErrInvalidOTP) {
		t.Errorf("used twice: %v", err)
	}
	f.allowResend()
	again, err := f.o.Request(ctx, TypeCustomer, phoneA)
	if err != nil {
		t.Fatal(err)
	}
	f.exec("UPDATE otp_requests SET expires_at = now() - interval '1 second' WHERE id = $1", again.ID)
	if _, err := f.o.Verify(ctx, TypeCustomer, again.ID, testCode); !errors.Is(err, ErrOTPExpired) {
		t.Errorf("expired: %v", err)
	}
}

func TestAttemptsAreLimitedEvenWithTheRightCode(t *testing.T) {
	f := newOTPFixture(t)
	ctx := context.Background()
	req, err := f.o.Request(ctx, TypeCustomer, phoneA)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < otpMaxAttempts; i++ {
		if _, err := f.o.Verify(ctx, TypeCustomer, req.ID, "000000"); !errors.Is(err, ErrInvalidOTP) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := f.o.Verify(ctx, TypeCustomer, req.ID, testCode); !errors.Is(err, ErrOTPAttempts) {
		t.Errorf("after the limit: %v", err)
	}
}

func TestParallelGuessesCannotBeatTheLimit(t *testing.T) {
	f := newOTPFixture(t)
	ctx := context.Background()
	req, err := f.o.Request(ctx, TypeCustomer, phoneA)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = f.o.Verify(ctx, TypeCustomer, req.ID, "000000")
		}()
	}
	wg.Wait()
	if n := f.count("SELECT attempts FROM otp_requests"); n != otpMaxAttempts {
		t.Errorf("%d attempts counted, want exactly %d", n, otpMaxAttempts)
	}
}

func TestOneCodeStartsOneSession(t *testing.T) {
	f := newOTPFixture(t)
	ctx := context.Background()
	req, err := f.o.Request(ctx, TypeCustomer, phoneA)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	worked := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.o.Verify(ctx, TypeCustomer, req.ID, testCode); err == nil {
				mu.Lock()
				worked++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if worked != 1 {
		t.Errorf("%d sign-ins from one code", worked)
	}
}

func TestResendLimits(t *testing.T) {
	f := newOTPFixture(t)
	ctx := context.Background()
	if _, err := f.o.Request(ctx, TypeCustomer, phoneA); err != nil {
		t.Fatal(err)
	}
	_, err := f.o.Request(ctx, TypeCustomer, phoneA)
	var wait *RetryAfterError
	if !errors.As(err, &wait) || !errors.Is(err, ErrResendLimit) || wait.Seconds < 1 || wait.Seconds > 31 {
		t.Fatalf("within 30 seconds: %v", err)
	}
	// Another number, and the other app's sign-in, are separate.
	if _, err := f.o.Request(ctx, TypeCustomer, phoneB); err != nil {
		t.Errorf("another phone: %v", err)
	}
	if _, err := f.o.Request(ctx, TypeRider, phoneA); err != nil {
		t.Errorf("the rider app: %v", err)
	}
	for i := 1; i < otpPerHour; i++ {
		f.allowResend()
		if _, err := f.o.Request(ctx, TypeCustomer, phoneA); err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
	}
	f.allowResend()
	_, err = f.o.Request(ctx, TypeCustomer, phoneA)
	if !errors.As(err, &wait) || wait.Seconds < 60 || wait.Seconds > 3600 {
		t.Errorf("over the hourly limit: %v", err)
	}
	// The wait must be when the oldest request leaves the hour, not always an hour.
	f.exec("UPDATE otp_requests SET created_at = now() - interval '50 minutes' WHERE phone = $1 AND actor_type = 'customer'", phoneA)
	_, err = f.o.Request(ctx, TypeCustomer, phoneA)
	if !errors.As(err, &wait) || wait.Seconds > 601 {
		t.Errorf("the hourly wait is not accurate: %v", err)
	}
	if n := f.sender.count(); n > 2 {
		t.Errorf("sent to %d phones", n)
	}
	f.exec("UPDATE otp_requests SET created_at = now() - interval '61 minutes'")
	if _, err := f.o.Request(ctx, TypeCustomer, phoneA); err != nil {
		t.Errorf("after an hour: %v", err)
	}
}

func TestABurstCannotSlipPastTheLimit(t *testing.T) {
	f := newOTPFixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = f.o.Request(ctx, TypeCustomer, phoneA)
		}()
	}
	wg.Wait()
	if n := f.count("SELECT count(*) FROM otp_requests"); n != 1 {
		t.Errorf("%d codes from a burst", n)
	}
}

func TestBadPhoneNumbers(t *testing.T) {
	f := newOTPFixture(t)
	for _, bad := range []string{"", "98765", "9876543210", "+0123456789", "+91 98765 43210", "+91987654321012345", "+9109876543210", "+919", "+912345678901", "+14155552671", "+1900555012345"} {
		if _, err := f.o.Request(context.Background(), TypeCustomer, bad); !errors.Is(err, ErrBadPhone) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestAnUnknownRiderLooksTheSameAndCostsNothing(t *testing.T) {
	f := newOTPFixture(t)
	f.rider(phoneA)
	ctx := context.Background()
	known, err := f.o.Request(ctx, TypeRider, phoneA)
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := f.o.Request(ctx, TypeRider, phoneB)
	if err != nil {
		t.Fatal(err)
	}
	if known.ExpiresInS != unknown.ExpiresInS || known.ResendAfterS != unknown.ResendAfterS || len(known.ID) != len(unknown.ID) {
		t.Errorf("the replies differ: %+v, %+v", known, unknown)
	}
	if _, sent := f.sender.sent[phoneB]; sent {
		t.Error("an SMS went to an unregistered number")
	}
	if _, sent := f.sender.sent[phoneA]; !sent {
		t.Error("no SMS went to a registered rider")
	}
	// Even the fixed test code must not open a rider account that does not exist.
	if _, err := f.o.Verify(ctx, TypeRider, unknown.ID, testCode); !errors.Is(err, ErrInvalidOTP) {
		t.Errorf("right code: %v", err)
	}
	for i := 1; i < otpMaxAttempts; i++ {
		if _, err := f.o.Verify(ctx, TypeRider, unknown.ID, "000000"); !errors.Is(err, ErrInvalidOTP) {
			t.Errorf("attempt %d: %v", i, err)
		}
	}
	if _, err := f.o.Verify(ctx, TypeRider, unknown.ID, "000000"); !errors.Is(err, ErrOTPAttempts) {
		t.Errorf("the limit must look the same: %v", err)
	}
	f.exec("UPDATE otp_requests SET expires_at = now() - interval '1 second', attempts = 0 WHERE phone = $1", phoneB)
	if _, err := f.o.Verify(ctx, TypeRider, unknown.ID, "000000"); !errors.Is(err, ErrOTPExpired) {
		t.Errorf("expiry must look the same: %v", err)
	}
	// The limit applies too, so a repeat reply cannot reveal the account either.
	if _, err := f.o.Request(ctx, TypeRider, phoneB); !errors.Is(err, ErrResendLimit) {
		t.Errorf("resend: %v", err)
	}
	if n := f.count("SELECT count(*) FROM riders"); n != 1 {
		t.Errorf("%d riders: a sign-in must never create one", n)
	}
}

func TestRiderSignIn(t *testing.T) {
	f := newOTPFixture(t)
	id := f.rider(phoneA)
	v := f.signIn(TypeRider, phoneA)
	if v.Actor.ID != id || v.Status != "active" {
		t.Errorf("%+v", v)
	}
	f.mustRefresh(v.Pair.RefreshToken, TypeRider)
}

func TestAnApplicantCanSignIn(t *testing.T) {
	f := newOTPFixture(t)
	id := f.rider(phoneA)
	f.exec("UPDATE riders SET status = 'applicant' WHERE id = $1", id)
	if v := f.signIn(TypeRider, phoneA); v.Status != "applicant" {
		t.Errorf("status %q", v.Status)
	}
}

func TestBlockedAndSuspendedLearnOnlyAfterAProvenCode(t *testing.T) {
	f := newOTPFixture(t)
	ctx := context.Background()
	id := f.rider(phoneA)
	f.exec("UPDATE riders SET status = 'suspended' WHERE id = $1", id)

	req, err := f.o.Request(ctx, TypeRider, phoneA)
	if err != nil {
		t.Fatalf("a suspended rider's request must look normal: %v", err)
	}
	if _, err := f.o.Verify(ctx, TypeRider, req.ID, "000000"); !errors.Is(err, ErrInvalidOTP) {
		t.Errorf("a wrong code reveals nothing: %v", err)
	}
	if _, err := f.o.Verify(ctx, TypeRider, req.ID, testCode); !errors.Is(err, ErrAccountBlocked) {
		t.Errorf("suspended: %v", err)
	}
	// A suspended rider can still order food.
	customer := f.signIn(TypeCustomer, phoneA)
	f.mustRefresh(customer.Pair.RefreshToken, TypeCustomer)

	f.exec("UPDATE people SET status = 'blocked' WHERE phone = $1", phoneA)
	f.allowResend()
	again, err := f.o.Request(ctx, TypeCustomer, phoneA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.o.Verify(ctx, TypeCustomer, again.ID, testCode); !errors.Is(err, ErrAccountBlocked) {
		t.Errorf("blocked: %v", err)
	}
	if n := f.count("SELECT count(*) FROM refresh_tokens WHERE actor_type = 'rider'"); n != 0 {
		t.Errorf("a suspended rider got %d sessions", n)
	}
}

func TestAFailedSendDoesNotCountAgainstTheLimit(t *testing.T) {
	f := newOTPFixture(t)
	ctx := context.Background()
	f.sender.fail = errors.New("the provider is down")
	if _, err := f.o.Request(ctx, TypeCustomer, phoneA); err == nil || errors.Is(err, ErrResendLimit) {
		t.Fatalf("got %v", err)
	}
	if n := f.count("SELECT count(*) FROM otp_requests"); n != 0 {
		t.Errorf("%d rows left for a code that never arrived", n)
	}
	f.sender.fail = nil
	if _, err := f.o.Request(ctx, TypeCustomer, phoneA); err != nil {
		t.Errorf("the retry: %v", err)
	}
}

func TestEndAllWaitsForASignInInProgress(t *testing.T) {
	f := newOTPFixture(t)
	v := f.signIn(TypeCustomer, phoneA)
	ctx := context.Background()
	tx, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if err := lockActor(ctx, tx, TypeCustomer, v.Actor.ID); err != nil {
		t.Fatal(err)
	}
	inProgress, _, err := f.s.Start(ctx, tx, TypeCustomer, v.Actor.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- database.InTx(ctx, f.s.Pool, func(endTx pgx.Tx) error { return f.s.EndAll(ctx, endTx, TypeCustomer, v.Actor.ID, "") })
	}()
	select {
	case err := <-done:
		t.Fatalf("it did not wait for the sign-in in progress: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.refused(inProgress.RefreshToken, TypeCustomer)
}

// These two force the race, by making the second request arrive while the first still holds its
// lock. If the lock is gone, the second reads the old state and wrongly succeeds.
func TestVerifyWaitsSoACodeCannotBeUsedTwice(t *testing.T) {
	f := newOTPFixture(t)
	ctx := context.Background()
	req, err := f.o.Request(ctx, TypeCustomer, phoneA)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, "SELECT 1 FROM otp_requests WHERE id = $1 FOR UPDATE", req.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.o.Verify(ctx, TypeCustomer, req.ID, testCode)
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	// The first sign-in uses the code, and commits.
	if _, err := tx.Exec(ctx, "UPDATE otp_requests SET consumed_at = now() WHERE id = $1", req.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrInvalidOTP) {
		t.Errorf("the second use of a code: %v", err)
	}
	if n := f.count("SELECT count(*) FROM refresh_tokens"); n != 0 {
		t.Errorf("%d sessions from a used code", n)
	}
}

// holdActor starts a sign-in that will wait for this transaction, which then makes a change.
func (f *otpFixture) holdActor(actorType, actorID, phone, change string, args ...any) error {
	f.t.Helper()
	ctx := context.Background()
	req, err := f.o.Request(ctx, actorType, phone)
	if err != nil {
		f.t.Fatal(err)
	}
	tx, err := f.s.Pool.Begin(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if err := lockActor(ctx, tx, actorType, actorID); err != nil {
		f.t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.o.Verify(ctx, actorType, req.ID, testCode)
		done <- err
	}()
	f.waitForBlockedQuery()
	if _, err := tx.Exec(ctx, change, args...); err != nil {
		f.t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		f.t.Fatal(err)
	}
	return <-done
}

func TestASignInWaitingForALockIsStoppedByWhatHappenedMeanwhile(t *testing.T) {
	const newPhone = "+919876543299"
	cases := []struct {
		name, actorType, change string
		want                    error
	}{
		{"a rider's phone changed", TypeRider, "UPDATE people SET phone = '" + newPhone + "'", ErrInvalidOTP},
		{"a rider was suspended", TypeRider, "UPDATE riders SET status = 'suspended'", ErrAccountBlocked},
		{"a rider's person was blocked", TypeRider, "UPDATE people SET status = 'blocked'", ErrAccountBlocked},
		{"a customer's phone changed", TypeCustomer, "UPDATE people SET phone = '" + newPhone + "'", ErrInvalidOTP},
		{"a customer's person was blocked", TypeCustomer, "UPDATE people SET status = 'blocked'", ErrAccountBlocked},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newOTPFixture(t)
			var id string
			if c.actorType == TypeRider {
				id = f.rider(phoneA)
			} else {
				id = f.signIn(TypeCustomer, phoneA).Actor.ID
				f.exec("DELETE FROM refresh_tokens")
				f.allowResend()
			}
			if err := f.holdActor(c.actorType, id, phoneA, c.change); !errors.Is(err, c.want) {
				t.Errorf("got %v, want %v", err, c.want)
			}
			if n := f.count("SELECT count(*) FROM refresh_tokens"); n != 0 {
				t.Errorf("%d sessions were issued", n)
			}
		})
	}
}

func TestAMissingOrBadRequestIDIsCheapAndRefused(t *testing.T) {
	f := newOTPFixture(t)
	ctx := context.Background()
	for _, bad := range []string{"", "x", "11111111-1111-1111-1111", "11111111-1111-1111-1111-11111111111g", "' OR 1=1 --"} {
		if _, err := f.o.Verify(ctx, TypeCustomer, bad, testCode); !errors.Is(err, ErrInvalidOTP) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	// The lookup must use the primary key, or a flood of bad requests scans the table.
	src, err := os.ReadFile("otp.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "id::text = $1") {
		t.Error("a cast on the column stops the primary key being used")
	}
}

// waitForBlockedQuery waits until a request is really waiting on a lock in this database, instead
// of guessing how long that takes. Reaching the time limit fails the test, so it can never pass
// without the race having happened.
func (f *otpFixture) waitForBlockedQuery() {
	f.t.Helper()
	for i := 0; i < 400; i++ {
		if f.count(`SELECT count(*) FROM pg_locks WHERE NOT granted AND
			(locktype = 'transactionid' OR database = (SELECT oid FROM pg_database WHERE datname = current_database()))`) > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.t.Fatal("no request ever waited on the lock")
}

func TestARiderWhoseNumberChangedBeforeTheCodeIsRefused(t *testing.T) {
	f := newOTPFixture(t)
	ctx := context.Background()
	f.rider(phoneA)
	req, err := f.o.Request(ctx, TypeRider, phoneA)
	if err != nil {
		t.Fatal(err)
	}
	f.exec("UPDATE people SET phone = $1 WHERE phone = $2", phoneB, phoneA)
	if _, err := f.o.Verify(ctx, TypeRider, req.ID, testCode); !errors.Is(err, ErrInvalidOTP) {
		t.Errorf("this is a refusal, not an outage: %v", err)
	}
}

func TestRequireSMS(t *testing.T) {
	if err := RequireSMS(config.Config{OTPTestMode: true}); err != nil {
		t.Errorf("test mode: %v", err)
	}
	if err := RequireSMS(config.Config{}); err == nil {
		t.Error("with no SMS provider, only test mode can start")
	}
}

func TestRequestWaitsSoABurstCannotSlipPast(t *testing.T) {
	f := newOTPFixture(t)
	ctx := context.Background()
	tx, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 1))", TypeCustomer+":"+phoneA); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO otp_requests (actor_type, purpose, phone, code_hash, expires_at)
		VALUES ($1, $2, $3, $4, now() + interval '5 minutes')`, TypeCustomer, purposeSignIn, phoneA, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.o.Request(ctx, TypeCustomer, phoneA)
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrResendLimit) {
		t.Errorf("the second request in a burst: %v", err)
	}
}

// ---------- HTTP

func (f *otpFixture) otpServer(actorType string) *httptest.Server {
	f.t.Helper()
	handler := httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		MountOTP(r, f.o, actorType)
	})
	srv := httptest.NewServer(handler)
	f.t.Cleanup(srv.Close)
	return srv
}

// contractRequired reads a schema's required fields from an app's contract.
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

func TestCustomerOTPEndpoints(t *testing.T) {
	f := newOTPFixture(t)
	srv := f.otpServer(TypeCustomer)
	code, body := call(t, "POST", srv.URL+"/v1/auth/otp/request", "", `{"phone":"`+phoneA+`"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	for _, field := range contractRequired(t, "customer", "OtpRequestResult") {
		if _, ok := body[field]; !ok {
			t.Errorf("the request reply has no %s: %v", field, body)
		}
	}
	code, session := call(t, "POST", srv.URL+"/v1/auth/otp/verify", "", `{"request_id":"`+body["request_id"].(string)+`","otp":"`+testCode+`"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, session)
	}
	for _, field := range contractRequired(t, "customer", "CustomerSession") {
		if _, ok := session[field]; !ok {
			t.Errorf("the session has no %s: %v", field, session)
		}
	}
	if session["phone_verified"] != true {
		t.Errorf("a phone sign-in is verified at once: %v", session)
	}
}

func TestRiderOTPEndpoints(t *testing.T) {
	f := newOTPFixture(t)
	f.rider(phoneA)
	srv := f.otpServer(TypeRider)
	_, body := call(t, "POST", srv.URL+"/v1/auth/otp/request", "", `{"phone":"`+phoneA+`"}`)
	code, session := call(t, "POST", srv.URL+"/v1/auth/otp/verify", "", `{"request_id":"`+body["request_id"].(string)+`","otp":"`+testCode+`"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, session)
	}
	for _, field := range contractRequired(t, "rider", "RiderSession") {
		if _, ok := session[field]; !ok {
			t.Errorf("the session has no %s: %v", field, session)
		}
	}
	if session["status"] != "active" {
		t.Errorf("%v", session)
	}
}

func TestOTPErrorsMatchTheContract(t *testing.T) {
	f := newOTPFixture(t)
	f.rider(phoneB)
	srv := f.otpServer(TypeCustomer)
	post := func(path, body string) (int, map[string]any) { return call(t, "POST", srv.URL+path, "", body) }

	for name, c := range map[string]struct {
		path, body string
		status     int
		code       string
	}{
		"not JSON":         {"/v1/auth/otp/request", `nope`, 400, "invalid_request"},
		"bad phone":        {"/v1/auth/otp/request", `{"phone":"98765"}`, 400, "invalid_request"},
		"verify, no body":  {"/v1/auth/otp/verify", `nope`, 400, "invalid_request"},
		"verify, no code":  {"/v1/auth/otp/verify", `{"request_id":"x"}`, 400, "invalid_request"},
		"a malformed code": {"/v1/auth/otp/verify", `{"request_id":"x","otp":"abc"}`, 400, "invalid_request"},
		"unknown request":  {"/v1/auth/otp/verify", `{"request_id":"x","otp":"123456"}`, 400, "invalid_otp"},
	} {
		got, out := post(c.path, c.body)
		if got != c.status || out["code"] != c.code {
			t.Errorf("%s: %d %v", name, got, out)
		}
		for _, field := range requiredFields(t, "Error") {
			if _, ok := out[field]; !ok {
				t.Errorf("%s: the error has no %s", name, field)
			}
		}
	}

	_, first := post("/v1/auth/otp/request", `{"phone":"`+phoneA+`"}`)
	req, err := http.NewRequest("POST", srv.URL+"/v1/auth/otp/request", strings.NewReader(`{"phone":"`+phoneA+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Errorf("resend: %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	id := first["request_id"].(string)
	if got, out := post("/v1/auth/otp/verify", `{"request_id":"`+id+`","otp":"000000"}`); got != 400 || out["code"] != "invalid_otp" {
		t.Errorf("wrong: %d %v", got, out)
	}
	for i := 1; i < otpMaxAttempts; i++ {
		post("/v1/auth/otp/verify", `{"request_id":"`+id+`","otp":"000000"}`)
	}
	if got, out := post("/v1/auth/otp/verify", `{"request_id":"`+id+`","otp":"000000"}`); got != 429 || out["code"] != "otp_attempts_exceeded" {
		t.Errorf("limit: %d %v", got, out)
	}
	limited, err := http.Post(srv.URL+"/v1/auth/otp/verify", "application/json", strings.NewReader(`{"request_id":"`+id+`","otp":"000000"}`))
	if err != nil {
		t.Fatal(err)
	}
	limited.Body.Close()
	if limited.Header.Get("Retry-After") == "" {
		t.Error("the attempts limit has no Retry-After")
	}
	// A malformed code must not burn an attempt.
	before := f.count("SELECT attempts FROM otp_requests WHERE id = $1", id)
	post("/v1/auth/otp/verify", `{"request_id":"`+id+`","otp":"abc"}`)
	if after := f.count("SELECT attempts FROM otp_requests WHERE id = $1", id); after != before {
		t.Errorf("a malformed code burned an attempt: %d, %d", before, after)
	}
	f.exec("UPDATE otp_requests SET attempts = 0, expires_at = now() - interval '1 second'")
	if got, out := post("/v1/auth/otp/verify", `{"request_id":"`+id+`","otp":"000000"}`); got != 400 || out["code"] != "otp_expired" {
		t.Errorf("expired: %d %v", got, out)
	}
	f.exec("UPDATE otp_requests SET expires_at = now() + interval '5 minutes'")
	f.exec("UPDATE people SET status = 'blocked' WHERE phone = $1", phoneB)
	f.allowResend()
	_, blocked := post("/v1/auth/otp/request", `{"phone":"`+phoneB+`"}`)
	got, out := post("/v1/auth/otp/verify", `{"request_id":"`+blocked["request_id"].(string)+`","otp":"`+testCode+`"}`)
	if got != 403 || out["code"] != "account_blocked" || out["missing_permission"] == nil {
		t.Errorf("blocked: %d %v", got, out)
	}
}

func TestAnOutageDuringSignInIsRetryable(t *testing.T) {
	f := newOTPFixture(t)
	srv := f.otpServer(TypeCustomer)
	f.s.Pool.Close()
	for _, c := range []struct{ path, body string }{
		{"/v1/auth/otp/request", `{"phone":"` + phoneA + `"}`},
		{"/v1/auth/otp/verify", `{"request_id":"11111111-1111-1111-1111-111111111111","otp":"123456"}`},
	} {
		if code, body := call(t, "POST", srv.URL+c.path, "", c.body); code != 503 || body["retryable"] != true {
			t.Errorf("%s: %d %v", c.path, code, body)
		}
	}
}
