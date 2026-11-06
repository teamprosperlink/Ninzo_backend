package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/outbox"
)

const purposeVerifyPhone = "verify_phone"

var (
	// ErrPhoneAlreadySet means this account has a different verified phone. Changing a phone is a
	// separate flow, because it moves every role.
	ErrPhoneAlreadySet = errors.New("identity: this account already has a different verified phone")
	// ErrPhoneAlreadyRegistered means another account holds this number. Accounts never merge: that
	// person signs in the way they signed up.
	ErrPhoneAlreadyRegistered = errors.New("identity: this number already has an account")
)

// RequestPhone sends a code to prove a phone. The code works only in this customer's sessions.
func (o *OTPs) RequestPhone(ctx context.Context, a Actor, phone string) (OTPRequest, error) {
	var exists bool
	err := o.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM customers c JOIN people p ON p.id = c.person_id
		WHERE c.id = $1 AND p.status = 'active')`, a.ID).Scan(&exists)
	if err != nil {
		return OTPRequest{}, err
	}
	live, err := sessionLive(ctx, o.Pool, a)
	if err != nil {
		return OTPRequest{}, err
	}
	// An old access token works for up to 15 minutes, so it can outlive its session, a block or a
	// removal. Each code costs money.
	if !exists || !live {
		return OTPRequest{}, ErrSessionInvalid
	}
	return o.request(ctx, a.Type, phone, purposeVerifyPhone, a.ID)
}

// VerifyPhone proves a phone for delivery, and returns a new session. It does not change how the
// customer signs in, and it never joins two accounts.
func (o *OTPs) VerifyPhone(ctx context.Context, a Actor, requestID, code string) (Verified, error) {
	if !requestIDPattern.MatchString(requestID) {
		return Verified{}, ErrInvalidOTP
	}
	var out Verified
	var refusal error
	err := database.InTx(ctx, o.Pool, func(tx pgx.Tx) error {
		phone, wrong, err := o.checkCode(ctx, tx, a.Type, purposeVerifyPhone, a.ID, requestID, code)
		if err != nil {
			return err
		}
		if wrong != nil {
			refusal = wrong
			return nil
		}
		v, err := o.linkPhone(ctx, tx, a.ID, phone)
		if errors.Is(err, ErrAccountBlocked) || errors.Is(err, ErrPhoneAlreadySet) || errors.Is(err, ErrPhoneAlreadyRegistered) || errors.Is(err, ErrInvalidOTP) {
			refusal = err
			return nil
		}
		out = v
		return err
	})
	if err != nil {
		return Verified{}, err
	}
	if refusal != nil {
		return Verified{}, refusal
	}
	return out, nil
}

func (o *OTPs) linkPhone(ctx context.Context, tx pgx.Tx, customer, phone string) (Verified, error) {
	// The same order as a phone sign-in: phone, then actor.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 2))", phone); err != nil {
		return Verified{}, err
	}
	if err := lockActor(ctx, tx, TypeCustomer, customer); err != nil {
		return Verified{}, err
	}
	var person, status string
	var current *string
	err := tx.QueryRow(ctx, `SELECT p.id::text, p.status, p.phone FROM customers c JOIN people p ON p.id = c.person_id WHERE c.id = $1`, customer).
		Scan(&person, &status, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return Verified{}, ErrInvalidOTP // This customer was removed after asking for the code.
	}
	if err != nil {
		return Verified{}, err
	}
	if status != "active" {
		return Verified{}, ErrAccountBlocked
	}
	if current != nil {
		if *current != phone {
			return Verified{}, ErrPhoneAlreadySet
		}
		return o.freshSession(ctx, tx, customer, phone, "phone verified again")
	}

	var ownerStatus string
	err = tx.QueryRow(ctx, "SELECT status FROM people WHERE phone = $1", phone).Scan(&ownerStatus)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return Verified{}, err
	case ownerStatus != "active":
		return Verified{}, ErrAccountBlocked
	default:
		return Verified{}, ErrPhoneAlreadyRegistered
	}
	if _, err := tx.Exec(ctx, "UPDATE people SET phone = $2, updated_at = now() WHERE id = $1", person, phone); err != nil {
		return Verified{}, err
	}
	// A Google account on a phone nobody holds yet must be told to that phone. Its real owner may
	// not be the one who just proved it.
	if err := notifyLinked(ctx, tx, person); err != nil {
		return Verified{}, err
	}
	return o.freshSession(ctx, tx, customer, phone, "phone verified")
}

func notifyLinked(ctx context.Context, tx pgx.Tx, person string) error {
	var linked bool
	err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM person_identities WHERE person_id = $1)", person).Scan(&linked)
	if err != nil || !linked {
		return err
	}
	return outbox.Add(ctx, tx, "identity.account_linked", map[string]any{"person_id": person})
}

// freshSession ends the customer's sessions, and starts a new one.
func (o *OTPs) freshSession(ctx context.Context, tx pgx.Tx, customer, phone, reason string) (Verified, error) {
	if err := o.Sessions.EndAll(ctx, tx, TypeCustomer, customer, ""); err != nil {
		return Verified{}, err
	}
	// Changes are already made by now, so this must roll them back, and must not read as a refusal
	// that commits them. The app retries, and gets the true answer.
	if err := stillAllowed(ctx, tx, TypeCustomer, customer, phone); err != nil {
		return Verified{}, fmt.Errorf("identity: the account changed during verification, so try again: %v", err)
	}
	pair, actor, err := o.Sessions.Start(ctx, tx, TypeCustomer, customer)
	if err != nil {
		return Verified{}, err
	}
	err = audit.Record(ctx, tx, audit.Event{
		ActorType: TypeCustomer, ActorID: customer, Action: "session.started", EntityType: "session", EntityID: actor.ChainID, Reason: reason,
	})
	return Verified{Pair: pair, Actor: actor}, err
}
