package identity

import (
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/config"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/ratelimit"
)

// These are per network address unless they say otherwise. Many mobile users in India share one
// address, so the limits leave room for a neighbourhood. Change the numbers here.
var (
	ruleCodeRequestMinute = ratelimit.Rule{Name: "otp_request_ip_min", Limit: 20, Window: time.Minute}
	ruleCodeRequestHour   = ratelimit.Rule{Name: "otp_request_ip_hour", Limit: 100, Window: time.Hour}
	ruleCodeVerifyMinute  = ratelimit.Rule{Name: "otp_verify_ip_min", Limit: 30, Window: time.Minute}
	ruleCodeVerifyHour    = ratelimit.Rule{Name: "otp_verify_ip_hour", Limit: 200, Window: time.Hour}
	ruleGoogleMinute      = ratelimit.Rule{Name: "google_ip_min", Limit: 30, Window: time.Minute}
	ruleRefreshMinute     = ratelimit.Rule{Name: "refresh_ip_min", Limit: 120, Window: time.Minute}
	ruleConsoleMinute     = ratelimit.Rule{Name: "console_signin_ip_min", Limit: 20, Window: time.Minute}
	ruleConsoleHour       = ratelimit.Rule{Name: "console_signin_ip_hour", Limit: 100, Window: time.Hour}
	// One token: a used token repeated in a loop adds a row each time.
	ruleRefreshToken = ratelimit.Rule{Name: "refresh_token_min", Limit: 20, Window: time.Minute}
	// One phone from one address. A stranger reaches this long before the wider otpPerHour, so they
	// cannot lock the phone's owner out from another address.
	rulePhoneAndAddress = ratelimit.Rule{Name: "otp_phone_ip_hour", Limit: 5, Window: time.Hour}
	// One session, on sign-in and account routes. Rider location pings must not share this rule.
	ruleSession = ratelimit.Rule{Name: "session_min", Limit: 30, Window: time.Minute}
	// One session's location pings. The app pings every 2 to 3 seconds at most.
	rulePings = ratelimit.Rule{Name: "pings_min", Limit: 90, Window: time.Minute}
)

// Limits applies the rules. A nil *Limits limits nothing, for tests of other behaviour.
type Limits struct {
	Limiter  ratelimit.Limiter
	ClientIP func(*http.Request) string
	Log      *slog.Logger
}

// NewLimits builds Limits from a service's settings.
func NewLimits(l ratelimit.Limiter, cfg config.Config, log *slog.Logger) *Limits {
	var warned sync.Once
	return &Limits{
		Limiter: l,
		ClientIP: func(r *http.Request) string {
			// Otherwise a misnamed header would quietly put every customer under one limit.
			if cfg.ClientIPHeader != "" && !httpserver.HasClientIP(r, cfg.ClientIPHeader) {
				warned.Do(func() {
					log.Warn("identity: CLIENT_IP_HEADER is missing from a request, so callers may share one limit", "header", cfg.ClientIPHeader)
				})
			}
			return httpserver.ClientIP(r, cfg.ClientIPHeader)
		},
		Log: log,
	}
}

// RequireClientIP stops an API starting behind a proxy with no way to tell its callers apart, which
// would put every customer under one limit.
func RequireClientIP(cfg config.Config) error {
	if !cfg.IsLocal() && cfg.ClientIPHeader == "" {
		return errors.New("CLIENT_IP_HEADER must be set outside local")
	}
	return nil
}

// allow reports whether the request may go on. If not, it has written the reply. A limiter that
// fails lets the request through: sign-in matters more than the limit, and the per-phone limits in
// Postgres still hold.
func (l *Limits) allow(w http.ResponseWriter, r *http.Request, key string, rule ratelimit.Rule, code string) bool {
	if l == nil {
		return true
	}
	ok, wait, err := l.Limiter.Allow(r.Context(), key, rule)
	if err != nil {
		l.Log.Warn("identity: rate limiter failed", "request_id", httpserver.RequestID(r.Context()), "rule", rule.Name, "error", err.Error())
		return true
	}
	if ok {
		return true
	}
	// The address is not logged, and neither is the key, which can hold a phone number.
	l.Log.Warn("identity: rate limited", "request_id", httpserver.RequestID(r.Context()), "rule", rule.Name)
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
	httpserver.WriteError(w, http.StatusTooManyRequests, code, "Too many requests.", true)
	return false
}

// byAddress limits by the caller's network address, before any database work.
func (l *Limits) byAddress(rules ...ratelimit.Rule) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if l != nil {
				ip := l.ClientIP(r)
				for _, rule := range rules {
					if !l.allow(w, r, ip, rule, "rate_limited") {
						return
					}
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ByPings limits one session's location pings. Use it after RequireActor.
// ConsoleSignIn limits the console's sign-in steps by address.
func (l *Limits) ConsoleSignIn() func(http.Handler) http.Handler {
	return l.byAddress(ruleConsoleMinute, ruleConsoleHour)
}

func (l *Limits) ByPings() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if actor, ok := ActorFrom(r.Context()); ok && !l.allow(w, r, actor.ChainID, rulePings, "rate_limited") {
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// BySession limits one signed-in session. Use it after RequireActor.
func (l *Limits) BySession() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if actor, ok := ActorFrom(r.Context()); ok && !l.allow(w, r, actor.ChainID, ruleSession, "rate_limited") {
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// allowPhone limits one phone from one address, with the reply a code request already has.
func (l *Limits) allowPhone(w http.ResponseWriter, r *http.Request, phone string) bool {
	if l == nil || !phonePattern.MatchString(phone) {
		return true // A bad number is refused next, and must not fill the table.
	}
	return l.allow(w, r, phone+"|"+l.ClientIP(r), rulePhoneAndAddress, "otp_resend_limit")
}

// refundPhone gives back the count when no code was made, such as during an SMS outage, or a tap
// inside the 30 seconds. Those must not use up someone's 5 codes.
func (l *Limits) refundPhone(r *http.Request, phone string) {
	if l == nil || !phonePattern.MatchString(phone) {
		return
	}
	if err := l.Limiter.Release(r.Context(), phone+"|"+l.ClientIP(r), rulePhoneAndAddress); err != nil {
		l.Log.Warn("identity: rate limiter failed", "request_id", httpserver.RequestID(r.Context()), "rule", rulePhoneAndAddress.Name, "error", err.Error())
	}
}
