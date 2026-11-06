package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/teamprosperlink/Ninzo_backend/internal/dbtarget"
)

const (
	localURL = "postgres://ninzo:S3CRET@localhost:5432/ninzo?sslmode=disable"
	pooler   = "aws-0-ap-south-1.pooler.supabase.com"
	otherRef = "fvyyfghhccivwwvywvuu"
)

var (
	serviceURL   = "postgres://ninzo_service." + dbtarget.NinzoRef + ":S3CRET@" + pooler + ":5432/postgres?sslmode=require"
	migrationURL = "postgres://postgres." + dbtarget.NinzoRef + ":S3CRET@" + pooler + ":5432/postgres?sslmode=require"
)

const (
	localKey = LocalKeyPrefix + "key-for-tests-long-enough"
	realKey  = "a-real-looking-signing-key-long-enough-S3CRET"
)

// env adds a valid signing key, which each case can override or remove.
func env(values map[string]string) func(string) string {
	return func(k string) string {
		if v, ok := values[k]; ok {
			return v
		}
		if k == "JWT_SIGNING_KEY" {
			if values["APP_ENV"] == "local" || values["APP_ENV"] == "" {
				return localKey
			}
			return realKey
		}
		return ""
	}
}

func TestLoadGoodSettings(t *testing.T) {
	c, err := Load(env(map[string]string{"APP_ENV": "local", "DATABASE_URL": localURL}))
	if err != nil {
		t.Fatal(err)
	}
	if !c.IsLocal() || c.Port != 8080 || c.DBMaxConns != 5 || c.LogLevel != slog.LevelInfo || c.OTPTestMode {
		t.Errorf("defaults: %+v", c)
	}
	c, err = Load(env(map[string]string{
		"APP_ENV": "staging", "DATABASE_URL": serviceURL, "HTTP_PORT": "9000", "DB_MAX_CONNS": "3", "LOG_LEVEL": "debug", "OTP_TEST_MODE": "true",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.IsLocal() || c.Port != 9000 || c.DBMaxConns != 3 || c.LogLevel != slog.LevelDebug || !c.OTPTestMode {
		t.Errorf("staging: %+v", c)
	}
	if _, err = Load(env(map[string]string{"APP_ENV": "production", "DATABASE_URL": serviceURL})); err != nil {
		t.Errorf("production with test OTPs off: %v", err)
	}
}

func TestLoadRefuses(t *testing.T) {
	cases := []struct {
		name   string
		values map[string]string
		want   string
	}{
		{"no environment", map[string]string{"DATABASE_URL": localURL}, "APP_ENV is not set"},
		{"unknown environment", map[string]string{"APP_ENV": "prod", "DATABASE_URL": localURL}, "APP_ENV must be"},
		{"no database", map[string]string{"APP_ENV": "local"}, "DATABASE_URL is not set"},
		{"bad database URL", map[string]string{"APP_ENV": "local", "DATABASE_URL": "://S3CRET"}, "not valid"},
		{"local running on Supabase", map[string]string{"APP_ENV": "local", "DATABASE_URL": serviceURL}, "local Postgres only"},
		{"staging on localhost", map[string]string{"APP_ENV": "staging", "DATABASE_URL": localURL}, "refusing localhost"},
		{"staging as postgres", map[string]string{"APP_ENV": "staging", "DATABASE_URL": migrationURL}, "ninzo_service"},
		{"staging on the other project", map[string]string{"APP_ENV": "staging", "DATABASE_URL": "postgres://ninzo_service." + otherRef + ":S3CRET@" + pooler + ":5432/postgres?sslmode=require"}, "refusing project"},
		{"staging with no TLS", map[string]string{"APP_ENV": "staging", "DATABASE_URL": "postgres://ninzo_service." + dbtarget.NinzoRef + ":S3CRET@" + pooler + ":5432/postgres"}, "sslmode=require"},
		{"production with test OTPs", map[string]string{"APP_ENV": "production", "DATABASE_URL": serviceURL, "OTP_TEST_MODE": "true"}, "must be false in production"},
		{"unclear test OTP setting", map[string]string{"APP_ENV": "local", "DATABASE_URL": localURL, "OTP_TEST_MODE": "maybe"}, "true or false"},
		{"bad port", map[string]string{"APP_ENV": "local", "DATABASE_URL": localURL, "HTTP_PORT": "0"}, "HTTP_PORT"},
		{"too many connections", map[string]string{"APP_ENV": "local", "DATABASE_URL": localURL, "DB_MAX_CONNS": "50"}, "DB_MAX_CONNS"},
		{"bad log level", map[string]string{"APP_ENV": "local", "DATABASE_URL": localURL, "LOG_LEVEL": "loud"}, "LOG_LEVEL"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(env(c.values))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
			if strings.Contains(err.Error(), "S3CRET") {
				t.Errorf("the error shows the password: %v", err)
			}
		})
	}
}

func TestSessionSettings(t *testing.T) {
	c, err := Load(env(map[string]string{"APP_ENV": "local", "DATABASE_URL": localURL}))
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessTTL != 15*time.Minute || c.RefreshTTL != 90*24*time.Hour {
		t.Errorf("defaults: %s, %s", c.AccessTTL, c.RefreshTTL)
	}
	c, err = Load(env(map[string]string{"APP_ENV": "local", "DATABASE_URL": localURL, "JWT_ACCESS_TTL_SECONDS": "600", "JWT_REFRESH_TTL_SECONDS": "7200"}))
	if err != nil || c.AccessTTL != 10*time.Minute || c.RefreshTTL != 2*time.Hour {
		t.Errorf("set: %s, %s, %v", c.AccessTTL, c.RefreshTTL, err)
	}
	for name, values := range map[string]map[string]string{
		"no key":                         {"APP_ENV": "local", "DATABASE_URL": localURL, "JWT_SIGNING_KEY": ""},
		"a short key":                    {"APP_ENV": "local", "DATABASE_URL": localURL, "JWT_SIGNING_KEY": "short"},
		"the local key on staging":       {"APP_ENV": "staging", "DATABASE_URL": serviceURL, "JWT_SIGNING_KEY": localKey},
		"the local key in production":    {"APP_ENV": "production", "DATABASE_URL": serviceURL, "JWT_SIGNING_KEY": localKey},
		"an hour-long-plus access token": {"APP_ENV": "local", "DATABASE_URL": localURL, "JWT_ACCESS_TTL_SECONDS": "7200"},
		"a bad access length":            {"APP_ENV": "local", "DATABASE_URL": localURL, "JWT_ACCESS_TTL_SECONDS": "x"},
		"a tiny refresh length":          {"APP_ENV": "local", "DATABASE_URL": localURL, "JWT_REFRESH_TTL_SECONDS": "60"},
	} {
		_, err := Load(env(values))
		if err == nil {
			t.Errorf("%s must be refused", name)
			continue
		}
		if strings.Contains(err.Error(), "S3CRET") || strings.Contains(err.Error(), localKey) {
			t.Errorf("%s: the error shows the key: %v", name, err)
		}
	}
}

func TestPrintingConfigHidesTheSigningKey(t *testing.T) {
	c, err := Load(env(map[string]string{"APP_ENV": "staging", "DATABASE_URL": serviceURL}))
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		if strings.Contains(fmt.Sprintf(format, c), "real-looking") {
			t.Errorf("%s shows the key", format)
		}
	}
}

func TestPlatformPort(t *testing.T) {
	c, err := Load(env(map[string]string{"APP_ENV": "local", "DATABASE_URL": localURL, "PORT": "7000"}))
	if err != nil || c.Port != 7000 {
		t.Errorf("PORT: %d, %v", c.Port, err)
	}
	c, err = Load(env(map[string]string{"APP_ENV": "local", "DATABASE_URL": localURL, "PORT": "7000", "HTTP_PORT": "7001"}))
	if err != nil || c.Port != 7001 {
		t.Errorf("HTTP_PORT wins: %d, %v", c.Port, err)
	}
	if _, err = Load(env(map[string]string{"APP_ENV": "local", "DATABASE_URL": localURL, "PORT": "nope"})); err == nil {
		t.Error("a bad PORT must be refused")
	}
}

func TestPrintingConfigHidesThePassword(t *testing.T) {
	c, err := Load(env(map[string]string{"APP_ENV": "local", "DATABASE_URL": localURL}))
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		if got := fmt.Sprintf(format, c); strings.Contains(got, "S3CRET") {
			t.Errorf("%s shows the password: %s", format, got)
		}
	}
}

func TestSecretIsHiddenEveryWay(t *testing.T) {
	c, err := Load(env(map[string]string{"APP_ENV": "local", "DATABASE_URL": localURL}))
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x"} {
		for name, value := range map[string]any{"config": c, "secret": c.DatabaseURL, "target": c.Database} {
			got := fmt.Sprintf(format, value)
			if strings.Contains(got, "S3CRET") || strings.Contains(got, fmt.Sprintf("%x", "S3CRET")) {
				t.Errorf("%s with %s shows the password: %s", name, format, got)
			}
		}
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	log.Info("x", "config", c, "secret", c.DatabaseURL, "target", c.Database)
	log.With("secret", c.DatabaseURL).Info("y", slog.Group("g", "config", c))
	if out := logs.String(); strings.Contains(out, "S3CRET") || !strings.Contains(out, `"msg":"y"`) {
		t.Errorf("logs: %s", out)
	}
	raw, err := json.Marshal(c)
	if err != nil || strings.Contains(string(raw), "S3CRET") {
		t.Errorf("JSON: %s, %v", raw, err)
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	_, err := Load(env(map[string]string{"HTTP_PORT": "x"}))
	for _, want := range []string{"APP_ENV", "HTTP_PORT", "DATABASE_URL"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("missing %s in %v", want, err)
		}
	}
}
