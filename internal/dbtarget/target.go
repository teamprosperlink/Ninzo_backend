// Package dbtarget decides whether a database URL may be used, using the same parser
// that later connects to it. Its errors never contain the URL, because the URL holds the
// password.
package dbtarget

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// NinzoRef is Ninzo's Supabase project. The organisation also has another product's
// project, which must never be touched.
const NinzoRef = "chyejfgsipcsdxnginei"

// transactionPoolerPort is refused: decision 0005 uses the session pooler.
const transactionPoolerPort = 6543

const (
	poolerSuffix = ".pooler.supabase.com"
	directSuffix = ".supabase.co"
)

// Target is what a database URL really connects to.
type Target struct {
	hosts    []string
	port     uint16
	user     string
	password string
	tls      bool
}

// Parse reads a database URL.
func Parse(dsn string) (Target, error) {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return Target{}, errors.New("the database URL is not valid")
	}
	// An unencoded / in a password makes the rest of it look like the database name,
	// which drivers then print in their errors.
	if strings.ContainsAny(cfg.Database, "@/") {
		return Target{}, errors.New("the database URL is not valid: URL-encode any special characters in the password")
	}
	t := Target{hosts: []string{cfg.Host}, port: cfg.Port, user: cfg.User, password: cfg.Password, tls: cfg.TLSConfig != nil}
	for _, f := range cfg.Fallbacks {
		t.hosts = append(t.hosts, f.Host)
		t.tls = t.tls && f.TLSConfig != nil
	}
	return t, nil
}

// IsLocal reports whether every host the URL can reach is this machine.
func (t Target) IsLocal() bool {
	for _, h := range t.hosts {
		if h != "localhost" && h != "127.0.0.1" && h != "::1" {
			return false
		}
	}
	return true
}

// Ref returns the Supabase project ref only for the two URL shapes Supabase really uses:
// postgres.<ref> on a pooler host, or postgres on db.<ref>.supabase.co. It is empty for
// anything else, including a URL with more than one host, because the driver could fall
// back to a host that was never checked.
func (t Target) Ref() string {
	for _, h := range t.hosts {
		if h != t.hosts[0] {
			return ""
		}
	}
	host := t.hosts[0]
	if strings.HasSuffix(host, poolerSuffix) {
		ref, _ := strings.CutPrefix(t.user, "postgres.")
		if ref == t.user {
			return ""
		}
		return ref
	}
	if t.user == "postgres" && strings.HasPrefix(host, "db.") && strings.HasSuffix(host, directSuffix) {
		return strings.TrimSuffix(strings.TrimPrefix(host, "db."), directSuffix)
	}
	return ""
}

// Host returns the host to show people.
func (t Target) Host() string { return t.hosts[0] }

// CheckLocal refuses anything but this machine.
func (t Target) CheckLocal() error {
	if !t.IsLocal() {
		return fmt.Errorf("refusing %s: this is for local Postgres only", t.Host())
	}
	return nil
}

// CheckRemote refuses anything but Ninzo's project, over TLS, on the session pooler or
// a direct connection.
func (t Target) CheckRemote() error {
	if t.IsLocal() {
		return errors.New("refusing localhost: this is for the Supabase project")
	}
	if ref := t.Ref(); ref != NinzoRef {
		return fmt.Errorf("refusing project %q: only Ninzo's project %s is allowed", ref, NinzoRef)
	}
	if t.port == transactionPoolerPort {
		return errors.New("refusing port 6543, the transaction pooler: use the session pooler")
	}
	if !t.tls {
		return errors.New("refusing a connection that can fall back to no TLS: add sslmode=require to the URL")
	}
	return nil
}

// Safe removes the password from an error from a driver.
func (t Target) Safe(err error) string {
	msg := err.Error()
	if t.password != "" {
		msg = strings.ReplaceAll(msg, t.password, "***")
	}
	return msg
}
