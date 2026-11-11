// Package console serves the ops console (decision 0015): two-step sign-in, console users and
// roles, permissions on every call, and changes that wait for a second person's approval. Its
// contract is contracts/console/openapi.yaml.
package console

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

// Sign-in limits (decision 0015).
const (
	challengeLife    = 5 * time.Minute
	challengeTries   = 5
	lockAfter        = 5
	lockWindow       = 15 * time.Minute
	lockFor          = 15 * time.Minute
	minPasswordChars = 12
)

var idPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Service serves the console API.
type Service struct {
	Pool     *pgxpool.Pool
	Log      *slog.Logger
	Sessions *identity.Sessions
	Sealer   Sealer
	Issuer   string // Shown in authenticator apps. "Ninzo" by default.
	Limits   *identity.Limits
	// TOTPOptional: a user with no authenticator signs in with the password alone. Off in
	// production (config refuses it); on for staging until pre-launch.
	TOTPOptional bool
	now          func() time.Time
}

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func serverTime() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func stamp(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func (s *Service) unavailable(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.Error("console: request failed", "request_id", httpserver.RequestID(r.Context()), "error", err.Error())
	httpserver.WriteError(w, http.StatusInternalServerError, "internal_error", "Try again shortly.", true)
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// MountAuth adds consoleLogin, enrollTotp, verifyTotp, refreshToken, signOut and changePassword.
func (s *Service) MountAuth(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	identity.MountSessions(r, s.Sessions, identity.TypeConsole)
	r.Group(func(g chi.Router) {
		if s.Limits != nil {
			g.Use(s.Limits.ConsoleSignIn())
		}
		g.Post("/auth/login", s.login)
		g.Post("/auth/totp/enroll", s.enroll)
		g.Post("/auth/totp/verify", s.verify)
	})
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.With(idem).Post("/me/password", s.changePassword)
	})
}

func (s *Service) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LoginID  string `json:"login_id"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.LoginID == "" || body.Password == "" || len(body.LoginID) > 254 || len(body.Password) > 200 {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send login_id and password.", false)
		return
	}
	now := s.clock()
	login := strings.ToLower(strings.TrimSpace(body.LoginID))
	// The slow password check runs before any lock, so a flood on one ID cannot hold its row.
	var id, hash string
	err := s.Pool.QueryRow(r.Context(), "SELECT id::text, password_hash FROM console_users WHERE login_id = $1", login).Scan(&id, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		CheckPassword(dummyHash, body.Password) // The same work as a real check.
		httpserver.WriteError(w, http.StatusUnauthorized, "invalid_credentials", "That email or password is wrong.", false)
		return
	}
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	right := CheckPassword(hash, body.Password)
	var token, role string
	var enrolled, mustChange, refused bool
	var pair identity.Pair
	err = database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		var current string
		var active bool
		var failed int
		var since, locked, enrolledAt *time.Time
		if err := tx.QueryRow(r.Context(), `SELECT password_hash, active, failed_logins, failed_since, locked_until, totp_enrolled_at, must_change_password
			FROM console_users WHERE id = $1 FOR UPDATE`, id).Scan(&current, &active, &failed, &since, &locked, &enrolledAt, &mustChange); err != nil {
			return err
		}
		lockedNow := locked != nil && now.Before(*locked)
		if !right || current != hash || !active || lockedNow {
			refused = true
			if lockedNow || !active {
				return nil // Nothing more to count; the answer is the same as a wrong password.
			}
			return countFailure(r.Context(), tx, id, failed, since, now)
		}
		enrolled = enrolledAt != nil
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return err
		}
		token = hex.EncodeToString(buf)
		if _, err := tx.Exec(r.Context(), "UPDATE console_users SET failed_logins = 0, failed_since = NULL, locked_until = NULL WHERE id = $1", id); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), "INSERT INTO console_challenges (token_hash, user_id, expires_at) VALUES ($1, $2, $3)", hashToken(token), id, now.Add(challengeLife)); err != nil {
			return err
		}
		if !s.TOTPOptional || enrolled {
			return nil // The code is still needed: verifyTotp starts the session.
		}
		// No authenticator yet, and it is not required: the password alone starts the session.
		if err := tx.QueryRow(r.Context(), "SELECT role FROM console_users WHERE id = $1", id).Scan(&role); err != nil {
			return err
		}
		var actor identity.Actor
		var err error
		if pair, actor, err = s.Sessions.Start(r.Context(), tx, identity.TypeConsole, id); err != nil {
			return err
		}
		return audit.Record(r.Context(), tx, audit.Event{ActorType: identity.TypeConsole, ActorID: id, Action: "session.started", EntityType: "session", EntityID: actor.ChainID,
			Reason: "password only: the authenticator is not required yet"})
	})
	switch {
	case err != nil:
		s.unavailable(w, r, err)
	case refused:
		httpserver.WriteError(w, http.StatusUnauthorized, "invalid_credentials", "That email or password is wrong, or the account is locked for a while.", false)
	default:
		reply := map[string]any{"challenge_id": token, "totp_enrolled": enrolled, "totp_required": pair.AccessToken == "", "must_change_password": mustChange,
			"expires_in_s": int(challengeLife.Seconds()), "session": nil, "server_time": serverTime()}
		if pair.AccessToken != "" {
			reply["session"] = map[string]any{"access_token": pair.AccessToken, "refresh_token": pair.RefreshToken, "expires_in_s": pair.ExpiresInS,
				"user_id": id, "role": role, "must_change_password": mustChange, "server_time": serverTime()}
		}
		httpserver.WriteJSON(w, http.StatusOK, reply)
	}
}

// countFailure counts a wrong password or code against the account: 5 in 15 minutes lock it for 15.
// The caller holds the user's row lock.
func countFailure(ctx context.Context, tx pgx.Tx, id string, failed int, since *time.Time, now time.Time) error {
	if since == nil || now.Sub(*since) > lockWindow {
		failed, since = 0, &now
	}
	failed++
	var until any
	if failed >= lockAfter {
		until = now.Add(lockFor)
	}
	if _, err := tx.Exec(ctx, "UPDATE console_users SET failed_logins = $2, failed_since = $3, locked_until = coalesce($4, locked_until) WHERE id = $1",
		id, failed, since, until); err != nil {
		return err
	}
	if until == nil {
		return nil
	}
	return audit.Record(ctx, tx, audit.Event{ActorType: audit.ActorSystem, Action: "console.locked", EntityType: "console_user", EntityID: id, Reason: "failed sign-ins"})
}

var errChallenge = errors.New("console: challenge not valid")

// challenge reads a live challenge under its lock.
func challenge(ctx context.Context, tx pgx.Tx, token string, now time.Time, counted bool) (string, string, int, error) {
	var id, user string
	var attempts int
	var expires time.Time
	var used *time.Time
	err := tx.QueryRow(ctx, "SELECT id::text, user_id::text, attempts, expires_at, used_at FROM console_challenges WHERE token_hash = $1 FOR UPDATE", hashToken(token)).
		Scan(&id, &user, &attempts, &expires, &used)
	limit := challengeTries
	if counted {
		limit++ // This call's own attempt is already in the count.
	}
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (used != nil || !now.Before(expires) || attempts >= limit)) {
		return "", "", 0, errChallenge
	}
	return id, user, attempts, err
}

func (s *Service) enroll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChallengeID string `json:"challenge_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ChallengeID == "" || len(body.ChallengeID) > 200 {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send challenge_id.", false)
		return
	}
	var secret []byte
	var login string
	already := false
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		_, user, _, err := challenge(r.Context(), tx, body.ChallengeID, s.clock(), false)
		if err != nil {
			return err
		}
		var enrolled *time.Time
		if err := tx.QueryRow(r.Context(), "SELECT login_id, totp_enrolled_at FROM console_users WHERE id = $1 FOR UPDATE", user).Scan(&login, &enrolled); err != nil {
			return err
		}
		if enrolled != nil {
			already = true
			return nil
		}
		// A new secret each time until the first code is verified with it.
		if secret, err = NewTOTPSecret(); err != nil {
			return err
		}
		sealed, err := s.Sealer.Seal(secret, user)
		if err != nil {
			return err
		}
		_, err = tx.Exec(r.Context(), "UPDATE console_users SET totp_secret = $2, totp_last_step = 0 WHERE id = $1", user, sealed)
		return err
	})
	switch {
	case errors.Is(err, errChallenge):
		httpserver.WriteError(w, http.StatusConflict, "challenge_expired", "Sign in again.", false)
	case err != nil:
		s.unavailable(w, r, err)
	case already:
		httpserver.WriteError(w, http.StatusConflict, "totp_already_enrolled", "The authenticator is already set up.", false)
	default:
		issuer := s.Issuer
		if issuer == "" {
			issuer = "Ninzo"
		}
		code := EncodeSecret(secret)
		label := url.PathEscape(issuer) + ":" + url.PathEscape(login)
		httpserver.WriteJSON(w, http.StatusOK, map[string]any{"secret": code,
			"otpauth_uri": "otpauth://totp/" + label + "?secret=" + code + "&issuer=" + url.QueryEscape(issuer) + "&digits=6&period=30", "server_time": serverTime()})
	}
}

var errTOTP = errors.New("console: wrong code")

func (s *Service) verify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChallengeID string `json:"challenge_id"`
		Code        string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ChallengeID == "" || len(body.ChallengeID) > 200 || len(body.Code) != 6 {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send challenge_id and the 6-digit code.", false)
		return
	}
	now := s.clock()
	// Take one attempt first, in its own statement, so parallel guesses can never pass the limit.
	var attempts int
	err := s.Pool.QueryRow(r.Context(), `UPDATE console_challenges SET attempts = attempts + 1
		WHERE token_hash = $1 AND used_at IS NULL AND expires_at > $2 AND attempts < $3 RETURNING attempts`, hashToken(body.ChallengeID), now, challengeTries).Scan(&attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		httpserver.WriteError(w, http.StatusConflict, "challenge_expired", "Sign in again.", false)
		return
	}
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	var pair identity.Pair
	var user, role string
	var mustChange, wrong bool
	err = database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		id, u, _, err := challenge(r.Context(), tx, body.ChallengeID, now, true)
		if err != nil {
			return err
		}
		user = u
		var sealed []byte
		var last int64
		var active bool
		var failed int
		var since, locked *time.Time
		if err := tx.QueryRow(r.Context(), "SELECT role, totp_secret, totp_last_step, must_change_password, active, failed_logins, failed_since, locked_until FROM console_users WHERE id = $1 FOR UPDATE", user).
			Scan(&role, &sealed, &last, &mustChange, &active, &failed, &since, &locked); err != nil {
			return err
		}
		if !active || sealed == nil || (locked != nil && now.Before(*locked)) {
			return errChallenge
		}
		secret, err := s.Sealer.Open(sealed, user)
		if err != nil {
			return err
		}
		step, ok := MatchTOTP(secret, body.Code, now, last)
		if !ok {
			wrong = true
			return countFailure(r.Context(), tx, user, failed, since, now)
		}
		if _, err := tx.Exec(r.Context(), "UPDATE console_challenges SET used_at = $2 WHERE id = $1", id, now); err != nil {
			return err
		}
		var first bool
		if err := tx.QueryRow(r.Context(), `UPDATE console_users SET totp_last_step = $2, totp_enrolled_at = coalesce(totp_enrolled_at, $3), failed_logins = 0, failed_since = NULL
			WHERE id = $1 RETURNING totp_enrolled_at = $3`, user, step, now).Scan(&first); err != nil {
			return err
		}
		if first {
			if err := audit.Record(r.Context(), tx, audit.Event{ActorType: identity.TypeConsole, ActorID: user, Action: "console.totp_enrolled", EntityType: "console_user", EntityID: user}); err != nil {
				return err
			}
		}
		var actor identity.Actor
		if pair, actor, err = s.Sessions.Start(r.Context(), tx, identity.TypeConsole, user); err != nil {
			return err
		}
		return audit.Record(r.Context(), tx, audit.Event{ActorType: identity.TypeConsole, ActorID: user, Action: "session.started", EntityType: "session", EntityID: actor.ChainID,
			Reason: "password and authenticator"})
	})
	switch {
	case errors.Is(err, errChallenge):
		httpserver.WriteError(w, http.StatusConflict, "challenge_expired", "Sign in again.", false)
	case err != nil:
		s.unavailable(w, r, err)
	case wrong && attempts >= challengeTries:
		httpserver.WriteError(w, http.StatusTooManyRequests, "totp_attempts_exceeded", "Too many wrong codes. Sign in again.", false)
	case wrong:
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_totp", "That code is wrong.", false)
	default:
		httpserver.WriteJSON(w, http.StatusOK, map[string]any{"access_token": pair.AccessToken, "refresh_token": pair.RefreshToken, "expires_in_s": pair.ExpiresInS,
			"user_id": user, "role": role, "must_change_password": mustChange, "server_time": serverTime()})
	}
}

func (s *Service) changePassword(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Current) > 200 || len(body.New) > 200 {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send current_password and new_password.", false)
		return
	}
	if len([]rune(body.New)) < minPasswordChars || body.New == body.Current {
		httpserver.WriteError(w, http.StatusBadRequest, "weak_password", "Use at least 12 characters, different from the current password.", false)
		return
	}
	hash, err := HashPassword(body.New)
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	wrong := false
	err = database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		if err := identity.CheckSession(r.Context(), tx, actor); err != nil {
			return err
		}
		var current string
		if err := tx.QueryRow(r.Context(), "SELECT password_hash FROM console_users WHERE id = $1 AND active FOR UPDATE", actor.ID).Scan(&current); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return identity.ErrSessionInvalid
			}
			return err
		}
		if !CheckPassword(current, body.Current) {
			wrong = true
			return nil
		}
		if _, err := tx.Exec(r.Context(), "UPDATE console_users SET password_hash = $2, must_change_password = false, updated_at = now() WHERE id = $1", actor.ID, hash); err != nil {
			return err
		}
		// Every other session ends, through the session lock so a refresh in flight cannot survive;
		// this one stays. Open sign-in challenges end too.
		if err := s.Sessions.EndAll(r.Context(), tx, identity.TypeConsole, actor.ID, actor.ChainID); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), "UPDATE console_challenges SET used_at = now() WHERE user_id = $1 AND used_at IS NULL", actor.ID); err != nil {
			return err
		}
		if err := audit.Record(r.Context(), tx, audit.Event{ActorType: identity.TypeConsole, ActorID: actor.ID, Action: "console.password_changed", EntityType: "console_user", EntityID: actor.ID}); err != nil {
			return err
		}
		return idempotency.Save(r.Context(), tx, http.StatusOK, map[string]any{"server_time": serverTime()})
	})
	switch {
	case errors.Is(err, identity.ErrSessionInvalid):
		httpserver.WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in again.", false)
	case err != nil:
		s.unavailable(w, r, err)
	case wrong:
		httpserver.WriteError(w, http.StatusUnauthorized, "invalid_credentials", "The current password is wrong.", false)
	default:
		httpserver.WriteJSON(w, http.StatusOK, map[string]any{"server_time": serverTime()})
	}
}
