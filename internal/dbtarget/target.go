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

// The two logins for Ninzo's project. Migrations use one, and services use the other.
const (
	MigrationRole = "postgres"
	ServiceRole   = "ninzo_service"
)

// transactionPoolerPort is refused: decision 0005 uses the session pooler.
const transactionPoolerPort = 6543

const (
	poolerSuffix = ".pooler.supabase.com"
	directSuffix = ".supabase.co"
)

// Target is what a database URL really connects to.
type Target struct {
	hosts    []string
	ports    []uint16
	user     string
	password string
	options  string
	tls      bool
}

// String and GoString stop the password reaching a log, however a Target is printed.
func (t Target) String() string { return "database at " + t.Host() }

// GoString is the same as String.
func (t Target) GoString() string { return t.String() }

// Format covers every fmt verb, such as %d and %x, which skip String.
func (t Target) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(t.String())) }

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
	t := Target{
		hosts:    []string{cfg.Host},
		ports:    []uint16{cfg.Port},
		user:     cfg.User,
		password: cfg.Password,
		options:  cfg.RuntimeParams["options"],
		tls:      cfg.TLSConfig != nil,
	}
	for _, f := range cfg.Fallbacks {
		t.hosts = append(t.hosts, f.Host)
		t.ports = append(t.ports, f.Port)
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

// parts returns the login and project ref only for the two URL shapes Supabase really uses:
// <role>.<ref> on a pooler host, or <role> on db.<ref>.supabase.co, for our two logins. They
// are empty for anything else, including a URL with more than one host, because the driver
// could fall back to a host that was never checked.
func (t Target) parts() (role, ref string) {
	for _, h := range t.hosts {
		if h != t.hosts[0] {
			return "", ""
		}
	}
	known := func(r string) bool { return r == MigrationRole || r == ServiceRole }
	host := t.hosts[0]
	if strings.HasSuffix(host, poolerSuffix) {
		if r, ref, ok := strings.Cut(t.user, "."); ok && known(r) {
			return r, ref
		}
		return "", ""
	}
	if known(t.user) && strings.HasPrefix(host, "db.") && strings.HasSuffix(host, directSuffix) {
		return t.user, strings.TrimSuffix(strings.TrimPrefix(host, "db."), directSuffix)
	}
	return "", ""
}

// Ref returns the Supabase project ref, or nothing for a URL that is not a known shape.
func (t Target) Ref() string {
	_, ref := t.parts()
	return ref
}

// Host returns the host to show people.
func (t Target) Host() string {
	if len(t.hosts) == 0 {
		return "unknown"
	}
	return t.hosts[0]
}

// CheckLocal refuses anything but this machine.
func (t Target) CheckLocal() error {
	if !t.IsLocal() {
		return fmt.Errorf("refusing %s: this is for local Postgres only", t.Host())
	}
	return nil
}

// CheckRemote is for migrations: only Ninzo's project, as postgres.
func (t Target) CheckRemote() error { return t.check(MigrationRole) }

// CheckService is for services: only Ninzo's project, as ninzo_service.
func (t Target) CheckService() error { return t.check(ServiceRole) }

func (t Target) check(wantRole string) error {
	if t.IsLocal() {
		return errors.New("refusing localhost: this is for the Supabase project")
	}
	role, ref := t.parts()
	if ref != NinzoRef {
		return fmt.Errorf("refusing project %q: only Ninzo's project %s is allowed", ref, NinzoRef)
	}
	if role != wantRole {
		return fmt.Errorf("refusing the login %s: this must use %s", role, wantRole)
	}
	// Connection options may be able to reroute a pooler connection to another project.
	if t.options != "" {
		return errors.New("refusing connection options in the URL or PGOPTIONS")
	}
	for _, port := range t.ports {
		if port == transactionPoolerPort {
			return errors.New("refusing port 6543, the transaction pooler: use the session pooler")
		}
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
