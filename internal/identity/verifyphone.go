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
	// ErrPhoneAlreadyLinked means the phone's account has a different Google or Apple account.
	ErrPhoneAlreadyLinked = errors.New("identity: this phone already has a different linked account")
)

// RequestPhone sends a code to prove a phone. The code works only in this customer's sessions.
func (o *OTPs) RequestPhone(ctx context.Context, a Actor, phone string) (OTPRequest, error) {
	var exists bool
	err := o.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM customers c JOIN people p ON p.id = c.person_id
		WHERE c.id = $1 AND p.status = 'active')`, a.ID).Scan(&exists)
	if err != nil {
		return OTPRequest{}, err
	}
	// An old access token works for up to 15 minutes, so it can outlive a merge or a block.
	if !exists {
		return OTPRequest{}, ErrSessionInvalid
	}
	return o.request(ctx, a.Type, phone, purposeVerifyPhone, a.ID)
}

// VerifyPhone proves a phone, and returns a new session. If the phone already has an account, this
// account merges into it, and customer_id changes. A customer with no phone can only view the menu,
// so the account being merged away holds nothing that a merge would lose, except its name.
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
		if errors.Is(err, ErrAccountBlocked) || errors.Is(err, ErrPhoneAlreadySet) || errors.Is(err, ErrPhoneAlreadyLinked) || errors.Is(err, ErrInvalidOTP) {
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
		return Verified{}, ErrInvalidOTP // This customer was merged away after asking for the code.
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
		return o.freshSession(ctx, tx, customer, customer, phone, "phone verified again")
	}

	var owner string
	err = tx.QueryRow(ctx, "SELECT id::text FROM people WHERE phone = $1", phone).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := tx.Exec(ctx, "UPDATE people SET phone = $2, updated_at = now() WHERE id = $1", person, phone); err != nil {
			return Verified{}, err
		}
		// Linking a Google account to a phone nobody holds yet must also be told to that phone. The
		// owner may sign in later and find this account already there.
		if err := notifyLinked(ctx, tx, person); err != nil {
			return Verified{}, err
		}
		return o.freshSession(ctx, tx, customer, customer, phone, "phone verified")
	}
	if err != nil {
		return Verified{}, err
	}
	return o.merge(ctx, tx, customer, person, owner, phone)
}

func notifyLinked(ctx context.Context, tx pgx.Tx, person string) error {
	var linked bool
	err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM person_identities WHERE person_id = $1)", person).Scan(&linked)
	if err != nil || !linked {
		return err
	}
	return outbox.Add(ctx, tx, "identity.account_linked", map[string]any{"person_id": person})
}

// merge moves a phoneless person's Google or Apple account and customer onto the person who owns
// the phone, and removes the phoneless person. Every check comes before the first change, so a
// refusal never commits a partial merge.
func (o *OTPs) merge(ctx context.Context, tx pgx.Tx, customer, person, owner, phone string) (Verified, error) {
	var existing string
	err := tx.QueryRow(ctx, "SELECT id::text FROM customers WHERE person_id = $1", owner).Scan(&existing)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Verified{}, err
	}
	// The actor lock comes before the row lock, in the same order as anything ending a session and
	// then changing a status, so the two cannot deadlock.
	if existing != "" {
		if err := lockActor(ctx, tx, TypeCustomer, existing); err != nil {
			return Verified{}, err
		}
	}
	// Held to the end, so a block cannot commit part way through this merge. A block that committed
	// while this waited is seen here.
	var ownerStatus string
	if err := tx.QueryRow(ctx, "SELECT status FROM people WHERE id = $1 FOR UPDATE", owner).Scan(&ownerStatus); err != nil {
		return Verified{}, err
	}
	if ownerStatus != "active" {
		return Verified{}, ErrAccountBlocked
	}
	var conflict, otherRoles bool
	err = tx.QueryRow(ctx, `SELECT
			EXISTS (SELECT 1 FROM person_identities a JOIN person_identities b ON a.provider = b.provider
				WHERE a.person_id = $1 AND b.person_id = $2),
			EXISTS (SELECT 1 FROM riders WHERE person_id = $1)`, person, owner).Scan(&conflict, &otherRoles)
	if err != nil {
		return Verified{}, err
	}
	// Refused, not replaced: one intercepted code must not swap the Google account on someone's phone.
	if conflict || otherRoles {
		return Verified{}, ErrPhoneAlreadyLinked
	}

	// Audit rows cannot be changed later, so this must be enough to trace the merge for good.
	rows, err := tx.Query(ctx, "SELECT provider, subject FROM person_identities WHERE person_id = $1 ORDER BY 1", person)
	if err != nil {
		return Verified{}, err
	}
	moved, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (map[string]string, error) {
		var provider, subject string
		err := row.Scan(&provider, &subject)
		return map[string]string{"provider": provider, "subject": subject}, err
	})
	if err != nil {
		return Verified{}, err
	}

	if _, err := tx.Exec(ctx, "UPDATE person_identities SET person_id = $2 WHERE person_id = $1", person, owner); err != nil {
		return Verified{}, err
	}
	target := customer
	if existing == "" {
		// A rider-only owner: the customer profile moves to them.
		if _, err := tx.Exec(ctx, "UPDATE customers SET person_id = $2, updated_at = now() WHERE id = $1", customer, owner); err != nil {
			return Verified{}, err
		}
	} else {
		target = existing
		// devices has no foreign key to profiles. freshSession below ends this customer's sessions.
		if _, err := tx.Exec(ctx, "DELETE FROM devices WHERE actor_type = $1 AND actor_id = $2", TypeCustomer, customer); err != nil {
			return Verified{}, err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM customers WHERE id = $1", customer); err != nil {
			return Verified{}, err
		}
	}
	if _, err := tx.Exec(ctx, "DELETE FROM people WHERE id = $1", person); err != nil {
		return Verified{}, err
	}
	// Tells the phone a Google account was linked, because one intercepted code could otherwise link
	// an attacker's account with nobody knowing. Delivered by the worker, when it exists.
	if err := outbox.Add(ctx, tx, "identity.account_linked", map[string]any{"person_id": owner}); err != nil {
		return Verified{}, err
	}
	if err := audit.Record(ctx, tx, audit.Event{
		ActorType: TypeCustomer, ActorID: target, Action: "customer.merged", EntityType: "customer", EntityID: target,
		Before: map[string]any{"customer_id": customer, "person_id": person, "identities": moved},
		After:  map[string]any{"customer_id": target, "person_id": owner},
		Reason: "phone proven by OTP",
	}); err != nil {
		return Verified{}, err
	}
	return o.freshSession(ctx, tx, customer, target, phone, "phone verified, accounts merged")
}

// freshSession ends the old customer's sessions, and starts one for the customer who holds the phone.
func (o *OTPs) freshSession(ctx context.Context, tx pgx.Tx, oldCustomer, customer, phone, reason string) (Verified, error) {
	if err := o.Sessions.EndAll(ctx, tx, TypeCustomer, oldCustomer, ""); err != nil {
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
