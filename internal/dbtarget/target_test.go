package dbtarget

import (
	"errors"
	"strings"
	"testing"
)

const (
	otherRef = "fvyyfghhccivwwvywvuu"
	pooler   = "aws-0-ap-south-1.pooler.supabase.com"
)

func parse(t *testing.T, dsn string) Target {
	t.Helper()
	target, err := Parse(dsn)
	if err != nil {
		t.Fatalf("%s: %v", dsn, err)
	}
	return target
}

func TestCheckLocal(t *testing.T) {
	for dsn, want := range map[string]bool{
		"postgres://u:p@localhost:5432/d?sslmode=disable":              true,
		"postgres://u:p@127.0.0.1:5432/d":                              true,
		"postgres://u:p@127.0.0.2:5432/d":                              false,
		"postgres://u:p@localhost:5432/d?host=127.0.0.2":               false,
		"postgres://u:p@localhost,db.example.com:5432/d":               false,
		"postgres://postgres." + NinzoRef + ":p@" + pooler + ":5432/d": false,
		"postgres://u:p@localhost.example.com:5432/d?sslmode=require":  false,
	} {
		if err := parse(t, dsn).CheckLocal(); (err == nil) != want {
			t.Errorf("%s: local %v, got %v", dsn, want, err)
		}
	}
}

func TestCheckRemote(t *testing.T) {
	good := []string{
		"postgres://postgres." + NinzoRef + ":p@" + pooler + ":5432/postgres?sslmode=require",
		"postgres://postgres." + NinzoRef + ":p@" + pooler + ":5432/postgres?sslmode=verify-full",
		"postgres://postgres:p@db." + NinzoRef + ".supabase.co:5432/postgres?sslmode=require",
	}
	for _, dsn := range good {
		if err := parse(t, dsn).CheckRemote(); err != nil {
			t.Errorf("%s: %v", dsn, err)
		}
	}
	bad := map[string]string{
		"the other project through the pooler":  "postgres://postgres." + otherRef + ":p@" + pooler + ":5432/postgres?sslmode=require",
		"the other project directly":            "postgres://postgres:p@db." + otherRef + ".supabase.co:5432/postgres?sslmode=require",
		"localhost":                             "postgres://u:p@localhost:5432/d?sslmode=require",
		"an unknown server":                     "postgres://u:p@db.example.com:5432/d?sslmode=require",
		"the transaction pooler":                "postgres://postgres." + NinzoRef + ":p@" + pooler + ":6543/postgres?sslmode=require",
		"no sslmode, which can fall back":       "postgres://postgres." + NinzoRef + ":p@" + pooler + ":5432/postgres",
		"sslmode=prefer":                        "postgres://postgres." + NinzoRef + ":p@" + pooler + ":5432/postgres?sslmode=prefer",
		"sslmode=disable":                       "postgres://postgres." + NinzoRef + ":p@" + pooler + ":5432/postgres?sslmode=disable",
		"Ninzo's user on the other project":     "postgres://postgres." + NinzoRef + ":p@db." + otherRef + ".supabase.co:5432/postgres?sslmode=require",
		"Ninzo's user on an unknown server":     "postgres://postgres." + NinzoRef + ":p@evil.example.com:5432/postgres?sslmode=require",
		"a look-alike pooler host":              "postgres://postgres." + NinzoRef + ":p@pooler.supabase.com.evil.example.com:5432/postgres?sslmode=require",
		"a fallback to the other project":       "postgres://postgres:p@db." + NinzoRef + ".supabase.co,db." + otherRef + ".supabase.co:5432/postgres?sslmode=require",
		"a fallback from the pooler":            "postgres://postgres." + NinzoRef + ":p@" + pooler + ",db." + otherRef + ".supabase.co:5432/postgres?sslmode=require",
		"the other user on Ninzo's direct host": "postgres://postgres." + otherRef + ":p@db." + NinzoRef + ".supabase.co:5432/postgres?sslmode=require",
		"options naming the other project":      "postgres://postgres." + NinzoRef + ":p@" + pooler + ":5432/postgres?sslmode=require&options=reference%3D" + otherRef,
		"a fallback to the transaction pooler":  "postgres://postgres." + NinzoRef + ":p@" + pooler + ":5432," + pooler + ":6543/postgres?sslmode=require",
		"a nested direct host":                  "postgres://postgres:p@db." + NinzoRef + ".evil.supabase.co:5432/postgres?sslmode=require",
	}
	for name, dsn := range bad {
		if err := parse(t, dsn).CheckRemote(); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

func TestErrorsNeverShowThePassword(t *testing.T) {
	const secret = "S3CRET-pass"
	for _, dsn := range []string{
		"postgres://u:123/" + secret + "@host/db",
		"postgres://u:" + secret + "@host:notaport/db",
		"://" + secret,
	} {
		_, err := Parse(dsn)
		if err == nil {
			t.Errorf("%s must be refused", dsn)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error shows the password: %v", err)
		}
	}
	target := parse(t, "postgres://u:"+secret+"@db.example.com:5432/d")
	if got := target.Safe(errors.New("failed with " + secret)); strings.Contains(got, secret) {
		t.Errorf("Safe kept the password: %s", got)
	}
	if err := target.CheckRemote(); err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("CheckRemote: %v", err)
	}
}

func TestLoginsAreKeptApart(t *testing.T) {
	service := "postgres://" + ServiceRole + "." + NinzoRef + ":p@" + pooler + ":5432/postgres?sslmode=require"
	migration := "postgres://" + MigrationRole + "." + NinzoRef + ":p@" + pooler + ":5432/postgres?sslmode=require"
	if err := parse(t, service).CheckService(); err != nil {
		t.Errorf("the service login: %v", err)
	}
	if err := parse(t, service).CheckRemote(); err == nil {
		t.Error("migrations must not run as the service login")
	}
	if err := parse(t, migration).CheckService(); err == nil {
		t.Error("a service must not run as postgres")
	}
	direct := "postgres://" + ServiceRole + ":p@db." + NinzoRef + ".supabase.co:5432/postgres?sslmode=require"
	if err := parse(t, direct).CheckService(); err != nil {
		t.Errorf("a direct service login: %v", err)
	}
	for name, dsn := range map[string]string{
		"the other project":       "postgres://" + ServiceRole + "." + otherRef + ":p@" + pooler + ":5432/postgres?sslmode=require",
		"an unknown login":        "postgres://someone." + NinzoRef + ":p@" + pooler + ":5432/postgres?sslmode=require",
		"a service login on evil": "postgres://" + ServiceRole + "." + NinzoRef + ":p@evil.example.com:5432/postgres?sslmode=require",
		"no TLS":                  "postgres://" + ServiceRole + "." + NinzoRef + ":p@" + pooler + ":5432/postgres",
		"the transaction pooler":  "postgres://" + ServiceRole + "." + NinzoRef + ":p@" + pooler + ":6543/postgres?sslmode=require",
		"connection options":      "postgres://" + ServiceRole + "." + NinzoRef + ":p@" + pooler + ":5432/postgres?sslmode=require&options=reference%3D" + otherRef,
	} {
		if err := parse(t, dsn).CheckService(); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}
