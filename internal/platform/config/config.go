// Package config reads a service's settings from the environment and refuses to start on
// anything unsafe. Its errors name settings but never hold their values.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

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
}

// IsLocal reports whether this runs on a developer's machine.
func (c Config) IsLocal() bool { return c.Env == EnvLocal }

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

	if len(problems) > 0 {
		return Config{}, errors.Join(problems...)
	}
	return c, nil
}
