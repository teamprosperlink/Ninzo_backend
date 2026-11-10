package identity

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/config"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
)

const (
	otpLife        = 5 * time.Minute
	otpResendAfter = 30 * time.Second
	otpPerHour     = 20 // Across all addresses. One address gets rulePhoneAndAddress, which is far lower.
	otpMaxAttempts = 5
	purposeSignIn  = "sign_in"
)

// phonePattern is Indian mobile numbers only for the pilot. A gateway that strips a leading zero
// could otherwise reach one person through two numbers, and foreign numbers invite SMS pumping.
var phonePattern = regexp.MustCompile(`^\+91[6-9][0-9]{9}$`)

var requestIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// The results a sign-in can be refused with. The contract names each one.
var (
	ErrBadPhone       = errors.New("identity: the phone number is not valid")
	ErrResendLimit    = errors.New("identity: too many codes requested")
	ErrInvalidOTP     = errors.New("identity: that code is wrong")
	ErrOTPExpired     = errors.New("identity: that code has expired")
	ErrOTPAttempts    = errors.New("identity: too many wrong codes")
	ErrAccountBlocked = errors.New("identity: this account is blocked")
)

// Sender delivers a code. It is called after the request is committed, never in a transaction.
type Sender interface {
	SendOTP(ctx context.Context, phone, code string) error
}

// NoSMS sends nothing, for test mode, where the code is fixed.
type NoSMS struct{}

// SendOTP does nothing.
func (NoSMS) SendOTP(context.Context, string, string) error { return nil }

// OTPs signs people in by phone.
type OTPs struct {
	Pool     *pgxpool.Pool
	Sessions *Sessions
	Sender   Sender
	// HashKey keys the code hashes: a six-digit code alone would be trivial to reverse from a leak.
	HashKey []byte
	// TestCode replaces the random code when set. config refuses it in production.
	TestCode string
	Log      *slog.Logger
	Limits   *Limits
}

// NewOTPs builds OTPs from a service's settings. Real SMS is not built yet, so config accepts only
// test mode today.
func NewOTPs(pool *pgxpool.Pool, s *Sessions, cfg config.Config, log *slog.Logger) *OTPs {
	return &OTPs{
		Pool: pool, Sessions: s, Sender: NoSMS{}, HashKey: []byte("otp:" + string(cfg.JWTSigningKey)),
		TestCode: string(cfg.OTPTestCode), Log: log,
	}
}

// OTPRequest is what the app gets back.
type OTPRequest struct {
	ID           string
	ExpiresInS   int
	ResendAfterS int
}

// RetryAfterError says how long a refused request should wait.
type RetryAfterError struct {
	Err     error
	Seconds int
}

func (e *RetryAfterError) Error() string { return e.Err.Error() }
func (e *RetryAfterError) Unwrap() error { return e.Err }

func (o *OTPs) hash(phone, code string) string {
	mac := hmac.New(sha256.New, o.HashKey)
	mac.Write([]byte(phone + "|" + code))
	return hex.EncodeToString(mac.Sum(nil))
}

func (o *OTPs) newCode() (string, error) {
	if o.TestCode != "" {
		return o.TestCode, nil
	}
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// Request makes a code for a phone. A rider number with no account gets a request that behaves the
// same in every way, but with a code nobody knows and no SMS, so the reply cannot reveal who is
// registered, and an unknown number costs nothing. Limits count those requests too.
func (o *OTPs) Request(ctx context.Context, actorType, phone string) (OTPRequest, error) {
	return o.request(ctx, actorType, phone, purposeSignIn, "")
}

// request makes a code for one purpose. requestedBy ties a phone verification code to one customer.
func (o *OTPs) request(ctx context.Context, actorType, phone, purpose, requestedBy string) (OTPRequest, error) {
	if !phonePattern.MatchString(phone) {
		return OTPRequest{}, ErrBadPhone
	}
	var id, code string
	var send bool
	err := database.InTx(ctx, o.Pool, func(tx pgx.Tx) error {
		// One at a time per phone, so a burst cannot slip past the limits.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 1))", actorType+":"+phone); err != nil {
			return err
		}
		var recent int
		var wait, hourEnds float64
		err := tx.QueryRow(ctx, `SELECT count(*),
				COALESCE(EXTRACT(EPOCH FROM max(created_at) + make_interval(secs => $3) - now()), 0)::float8,
				COALESCE(EXTRACT(EPOCH FROM min(created_at) + interval '1 hour' - now()), 0)::float8
			FROM otp_requests WHERE actor_type = $1 AND phone = $2 AND created_at > now() - interval '1 hour'`,
			actorType, phone, otpResendAfter.Seconds()).Scan(&recent, &wait, &hourEnds)
		if err != nil {
			return err
		}
		if recent >= otpPerHour {
			return &RetryAfterError{Err: ErrResendLimit, Seconds: int(hourEnds) + 1}
		}
		if wait > 0 {
			return &RetryAfterError{Err: ErrResendLimit, Seconds: int(wait) + 1}
		}
		send = true
		if purpose == purposeSignIn {
			send, err = o.canSignIn(ctx, tx, actorType, phone)
			if err != nil {
				return err
			}
		}
		if code, err = o.newCode(); err != nil {
			return err
		}
		stored := code
		if !send {
			// Nobody is told this one, so the right code cannot exist for this request.
			raw := make([]byte, 32)
			if _, err := rand.Read(raw); err != nil {
				return err
			}
			stored = hex.EncodeToString(raw)
		}
		return tx.QueryRow(ctx, `INSERT INTO otp_requests (actor_type, purpose, phone, code_hash, expires_at, requested_by)
			VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5), NULLIF($6, '')::uuid) RETURNING id::text`,
			actorType, purpose, phone, o.hash(phone, stored), otpLife.Seconds(), requestedBy).Scan(&id)
	})
	if err != nil {
		return OTPRequest{}, err
	}
	if send {
		if err := o.Sender.SendOTP(ctx, phone, code); err != nil {
			// The code never arrived, so it must not count against the limits.
			if _, delErr := o.Pool.Exec(context.WithoutCancel(ctx), "DELETE FROM otp_requests WHERE id = $1", id); delErr != nil {
				o.Log.Warn("identity: could not remove an unsent code", "error", delErr.Error())
			}
			return OTPRequest{}, fmt.Errorf("identity: sending a code: %w", err)
		}
	}
	return OTPRequest{ID: id, ExpiresInS: int(otpLife.Seconds()), ResendAfterS: int(otpResendAfter.Seconds())}, nil
}

// canSignIn decides whether a code is really sent. Customers register by signing in, so any number
// can. Riders and hub staff must already have a profile. A blocked account still gets a code: it is told only
// after proving the number, so a stranger cannot learn it is blocked.
func (o *OTPs) canSignIn(ctx context.Context, tx pgx.Tx, actorType, phone string) (bool, error) {
	if actorType == TypeCustomer {
		return true, nil
	}
	query := `SELECT EXISTS (SELECT 1 FROM riders r JOIN people p ON p.id = r.person_id WHERE p.phone = $1)`
	if actorType == TypeStaff {
		query = `SELECT EXISTS (SELECT 1 FROM hub_staff s JOIN people p ON p.id = s.person_id WHERE p.phone = $1)`
	}
	var exists bool
	err := tx.QueryRow(ctx, query, phone).Scan(&exists)
	return exists, err
}

// Verified is a successful sign-in.
type Verified struct {
	Pair   Pair
	Actor  Actor
	Status string // A rider's status, or a staff member's role. Empty for a customer.
	HubID  string // A staff member's hub.
}

// Verify checks a code and starts a session. A wrong code's attempt is committed even though the
// request is refused, so guesses always count.
func (o *OTPs) Verify(ctx context.Context, actorType, requestID, code string) (Verified, error) {
	return o.verify(ctx, actorType, requestID, code, false)
}

// VerifyAndSwitch is Verify for a customer who chose to log in by phone from now on.
func (o *OTPs) VerifyAndSwitch(ctx context.Context, requestID, code string) (Verified, error) {
	return o.verify(ctx, TypeCustomer, requestID, code, true)
}

func (o *OTPs) verify(ctx context.Context, actorType, requestID, code string, switchMethod bool) (Verified, error) {
	if !requestIDPattern.MatchString(requestID) {
		return Verified{}, ErrInvalidOTP
	}
	var out Verified
	var refusal error
	err := database.InTx(ctx, o.Pool, func(tx pgx.Tx) error {
		phone, wrong, err := o.checkCode(ctx, tx, actorType, purposeSignIn, "", requestID, code)
		if err != nil {
			return err
		}
		if wrong != nil {
			refusal = wrong
			return nil
		}
		var signInErr error
		out, signInErr = o.signIn(ctx, tx, actorType, phone, switchMethod)
		// WrongMethodError stays an error, so this rolls back and the code stays usable: the customer
		// can choose to switch without another SMS.
		if errors.Is(signInErr, ErrAccountBlocked) || errors.Is(signInErr, ErrInvalidOTP) {
			refusal = signInErr
			return nil
		}
		return signInErr
	})
	if err != nil {
		return Verified{}, err
	}
	if refusal != nil {
		return Verified{}, refusal
	}
	return out, nil
}

// checkCode checks a code and uses it up. A wrong code's attempt must commit, so the caller treats
// the refusal as a result and not an error. requestedBy must match for a phone verification code.
func (o *OTPs) checkCode(ctx context.Context, tx pgx.Tx, actorType, purpose, requestedBy, requestID, code string) (string, error, error) {
	var phone, hash string
	var attempts int
	var consumed, expired bool
	err := tx.QueryRow(ctx, `SELECT phone, code_hash, attempts, consumed_at IS NOT NULL, expires_at <= now()
		FROM otp_requests WHERE id = $1::uuid AND actor_type = $2 AND purpose = $3
			AND ($4 = '' OR requested_by = NULLIF($4, '')::uuid) FOR UPDATE`,
		requestID, actorType, purpose, requestedBy).Scan(&phone, &hash, &attempts, &consumed, &expired)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", ErrInvalidOTP, nil
	case err != nil:
		return "", nil, err
	case consumed:
		return "", ErrInvalidOTP, nil
	case expired:
		return "", ErrOTPExpired, nil
	case attempts >= otpMaxAttempts:
		return "", ErrOTPAttempts, nil
	}
	if !hmac.Equal([]byte(hash), []byte(o.hash(phone, code))) {
		if _, err := tx.Exec(ctx, "UPDATE otp_requests SET attempts = attempts + 1 WHERE id = $1::uuid", requestID); err != nil {
			return "", nil, err
		}
		return "", ErrInvalidOTP, nil
	}
	if _, err := tx.Exec(ctx, "UPDATE otp_requests SET consumed_at = now() WHERE id = $1::uuid", requestID); err != nil {
		return "", nil, err
	}
	return phone, nil, nil
}

// signIn finds or creates the account for a proven phone, and starts a session.
func (o *OTPs) signIn(ctx context.Context, tx pgx.Tx, actorType, phone string, switchMethod bool) (Verified, error) {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 2))", phone); err != nil {
		return Verified{}, err
	}
	var v Verified
	var actorID string
	switch actorType {
	case TypeCustomer:
		id, err := findOrCreateCustomer(ctx, tx, phone)
		if err != nil {
			return Verified{}, err
		}
		actorID = id
	case TypeRider:
		var active bool
		err := tx.QueryRow(ctx, `SELECT r.id::text, r.status, p.status = 'active' AND r.status <> 'suspended'
			FROM riders r JOIN people p ON p.id = r.person_id WHERE p.phone = $1`, phone).Scan(&actorID, &v.Status, &active)
		if errors.Is(err, pgx.ErrNoRows) {
			return Verified{}, ErrInvalidOTP // The rider's phone changed after the code was sent.
		}
		if err != nil {
			return Verified{}, err
		}
		if !active {
			return Verified{}, ErrAccountBlocked
		}
	case TypeStaff:
		var active bool
		err := tx.QueryRow(ctx, `SELECT s.id::text, s.role, s.hub_id::text, p.status = 'active' AND s.status = 'active'
			FROM hub_staff s JOIN people p ON p.id = s.person_id WHERE p.phone = $1`, phone).Scan(&actorID, &v.Status, &v.HubID, &active)
		if errors.Is(err, pgx.ErrNoRows) {
			return Verified{}, ErrInvalidOTP // The phone changed after the code was sent.
		}
		if err != nil {
			return Verified{}, err
		}
		if !active {
			return Verified{}, ErrAccountBlocked
		}
	default:
		return Verified{}, fmt.Errorf("identity: unknown user type %q", actorType)
	}
	// Taken before the session starts, and by EndAll, so a reset cannot miss a sign-in in progress.
	if err := lockActor(ctx, tx, actorType, actorID); err != nil {
		return Verified{}, err
	}
	// Read again now the lock is held. A phone change, a suspension or a block that finished while
	// this waited must stop this sign-in, or the old number's holder would keep access.
	if err := stillAllowed(ctx, tx, actorType, actorID, phone); err != nil {
		return Verified{}, err
	}
	if actorType == TypeCustomer {
		if err := requirePhoneMethod(ctx, tx, o.Sessions, actorID, switchMethod); err != nil {
			return Verified{}, err
		}
	}
	pair, actor, err := o.Sessions.Start(ctx, tx, actorType, actorID)
	if err != nil {
		return Verified{}, err
	}
	v.Pair, v.Actor = pair, actor
	err = audit.Record(ctx, tx, audit.Event{
		ActorType: actorType, ActorID: actorID, Action: "session.started", EntityType: "session", EntityID: actor.ChainID, Reason: "phone OTP",
	})
	return v, err
}

// findOrCreateCustomer keeps one person per phone, so a rider who starts ordering food gets a
// customer profile on the same person, with nothing copied.
func findOrCreateCustomer(ctx context.Context, tx pgx.Tx, phone string) (string, error) {
	var person, status string
	err := tx.QueryRow(ctx, "SELECT id::text, status FROM people WHERE phone = $1", phone).Scan(&person, &status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if err := tx.QueryRow(ctx, "INSERT INTO people (phone) VALUES ($1) RETURNING id::text", phone).Scan(&person); err != nil {
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
		if err := tx.QueryRow(ctx, "INSERT INTO customers (person_id) VALUES ($1) RETURNING id::text", person).Scan(&id); err != nil {
			return "", err
		}
		return id, audit.Record(ctx, tx, audit.Event{
			ActorType: TypeCustomer, ActorID: id, Action: "customer.created", EntityType: "customer", EntityID: id, Reason: "first sign-in",
		})
	}
	return id, err
}

// lockActor makes a sign-in and ending all of one actor's sessions take turns.
func lockActor(ctx context.Context, tx pgx.Tx, actorType, actorID string) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 3))", actorType+":"+actorID)
	return err
}

// stillAllowed confirms the proven phone still belongs to this actor, and that it may sign in.
func stillAllowed(ctx context.Context, tx pgx.Tx, actorType, actorID, phone string) error {
	var query string
	switch actorType {
	case TypeCustomer:
		query = `SELECT p.phone = $2, p.status = 'active' FROM customers c JOIN people p ON p.id = c.person_id WHERE c.id = $1`
	case TypeRider:
		query = `SELECT p.phone = $2, p.status = 'active' AND r.status <> 'suspended' FROM riders r JOIN people p ON p.id = r.person_id WHERE r.id = $1`
	case TypeStaff:
		query = `SELECT p.phone = $2, p.status = 'active' AND s.status = 'active' FROM hub_staff s JOIN people p ON p.id = s.person_id WHERE s.id = $1`
	default:
		return fmt.Errorf("identity: unknown user type %q", actorType)
	}
	var samePhone *bool
	var active bool
	err := tx.QueryRow(ctx, query, actorID, phone).Scan(&samePhone, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidOTP
	}
	if err != nil {
		return err
	}
	if samePhone == nil || !*samePhone {
		return ErrInvalidOTP
	}
	if !active {
		return ErrAccountBlocked
	}
	return nil
}

// RequireSMS stops a sign-in service starting with no way to send a code. No SMS provider is built
// yet, so only test mode can run. Production refuses test mode, so production cannot start until
// real SMS exists. That is deliberate.
func RequireSMS(cfg config.Config) error {
	if !cfg.OTPTestMode {
		return errors.New("OTP_TEST_MODE must be true until an SMS provider is built")
	}
	return nil
}
