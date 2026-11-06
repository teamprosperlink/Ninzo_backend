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
	pair, _, err := s.issue(ctx, tx, actor)
	if err != nil {
		return Pair{}, Actor{}, err
	}
	return pair, actor, nil
}

// issue adds one refresh token to a chain, with a fresh 90 days, and signs an access token.
func (s *Sessions) issue(ctx context.Context, tx pgx.Tx, a Actor) (Pair, string, error) {
	token, hash, err := newRefreshToken()
	if err != nil {
		return Pair{}, "", err
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO refresh_tokens (actor_type, actor_id, chain_id, token_hash, expires_at)
		VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5)) RETURNING id::text`,
		a.Type, a.ID, a.ChainID, hash, s.RefreshTTL.Seconds()).Scan(&id)
	if err != nil {
		return Pair{}, "", fmt.Errorf("identity: storing a refresh token: %w", err)
	}
	access, err := s.Signer.Sign(a)
	if err != nil {
		return Pair{}, "", err
	}
	return Pair{AccessToken: access, RefreshToken: token, ExpiresInS: int(s.Signer.TTL.Seconds())}, id, nil
}

// Refresh swaps a refresh token for a new pair. See section 4 of the rules:
//   - An unused token gets a new pair.
//   - A used token whose successor is unused means the reply was lost, so it gets a new pair, and
//     that successor is revoked, because it can no longer be the app's only token.
//   - Anything else used or revoked may be theft, so the whole chain ends. A real app whose token
//     was replaced by a thief's retry therefore ends the thief's session too.
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

// refresh reports a refusal separately from an error, so a chain's revocation still commits.
func (s *Sessions) refresh(ctx context.Context, tx pgx.Tx, token, wantType string) (Pair, Actor, bool, error) {
	var id string
	var a Actor
	var replacedBy *string
	var used, revoked, expired bool
	err := tx.QueryRow(ctx, `SELECT id::text, actor_type, actor_id::text, chain_id::text, replaced_by::text,
			used_at IS NOT NULL, revoked_at IS NOT NULL, expires_at <= now()
		FROM refresh_tokens WHERE token_hash = $1 FOR UPDATE`, hashToken(token)).
		Scan(&id, &a.Type, &a.ID, &a.ChainID, &replacedBy, &used, &revoked, &expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return Pair{}, Actor{}, true, nil
	}
	if err != nil {
		return Pair{}, Actor{}, false, err
	}
	// Another role's token at this API is a mistake, not theft, so it changes nothing.
	if a.Type != wantType {
		return Pair{}, Actor{}, true, nil
	}
	if revoked {
		return Pair{}, Actor{}, true, s.endChain(ctx, tx, a, "a revoked refresh token was presented")
	}
	if expired {
		return Pair{}, Actor{}, true, nil
	}
	active, err := isActive(ctx, tx, a)
	if err != nil {
		return Pair{}, Actor{}, false, err
	}
	if !active {
		return Pair{}, Actor{}, true, s.endChain(ctx, tx, a, "the account is blocked, suspended or gone")
	}

	if used {
		lost, err := successorUnused(ctx, tx, replacedBy)
		if err != nil {
			return Pair{}, Actor{}, false, err
		}
		if !lost {
			return Pair{}, Actor{}, true, s.endChain(ctx, tx, a, "a used refresh token was presented")
		}
		// The app never received the successor, so nothing can use it after this.
		if _, err := tx.Exec(ctx, "UPDATE refresh_tokens SET revoked_at = now() WHERE id = $1", *replacedBy); err != nil {
			return Pair{}, Actor{}, false, err
		}
	}
	pair, next, err := s.issue(ctx, tx, a)
	if err != nil {
		return Pair{}, Actor{}, false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET replaced_by = NULL WHERE id = $1`, id); err != nil {
		return Pair{}, Actor{}, false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET used_at = COALESCE(used_at, now()), replaced_by = $2 WHERE id = $1`, id, next); err != nil {
		return Pair{}, Actor{}, false, err
	}
	return pair, a, false, nil
}

func successorUnused(ctx context.Context, tx pgx.Tx, successor *string) (bool, error) {
	if successor == nil {
		return false, nil
	}
	var unused bool
	err := tx.QueryRow(ctx, `SELECT used_at IS NULL AND revoked_at IS NULL FROM refresh_tokens WHERE id = $1 FOR UPDATE`, *successor).Scan(&unused)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return unused, err
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

// SignOut ends this device's session. Doing it twice is fine.
func (s *Sessions) SignOut(ctx context.Context, a Actor) error {
	_, err := s.Pool.Exec(ctx, "UPDATE refresh_tokens SET revoked_at = now() WHERE chain_id = $1 AND actor_id = $2 AND actor_type = $3 AND revoked_at IS NULL",
		a.ChainID, a.ID, a.Type)
	return err
}

// EndAll ends every session of one role, in the caller's transaction, for a suspension, a password
// reset or a phone change. keepChain is left running, for the session making the change, and may
// be empty. Access tokens already issued last up to their TTL, so status is also checked at refresh.
func (s *Sessions) EndAll(ctx context.Context, tx pgx.Tx, actorType, actorID, keepChain string) error {
	_, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now()
		WHERE actor_type = $1 AND actor_id = $2 AND revoked_at IS NULL AND ($3 = '' OR chain_id::text <> $3)`,
		actorType, actorID, keepChain)
	return err
}
