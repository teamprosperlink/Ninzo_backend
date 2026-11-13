package identity

import (
	"os"
	"strings"
	"testing"
)

// Issue 235, part B1: the rider app has a screen for a bad number, so the rider sign-in answers
// invalid_phone. The customer sign-in must go on answering invalid_request, as its contract says.

func TestRiderSignInAnswersInvalidPhoneForABadNumber(t *testing.T) {
	f := newOTPFixture(t)
	f.rider(phoneA)
	srv := f.otpServer(TypeRider)
	for name, body := range map[string]string{
		"no country code":        `{"phone":"9876543210"}`,
		"too short":              `{"phone":"98765"}`,
		"a foreign number":       `{"phone":"+14155552671"}`,
		"an Indian landline":     `{"phone":"+915876543210"}`,
		"spaces":                 `{"phone":"+91 98765 43210"}`,
		"too long":               `{"phone":"+919876543210123"}`,
		"a number, not text":     `{"phone":9876543210}`,
		"a list":                 `{"phone":["+919876543210"]}`,
		"a boolean":              `{"phone":true}`,
		"text with a zero first": `{"phone":"+910876543210"}`,
	} {
		code, out := call(t, "POST", srv.URL+"/v1/auth/otp/request", "", body)
		if code != 400 || out["code"] != "invalid_phone" || out["retryable"] != false {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if n := f.count("SELECT count(*) FROM otp_requests"); n != 0 {
		t.Errorf("%d codes were made for bad numbers", n)
	}
}

func TestRiderSignInKeepsInvalidRequestForAMissingPhoneOrABrokenBody(t *testing.T) {
	f := newOTPFixture(t)
	srv := f.otpServer(TypeRider)
	for name, body := range map[string]string{
		"an empty object": `{}`, "an empty phone": `{"phone":""}`, "a null phone": `{"phone":null}`,
		"a blank phone": `{"phone":"   "}`, "a tab phone": `{"phone":"\t\n"}`,
		"not JSON": `nope`, "an array": `[`, "no body": ``, "a number for the body": `5`,
	} {
		code, out := call(t, "POST", srv.URL+"/v1/auth/otp/request", "", body)
		if code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
}

func TestRiderSignInStillWorksForGoodNumbersKnownOrNot(t *testing.T) {
	f := newOTPFixture(t)
	f.rider(phoneA)
	srv := f.otpServer(TypeRider)
	for name, phone := range map[string]string{"a rider": phoneA, "no account": phoneB} {
		code, out := call(t, "POST", srv.URL+"/v1/auth/otp/request", "", `{"phone":"`+phone+`"}`)
		if code != 200 || out["request_id"] == nil {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
}

func TestCustomerSignInStillAnswersInvalidRequestForABadNumber(t *testing.T) {
	f := newOTPFixture(t)
	srv := f.otpServer(TypeCustomer)
	for name, body := range map[string]string{
		"no country code": `{"phone":"9876543210"}`, "too short": `{"phone":"98765"}`, "a foreign number": `{"phone":"+14155552671"}`,
		"a number, not text": `{"phone":9876543210}`, "a list": `{"phone":["+919876543210"]}`,
		"an empty object": `{}`, "an empty phone": `{"phone":""}`, "not JSON": `nope`,
	} {
		code, out := call(t, "POST", srv.URL+"/v1/auth/otp/request", "", body)
		if code != 400 || out["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	// Hub staff sign in the same way as customers do here: no new code for them either.
	staff := f.otpServer(TypeStaff)
	if code, out := call(t, "POST", staff.URL+"/v1/auth/otp/request", "", `{"phone":"98765"}`); code != 400 || out["code"] != "invalid_request" {
		t.Errorf("staff: %d %v", code, out)
	}
	// The customer contract does not know invalid_phone, so a client built from it never sees one.
	for _, app := range []string{"customer", "partner"} {
		raw, err := os.ReadFile("../../contracts/" + app + "/openapi.yaml")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "invalid_phone") {
			t.Errorf("the %s contract mentions invalid_phone", app)
		}
	}
}
