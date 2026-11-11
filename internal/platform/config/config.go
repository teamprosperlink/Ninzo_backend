// Package config reads a service's settings from the environment and refuses to start on
// anything unsafe. Its errors name settings but never hold their values.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/teamprosperlink/Ninzo_backend/internal/dbtarget"
)

const (
	EnvLocal      = "local"
	EnvStaging    = "staging"
	EnvProduction = "production"
)

// Secret is a value that must not reach a log line, however it is printed.
type Secret string

func (Secret) String() string   { return "***" }
func (Secret) GoString() string { return "***" }

// Format covers every fmt verb, such as %d and %x, which skip String.
func (Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte("***")) }

// LogValue covers a Secret logged directly.
func (Secret) LogValue() slog.Value { return slog.StringValue("***") }

// MarshalJSON covers a Secret inside a struct, which JSON logs encode with encoding/json.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"***"`), nil }

// Config holds what every service needs. Order timer values are not here: they are console
// settings stored in the database.
type Config struct {
	Env         string
	LogLevel    slog.Level
	Port        int
	DatabaseURL Secret
	DBMaxConns  int32
	Database    dbtarget.Target
	OTPTestMode bool
	OTPTestCode Secret
	// ConsoleTOTPOptional lets console staff with no authenticator sign in with their password
	// alone (owner, 2026-11-11: until pre-launch). Production refuses it.
	ConsoleTOTPOptional bool
	// JWTSigningKey signs access tokens.
	JWTSigningKey Secret
	// GoogleClientIDs are the audiences a Google ID token may name. They are public.
	GoogleClientIDs []string
	// ClientIPHeader names the header the platform's proxy sets to the caller's address.
	ClientIPHeader string
	// SignInMethods are the sign-in methods this customer API offers.
	SignInMethods []string
	AccessTTL     time.Duration
	RefreshTTL    time.Duration
}

// IsLocal reports whether this runs on a developer's machine.
func (c Config) IsLocal() bool { return c.Env == EnvLocal }

const minSigningKey = 32

var otpCodePattern = regexp.MustCompile(`^[0-9]{6}$`)

// LocalKeyPrefix marks the key the local tasks use, so it can never reach a real environment.
const LocalKeyPrefix = "local-only-"

func seconds(getenv func(string) string, name string, def, low, high time.Duration, problem func(string, ...any)) time.Duration {
	v := getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	d := time.Duration(n) * time.Second
	if err != nil || d < low || d > high {
		problem("%s must be a number of seconds from %d to %d", name, int(low.Seconds()), int(high.Seconds()))
		return def
	}
	return d
}

// Load reads the settings, and reports every problem at once.
func Load(getenv func(string) string) (Config, error) {
	var c Config
	var problems []error
	problem := func(format string, args ...any) { problems = append(problems, fmt.Errorf(format, args...)) }

	c.Env = getenv("APP_ENV")
	switch c.Env {
	case EnvLocal, EnvStaging, EnvProduction:
	case "":
		problem("APP_ENV is not set: use local, staging or production")
	default:
		problem("APP_ENV must be local, staging or production")
	}

	switch strings.ToLower(getenv("LOG_LEVEL")) {
	case "debug":
		c.LogLevel = slog.LevelDebug
	case "", "info":
		c.LogLevel = slog.LevelInfo
	case "warn":
		c.LogLevel = slog.LevelWarn
	case "error":
		c.LogLevel = slog.LevelError
	default:
		problem("LOG_LEVEL must be debug, info, warn or error")
	}

	c.Port = 8080
	// Hosting platforms such as Railway set PORT. HTTP_PORT is for running several APIs locally.
	portValue := getenv("HTTP_PORT")
	if portValue == "" {
		portValue = getenv("PORT")
	}
	if portValue != "" {
		port, err := strconv.Atoi(portValue)
		if err != nil || port < 1 || port > 65535 {
			problem("HTTP_PORT or PORT must be a number from 1 to 65535")
		}
		c.Port = port
	}

	c.DBMaxConns = 5
	if v := getenv("DB_MAX_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 10 {
			problem("DB_MAX_CONNS must be a number from 1 to 10: all services share the plan's connection limit")
		}
		c.DBMaxConns = int32(n)
	}

	if v := getenv("OTP_TEST_MODE"); v != "" {
		on, err := strconv.ParseBool(v)
		if err != nil {
			problem("OTP_TEST_MODE must be true or false")
		}
		c.OTPTestMode = on
	}
	if c.OTPTestMode && c.Env == EnvProduction {
		problem("OTP_TEST_MODE must be false in production")
	}
	switch getenv("CONSOLE_TOTP") {
	case "", "required":
	case "optional":
		c.ConsoleTOTPOptional = true
	default:
		problem("CONSOLE_TOTP must be required or optional")
	}
	if c.ConsoleTOTPOptional && c.Env == EnvProduction {
		problem("CONSOLE_TOTP must be required in production")
	}
	c.OTPTestCode = Secret(getenv("OTP_TEST_CODE"))
	if c.OTPTestMode && !otpCodePattern.MatchString(string(c.OTPTestCode)) {
		problem("OTP_TEST_CODE must be six digits when OTP_TEST_MODE is true")
	}

	c.DatabaseURL = Secret(getenv("DATABASE_URL"))
	if c.DatabaseURL == "" {
		problem("DATABASE_URL is not set")
	} else if target, err := dbtarget.Parse(string(c.DatabaseURL)); err != nil {
		problems = append(problems, err)
	} else {
		c.Database = target
		switch c.Env {
		case EnvLocal:
			err = target.CheckLocal()
		case EnvStaging, EnvProduction:
			err = target.CheckService()
		}
		if err != nil {
			problems = append(problems, err)
		}
	}

	for _, id := range strings.Split(getenv("GOOGLE_CLIENT_IDS"), ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if !strings.HasSuffix(id, ".apps.googleusercontent.com") {
			problem("GOOGLE_CLIENT_IDS must hold Google client IDs, separated by commas")
			continue
		}
		c.GoogleClientIDs = append(c.GoogleClientIDs, id)
	}
	c.ClientIPHeader = strings.TrimSpace(getenv("CLIENT_IP_HEADER"))
	methods := getenv("SIGNIN_METHODS")
	if methods == "" {
		methods = "phone,google"
	}
	for _, m := range strings.Split(methods, ",") {
		m = strings.TrimSpace(m)
		if m != "phone" && m != "google" {
			problem("SIGNIN_METHODS must list phone or google, separated by commas")
			continue
		}
		c.SignInMethods = append(c.SignInMethods, m)
	}
	c.JWTSigningKey = Secret(getenv("JWT_SIGNING_KEY"))
	switch {
	case len(c.JWTSigningKey) < minSigningKey:
		problem("JWT_SIGNING_KEY must be at least %d characters", minSigningKey)
	case c.Env != EnvLocal && strings.HasPrefix(string(c.JWTSigningKey), LocalKeyPrefix):
		problem("JWT_SIGNING_KEY is the local development key, which staging and production must not use")
	}
	c.AccessTTL = seconds(getenv, "JWT_ACCESS_TTL_SECONDS", 15*time.Minute, time.Minute, time.Hour, problem)
	c.RefreshTTL = seconds(getenv, "JWT_REFRESH_TTL_SECONDS", 90*24*time.Hour, time.Hour, 365*24*time.Hour, problem)

	if len(problems) > 0 {
		return Config{}, errors.Join(problems...)
	}
	return c, nil
}
