package identity

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	testKey = []byte("test-signing-key-long-enough-for-hs256")
	rider   = Actor{Type: TypeRider, ID: "11111111-1111-1111-1111-111111111111", ChainID: "22222222-2222-2222-2222-222222222222"}
)

func TestTokenRoundTrip(t *testing.T) {
	s := Signer{Key: testKey, TTL: 15 * time.Minute}
	token, err := s.Sign(rider)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Verify(token, TypeRider)
	if err != nil || got != rider {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestTokenIsRefused(t *testing.T) {
	good := Signer{Key: testKey, TTL: 15 * time.Minute}
	token, err := good.Sign(rider)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := good.Verify(token, TypeCustomer); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("a rider token on the customer API: %v", err)
	}
	other := Signer{Key: []byte("a-different-signing-key-long-enough"), TTL: time.Minute}
	if _, err := other.Verify(token, TypeRider); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("another key: %v", err)
	}
	parts := strings.Split(token, ".")
	claims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(claims), rider.ID, "33333333-3333-3333-3333-333333333333", 1))) + "." + parts[2]
	if _, err := good.Verify(tampered, TypeRider); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("a changed actor: %v", err)
	}
	for _, bad := range []string{"", "x", "a.b.c", token + "x"} {
		if _, err := good.Verify(bad, TypeRider); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestOtherAlgorithmsAreRefused(t *testing.T) {
	s := Signer{Key: testKey, TTL: time.Minute}
	claims := accessClaims{ChainID: rider.ChainID, RegisteredClaims: jwt.RegisteredClaims{
		Issuer: issuer, Subject: rider.ID, Audience: jwt.ClaimStrings{TypeRider},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}}
	none, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(none, TypeRider); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("alg none: %v", err)
	}
	hs512, err := jwt.NewWithClaims(jwt.SigningMethodHS512, claims).SignedString(testKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(hs512, TypeRider); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("HS512: %v", err)
	}
}

func TestTokenClaimsAreRequired(t *testing.T) {
	s := Signer{Key: testKey, TTL: time.Minute}
	base := jwt.RegisteredClaims{Issuer: issuer, Subject: rider.ID, Audience: jwt.ClaimStrings{TypeRider}, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}
	cases := map[string]accessClaims{
		"no expiry":    {ChainID: "c", RegisteredClaims: jwt.RegisteredClaims{Issuer: issuer, Subject: rider.ID, Audience: jwt.ClaimStrings{TypeRider}}},
		"no session":   {RegisteredClaims: base},
		"no subject":   {ChainID: "c", RegisteredClaims: jwt.RegisteredClaims{Issuer: issuer, Audience: jwt.ClaimStrings{TypeRider}, ExpiresAt: base.ExpiresAt}},
		"wrong issuer": {ChainID: "c", RegisteredClaims: jwt.RegisteredClaims{Issuer: "other", Subject: rider.ID, Audience: jwt.ClaimStrings{TypeRider}, ExpiresAt: base.ExpiresAt}},
	}
	for name, claims := range cases {
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(testKey)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Verify(token, TypeRider); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestExpiryIsReportedSoTheAppCanRefresh(t *testing.T) {
	start := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	now := start
	s := Signer{Key: testKey, TTL: 15 * time.Minute, now: func() time.Time { return now }}
	token, err := s.Sign(rider)
	if err != nil {
		t.Fatal(err)
	}
	now = start.Add(14 * time.Minute)
	if _, err := s.Verify(token, TypeRider); err != nil {
		t.Errorf("before expiry: %v", err)
	}
	now = start.Add(16 * time.Minute)
	if _, err := s.Verify(token, TypeRider); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("after expiry: %v", err)
	}
}

func TestSignNeedsEveryField(t *testing.T) {
	s := Signer{Key: testKey, TTL: time.Minute}
	for _, a := range []Actor{{ID: "i", ChainID: "c"}, {Type: "t", ChainID: "c"}, {Type: "t", ID: "i"}} {
		if _, err := s.Sign(a); err == nil {
			t.Errorf("%+v must be refused", a)
		}
	}
}

func TestTokensHoldNoPersonalData(t *testing.T) {
	s := Signer{Key: testKey, TTL: time.Minute}
	token, err := s.Sign(rider)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1])
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"phone", "email", "name"} {
		if strings.Contains(string(raw), field) {
			t.Errorf("the token holds %s: %s", field, raw)
		}
	}
}
