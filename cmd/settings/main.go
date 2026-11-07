// Command settings shows and changes the order settings until the admin console exists. Every change
// is a new version, and is recorded in the audit log.
//
//	settings -local|-remote show [zone-id]
//	settings -local|-remote set [-zone zone-id] -reason "why" -by "who" name=value [name=value...]
//
// "name=" clears a zone's override. -remote applies to Supabase, so use it only through
// `task staging:settings`, which asks first.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/dbtarget"
	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
)

const usage = `usage: settings -local|-remote show [zone-id]
       settings -local|-remote set [-zone zone-id] -reason "why" -by "who" name=value...`

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "settings:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) < 2 || (args[0] != "-local" && args[0] != "-remote") || (args[1] != "show" && args[1] != "set") {
		return errors.New(usage)
	}
	remote, command := args[0] == "-remote", args[1]
	var zone string
	var changes []geo.Change
	var reason, by string
	if command == "set" {
		fs := flag.NewFlagSet("set", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.StringVar(&zone, "zone", "", "")
		fs.StringVar(&reason, "reason", "", "")
		fs.StringVar(&by, "by", "", "")
		if err := fs.Parse(args[2:]); err != nil {
			return errors.New(usage)
		}
		var err error
		if changes, err = geo.ParseChanges(fs.Args()); err != nil {
			return err
		}
	} else if len(args) == 3 {
		zone = args[2]
	} else if len(args) > 3 {
		return errors.New(usage)
	}

	dsn := getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is not set")
	}
	target, err := dbtarget.Parse(dsn)
	if err != nil {
		return err
	}
	if remote {
		err = target.CheckRemote()
	} else {
		err = target.CheckLocal()
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "target: host %s, project %q\n", target.Host(), target.Ref())

	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	pool, err := database.Open(ctx, dsn, 2)
	if err != nil {
		return err
	}
	defer pool.Close()
	if command == "set" {
		var version int
		err := database.InTx(ctx, pool, func(tx pgx.Tx) error {
			var err error
			version, err = geo.ChangeSettings(ctx, tx, zone, changes, reason, by)
			return err
		})
		if err != nil {
			return errors.New(target.Safe(err))
		}
		fmt.Fprintf(out, "stored version %d\n", version)
	}
	e, err := geo.LoadSettings(ctx, pool, zone)
	if err != nil {
		return errors.New(target.Safe(err))
	}
	fmt.Fprintf(out, "global version %d, zone version %d\n%s", e.GlobalVersion, e.ZoneVersion, e.Describe())
	return nil
}
