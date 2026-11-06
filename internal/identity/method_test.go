package identity

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/config"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

// googleWithPhone is a Google customer who verified a phone at ordering.
func (f *googleFixture) googleWithPhone(subject, phone string) Verified {
	f.t.Helper()
	v, err := f.verify(f.googleSignIn(subject).Actor, phone)
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

// askCode asks for a sign-in code.
func (f *googleFixture) askCode(phone string) OTPRequest {
	f.t.Helper()
	f.allowResend()
	req, err := f.o.Request(context.Background(), TypeCustomer, phone)
	if err != nil {
		f.t.Fatal(err)
	}
	return req
}

func (f *googleFixture) method(customer string) string {
	f.t.Helper()
	var m string
	if err := f.s.Pool.QueryRow(context.Background(), "SELECT sign_in_method FROM customers WHERE id = $1", customer).Scan(&m); err != nil {
		f.t.Fatal(err)
	}
	return m
}

func TestEachCustomerStartsWithTheMethodTheySignedUpWith(t *testing.T) {
	f := newGoogleFixture(t)
	if m := f.method(f.signIn(TypeCustomer, phoneA).Actor.ID); m != MethodPhone {
		t.Errorf("phone sign-up: %s", m)
	}
	if m := f.method(f.googleSignIn("sub-1").Actor.ID); m != MethodGoogle {
		t.Errorf("Google sign-up: %s", m)
	}
}

func TestVerifyingAPhoneDoesNotChangeTheMethod(t *testing.T) {
	f := newGoogleFixture(t)
	v := f.googleWithPhone("sub-1", phoneA)
	if m := f.method(v.Actor.ID); m != MethodGoogle {
		t.Errorf("got %s", m)
	}
}

func TestAPhoneCustomerTappingGoogleByMistakeNeverJoinsTheirAccount(t *testing.T) {
	f := newGoogleFixture(t)
	owner := f.signIn(TypeCustomer, phoneA)
	mistake := f.googleSignIn("sub-1")
	if mistake.Actor.ID == owner.Actor.ID {
		t.Fatal("Google found the phone account")
	}
	if _, err := f.verify(mistake.Actor, phoneA); !errors.Is(err, ErrPhoneAlreadyRegistered) {
		t.Errorf("got %v", err)
	}
	if n := f.count("SELECT count(*) FROM person_identities i JOIN people p ON p.id = i.person_id WHERE p.phone = $1", phoneA); n != 0 {
		t.Error("the Google account joined the phone's account")
	}
	f.mustRefresh(owner.Pair.RefreshToken, TypeCustomer)
	if m := f.method(owner.Actor.ID); m != MethodPhone {
		t.Errorf("the phone account's method changed to %s", m)
	}
}

func TestANumberHeldOnlyByARiderIsRefusedToo(t *testing.T) {
	f := newGoogleFixture(t)
	f.rider(phoneA)
	r := f.googleSignIn("sub-1")
	if _, err := f.verify(r.Actor, phoneA); !errors.Is(err, ErrPhoneAlreadyRegistered) {
		t.Errorf("got %v", err)
	}
	// In the customer app, that rider uses phone and OTP, and stays one person.
	f.allowResend()
	v := f.signIn(TypeCustomer, phoneA)
	if n := f.count("SELECT count(*) FROM customers c JOIN riders r ON r.person_id = c.person_id WHERE c.id = $1", v.Actor.ID); n != 1 {
		t.Error("the customer and rider are not one person")
	}
}

func TestAGoogleCustomerUsingOTPIsToldAndNothingChanges(t *testing.T) {
	f := newGoogleFixture(t)
	ctx := context.Background()
	account := f.googleWithPhone("sub-1", phoneA)
	req := f.askCode(phoneA)
	_, err := f.o.Verify(ctx, TypeCustomer, req.ID, testCode)
	var wrong *WrongMethodError
	if !errors.As(err, &wrong) || wrong.Method != MethodGoogle {
		t.Fatalf("got %v", err)
	}
	f.mustRefresh(account.Pair.RefreshToken, TypeCustomer) // Nothing ended.
	if m := f.method(account.Actor.ID); m != MethodGoogle {
		t.Errorf("got %s", m)
	}
	if n := f.count("SELECT count(*) FROM otp_requests WHERE id = $1 AND consumed_at IS NULL AND attempts = 0", req.ID); n != 1 {
		t.Error("the code must stay usable, so they can choose to switch without a new SMS")
	}
}

func TestAWrongCodeIsCountedBeforeTheMethodIsRevealed(t *testing.T) {
	f := newGoogleFixture(t)
	f.googleWithPhone("sub-1", phoneA)
	req := f.askCode(phoneA)
	_, err := f.o.Verify(context.Background(), TypeCustomer, req.ID, "000000")
	if !errors.Is(err, ErrInvalidOTP) {
		t.Errorf("only the phone's holder may learn the method: %v", err)
	}
	if n := f.count("SELECT count(*) FROM otp_requests WHERE id = $1 AND attempts = 1", req.ID); n != 1 {
		t.Error("a wrong guess must count")
	}
	if _, err := f.o.VerifyAndSwitch(context.Background(), req.ID, "000000"); !errors.Is(err, ErrInvalidOTP) {
		t.Errorf("switching with a wrong code: %v", err)
	}
	if n := f.count("SELECT count(*) FROM person_identities"); n != 1 {
		t.Error("a wrong code changed the method")
	}
}

func TestLostGoogleAccountSwitchesToPhoneAtSignIn(t *testing.T) {
	f := newGoogleFixture(t)
	ctx := context.Background()
	account := f.googleWithPhone("sub-1", phoneA)
	req := f.askCode(phoneA)
	if _, err := f.o.Verify(ctx, TypeCustomer, req.ID, testCode); err == nil {
		t.Fatal("no switch was asked for")
	}
	v, err := f.o.VerifyAndSwitch(ctx, req.ID, testCode) // The same code, with no new SMS.
	if err != nil {
		t.Fatal(err)
	}
	if v.Actor.ID != account.Actor.ID {
		t.Errorf("the account must be kept: %s, %s", v.Actor.ID, account.Actor.ID)
	}
	if m := f.method(v.Actor.ID); m != MethodPhone {
		t.Errorf("got %s", m)
	}
	f.refused(account.Pair.RefreshToken, TypeCustomer) // Every other session ends.
	f.mustRefresh(v.Pair.RefreshToken, TypeCustomer)
	if n := f.count("SELECT count(*) FROM person_identities"); n != 0 {
		t.Error("the old Google account can still sign in")
	}
	if n := f.count("SELECT count(*) FROM outbox_events WHERE topic = 'identity.sign_in_method_changed'"); n != 1 {
		t.Errorf("the phone is not told: %d events", n)
	}
	var before, after string
	if err := f.s.Pool.QueryRow(ctx, "SELECT before::text, after::text FROM audit_events WHERE action = 'customer.sign_in_method_changed'").Scan(&before, &after); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(before, "sub-1") || !strings.Contains(after, MethodPhone) {
		t.Errorf("the change cannot be traced: %s, %s", before, after)
	}
	// Plain phone sign-in now works, and that code cannot be used twice.
	if _, err := f.o.VerifyAndSwitch(ctx, req.ID, testCode); !errors.Is(err, ErrInvalidOTP) {
		t.Errorf("a used code: %v", err)
	}
	again := f.askCode(phoneA)
	if got, err := f.o.Verify(ctx, TypeCustomer, again.ID, testCode); err != nil || got.Actor.ID != account.Actor.ID {
		t.Errorf("%+v, %v", got.Actor, err)
	}
	// That Google account is now a stranger here.
	if g := f.googleSignIn("sub-1"); g.Actor.ID == account.Actor.ID || g.PhoneVerified {
		t.Errorf("the old Google account got back in: %+v", g)
	}
}

func TestASwitchAskedForOnAPhoneAccountIsAPlainSignIn(t *testing.T) {
	f := newGoogleFixture(t)
	first := f.signIn(TypeCustomer, phoneA)
	req := f.askCode(phoneA)
	if _, err := f.o.VerifyAndSwitch(context.Background(), req.ID, testCode); err != nil {
		t.Fatal(err)
	}
	f.mustRefresh(first.Pair.RefreshToken, TypeCustomer) // Other devices carry on.
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'customer.sign_in_method_changed'"); n != 0 {
		t.Error("nothing changed, so nothing should be recorded")
	}
}

func TestABlockedGoogleCustomerCannotSwitch(t *testing.T) {
	f := newGoogleFixture(t)
	account := f.googleWithPhone("sub-1", phoneA)
	req := f.askCode(phoneA)
	f.exec("UPDATE people SET status = 'blocked'")
	if _, err := f.o.VerifyAndSwitch(context.Background(), req.ID, testCode); !errors.Is(err, ErrAccountBlocked) {
		t.Errorf("got %v", err)
	}
	if m := f.method(account.Actor.ID); m != MethodGoogle {
		t.Errorf("got %s", m)
	}
}

func TestSwitchingWhileAGoogleSignInWaitsStopsIt(t *testing.T) {
	f := newGoogleFixture(t)
	ctx := context.Background()
	account := f.googleWithPhone("sub-1", phoneA)
	tx, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if err := lockActor(ctx, tx, TypeCustomer, account.Actor.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.google.SignIn(ctx, f.g.token(t, "sub-1"))
		done <- err
	}()
	f.waitForBlockedQuery()
	if _, err := tx.Exec(ctx, "DELETE FROM person_identities"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Error("a Google account that was just removed signed in")
	}
}

func TestChangingFromPhoneToGoogleInSettings(t *testing.T) {
	f := newGoogleFixture(t)
	ctx := context.Background()
	other := f.signIn(TypeCustomer, phoneA)
	f.allowResend()
	asker := f.signIn(TypeCustomer, phoneA)
	v, err := f.google.ChangeToGoogle(ctx, asker.Actor, f.g.token(t, "sub-1"))
	if err != nil {
		t.Fatal(err)
	}
	if v.Actor.ID != asker.Actor.ID {
		t.Error("the customer changed")
	}
	f.refused(other.Pair.RefreshToken, TypeCustomer)
	f.mustRefresh(v.Pair.RefreshToken, TypeCustomer)
	if g := f.googleSignIn("sub-1"); g.Actor.ID != asker.Actor.ID || !g.PhoneVerified {
		t.Errorf("%+v", g)
	}
	req := f.askCode(phoneA)
	var wrong *WrongMethodError
	if _, err := f.o.Verify(ctx, TypeCustomer, req.ID, testCode); !errors.As(err, &wrong) {
		t.Errorf("phone must now say to use Google: %v", err)
	}
}

func TestChangingToGoogleRemovesAnEmptyAccountFromAnEarlierMistake(t *testing.T) {
	f := newGoogleFixture(t)
	ctx := context.Background()
	mistake := f.googleSignIn("sub-1")
	f.exec("INSERT INTO devices (actor_type, actor_id, platform, push_token) VALUES ('customer', $1, 'android', 'tok')", mistake.Actor.ID)
	owner := f.signIn(TypeCustomer, phoneA)
	v, err := f.google.ChangeToGoogle(ctx, owner.Actor, f.g.token(t, "sub-1"))
	if err != nil {
		t.Fatal(err)
	}
	if v.Actor.ID != owner.Actor.ID {
		t.Error("the customer changed")
	}
	f.refused(mistake.Pair.RefreshToken, TypeCustomer)
	if n := f.count("SELECT count(*) FROM people"); n != 1 {
		t.Errorf("%d people", n)
	}
	if n := f.count("SELECT count(*) FROM devices"); n != 0 {
		t.Errorf("%d devices", n)
	}
	var before string
	if err := f.s.Pool.QueryRow(ctx, "SELECT before::text FROM audit_events WHERE action = 'customer.removed'").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(before, "sub-1") {
		t.Errorf("the removal cannot be traced: %s", before)
	}
	// Its old token cannot send a code now.
	if _, err := f.o.RequestPhone(ctx, mistake.Actor, phoneB); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("got %v", err)
	}
}

func TestChangingToAGoogleAccountOthersDependOnIsRefused(t *testing.T) {
	f := newGoogleFixture(t)
	ctx := context.Background()
	verified := f.googleWithPhone("in-use", phoneB)
	blocked := f.googleSignIn("blocked")
	f.exec("UPDATE people SET status = 'blocked' WHERE id = (SELECT person_id FROM customers WHERE id = $1)", blocked.Actor.ID)
	withRider := f.googleSignIn("rider")
	f.exec("INSERT INTO riders (person_id) SELECT person_id FROM customers WHERE id = $1", withRider.Actor.ID)
	owner := f.signIn(TypeCustomer, phoneA)
	for _, sub := range []string{"in-use", "blocked", "rider"} {
		if _, err := f.google.ChangeToGoogle(ctx, owner.Actor, f.g.token(t, sub)); !errors.Is(err, ErrGoogleInUse) {
			t.Errorf("%s: %v", sub, err)
		}
	}
	f.mustRefresh(owner.Pair.RefreshToken, TypeCustomer)
	f.mustRefresh(verified.Pair.RefreshToken, TypeCustomer)
	if m := f.method(owner.Actor.ID); m != MethodPhone {
		t.Errorf("got %s", m)
	}
	if n := f.count("SELECT count(*) FROM people"); n != 4 {
		t.Errorf("%d people: a refused change removed someone", n)
	}
}

func TestChangingToTheSameMethodIsRefused(t *testing.T) {
	f := newGoogleFixture(t)
	ctx := context.Background()
	phone := f.signIn(TypeCustomer, phoneA)
	f.allowResend()
	req, err := f.o.RequestPhone(ctx, phone.Actor, phoneA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.o.ChangeToPhone(ctx, phone.Actor, req.ID, testCode); !errors.Is(err, ErrSameMethod) {
		t.Errorf("phone: %v", err)
	}
	g := f.googleSignIn("sub-1")
	if _, err := f.google.ChangeToGoogle(ctx, g.Actor, f.g.token(t, "sub-2")); !errors.Is(err, ErrSameMethod) {
		t.Errorf("Google: %v", err)
	}
}

func TestChangingToPhoneInSettings(t *testing.T) {
	f := newGoogleFixture(t)
	ctx := context.Background()
	noPhone := f.googleSignIn("no-phone")
	req, err := f.o.RequestPhone(ctx, noPhone.Actor, phoneB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.o.ChangeToPhone(ctx, noPhone.Actor, req.ID, testCode); !errors.Is(err, ErrPhoneRequired) {
		t.Errorf("no verified phone: %v", err)
	}
	account := f.googleWithPhone("sub-1", phoneA)
	f.allowResend()
	wrongPhone, err := f.o.RequestPhone(ctx, account.Actor, phoneB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.o.ChangeToPhone(ctx, account.Actor, wrongPhone.ID, testCode); !errors.Is(err, ErrPhoneAlreadySet) {
		t.Errorf("another number: %v", err)
	}
	f.allowResend()
	good, err := f.o.RequestPhone(ctx, account.Actor, phoneA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.o.ChangeToPhone(ctx, noPhone.Actor, good.ID, testCode); !errors.Is(err, ErrInvalidOTP) {
		t.Errorf("another customer's code: %v", err)
	}
	v, err := f.o.ChangeToPhone(ctx, account.Actor, good.ID, testCode)
	if err != nil {
		t.Fatal(err)
	}
	if m := f.method(v.Actor.ID); m != MethodPhone {
		t.Errorf("got %s", m)
	}
	f.refused(account.Pair.RefreshToken, TypeCustomer)
	f.mustRefresh(v.Pair.RefreshToken, TypeCustomer)
}

func TestAChangeOfMethodRemovesEveryDevice(t *testing.T) {
	f := newGoogleFixture(t)
	ctx := context.Background()
	stranger := f.googleWithPhone("stranger", phoneA)
	f.exec("INSERT INTO devices (actor_type, actor_id, platform, push_token) VALUES ('customer', $1, 'android', 'stranger-phone')", stranger.Actor.ID)
	req := f.askCode(phoneA)
	if _, err := f.o.VerifyAndSwitch(ctx, req.ID, testCode); err != nil {
		t.Fatal(err)
	}
	if n := f.count("SELECT count(*) FROM devices"); n != 0 {
		t.Error("the stranger's phone would keep getting this account's notifications")
	}
}

func TestAnEndedSessionCannotUndoASwitch(t *testing.T) {
	f := newGoogleFixture(t)
	ctx := context.Background()
	stranger := f.googleWithPhone("stranger", phoneA)
	req := f.askCode(phoneA)
	owner, err := f.o.VerifyAndSwitch(ctx, req.ID, testCode)
	if err != nil {
		t.Fatal(err)
	}
	// The stranger's access token still passes its signature check, for up to 15 minutes.
	if _, err := f.google.ChangeToGoogle(ctx, stranger.Actor, f.g.token(t, "stranger")); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("back to Google: %v", err)
	}
	if m := f.method(owner.Actor.ID); m != MethodPhone {
		t.Errorf("the owner's recovery was undone: %s", m)
	}
	f.mustRefresh(owner.Pair.RefreshToken, TypeCustomer)
	if n := f.count("SELECT count(*) FROM person_identities"); n != 0 {
		t.Error("the stranger's Google account came back")
	}
	sent := len(f.sender.sent)
	f.allowResend()
	if _, err := f.o.RequestPhone(ctx, stranger.Actor, phoneB); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("asking for a code: %v", err)
	}
	if len(f.sender.sent) != sent {
		t.Error("an ended session sent an SMS")
	}
}

func TestAnEndedSessionCannotChangeToPhone(t *testing.T) {
	f := newGoogleFixture(t)
	ctx := context.Background()
	account := f.googleWithPhone("sub-1", phoneA)
	f.allowResend()
	req, err := f.o.RequestPhone(ctx, account.Actor, phoneA)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.SignOut(ctx, account.Actor); err != nil {
		t.Fatal(err)
	}
	if _, err := f.o.ChangeToPhone(ctx, account.Actor, req.ID, testCode); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("got %v", err)
	}
	if m := f.method(account.Actor.ID); m != MethodGoogle {
		t.Errorf("got %s", m)
	}
}

// The account holds its lock while it verifies a phone. A change to Google, waiting for that lock,
// must then see the phone, and must not remove an account that is no longer empty.
func TestAnAccountThatJustVerifiedAPhoneIsNotRemoved(t *testing.T) {
	f := newGoogleFixture(t)
	ctx := context.Background()
	busy := f.googleSignIn("sub-1")
	owner := f.signIn(TypeCustomer, phoneA)
	tx, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if err := lockActor(ctx, tx, TypeCustomer, busy.Actor.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "UPDATE people SET phone = $1 WHERE id = (SELECT person_id FROM customers WHERE id = $2)", phoneB, busy.Actor.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.google.ChangeToGoogle(ctx, owner.Actor, f.g.token(t, "sub-1"))
		done <- err
	}()
	f.waitForBlockedQuery()
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrGoogleInUse) {
		t.Errorf("got %v", err)
	}
	if n := f.count("SELECT count(*) FROM customers WHERE id = $1", busy.Actor.ID); n != 1 {
		t.Error("a verified account was deleted")
	}
	if m := f.method(owner.Actor.ID); m != MethodPhone {
		t.Errorf("got %s", m)
	}
}

// ---------- HTTP and components

func (f *googleFixture) methodServer(enabled []string) *httptest.Server {
	f.t.Helper()
	handler := httpserver.NewRouter(quiet, func(context.Context) error { return nil }, func(r chi.Router) {
		MountMethods(r, enabled, PhoneMethod{OTPs: f.o, ActorType: TypeCustomer}, f.google)
		MountPhoneVerification(r, f.o, f.s.Signer)
		MountMethodChange(r, enabled, f.o, f.google, f.s.Signer)
	})
	srv := httptest.NewServer(handler)
	f.t.Cleanup(srv.Close)
	return srv
}

func TestSwitchingOverHTTP(t *testing.T) {
	f := newGoogleFixture(t)
	srv := f.methodServer([]string{MethodPhone, MethodGoogle})
	f.googleWithPhone("sub-1", phoneA)
	req := f.askCode(phoneA)
	code, out := call(t, "POST", srv.URL+"/v1/auth/otp/verify", "", `{"request_id":"`+req.ID+`","otp":"`+testCode+`"}`)
	if code != 409 || out["code"] != "sign_in_method_mismatch" || out["sign_in_method"] != MethodGoogle {
		t.Fatalf("%d %v", code, out)
	}
	code, out = call(t, "POST", srv.URL+"/v1/auth/otp/verify", "", `{"request_id":"`+req.ID+`","otp":"`+testCode+`","switch_to_phone":true}`)
	if code != 200 || out["phone_verified"] != true {
		t.Fatalf("%d %v", code, out)
	}
	for _, field := range contractRequired(t, "customer", "CustomerSession") {
		if _, ok := out[field]; !ok {
			t.Errorf("the reply has no %s", field)
		}
	}
}

func TestChangingMethodOverHTTP(t *testing.T) {
	f := newGoogleFixture(t)
	srv := f.methodServer([]string{MethodPhone, MethodGoogle})
	url := srv.URL + "/v1/me/sign-in-method"
	owner := f.signIn(TypeCustomer, phoneA)
	bearer := owner.Pair.AccessToken
	if code, _ := call(t, "POST", url, "", `{"method":"google","id_token":"x"}`); code != 401 {
		t.Errorf("no sign-in: %d", code)
	}
	for _, body := range []string{`{}`, `{"method":"apple"}`, `{"method":"google"}`, `{"method":"phone","otp":"123456"}`} {
		if code, out := call(t, "POST", url, bearer, body); code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", body, code, out)
		}
	}
	if code, out := call(t, "POST", url, bearer, `{"method":"google","id_token":"bad"}`); code != 401 || out["code"] != "invalid_google_token" {
		t.Errorf("a bad token: %d %v", code, out)
	}
	f.googleWithPhone("in-use", phoneB)
	if code, out := call(t, "POST", url, bearer, `{"method":"google","id_token":"`+f.g.token(t, "in-use")+`"}`); code != 409 || out["code"] != "google_account_in_use" {
		t.Errorf("in use: %d %v", code, out)
	}
	code, out := call(t, "POST", url, bearer, `{"method":"google","id_token":"`+f.g.token(t, "sub-1")+`"}`)
	if code != 200 || out["customer_id"] != owner.Actor.ID {
		t.Fatalf("%d %v", code, out)
	}
	for _, field := range contractRequired(t, "customer", "CustomerSession") {
		if _, ok := out[field]; !ok {
			t.Errorf("the reply has no %s", field)
		}
	}
	bearer = out["access_token"].(string)
	if code, out := call(t, "POST", url, bearer, `{"method":"google","id_token":"`+f.g.token(t, "sub-1")+`"}`); code != 409 || out["code"] != "sign_in_method_unchanged" {
		t.Errorf("unchanged: %d %v", code, out)
	}
}

func TestAPhoneWithAnAccountOverHTTP(t *testing.T) {
	f := newGoogleFixture(t)
	srv := f.methodServer([]string{MethodPhone, MethodGoogle})
	f.signIn(TypeCustomer, phoneA)
	r := f.googleSignIn("sub-1")
	f.allowResend()
	_, req := call(t, "POST", srv.URL+"/v1/me/phone/otp", r.Pair.AccessToken, `{"phone":"`+phoneA+`"}`)
	code, out := call(t, "POST", srv.URL+"/v1/me/phone/verify", r.Pair.AccessToken, `{"request_id":"`+req["request_id"].(string)+`","otp":"`+testCode+`"}`)
	if code != 409 || out["code"] != "phone_already_registered" {
		t.Errorf("%d %v", code, out)
	}
}

func TestAMethodCanBeDetached(t *testing.T) {
	f := newGoogleFixture(t)
	srv := f.methodServer([]string{MethodPhone})
	if code, _ := call(t, "POST", srv.URL+"/v1/auth/google", "", `{"id_token":"`+f.g.token(t, "sub-1")+`"}`); code != 404 {
		t.Errorf("Google is off: %d", code)
	}
	req := f.askCode(phoneA)
	code, out := call(t, "POST", srv.URL+"/v1/auth/otp/verify", "", `{"request_id":"`+req.ID+`","otp":"`+testCode+`"}`)
	if code != 200 {
		t.Fatalf("phone stays on: %d %v", code, out)
	}
	if code, out := call(t, "POST", srv.URL+"/v1/me/sign-in-method", out["access_token"].(string), `{"method":"google","id_token":"`+f.g.token(t, "sub-1")+`"}`); code != 400 || out["code"] != "invalid_request" {
		t.Errorf("changing to a method that is off: %d %v", code, out)
	}
	if err := RequireMethods(config.Config{SignInMethods: []string{MethodPhone}}); err != nil {
		t.Errorf("phone alone needs no Google client: %v", err)
	}
	if err := RequireMethods(config.Config{SignInMethods: []string{MethodGoogle}}); err == nil {
		t.Error("Google with no client ID must stop the API")
	}
}
