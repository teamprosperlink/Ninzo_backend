// Command testdata adds the test zones for Bengaluru, marked as test data, so they can be found and
// replaced by the real ones. Running it again changes nothing.
//
//	testdata -local|-remote zones|menu
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

const usage = "usage: testdata -local|-remote zones|menu"

// testItems are placeholders until the owner gives the real menu.
var testItems = []struct {
	name, description string
	price, order      int
	allergens         []string
}{
	{"Veg Burger (test)", "A test item until the real menu.", 14900, 1, []string{"gluten", "milk"}},
	{"Masala Chai (test)", "A test item until the real menu.", 4900, 2, []string{"milk"}},
	{"Samosa (test)", "A test item until the real menu.", 3900, 3, []string{"gluten"}},
}

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
	if len(args) != 2 || (args[0] != "-local" && args[0] != "-remote") || (args[1] != "zones" && args[1] != "menu") {
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
	if args[1] == "menu" {
		return database.InTx(ctx, pool, func(tx pgx.Tx) error { return addMenu(ctx, tx, out, target) })
	}
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
				// Put a test zone back as it should be, if someone paused it or cleared its area.
				tag, err := tx.Exec(ctx, `UPDATE zones SET status = 'active', area = extensions.ST_GeomFromText($2, 4326), updated_at = now()
					WHERE id = $1 AND (status <> 'active' OR area IS NULL OR NOT extensions.ST_Equals(area, extensions.ST_GeomFromText($2, 4326)))`, id, z.area)
				if err != nil {
					return errors.New(target.Safe(err))
				}
				if tag.RowsAffected() > 0 {
					fmt.Fprintf(out, "restored %s\n", z.name)
				} else {
					fmt.Fprintf(out, "kept %s\n", z.name)
				}
			}
		}
		return nil
	})
}

// addMenu adds the test items once. A real item with the same name is never touched.
func addMenu(ctx context.Context, tx pgx.Tx, out io.Writer, target dbtarget.Target) error {
	if _, err := tx.Exec(ctx, "SELECT set_config('ninzo.price_reason', 'test data', true)"); err != nil {
		return err
	}
	for _, it := range testItems {
		var id string
		var isTest bool
		err := tx.QueryRow(ctx, "SELECT id::text, is_test FROM menu_items WHERE name = $1", it.name).Scan(&id, &isTest)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			err = tx.QueryRow(ctx, `INSERT INTO menu_items (name, description, price_paise, allergens, status, sort_order, is_test)
				VALUES ($1, $2, $3, $4, 'active', $5, true) RETURNING id::text`, it.name, it.description, it.price, it.allergens, it.order).Scan(&id)
			if err != nil {
				return errors.New(target.Safe(err))
			}
			if err := audit.Record(ctx, tx, audit.Event{
				ActorType: audit.ActorSystem, Action: "menu_item.created", EntityType: "menu_item", EntityID: id, Reason: "test data",
			}); err != nil {
				return err
			}
			fmt.Fprintf(out, "added %s\n", it.name)
		case err != nil:
			return errors.New(target.Safe(err))
		case !isTest:
			return fmt.Errorf("a real item is called %q, so test data will not touch it", it.name)
		default:
			fmt.Fprintf(out, "kept %s\n", it.name)
		}
	}
	return nil
}
