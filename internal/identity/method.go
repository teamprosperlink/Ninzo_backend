package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/config"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/outbox"
)

// This file holds the rule that a customer has one sign-in method at a time, and how it changes.
// To change that rule later, this file and the checks that call it are the only places to look.
// See docs/decisions/0009-one-sign-in-method.md.

const (
	MethodPhone  = "phone"
	MethodGoogle = providerGoogle
)

var (
	// ErrSameMethod means the account already uses the method asked for.
	ErrSameMethod = errors.New("identity: this account already uses that sign-in method")
	// ErrGoogleInUse means this Google account belongs to another customer who holds real data.
	ErrGoogleInUse = errors.New("identity: this Google account is used by another account")
	// ErrPhoneRequired means the account needs a verified phone first.
	ErrPhoneRequired = errors.New("identity: verify a phone first")
)

// WrongMethodError means the code was right, but this account signs in another way.
type WrongMethodError struct{ Method string }

func (e *WrongMethodError) Error() string {
	return "identity: this account signs in with " + e.Method
}

// Method is one way to sign in, as a component. To attach a new one, such as Apple, write a type
// like GoogleSignIn and pass it to MountMethods. To detach one, take it out of SIGNIN_METHODS.
type Method interface {
	Name() string
	Mount(chi.Router)
}

// MountMethods adds only the methods this deployment has switched on.
func MountMethods(r chi.Router, enabled []string, methods ...Method) {
	for _, m := range methods {
		if slices.Contains(enabled, m.Name()) {
			m.Mount(r)
		}
	}
}

// PhoneMethod is phone and OTP sign-in.
type PhoneMethod struct {
	OTPs      *OTPs
	ActorType string
}

// Name implements Method.
func (PhoneMethod) Name() string { return MethodPhone }

// Mount implements Method.
func (p PhoneMethod) Mount(r chi.Router) { MountOTP(r, p.OTPs, p.ActorType) }

// Name implements Method.
func (*GoogleSignIn) Name() string { return MethodGoogle }

// Mount implements Method.
func (g *GoogleSignIn) Mount(r chi.Router) { MountGoogle(r, g) }

// RequireMethods stops the customer API starting with a method on and no way to check it.
func RequireMethods(cfg config.Config) error {
	if slices.Contains(cfg.SignInMethods, MethodGoogle) {
		return RequireGoogle(cfg)
	}
	return nil
}

// requirePhoneMethod applies the rule at a phone sign-in. A customer who chose to switch moves to
// phone here, and every other session ends.
func requirePhoneMethod(ctx context.Context, tx pgx.Tx, s *Sessions, customer string, switchMethod bool) error {
	var method string
	if err := tx.QueryRow(ctx, "SELECT sign_in_method FROM customers WHERE id = $1", customer).Scan(&method); err != nil {
		return err
	}
	if method == MethodPhone {
		return nil
	}
	if !switchMethod {
		return &WrongMethodError{Method: method}
	}
	return switchToPhone(ctx, tx, s, customer, method, "switched at sign-in, with the verified phone")
}

// sessionLive says whether this access token's session still exists. An access token outlives its
// session by up to 15 minutes, which must not be enough to undo a change that ended it. Called with
// the actor lock held, so it takes turns with EndAll.
func sessionLive(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, a Actor) (bool, error) {
	var live bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM refresh_tokens WHERE chain_id = $1 AND actor_id = $2
		AND actor_type = $3 AND revoked_at IS NULL AND expires_at > now())`, a.ChainID, a.ID, a.Type).Scan(&live)
	return live, err
}

// SessionLive says whether this access token's session still exists, for reads that take no lock.
func SessionLive(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, a Actor) (bool, error) {
	return sessionLive(ctx, q, a)
}

// CheckSession refuses an access token whose session has ended. It takes the actor lock, so a change
// that ends the session, and removes its devices, cannot miss what this goes on to add.
func CheckSession(ctx context.Context, tx pgx.Tx, a Actor) error {
	if err := lockActor(ctx, tx, a.Type, a.ID); err != nil {
		return err
	}
	live, err := sessionLive(ctx, tx, a)
	if err != nil {
		return err
	}
	if !live {
		return ErrSessionInvalid
	}
	return nil
}

// switchToPhone removes the linked accounts, so they can no longer sign in here.
func switchToPhone(ctx context.Context, tx pgx.Tx, s *Sessions, customer, from, reason string) error {
	var person string
	if err := tx.QueryRow(ctx, "SELECT person_id::text FROM customers WHERE id = $1", customer).Scan(&person); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, "SELECT provider, subject FROM person_identities WHERE person_id = $1 ORDER BY 1", person)
	if err != nil {
		return err
	}
	removed, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (map[string]string, error) {
		var provider, subject string
		err := row.Scan(&provider, &subject)
		return map[string]string{"provider": provider, "subject": subject}, err
	})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM person_identities WHERE person_id = $1", person); err != nil {
		return err
	}
	return recordMethodChange(ctx, tx, s, customer, person, MethodPhone, reason,
		map[string]any{"sign_in_method": from, "identities": removed})
}

// recordMethodChange stores the change, ends every session and tells the phone. Audit rows cannot
// be changed later, so this must be enough to trace the change for good.
func recordMethodChange(ctx context.Context, tx pgx.Tx, s *Sessions, customer, person, to, reason string, before map[string]any) error {
	if _, err := tx.Exec(ctx, "UPDATE customers SET sign_in_method = $2, updated_at = now() WHERE id = $1", customer, to); err != nil {
		return err
	}
	if err := s.EndAll(ctx, tx, TypeCustomer, customer, ""); err != nil {
		return err
	}
	// Otherwise whoever was removed would keep getting this account's notifications. The app
	// registers its device again with the new session.
	if _, err := tx.Exec(ctx, "DELETE FROM devices WHERE actor_type = 'customer' AND actor_id = $1", customer); err != nil {
		return err
	}
	// One intercepted code could change this, so the phone must be told. Delivered by the worker,
	// when it exists.
	if err := outbox.Add(ctx, tx, "identity.sign_in_method_changed", map[string]any{"person_id": person}); err != nil {
		return err
	}
	return audit.Record(ctx, tx, audit.Event{
		ActorType: TypeCustomer, ActorID: customer, Action: "customer.sign_in_method_changed", EntityType: "customer", EntityID: customer,
		Before: before, After: map[string]any{"sign_in_method": to}, Reason: reason,
	})
}

// ChangeToPhone is the settings change from Google to phone, with a code from RequestPhone.
func (o *OTPs) ChangeToPhone(ctx context.Context, a Actor, requestID, code string) (Verified, error) {
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
			refusal = wrong // Must commit, so a wrong guess always counts.
			return nil
		}
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 2))", phone); err != nil {
			return err
		}
		if err := lockActor(ctx, tx, TypeCustomer, a.ID); err != nil {
			return err
		}
		live, err := sessionLive(ctx, tx, a)
		if err != nil {
			return err
		}
		if !live {
			refusal = ErrSessionInvalid
			return nil
		}
		var status, method string
		var current *string
		err = tx.QueryRow(ctx, `SELECT p.status, p.phone, c.sign_in_method FROM customers c JOIN people p ON p.id = c.person_id
			WHERE c.id = $1`, a.ID).Scan(&status, &current, &method)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			refusal = ErrSessionInvalid
			return nil
		case err != nil:
			return err
		case status != "active":
			refusal = ErrAccountBlocked
		case current == nil:
			refusal = ErrPhoneRequired
		case *current != phone:
			refusal = ErrPhoneAlreadySet
		case method == MethodPhone:
			refusal = ErrSameMethod
		}
		if refusal != nil {
			return nil
		}
		if err := switchToPhone(ctx, tx, o.Sessions, a.ID, method, "changed in settings"); err != nil {
			return err
		}
		pair, actor, err := o.Sessions.Start(ctx, tx, TypeCustomer, a.ID)
		if err != nil {
			return err
		}
		out = Verified{Pair: pair, Actor: actor}
		return audit.Record(ctx, tx, audit.Event{
			ActorType: TypeCustomer, ActorID: a.ID, Action: "session.started", EntityType: "session", EntityID: actor.ChainID, Reason: "sign-in method changed",
		})
	})
	if err != nil {
		return Verified{}, err
	}
	if refusal != nil {
		return Verified{}, refusal
	}
	return out, nil
}

// ChangeToGoogle is the settings change from phone to Google. If this Google account was only used
// by mistake, leaving an empty account with no phone, that empty account is removed.
func (g *GoogleSignIn) ChangeToGoogle(ctx context.Context, a Actor, idToken string) (Verified, error) {
	profile, err := g.Verifier.Verify(ctx, idToken)
	if err != nil {
		return Verified{}, err
	}
	var out Verified
	err = database.InTx(ctx, g.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 4))", providerGoogle+":"+profile.Subject); err != nil {
			return err
		}
		if err := lockActor(ctx, tx, TypeCustomer, a.ID); err != nil {
			return err
		}
		live, err := sessionLive(ctx, tx, a)
		if err != nil {
			return err
		}
		if !live {
			return ErrSessionInvalid
		}
		var person, status, method string
		err = tx.QueryRow(ctx, `SELECT p.id::text, p.status, c.sign_in_method FROM customers c JOIN people p ON p.id = c.person_id
			WHERE c.id = $1`, a.ID).Scan(&person, &status, &method)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrSessionInvalid
		case err != nil:
			return err
		case status != "active":
			return ErrAccountBlocked
		case method == MethodGoogle:
			return ErrSameMethod
		}
		if err := g.freeGoogleAccount(ctx, tx, profile.Subject, person); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO person_identities (person_id, provider, subject, email) VALUES ($1, $2, $3, NULLIF($4, ''))",
			person, providerGoogle, profile.Subject, profile.Email); err != nil {
			return err
		}
		if err := recordMethodChange(ctx, tx, g.Sessions, a.ID, person, MethodGoogle, "changed in settings", map[string]any{
			"sign_in_method": method, "new_identity": map[string]string{"provider": providerGoogle, "subject": profile.Subject},
		}); err != nil {
			return err
		}
		pair, actor, err := g.Sessions.Start(ctx, tx, TypeCustomer, a.ID)
		if err != nil {
			return err
		}
		out = Verified{Pair: pair, Actor: actor}
		return audit.Record(ctx, tx, audit.Event{
			ActorType: TypeCustomer, ActorID: a.ID, Action: "session.started", EntityType: "session", EntityID: actor.ChainID, Reason: "sign-in method changed",
		})
	})
	if err != nil {
		return Verified{}, err
	}
	return out, nil
}

// freeGoogleAccount removes an empty account holding this Google account. With no phone, it cannot
// have orders, gates or tickets. Anything else is refused.
func (g *GoogleSignIn) freeGoogleAccount(ctx context.Context, tx pgx.Tx, subject, person string) error {
	var customer string
	err := tx.QueryRow(ctx, `SELECT c.id::text FROM person_identities i JOIN customers c ON c.person_id = i.person_id
		WHERE i.provider = $1 AND i.subject = $2`, providerGoogle, subject).Scan(&customer)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := lockActor(ctx, tx, TypeCustomer, customer); err != nil {
		return err
	}
	// Read only now the lock is held, because that account may have just verified a phone. The row
	// lock makes a block or a new rider profile take turns with this removal.
	var owner, status string
	var empty bool
	err = tx.QueryRow(ctx, `SELECT p.id::text, p.status,
			p.phone IS NULL AND NOT EXISTS (SELECT 1 FROM riders r WHERE r.person_id = p.id)
		FROM person_identities i JOIN people p ON p.id = i.person_id
		WHERE i.provider = $1 AND i.subject = $2 FOR UPDATE OF p`, providerGoogle, subject).Scan(&owner, &status, &empty)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // Switched to phone while this waited, so the Google account is free.
	}
	if err != nil {
		return err
	}
	// A blocked account must stay, or removing it would undo the block.
	if owner == person || !empty || status != "active" {
		return ErrGoogleInUse
	}
	if err := g.Sessions.EndAll(ctx, tx, TypeCustomer, customer, ""); err != nil {
		return err
	}
	for _, step := range []struct {
		sql string
		arg string
	}{
		{"DELETE FROM devices WHERE actor_type = 'customer' AND actor_id = $1", customer},
		{"DELETE FROM customers WHERE id = $1", customer},
		{"DELETE FROM person_identities WHERE person_id = $1", owner},
		{"DELETE FROM people WHERE id = $1", owner},
	} {
		if _, err := tx.Exec(ctx, step.sql, step.arg); err != nil {
			return fmt.Errorf("identity: removing an empty account: %w", err)
		}
	}
	return audit.Record(ctx, tx, audit.Event{
		ActorType: audit.ActorSystem, Action: "customer.removed", EntityType: "customer", EntityID: customer,
		Before: map[string]any{"person_id": owner, "provider": providerGoogle, "subject": subject},
		Reason: "an empty account, whose Google account moved to another customer",
	})
}

// MountMethodChange adds changeSignInMethod. Only the customer API has it.
func MountMethodChange(r chi.Router, enabled []string, o *OTPs, g *GoogleSignIn, signer Signer) {
	r.Group(func(signedIn chi.Router) {
		signedIn.Use(RequireActor(signer, TypeCustomer), o.Limits.BySession())
		signedIn.Post("/me/sign-in-method", func(w http.ResponseWriter, req *http.Request) {
			actor, _ := ActorFrom(req.Context())
			var body struct {
				Method    string `json:"method"`
				IDToken   string `json:"id_token"`
				RequestID string `json:"request_id"`
				OTP       string `json:"otp"`
			}
			err := json.NewDecoder(req.Body).Decode(&body)
			if err != nil || !slices.Contains(enabled, body.Method) ||
				(body.Method == MethodGoogle && body.IDToken == "") ||
				(body.Method == MethodPhone && (body.RequestID == "" || !otpFormat.MatchString(body.OTP))) {
				httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send a sign-in method that is on, with its proof.", false)
				return
			}
			var v Verified
			if body.Method == MethodGoogle {
				v, err = g.ChangeToGoogle(req.Context(), actor, body.IDToken)
			} else {
				v, err = o.ChangeToPhone(req.Context(), actor, body.RequestID, body.OTP)
			}
			switch {
			case errors.Is(err, ErrGoogleToken):
				httpserver.WriteError(w, http.StatusUnauthorized, "invalid_google_token", "Google could not confirm this sign-in.", false)
			case errors.Is(err, ErrSessionInvalid):
				httpserver.WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in again.", false)
			case errors.Is(err, ErrInvalidOTP):
				httpserver.WriteError(w, http.StatusBadRequest, "invalid_otp", "That code is wrong.", false)
			case errors.Is(err, ErrOTPExpired):
				httpserver.WriteError(w, http.StatusBadRequest, "otp_expired", "That code has expired.", false)
			case errors.Is(err, ErrOTPAttempts):
				w.Header().Set("Retry-After", strconv.Itoa(int(otpResendAfter.Seconds())))
				httpserver.WriteError(w, http.StatusTooManyRequests, "otp_attempts_exceeded", "Too many wrong codes. Ask for a new one.", false)
			case errors.Is(err, ErrAccountBlocked):
				WriteBlocked(w)
			case errors.Is(err, ErrSameMethod):
				httpserver.WriteError(w, http.StatusConflict, "sign_in_method_unchanged", "This account already uses that sign-in method.", false)
			case errors.Is(err, ErrGoogleInUse):
				httpserver.WriteError(w, http.StatusConflict, "google_account_in_use", "This Google account is used by another account.", false)
			case errors.Is(err, ErrPhoneRequired):
				httpserver.WriteError(w, http.StatusConflict, "phone_required", "Verify a phone first.", false)
			case errors.Is(err, ErrPhoneAlreadySet):
				httpserver.WriteError(w, http.StatusConflict, "phone_already_set", "This account has a different verified phone.", false)
			case err != nil:
				o.unavailable(w, req, err)
			default:
				httpserver.WriteJSON(w, http.StatusOK, map[string]any{
					"access_token": v.Pair.AccessToken, "refresh_token": v.Pair.RefreshToken, "expires_in_s": v.Pair.ExpiresInS,
					"customer_id": v.Actor.ID, "phone_verified": true, "server_time": serverTime(),
				})
			}
		})
	})
}
