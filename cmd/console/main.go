// Command console makes the first console admin, and resets a console user who is locked out or
// lost their authenticator. Everything else is done in the console. Each change is audited.
//
//	console -local|-remote add-admin -login asha.k -name "Asha K"
//	console -local|-remote reset -login asha.k -reason "lost the authenticator"
//
// It prints a temporary password once; the user changes it and sets up the authenticator at first
// sign-in. -remote applies to Supabase, so use it only through `task staging:console`, which asks first.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/console"
	"github.com/teamprosperlink/Ninzo_backend/internal/dbtarget"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
)

const usage = `usage: console -local|-remote add-admin -login id -name "Name" | reset -login id -reason "why"`

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "console:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) < 2 || (args[0] != "-local" && args[0] != "-remote") || (args[1] != "add-admin" && args[1] != "reset") {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet(args[1], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	login := fs.String("login", "", "")
	name := fs.String("name", "", "")
	reason := fs.String("reason", "", "")
	if err := fs.Parse(args[2:]); err != nil || fs.NArg() != 0 || strings.TrimSpace(*login) == "" {
		return errors.New(usage)
	}
	if (args[1] == "add-admin" && strings.TrimSpace(*name) == "") || (args[1] == "reset" && len(strings.TrimSpace(*reason)) < 3) {
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
	if args[0] == "-remote" {
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
	var temp string
	err = database.InTx(ctx, pool, func(tx pgx.Tx) error {
		if args[1] == "add-admin" {
			_, t, err := console.CreateUser(ctx, tx, "", *login, *name, "admin", nil)
			temp = t
			return err
		}
		var id string
		err := tx.QueryRow(ctx, "SELECT id::text FROM console_users WHERE login_id = $1", strings.ToLower(strings.TrimSpace(*login))).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("no console user %q", *login)
		}
		if err != nil {
			return err
		}
		temp, err = console.ResetUser(ctx, tx, &identity.Sessions{Pool: pool}, "", id, *reason)
		return err
	})
	if err != nil {
		return errors.New(target.Safe(err))
	}
	fmt.Fprintf(out, "temporary password for %s, shown once: %s\n", *login, temp)
	return nil
}
