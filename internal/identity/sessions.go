package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/config"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
)

// ErrSessionInvalid means the user must sign in again. It does not say why, so a stolen token
// teaches an attacker nothing.
var ErrSessionInvalid = errors.New("identity: sign in again")

// Pair is what a sign-in or a refresh returns.
type Pair struct {
	AccessToken  string
	RefreshToken string
	ExpiresInS   int
}

// Sessions issues and renews sessions. A session is a chain of single-use refresh tokens, each
// stored only as a hash.
type Sessions struct {
	Pool       *pgxpool.Pool
	Signer     Signer
	RefreshTTL time.Duration
	Log        *slog.Logger
	Limits     *Limits
}

// NewSessions builds Sessions from a service's settings.
func NewSessions(pool *pgxpool.Pool, cfg config.Config, log *slog.Logger) *Sessions {
	return &Sessions{
		Pool:       pool,
		Signer:     Signer{Key: []byte(cfg.JWTSigningKey), TTL: cfg.AccessTTL},
		RefreshTTL: cfg.RefreshTTL,
		Log:        log,
	}
}

func newRefreshToken() (token, hash string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, hashToken(token), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Start begins a new session, in the caller's transaction, so it commits with the sign-in.
func (s *Sessions) Start(ctx context.Context, tx pgx.Tx, actorType, actorID string) (Pair, Actor, error) {
	var chain string
	if err := tx.QueryRow(ctx, "SELECT gen_random_uuid()::text").Scan(&chain); err != nil {
		return Pair{}, Actor{}, err
	}
	actor := Actor{Type: actorType, ID: actorID, ChainID: chain}
	pair, _, err := s.issue(ctx, tx, actor, 0)
	if err != nil {
		return Pair{}, Actor{}, err
	}
	return pair, actor, nil
}

// issue adds one refresh token to a chain, with a fresh 90 days, and signs an access token.
func (s *Sessions) issue(ctx context.Context, tx pgx.Tx, a Actor, generation int) (Pair, string, error) {
	token, hash, err := newRefreshToken()
	if err != nil {
		return Pair{}, "", err
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO refresh_tokens (actor_type, actor_id, chain_id, generation, token_hash, expires_at)
		VALUES ($1, $2, $3, $4, $5, now() + make_interval(secs => $6)) RETURNING id::text`,
		a.Type, a.ID, a.ChainID, generation, hash, s.RefreshTTL.Seconds()).Scan(&id)
	if err != nil {
		return Pair{}, "", fmt.Errorf("identity: storing a refresh token: %w", err)
	}
	access, err := s.Signer.Sign(a)
	if err != nil {
		return Pair{}, "", err
	}
	return Pair{AccessToken: access, RefreshToken: token, ExpiresInS: int(s.Signer.TTL.Seconds())}, id, nil
}

// Refresh swaps a refresh token for a new pair. See section 4 of the rules. Each renewal is one
// generation further along the chain:
//   - A token is good until a token of a later generation has been used, or another token of its
//     own generation has. A used token can therefore be sent again, so a lost reply or a delayed
//     duplicate never signs anyone out: the extra tokens it makes are simply never used.
//   - Otherwise there are two holders of this session, so one may be a thief, and the chain ends.
func (s *Sessions) Refresh(ctx context.Context, token, wantType string) (Pair, Actor, error) {
	var pair Pair
	var actor Actor
	var refused bool
	err := database.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		pair, actor, refused, err = s.refresh(ctx, tx, token, wantType)
		return err
	})
	if err != nil {
		return Pair{}, Actor{}, err
	}
	if refused {
		return Pair{}, Actor{}, ErrSessionInvalid
	}
	return pair, actor, nil
}

// lockChain makes everything that changes a chain take turns until its transaction ends. Without
// it, ending a chain could miss a token that a refresh in progress has not committed yet, and a
// thief renewing in a loop could outlive a sign-out or a password reset.
func lockChain(ctx context.Context, tx pgx.Tx, chain string) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", chain)
	return err
}

// refresh reports a refusal separately from an error, so a chain's revocation still commits.
func (s *Sessions) refresh(ctx context.Context, tx pgx.Tx, token, wantType string) (Pair, Actor, bool, error) {
	hash := hashToken(token)
	var chain string
	err := tx.QueryRow(ctx, "SELECT chain_id::text FROM refresh_tokens WHERE token_hash = $1", hash).Scan(&chain)
	if errors.Is(err, pgx.ErrNoRows) {
		return Pair{}, Actor{}, true, nil
	}
	if err != nil {
		return Pair{}, Actor{}, false, err
	}
	if err := lockChain(ctx, tx, chain); err != nil {
		return Pair{}, Actor{}, false, err
	}

	var id string
	var a Actor
	var generation int
	var revoked, expired bool
	err = tx.QueryRow(ctx, `SELECT id::text, actor_type, actor_id::text, chain_id::text, generation,
			revoked_at IS NOT NULL, expires_at <= now()
		FROM refresh_tokens WHERE token_hash = $1`, hash).
		Scan(&id, &a.Type, &a.ID, &a.ChainID, &generation, &revoked, &expired)
	if err != nil {
		return Pair{}, Actor{}, false, err
	}
	// Another role's token at this API is a mistake, not theft, so it changes nothing.
	if a.Type != wantType || revoked || expired {
		return Pair{}, Actor{}, true, nil
	}
	active, err := isActive(ctx, tx, a)
	if err != nil {
		return Pair{}, Actor{}, false, err
	}
	if !active {
		return Pair{}, Actor{}, true, s.endChain(ctx, tx, a, "the account is blocked, suspended or gone")
	}

	var twoHolders bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM refresh_tokens
		WHERE chain_id = $1 AND used_at IS NOT NULL AND (generation > $2 OR (generation = $2 AND id <> $3)))`,
		a.ChainID, generation, id).Scan(&twoHolders)
	if err != nil {
		return Pair{}, Actor{}, false, err
	}
	if twoHolders {
		return Pair{}, Actor{}, true, s.endChain(ctx, tx, a, "two holders of one session")
	}

	pair, next, err := s.issue(ctx, tx, a, generation+1)
	if err != nil {
		return Pair{}, Actor{}, false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET used_at = COALESCE(used_at, now()), replaced_by = $2 WHERE id = $1`, id, next); err != nil {
		return Pair{}, Actor{}, false, err
	}
	return pair, a, false, nil
}

// isActive checks the person and the role. An applicant is active: they can sign in, and only
// their actions are limited.
func isActive(ctx context.Context, tx pgx.Tx, a Actor) (bool, error) {
	var query string
	switch a.Type {
	case TypeCustomer:
		query = `SELECT p.status = 'active' FROM customers c JOIN people p ON p.id = c.person_id WHERE c.id = $1`
	case TypeRider:
		query = `SELECT p.status = 'active' AND r.status <> 'suspended' FROM riders r JOIN people p ON p.id = r.person_id WHERE r.id = $1`
	case TypeStaff:
		query = `SELECT p.status = 'active' AND s.status = 'active' FROM hub_staff s JOIN people p ON p.id = s.person_id WHERE s.id = $1`
	case TypeConsole:
		query = `SELECT active FROM console_users WHERE id = $1`
	default:
		return false, fmt.Errorf("identity: unknown user type %q", a.Type)
	}
	var active bool
	err := tx.QueryRow(ctx, query, a.ID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return active, err
}

func (s *Sessions) endChain(ctx context.Context, tx pgx.Tx, a Actor, reason string) error {
	tag, err := tx.Exec(ctx, "UPDATE refresh_tokens SET revoked_at = now() WHERE chain_id = $1 AND revoked_at IS NULL", a.ChainID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil // Already ended, so there is nothing new to record.
	}
	s.Log.Warn("identity: session ended", "actor_type", a.Type, "actor_id", a.ID, "reason", reason)
	return audit.Record(ctx, tx, audit.Event{
		ActorType: a.Type, ActorID: a.ID, Action: "session.ended",
		EntityType: "session", EntityID: a.ChainID, Reason: reason,
	})
}

// SignOut ends this device's session. Doing it twice is fine. An access token already issued lasts
// up to its TTL.
func (s *Sessions) SignOut(ctx context.Context, a Actor) error {
	return database.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := lockChain(ctx, tx, a.ChainID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "UPDATE refresh_tokens SET revoked_at = now() WHERE chain_id = $1 AND actor_id = $2 AND actor_type = $3 AND revoked_at IS NULL",
			a.ChainID, a.ID, a.Type)
		return err
	})
}

// EndAll ends every session of one role, in the caller's transaction, for a suspension, a password
// reset or a phone change. keepChain is left running, for the session making the change, and may
// be empty. Access tokens already issued last up to their TTL, so status is also checked at refresh.
func (s *Sessions) EndAll(ctx context.Context, tx pgx.Tx, actorType, actorID, keepChain string) error {
	// A sign-in in progress finishes first, so its new session is seen and ended below.
	if err := lockActor(ctx, tx, actorType, actorID); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT chain_id::text FROM refresh_tokens
		WHERE actor_type = $1 AND actor_id = $2 AND revoked_at IS NULL AND ($3 = '' OR chain_id::text <> $3)
		ORDER BY 1`, actorType, actorID, keepChain)
	if err != nil {
		return err
	}
	chains, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	// In a fixed order, so two calls cannot deadlock. A refresh in progress finishes first, and its
	// new token is then seen and ended below.
	for _, chain := range chains {
		if err := lockChain(ctx, tx, chain); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now()
		WHERE actor_type = $1 AND actor_id = $2 AND revoked_at IS NULL AND ($3 = '' OR chain_id::text <> $3)`,
		actorType, actorID, keepChain)
	return err
}
