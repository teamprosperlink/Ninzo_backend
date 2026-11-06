package identity

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/config"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
)

const (
	googleKeysURL      = "https://www.googleapis.com/oauth2/v3/certs"
	providerGoogle     = "google"
	googleKeysLife     = time.Hour
	googleRefetchEvery = time.Minute
)

var (
	// ErrGoogleToken means Google did not vouch for this sign-in. Trying again will not help.
	ErrGoogleToken = errors.New("identity: Google could not confirm this sign-in")
	// ErrGoogleUnavailable means Google's keys could not be fetched, so this must not read as a
	// refusal: the sign-in may be fine.
	ErrGoogleUnavailable = errors.New("identity: Google's keys are not available")
)

// GoogleProfile is what a verified ID token says.
type GoogleProfile struct {
	Subject string
	Email   string // Only if Google has verified it. For display only, and never used to merge.
}

// GoogleVerifier checks Google ID tokens against Google's public keys.
type GoogleVerifier struct {
	ClientIDs []string
	KeysURL   string
	HTTP      *http.Client
	now       func() time.Time

	fetching    sync.Mutex // One fetch at a time.
	mu          sync.Mutex // Guards the fields below, and is never held during a fetch.
	keys        map[string]*rsa.PublicKey
	fetchedAt   time.Time
	attemptedAt time.Time
	lastFailed  bool
}

func (g *GoogleVerifier) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

type googleClaims struct {
	Email         string `json:"email"`
	EmailVerified any    `json:"email_verified"` // Google has sent both true and "true".
	jwt.RegisteredClaims
}

// Verify checks the signature, issuer, audience and expiry.
func (g *GoogleVerifier) Verify(ctx context.Context, token string) (GoogleProfile, error) {
	claims := &googleClaims{}
	var unavailable error
	_, err := jwt.ParseWithClaims(token, claims,
		func(t *jwt.Token) (any, error) {
			kid, _ := t.Header["kid"].(string)
			key, err := g.key(ctx, kid)
			if errors.Is(err, ErrGoogleUnavailable) {
				unavailable = err
			}
			return key, err
		},
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(g.clock),
	)
	if unavailable != nil {
		return GoogleProfile{}, unavailable
	}
	if err != nil {
		return GoogleProfile{}, ErrGoogleToken
	}
	if claims.Issuer != "accounts.google.com" && claims.Issuer != "https://accounts.google.com" {
		return GoogleProfile{}, ErrGoogleToken
	}
	// A token for another app must not sign in here, even though Google signed it.
	if !slices.ContainsFunc(claims.Audience, func(aud string) bool { return slices.Contains(g.ClientIDs, aud) }) {
		return GoogleProfile{}, ErrGoogleToken
	}
	if claims.Subject == "" {
		return GoogleProfile{}, ErrGoogleToken
	}
	profile := GoogleProfile{Subject: claims.Subject}
	if claims.EmailVerified == true || claims.EmailVerified == "true" {
		profile.Email = claims.Email
	}
	return profile, nil
}

// key finds a signing key. A key already held is served at once, even if old, and an old list is
// refreshed in the background, so a slow Google holds up nobody. An unknown key waits for a fetch,
// and fetches, good or failed, happen at most once a minute, so bad tokens cannot hammer Google.
func (g *GoogleVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	g.mu.Lock()
	key, known := g.keys[kid]
	stale := g.clock().Sub(g.fetchedAt) >= googleKeysLife
	mayFetch := g.clock().Sub(g.attemptedAt) >= googleRefetchEvery
	g.mu.Unlock()
	if known {
		if stale && mayFetch && g.fetching.TryLock() {
			go func() {
				defer g.fetching.Unlock()
				bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = g.refresh(bg)
			}()
		}
		return key, nil
	}

	g.fetching.Lock()
	defer g.fetching.Unlock()
	g.mu.Lock()
	key, known = g.keys[kid]
	recent, lastFailed := g.clock().Sub(g.attemptedAt) < googleRefetchEvery, g.lastFailed
	g.mu.Unlock()
	switch {
	case known:
		return key, nil
	case recent && lastFailed:
		return nil, ErrGoogleUnavailable // It may be a real new key: only a good list can say it is not.
	case recent:
		return nil, ErrGoogleToken
	}
	// Not on this request's context: one client hanging up must not count as Google failing, and
	// fail everyone else for a minute. fetch has its own time limit.
	if err := g.refresh(context.WithoutCancel(ctx)); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrGoogleUnavailable, err)
	}
	g.mu.Lock()
	key, known = g.keys[kid]
	g.mu.Unlock()
	if !known {
		return nil, ErrGoogleToken
	}
	return key, nil
}

// refresh fetches the list with no lock held during the call. Callers hold g.fetching.
func (g *GoogleVerifier) refresh(ctx context.Context) error {
	started := g.clock()
	keys, err := g.fetch(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.attemptedAt, g.lastFailed = started, err != nil
	if err != nil {
		return err
	}
	g.keys, g.fetchedAt = keys, started
	return nil
}

func (g *GoogleVerifier) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	url := g.KeysURL
	if url == "" {
		url = googleKeysURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := g.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return nil, errors.New("no usable keys")
	}
	return keys, nil
}

// GoogleSignIn signs customers in with Google. Riders do not use Google.
type GoogleSignIn struct {
	Pool     *pgxpool.Pool
	Sessions *Sessions
	Verifier interface {
		Verify(context.Context, string) (GoogleProfile, error)
	}
	Log    *slog.Logger
	Limits *Limits
}

// NewGoogleSignIn builds GoogleSignIn from a service's settings.
func NewGoogleSignIn(pool *pgxpool.Pool, s *Sessions, cfg config.Config, log *slog.Logger) *GoogleSignIn {
	return &GoogleSignIn{Pool: pool, Sessions: s, Verifier: &GoogleVerifier{ClientIDs: cfg.GoogleClientIDs}, Log: log}
}

// RequireGoogle stops the customer API starting with Google on the login screen and no way to
// check its tokens.
func RequireGoogle(cfg config.Config) error {
	if len(cfg.GoogleClientIDs) == 0 {
		return errors.New("GOOGLE_CLIENT_IDS is not set")
	}
	return nil
}

// GoogleResult is a successful Google sign-in.
type GoogleResult struct {
	Pair          Pair
	Actor         Actor
	PhoneVerified bool
}

// SignIn verifies the token, outside any transaction, then finds or creates the account. Email is
// never used to find an account, and accounts never merge. See decision 0009.
func (g *GoogleSignIn) SignIn(ctx context.Context, idToken string) (GoogleResult, error) {
	profile, err := g.Verifier.Verify(ctx, idToken)
	if err != nil {
		return GoogleResult{}, err
	}
	var out GoogleResult
	err = database.InTx(ctx, g.Pool, func(tx pgx.Tx) error {
		// One at a time per Google account, so a double tap cannot make two people.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 4))", providerGoogle+":"+profile.Subject); err != nil {
			return err
		}
		customer, err := findOrCreateGoogleCustomer(ctx, tx, profile)
		if err != nil {
			return err
		}
		if err := lockActor(ctx, tx, TypeCustomer, customer); err != nil {
			return err
		}
		// Read again now the lock is held: a change of method or a block may have just finished.
		var active, stillLinked, hasPhone bool
		err = tx.QueryRow(ctx, `SELECT p.status = 'active', p.phone IS NOT NULL,
				EXISTS (SELECT 1 FROM person_identities i WHERE i.person_id = p.id AND i.provider = $2 AND i.subject = $3)
			FROM customers c JOIN people p ON p.id = c.person_id WHERE c.id = $1`,
			customer, providerGoogle, profile.Subject).Scan(&active, &hasPhone, &stillLinked)
		if err != nil {
			return err // Includes a customer just removed: the app retries, and gets a fresh answer.
		}
		if !active {
			return ErrAccountBlocked
		}
		if !stillLinked {
			return errors.New("identity: this Google account just moved, so try again")
		}
		pair, actor, err := g.Sessions.Start(ctx, tx, TypeCustomer, customer)
		if err != nil {
			return err
		}
		out = GoogleResult{Pair: pair, Actor: actor, PhoneVerified: hasPhone}
		return audit.Record(ctx, tx, audit.Event{
			ActorType: TypeCustomer, ActorID: customer, Action: "session.started", EntityType: "session", EntityID: actor.ChainID, Reason: "Google",
		})
	})
	if err != nil {
		return GoogleResult{}, err
	}
	return out, nil
}

func findOrCreateGoogleCustomer(ctx context.Context, tx pgx.Tx, profile GoogleProfile) (string, error) {
	var person, status string
	err := tx.QueryRow(ctx, `SELECT p.id::text, p.status FROM person_identities i JOIN people p ON p.id = i.person_id
		WHERE i.provider = $1 AND i.subject = $2`, providerGoogle, profile.Subject).Scan(&person, &status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if err := tx.QueryRow(ctx, "INSERT INTO people DEFAULT VALUES RETURNING id::text").Scan(&person); err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO person_identities (person_id, provider, subject, email) VALUES ($1, $2, $3, NULLIF($4, ''))",
			person, providerGoogle, profile.Subject, profile.Email); err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	case status != "active":
		return "", ErrAccountBlocked
	}
	var id string
	err = tx.QueryRow(ctx, "SELECT id::text FROM customers WHERE person_id = $1", person).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.QueryRow(ctx, "INSERT INTO customers (person_id, sign_in_method) VALUES ($1, $2) RETURNING id::text", person, MethodGoogle).Scan(&id); err != nil {
			return "", err
		}
		return id, audit.Record(ctx, tx, audit.Event{
			ActorType: TypeCustomer, ActorID: id, Action: "customer.created", EntityType: "customer", EntityID: id, Reason: "first Google sign-in",
		})
	}
	return id, err
}
