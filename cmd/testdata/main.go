// Command testdata adds the test zones for Bengaluru, marked as test data, so they can be found and
// replaced by the real ones. Running it again changes nothing.
//
//	testdata -local|-remote zones
//
// -remote applies to Supabase, so use it only through `task staging:testdata`, which asks first.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/dbtarget"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
)

const usage = "usage: testdata -local|-remote zones"

// testZones are rough boxes around two Bengaluru areas, for testing only.
var testZones = []struct{ name, area string }{
	{"Koramangala (test)", "MULTIPOLYGON(((77.612 12.925, 77.640 12.925, 77.640 12.945, 77.612 12.945, 77.612 12.925)))"},
	{"Indiranagar (test)", "MULTIPOLYGON(((77.632 12.965, 77.650 12.965, 77.650 12.985, 77.632 12.985, 77.632 12.965)))"},
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "testdata:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) != 2 || (args[0] != "-local" && args[0] != "-remote") || args[1] != "zones" {
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
		for _, z := range testZones {
			var id string
			var isTest bool
			err := tx.QueryRow(ctx, "SELECT id::text, is_test FROM zones WHERE name = $1", z.name).Scan(&id, &isTest)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				err = tx.QueryRow(ctx, `INSERT INTO zones (name, city, status, is_test, area)
					VALUES ($1, 'Bengaluru', 'active', true, extensions.ST_GeomFromText($2, 4326)) RETURNING id::text`, z.name, z.area).Scan(&id)
				if err != nil {
					return errors.New(target.Safe(err))
				}
				if err := audit.Record(ctx, tx, audit.Event{
					ActorType: audit.ActorSystem, Action: "zone.created", EntityType: "zone", EntityID: id, Reason: "test data",
				}); err != nil {
					return err
				}
				fmt.Fprintf(out, "added %s\n", z.name)
			case err != nil:
				return errors.New(target.Safe(err))
			case !isTest:
				return fmt.Errorf("a real zone is called %q, so test data will not touch it", z.name)
			default:
				fmt.Fprintf(out, "kept %s\n", z.name)
			}
		}
		return nil
	})
}
