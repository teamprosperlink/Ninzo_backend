// Package idempotency makes a repeated write return its first result instead of acting twice.
// The rules are in contracts/README.md, under Idempotency.
//
// A handler that changes data must call Save in the same transaction as the change, so the two
// commit together. A crash can then never leave a change with no stored reply, which a retry
// would run a second time.
//
//	err := database.InTx(ctx, pool, func(tx pgx.Tx) error {
//	    ...change data...
//	    return idempotency.Save(ctx, tx, http.StatusOK, reply)
//	})
//	if err == nil { httpserver.WriteJSON(w, http.StatusOK, reply) }
//
// Rider actions that can be queued offline do not use this: their key is the event_id, tracked
// per rider across the direct and queued paths (contracts/README.md, Rider offline events).
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/sensitive"
)

const (
	// lockFor is how long a running request holds its key. It must outlast the server's request
	// timeout, so a request still running can never lose its key to a retry.
	lockFor = 30 * time.Second
	keptFor = 24 * time.Hour
)

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

// ErrClaimLost means another request took over this key, so the change must not commit.
var ErrClaimLost = errors.New("idempotency: this request no longer holds its key")

// Options configures the middleware for a group of routes.
type Options struct {
	// Actor returns the signed-in user's ID. A key belongs to one user.
	Actor func(*http.Request) (string, bool)
	// IgnoreBodyFields are left out of the request hash, such as passwords, which must not be
	// fingerprinted in a stored row.
	IgnoreBodyFields []string
	Log              *slog.Logger
}

type claim struct {
	actor, method, path, key string
	lockedUntil              time.Time
	saved                    bool
}

type claimKey struct{}

// Save stores the reply in the handler's transaction. Secret fields are left out of the stored
// copy. handover_otp stays, as null, so a replayed Order is still valid: the app gets the live
// code from GET or the stream, and a stored copy can never outlive the order clearing it.
func Save(ctx context.Context, tx pgx.Tx, status int, body any) error {
	c, ok := ctx.Value(claimKey{}).(*claim)
	if !ok {
		return errors.New("idempotency: Save was called outside the middleware")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("idempotency: %w", err)
	}
	if err := store(ctx, tx, c, status, raw); err != nil {
		return err
	}
	c.saved = true
	return nil
}

// txExec is a transaction, or the pool for the fallback in finish.
type txExec interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func store(ctx context.Context, tx txExec, c *claim, status int, raw []byte) error {
	if status < 200 || status > 299 {
		return fmt.Errorf("idempotency: only 2xx replies are stored, got %d", status)
	}
	stored, err := sensitive.Strip(raw)
	if err != nil {
		return fmt.Errorf("idempotency: the reply is not JSON: %w", err)
	}
	// locked_until is the fence: a request whose lock expired and was taken over matches no row.
	tag, err := tx.Exec(ctx, `UPDATE idempotency_keys SET status = $5, response = $6, locked_until = NULL
		WHERE actor_id = $1 AND method = $2 AND path = $3 AND key = $4 AND status IS NULL AND locked_until = $7`,
		c.actor, c.method, c.path, c.key, status, stored, c.lockedUntil)
	if err != nil {
		return fmt.Errorf("idempotency: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrClaimLost
	}
	return nil
}

type recorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Middleware applies the contract's rules to the writes it wraps.
func Middleware(pool *pgxpool.Pool, opts Options) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("Idempotency-Key")
			if !keyPattern.MatchString(key) {
				httpserver.WriteError(w, http.StatusBadRequest, "idempotency_key_required", "Send an Idempotency-Key of 8 to 128 characters.", false)
				return
			}
			actor, ok := opts.Actor(r)
			if !ok {
				httpserver.WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in again.", false)
				return
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "The body could not be read.", false)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			c := &claim{actor: actor, method: r.Method, path: r.URL.Path, key: key}
			outcome, err := c.acquire(r.Context(), pool, hashBody(body, opts.IgnoreBodyFields))
			if err != nil {
				opts.Log.Error("idempotency: claim failed", "request_id", httpserver.RequestID(r.Context()), "error", err.Error())
				w.Header().Set("Retry-After", "5")
				httpserver.WriteError(w, http.StatusServiceUnavailable, "service_unavailable", "Try again shortly.", true)
				return
			}
			switch outcome.kind {
			case replay:
				w.Header().Set("Idempotency-Replayed", "true")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(outcome.status)
				_, _ = w.Write(outcome.body)
				return
			case inProgress:
				w.Header().Set("Retry-After", strconv.Itoa(outcome.retryAfter))
				httpserver.WriteError(w, http.StatusConflict, "idempotency_in_progress", "This key is still being processed.", true)
				return
			case reused:
				httpserver.WriteError(w, http.StatusUnprocessableEntity, "idempotency_key_reused", "This key was used with a different body.", false)
				return
			}

			rec := &recorder{ResponseWriter: w}
			finished := false
			defer func() {
				if !finished {
					c.release(pool, opts.Log) // A panic: the outer handler reports it.
				}
			}()
			next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), claimKey{}, c)))
			finished = true
			c.finish(pool, opts.Log, rec)
		})
	}
}

// finish keeps a saved reply, and frees the key otherwise, so a rejected request can retry.
func (c *claim) finish(pool *pgxpool.Pool, log *slog.Logger, rec *recorder) {
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	if status < 200 || status > 299 {
		c.release(pool, log)
		return
	}
	// Normally the handler's Save committed, so this changes nothing and returns ErrClaimLost. It
	// matters when a handler forgot Save, or rolled back and still replied 2xx: without a stored
	// reply, a retry would act again. A crash before this line can still repeat the action, which
	// is why handlers must save in their own transaction.
	raw := rec.body.Bytes()
	if !json.Valid(raw) {
		raw = []byte("null") // A replay with no body beats acting twice.
	}
	// Short when Save ran, because a committed reply must not wait on a stalled pool.
	wait := 5 * time.Second
	if c.saved {
		wait = time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	switch err := store(ctx, pool, c, status, raw); {
	case err == nil:
		log.Error("idempotency: a handler replied 2xx with no committed Save, which is a bug to fix", "path", c.path, "status", status)
	case errors.Is(err, ErrClaimLost):
		if !c.saved {
			log.Error("idempotency: a handler lost its key and still replied 2xx", "path", c.path, "status", status)
		}
	default:
		log.Error("idempotency: could not store the reply", "path", c.path, "error", err.Error())
		if !c.saved {
			c.release(pool, log)
		}
	}
}

// release frees an unfinished key, on its own context because the request's may have ended. Its
// condition keeps a saved reply, and leaves a key another request has taken over.
func (c *claim) release(pool *pgxpool.Pool, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx, `DELETE FROM idempotency_keys
		WHERE actor_id = $1 AND method = $2 AND path = $3 AND key = $4 AND status IS NULL AND locked_until = $5`,
		c.actor, c.method, c.path, c.key, c.lockedUntil)
	if err != nil {
		log.Warn("idempotency: could not free a key, so it frees when its lock ends", "path", c.path, "error", err.Error())
	}
}

type outcomeKind int

const (
	acquired outcomeKind = iota
	replay
	inProgress
	reused
)

type outcome struct {
	kind       outcomeKind
	status     int
	body       []byte
	retryAfter int
}

// acquire takes the key in one statement, so two requests can never both win. It wins when the
// key is new, older than 24 hours, or held by a crashed request with the same body.
func (c *claim) acquire(ctx context.Context, pool *pgxpool.Pool, hash string) (outcome, error) {
	for attempt := 0; attempt < 3; attempt++ {
		err := pool.QueryRow(ctx, `INSERT INTO idempotency_keys (actor_id, method, path, key, request_hash, locked_until)
			VALUES ($1, $2, $3, $4, $5, now() + make_interval(secs => $6))
			ON CONFLICT (actor_id, method, path, key) DO UPDATE
				SET request_hash = EXCLUDED.request_hash, locked_until = EXCLUDED.locked_until,
				    status = NULL, response = NULL, created_at = now()
				WHERE idempotency_keys.created_at < now() - make_interval(secs => $7)
				   OR (idempotency_keys.status IS NULL
				       AND idempotency_keys.request_hash = EXCLUDED.request_hash
				       AND idempotency_keys.locked_until < now())
			RETURNING locked_until`,
			c.actor, c.method, c.path, c.key, hash, lockFor.Seconds(), keptFor.Seconds()).Scan(&c.lockedUntil)
		if err == nil {
			return outcome{kind: acquired}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return outcome{}, err
		}

		var storedHash string
		var status *int
		var response []byte
		var wait float64
		err = pool.QueryRow(ctx, `SELECT request_hash, status, response,
				COALESCE(EXTRACT(EPOCH FROM locked_until - now()), 0)::float8
			FROM idempotency_keys WHERE actor_id = $1 AND method = $2 AND path = $3 AND key = $4`,
			c.actor, c.method, c.path, c.key).Scan(&storedHash, &status, &response, &wait)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // The other request released it just now, so try again.
		}
		if err != nil {
			return outcome{}, err
		}
		switch {
		case storedHash != hash:
			return outcome{kind: reused}, nil
		case status != nil:
			if string(response) == "null" {
				response = nil
			}
			return outcome{kind: replay, status: *status, body: response}, nil
		default:
			retry := int(wait) + 1
			if retry < 1 {
				retry = 1
			}
			return outcome{kind: inProgress, retryAfter: retry}, nil
		}
	}
	return outcome{kind: inProgress, retryAfter: 1}, nil
}

// hashBody fingerprints the request. Listed fields are left out, which needs JSON. A body that
// cannot be parsed is hashed as it is, because no field can be told apart.
func hashBody(body []byte, ignore []string) string {
	if len(ignore) > 0 {
		var m map[string]any
		if err := json.Unmarshal(body, &m); err == nil {
			for _, f := range ignore {
				delete(m, f)
			}
			if canonical, err := json.Marshal(m); err == nil {
				body = canonical
			}
		}
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
