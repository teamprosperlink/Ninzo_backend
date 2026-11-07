// Command riders approves a rider until the admin console exists (v0.9.0). Each approval is audited.
//
//	riders -local|-remote approve -phone +91... -zone zone-id -reason "why" -by "who"
//
// It makes the rider active in a zone, and records their documents as checked by hand. -remote
// applies to Supabase, so use it only through `task staging:riders`, which asks first.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/dbtarget"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
)

const usage = `usage: riders -local|-remote approve -phone +91... -zone zone-id -reason "why" -by "who"`

var phonePattern = regexp.MustCompile(`^\+91[6-9][0-9]{9}$`)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "riders:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) < 2 || (args[0] != "-local" && args[0] != "-remote") || args[1] != "approve" {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	phone := fs.String("phone", "", "")
	zone := fs.String("zone", "", "")
	reason := fs.String("reason", "", "")
	by := fs.String("by", "", "")
	if err := fs.Parse(args[2:]); err != nil || fs.NArg() != 0 {
		return errors.New(usage)
	}
	if !phonePattern.MatchString(*phone) || *zone == "" || len(strings.TrimSpace(*reason)) < 3 || strings.TrimSpace(*by) == "" {
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
	return database.InTx(ctx, pool, func(tx pgx.Tx) error {
		var rider, status string
		err := tx.QueryRow(ctx, `SELECT r.id::text, r.status FROM riders r JOIN people p ON p.id = r.person_id
			WHERE p.phone = $1 FOR UPDATE OF r`, *phone).Scan(&rider, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("no rider has that phone. They must apply first")
		}
		if err != nil {
			return errors.New(target.Safe(err))
		}
		if status == "suspended" {
			return errors.New("this rider is suspended, so approving them would undo that")
		}
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM zones WHERE id::text = $1)", *zone).Scan(&exists); err != nil || !exists {
			return errors.New("no such zone")
		}
		if _, err := tx.Exec(ctx, "UPDATE riders SET status = 'active', zone_id = $2, updated_at = now() WHERE id = $1", rider, *zone); err != nil {
			return errors.New(target.Safe(err))
		}
		if _, err := tx.Exec(ctx, `INSERT INTO rider_verifications (rider_id, status, method, reason) VALUES ($1, 'verified', 'manual_upload', $2)`,
			rider, strings.TrimSpace(*by)+": "+strings.TrimSpace(*reason)); err != nil {
			return errors.New(target.Safe(err))
		}
		if err := audit.Record(ctx, tx, audit.Event{
			ActorType: audit.ActorSystem, Action: "rider.approved", EntityType: "rider", EntityID: rider,
			Before: map[string]any{"status": status}, After: map[string]any{"status": "active", "zone_id": *zone},
			Reason: strings.TrimSpace(*by) + ": " + strings.TrimSpace(*reason),
		}); err != nil {
			return err
		}
		fmt.Fprintf(out, "approved rider %s in zone %s\n", rider, *zone)
		return nil
	})
}
