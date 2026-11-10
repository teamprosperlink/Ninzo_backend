package identity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

type actorKey struct{}

// ActorFrom returns the signed-in actor, set by RequireActor.
func ActorFrom(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(Actor)
	return a, ok
}

// ActorID is what the idempotency middleware needs: it runs after RequireActor.
func ActorID(r *http.Request) (string, bool) {
	a, ok := ActorFrom(r.Context())
	return a.ID, ok
}

// RequireActor lets through only a valid access token for this API's user type.
func RequireActor(signer Signer, actorType string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !found || token == "" {
				httpserver.WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in first.", false)
				return
			}
			actor, err := signer.Verify(token, actorType)
			switch {
			case errors.Is(err, ErrTokenExpired):
				httpserver.WriteError(w, http.StatusUnauthorized, "token_expired", "The access token has expired.", false)
			case err != nil:
				httpserver.WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in first.", false)
			default:
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, actor)))
			}
		})
	}
}

// MountSessions adds refreshToken and signOut for one API's user type.
func MountSessions(r chi.Router, s *Sessions, actorType string) {
	r.With(s.Limits.byAddress(ruleRefreshMinute)).Post("/auth/refresh", s.handleRefresh(actorType))
	r.With(RequireActor(s.Signer, actorType), s.Limits.BySession()).Post("/auth/signout", s.handleSignOut())
}

func serverTime() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (s *Sessions) handleRefresh(actorType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.RefreshToken == "" {
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send refresh_token.", false)
			return
		}
		if !s.Limits.allow(w, r, hashToken(body.RefreshToken), ruleRefreshToken, "rate_limited") {
			return
		}
		pair, _, err := s.Refresh(r.Context(), body.RefreshToken, actorType)
		switch {
		case errors.Is(err, ErrSessionInvalid):
			httpserver.WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in again.", false)
		case err != nil:
			s.Log.Error("identity: refresh failed", "request_id", httpserver.RequestID(r.Context()), "error", err.Error())
			w.Header().Set("Retry-After", "5")
			httpserver.WriteError(w, http.StatusServiceUnavailable, "service_unavailable", "Try again shortly.", true)
		default:
			httpserver.WriteJSON(w, http.StatusOK, map[string]any{
				"access_token":  pair.AccessToken,
				"refresh_token": pair.RefreshToken,
				"expires_in_s":  pair.ExpiresInS,
				"server_time":   serverTime(),
			})
		}
	}
}

func (s *Sessions) handleSignOut() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := ActorFrom(r.Context())
		if !ok {
			httpserver.WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in first.", false)
			return
		}
		if err := s.SignOut(r.Context(), actor); err != nil {
			s.Log.Error("identity: sign-out failed", "request_id", httpserver.RequestID(r.Context()), "error", err.Error())
			w.Header().Set("Retry-After", "5")
			httpserver.WriteError(w, http.StatusServiceUnavailable, "service_unavailable", "Try again shortly.", true)
			return
		}
		httpserver.WriteJSON(w, http.StatusOK, map[string]any{"server_time": serverTime()})
	}
}

// otpFormat is the contract's pattern for a code.
var otpFormat = regexp.MustCompile(`^[0-9]{4,8}$`)

// MountOTP adds requestOtp and verifyOtp for one API's user type.
func MountOTP(r chi.Router, o *OTPs, actorType string) {
	r.With(o.Limits.byAddress(ruleCodeRequestMinute, ruleCodeRequestHour)).Post("/auth/otp/request", o.handleRequest(actorType))
	r.With(o.Limits.byAddress(ruleCodeVerifyMinute, ruleCodeVerifyHour)).Post("/auth/otp/verify", o.handleVerify(actorType))
}

func (o *OTPs) handleRequest(actorType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Phone string `json:"phone"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send a phone number.", false)
			return
		}
		if !o.Limits.allowPhone(w, r, body.Phone) {
			return
		}
		req, err := o.Request(r.Context(), actorType, body.Phone)
		if err != nil {
			o.Limits.refundPhone(r, body.Phone)
		}
		var wait *RetryAfterError
		switch {
		case errors.Is(err, ErrBadPhone):
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "That phone number is not valid.", false)
		case errors.As(err, &wait):
			w.Header().Set("Retry-After", strconv.Itoa(wait.Seconds))
			httpserver.WriteError(w, http.StatusTooManyRequests, "otp_resend_limit", "Too many codes requested.", true)
		case err != nil:
			o.unavailable(w, r, err)
		default:
			httpserver.WriteJSON(w, http.StatusOK, map[string]any{
				"request_id":     req.ID,
				"expires_in_s":   req.ExpiresInS,
				"resend_after_s": req.ResendAfterS,
				"server_time":    serverTime(),
			})
		}
	}
}

func (o *OTPs) handleVerify(actorType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RequestID     string `json:"request_id"`
			OTP           string `json:"otp"`
			SwitchToPhone bool   `json:"switch_to_phone"` // Customers only.
		}
		// A malformed code is a bad request, so it does not burn one of the five attempts.
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.RequestID == "" || !otpFormat.MatchString(body.OTP) {
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send request_id and otp.", false)
			return
		}
		v, err := o.verify(r.Context(), actorType, body.RequestID, body.OTP, body.SwitchToPhone && actorType == TypeCustomer)
		var wrongMethod *WrongMethodError
		switch {
		case errors.As(err, &wrongMethod):
			httpserver.WriteJSON(w, http.StatusConflict, map[string]any{
				"code": "sign_in_method_mismatch", "message": "This account signs in another way.", "retryable": false,
				"sign_in_method": wrongMethod.Method, "server_time": serverTime(),
			})
		case errors.Is(err, ErrInvalidOTP):
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_otp", "That code is wrong.", false)
		case errors.Is(err, ErrOTPExpired):
			httpserver.WriteError(w, http.StatusBadRequest, "otp_expired", "That code has expired.", false)
		case errors.Is(err, ErrOTPAttempts):
			w.Header().Set("Retry-After", strconv.Itoa(int(otpResendAfter.Seconds())))
			httpserver.WriteError(w, http.StatusTooManyRequests, "otp_attempts_exceeded", "Too many wrong codes. Ask for a new one.", false)
		case errors.Is(err, ErrAccountBlocked):
			httpserver.WriteJSON(w, http.StatusForbidden, map[string]any{
				"code": "account_blocked", "message": "This account is blocked.", "retryable": false,
				"missing_permission": "account_active", "server_time": serverTime(),
			})
		case err != nil:
			o.unavailable(w, r, err)
		default:
			reply := map[string]any{
				"access_token":  v.Pair.AccessToken,
				"refresh_token": v.Pair.RefreshToken,
				"expires_in_s":  v.Pair.ExpiresInS,
				"server_time":   serverTime(),
			}
			switch actorType {
			case TypeRider:
				reply["rider_id"], reply["status"] = v.Actor.ID, v.Status
			case TypeStaff:
				reply["staff_id"], reply["role"], reply["hub_id"] = v.Actor.ID, v.Status, v.HubID
			default:
				reply["customer_id"], reply["phone_verified"] = v.Actor.ID, true
			}
			httpserver.WriteJSON(w, http.StatusOK, reply)
		}
	}
}

func (o *OTPs) unavailable(w http.ResponseWriter, r *http.Request, err error) {
	o.Log.Error("identity: sign-in failed", "request_id", httpserver.RequestID(r.Context()), "error", err.Error())
	w.Header().Set("Retry-After", "5")
	httpserver.WriteError(w, http.StatusServiceUnavailable, "service_unavailable", "Try again shortly.", true)
}

// MountGoogle adds signInWithGoogle. Only the customer API has it.
func MountGoogle(r chi.Router, g *GoogleSignIn) {
	r.With(g.Limits.byAddress(ruleGoogleMinute)).Post("/auth/google", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			IDToken string `json:"id_token"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.IDToken == "" {
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send id_token.", false)
			return
		}
		out, err := g.SignIn(req.Context(), body.IDToken)
		switch {
		case errors.Is(err, ErrGoogleToken):
			httpserver.WriteError(w, http.StatusUnauthorized, "invalid_google_token", "Google could not confirm this sign-in.", false)
		case errors.Is(err, ErrAccountBlocked):
			WriteBlocked(w)
		case err != nil:
			g.Log.Error("identity: Google sign-in failed", "request_id", httpserver.RequestID(req.Context()), "error", err.Error())
			w.Header().Set("Retry-After", "5")
			httpserver.WriteError(w, http.StatusServiceUnavailable, "service_unavailable", "Try again shortly.", true)
		default:
			httpserver.WriteJSON(w, http.StatusOK, map[string]any{
				"access_token":   out.Pair.AccessToken,
				"refresh_token":  out.Pair.RefreshToken,
				"expires_in_s":   out.Pair.ExpiresInS,
				"customer_id":    out.Actor.ID,
				"phone_verified": out.PhoneVerified,
				"server_time":    serverTime(),
			})
		}
	})
}

// WriteBlocked sends the reply for a blocked person.
func WriteBlocked(w http.ResponseWriter) {
	httpserver.WriteJSON(w, http.StatusForbidden, map[string]any{
		"code": "account_blocked", "message": "This account is blocked.", "retryable": false,
		"missing_permission": "account_active", "server_time": serverTime(),
	})
}

// MountPhoneVerification adds requestPhoneOtp and verifyPhone. Only the customer API has them.
func MountPhoneVerification(r chi.Router, o *OTPs, signer Signer) {
	r.Group(func(signedIn chi.Router) {
		signedIn.Use(RequireActor(signer, TypeCustomer), o.Limits.BySession())
		// The same counts as /auth/otp/request, because each code costs the same.
		signedIn.With(o.Limits.byAddress(ruleCodeRequestMinute, ruleCodeRequestHour)).Post("/me/phone/otp", func(w http.ResponseWriter, req *http.Request) {
			actor, _ := ActorFrom(req.Context())
			var body struct {
				Phone string `json:"phone"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send a phone number.", false)
				return
			}
			if !o.Limits.allowPhone(w, req, body.Phone) {
				return
			}
			out, err := o.RequestPhone(req.Context(), actor, body.Phone)
			if err != nil {
				o.Limits.refundPhone(req, body.Phone)
			}
			var wait *RetryAfterError
			switch {
			case errors.Is(err, ErrBadPhone):
				httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "That phone number is not valid.", false)
			case errors.As(err, &wait):
				w.Header().Set("Retry-After", strconv.Itoa(wait.Seconds))
				httpserver.WriteError(w, http.StatusTooManyRequests, "otp_resend_limit", "Too many codes requested.", true)
			case errors.Is(err, ErrSessionInvalid):
				httpserver.WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in again.", false)
			case err != nil:
				o.unavailable(w, req, err)
			default:
				httpserver.WriteJSON(w, http.StatusOK, map[string]any{
					"request_id": out.ID, "expires_in_s": out.ExpiresInS, "resend_after_s": out.ResendAfterS, "server_time": serverTime(),
				})
			}
		})
		signedIn.Post("/me/phone/verify", func(w http.ResponseWriter, req *http.Request) {
			actor, _ := ActorFrom(req.Context())
			var body struct {
				RequestID string `json:"request_id"`
				OTP       string `json:"otp"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.RequestID == "" || !otpFormat.MatchString(body.OTP) {
				httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send request_id and otp.", false)
				return
			}
			v, err := o.VerifyPhone(req.Context(), actor, body.RequestID, body.OTP)
			switch {
			case errors.Is(err, ErrInvalidOTP):
				httpserver.WriteError(w, http.StatusBadRequest, "invalid_otp", "That code is wrong.", false)
			case errors.Is(err, ErrOTPExpired):
				httpserver.WriteError(w, http.StatusBadRequest, "otp_expired", "That code has expired.", false)
			case errors.Is(err, ErrOTPAttempts):
				w.Header().Set("Retry-After", strconv.Itoa(int(otpResendAfter.Seconds())))
				httpserver.WriteError(w, http.StatusTooManyRequests, "otp_attempts_exceeded", "Too many wrong codes. Ask for a new one.", false)
			case errors.Is(err, ErrAccountBlocked):
				WriteBlocked(w)
			case errors.Is(err, ErrPhoneAlreadySet):
				httpserver.WriteError(w, http.StatusConflict, "phone_already_set", "This account already has a different verified phone.", false)
			case errors.Is(err, ErrPhoneAlreadyRegistered):
				httpserver.WriteError(w, http.StatusConflict, "phone_already_registered", "This number already has an account. Log in with phone and OTP.", false)
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
