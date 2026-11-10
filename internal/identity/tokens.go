// Package identity owns people, role profiles and sessions. The rules and reasons are in
// docs/engineering/authentication_rules.md.
package identity

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// The user types with a session. Migration 0030 added staff and 0035 console to the actor_type checks.
const (
	TypeCustomer = "customer"
	TypeRider    = "rider"
	TypeStaff    = "staff"   // Hub staff and hub leads.
	TypeConsole  = "console" // Console staff (decision 0015).
)

const issuer = "ninzo"

var (
	// ErrTokenExpired means the app should refresh and send the request again.
	ErrTokenExpired = errors.New("identity: the access token has expired")
	// ErrTokenInvalid covers everything else wrong with an access token.
	ErrTokenInvalid = errors.New("identity: the access token is not valid")
)

// Actor is who a request is for: one role of one person.
type Actor struct {
	Type string
	ID   string
	// ChainID is the session, so signing out ends only this device.
	ChainID string
}

type accessClaims struct {
	ChainID string `json:"cid"`
	jwt.RegisteredClaims
}

// Signer makes and checks access tokens. Changing the key only makes apps refresh, because refresh
// tokens do not use it, so a key can be replaced at any time.
type Signer struct {
	Key []byte
	TTL time.Duration
	now func() time.Time
}

func (s Signer) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Sign makes an access token. The user type is the token's audience, so a token for one role is
// refused on another role's API.
func (s Signer) Sign(a Actor) (string, error) {
	if a.Type == "" || a.ID == "" || a.ChainID == "" {
		return "", errors.New("identity: an access token needs a type, an actor and a session")
	}
	now := s.clock()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims{
		ChainID: a.ChainID,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   a.ID,
			Audience:  jwt.ClaimStrings{a.Type},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.TTL)),
		},
	})
	signed, err := token.SignedString(s.Key)
	if err != nil {
		return "", fmt.Errorf("identity: signing: %w", err)
	}
	return signed, nil
}

// Verify checks an access token for one user type. It accepts only our algorithm, so a token that
// names another algorithm or none is refused.
func (s Signer) Verify(token, wantType string) (Actor, error) {
	a, _, err := s.VerifyUntil(token, wantType)
	return a, err
}

// VerifyUntil is Verify, and also says when the token expires, so a live stream can close then.
func (s Signer) VerifyUntil(token, wantType string) (Actor, time.Time, error) {
	claims := &accessClaims{}
	_, err := jwt.ParseWithClaims(token, claims,
		func(*jwt.Token) (any, error) { return s.Key, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithAudience(wantType),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(s.clock),
	)
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return Actor{}, time.Time{}, ErrTokenExpired
	case err != nil:
		return Actor{}, time.Time{}, ErrTokenInvalid
	case claims.Subject == "" || claims.ChainID == "":
		return Actor{}, time.Time{}, ErrTokenInvalid
	}
	return Actor{Type: wantType, ID: claims.Subject, ChainID: claims.ChainID}, claims.ExpiresAt.Time, nil
}
