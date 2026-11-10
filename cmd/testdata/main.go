// Command testdata adds the test zones for Bengaluru, marked as test data, so they can be found and
// replaced by the real ones. Running it again changes nothing.
//
//	testdata -local|-remote zones|menu|fleet|rider
//
// fleet adds a staging node and two scooters in each test zone, and ten lots of ten units of each
// test item. Running it again gives unused lots a new expiry. Run zones and menu first.
//
// rider makes the test rider, +919800000090, approved and verified in the first test zone, so the
// rider app can be tried on staging with the test code. Run zones first.
//
// -remote applies to Supabase, so use it only through `task staging:testdata`, which asks first.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/dbtarget"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
)

const usage = "usage: testdata -local|-remote zones|menu|fleet|hubs|rider|staff"

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
	if len(args) != 2 || (args[0] != "-local" && args[0] != "-remote") || (args[1] != "zones" && args[1] != "menu" && args[1] != "fleet" && args[1] != "hubs" && args[1] != "rider" && args[1] != "staff") {
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
	if args[1] == "staff" {
		return database.InTx(ctx, pool, func(tx pgx.Tx) error { return addStaff(ctx, tx, out, target) })
	}
	if args[1] == "rider" {
		return database.InTx(ctx, pool, func(tx pgx.Tx) error { return addRider(ctx, tx, out, target) })
	}
	if args[1] == "hubs" {
		return database.InTx(ctx, pool, func(tx pgx.Tx) error { return addHubs(ctx, tx, out, target) })
	}
	if args[1] == "fleet" {
		return database.InTx(ctx, pool, func(tx pgx.Tx) error { return addFleet(ctx, tx, out, target) })
	}
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

// addFleet adds what a test rider needs to start a shift and load stock. Codes make it idempotent.
func addFleet(ctx context.Context, tx pgx.Tx, out io.Writer, target dbtarget.Target) error {
	rows, err := tx.Query(ctx, `SELECT id::text, name, extensions.ST_Y(extensions.ST_Centroid(area)), extensions.ST_X(extensions.ST_Centroid(area))
		FROM zones WHERE is_test AND area IS NOT NULL ORDER BY name`)
	if err != nil {
		return errors.New(target.Safe(err))
	}
	type zone struct {
		id, name string
		lat, lng float64
	}
	zones, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (zone, error) {
		var z zone
		return z, r.Scan(&z.id, &z.name, &z.lat, &z.lng)
	})
	if err != nil {
		return err
	}
	if len(zones) == 0 {
		return errors.New("no test zones: run zones first")
	}
	for _, z := range zones {
		var node string
		err := tx.QueryRow(ctx, `INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity, is_test) VALUES ($1, 'Test node', $2, $3, 4, true)
			ON CONFLICT (zone_id, name) DO UPDATE SET capacity = staging_nodes.capacity RETURNING id::text`, z.id, z.lat, z.lng).Scan(&node)
		if err != nil {
			return errors.New(target.Safe(err))
		}
		for n := 1; n <= 2; n++ {
			code := fmt.Sprintf("TEST-SC-%s-%d", strings.ToUpper(strings.Fields(z.name)[0]), n)
			if _, err := tx.Exec(ctx, `INSERT INTO scooters (code, zone_id, node_id, is_test) VALUES ($1, $2, $3, true)
				ON CONFLICT (code) DO NOTHING`, code, z.id, node); err != nil {
				return errors.New(target.Safe(err))
			}
		}
		fmt.Fprintf(out, "fleet ready in %s\n", z.name)
	}
	items, err := tx.Query(ctx, "SELECT id::text FROM menu_items WHERE is_test ORDER BY sort_order, name")
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(items, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return errors.New("no test menu: run menu first")
	}
	for i, id := range ids {
		for n := 1; n <= 10; n++ {
			code := fmt.Sprintf("TEST-LOT-%d%02d", i+1, n)
			if _, err := tx.Exec(ctx, `INSERT INTO lots (code, item_id, qty, expires_at, is_test) VALUES ($1, $2, 10, now() + interval '30 days', true)
				ON CONFLICT (code) DO UPDATE SET expires_at = now() + interval '30 days'
				WHERE lots.is_test AND NOT EXISTS (SELECT 1 FROM scooter_stock WHERE lot_id = lots.id)`, code, id); err != nil {
				return errors.New(target.Safe(err))
			}
		}
	}
	fmt.Fprintf(out, "lots ready: TEST-LOT-101 to TEST-LOT-%d10\n", len(ids))
	return nil
}

// testRiderPhone is a made-up number, in the range staging keeps for test data.
const testRiderPhone = "+919800000090"

// addRider makes the test rider once. A real person with that number is never touched.
func addRider(ctx context.Context, tx pgx.Tx, out io.Writer, target dbtarget.Target) error {
	var zone string
	if err := tx.QueryRow(ctx, "SELECT id::text FROM zones WHERE is_test AND area IS NOT NULL ORDER BY name LIMIT 1").Scan(&zone); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("no test zones: run zones first")
		}
		return errors.New(target.Safe(err))
	}
	var person string
	err := tx.QueryRow(ctx, "SELECT id::text FROM people WHERE phone = $1", testRiderPhone).Scan(&person)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, "INSERT INTO people (phone) VALUES ($1) RETURNING id::text", testRiderPhone).Scan(&person)
	}
	if err != nil {
		return errors.New(target.Safe(err))
	}
	var rider string
	err = tx.QueryRow(ctx, "SELECT id::text FROM riders WHERE person_id = $1", person).Scan(&rider)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, "INSERT INTO riders (person_id, name, status, zone_id) VALUES ($1, 'Test Rider', 'active', $2) RETURNING id::text", person, zone).Scan(&rider)
		if err == nil {
			if _, err = tx.Exec(ctx, "INSERT INTO rider_verifications (rider_id, status, method, reason) VALUES ($1, 'verified', 'manual_upload', 'test data')", rider); err == nil {
				err = audit.Record(ctx, tx, audit.Event{ActorType: audit.ActorSystem, Action: "rider.approved", EntityType: "rider", EntityID: rider, Reason: "test data"})
			}
		}
		if err != nil {
			return errors.New(target.Safe(err))
		}
		fmt.Fprintf(out, "added the test rider %s\n", testRiderPhone)
		return nil
	}
	if err != nil {
		return errors.New(target.Safe(err))
	}
	fmt.Fprintf(out, "kept the test rider %s\n", testRiderPhone)
	return nil
}

// addHubs adds a test hub in each test zone, with lots waiting there for restocks. Real hubs come
// from the admin in the console (decision 0014).
func addHubs(ctx context.Context, tx pgx.Tx, out io.Writer, target dbtarget.Target) error {
	rows, err := tx.Query(ctx, `SELECT id::text, name, extensions.ST_Y(extensions.ST_Centroid(area)), extensions.ST_X(extensions.ST_Centroid(area))
		FROM zones WHERE is_test AND area IS NOT NULL ORDER BY name`)
	if err != nil {
		return errors.New(target.Safe(err))
	}
	type zone struct {
		id, name string
		lat, lng float64
	}
	zones, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (zone, error) {
		var z zone
		return z, r.Scan(&z.id, &z.name, &z.lat, &z.lng)
	})
	if err != nil {
		return err
	}
	if len(zones) == 0 {
		return errors.New("no test zones: run zones first")
	}
	items, err := tx.Query(ctx, "SELECT id::text FROM menu_items WHERE is_test ORDER BY sort_order, name")
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(items, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return errors.New("no test menu: run menu first")
	}
	for zi, z := range zones {
		var hub string
		err := tx.QueryRow(ctx, `INSERT INTO hubs (zone_id, name, lat, lng, is_test) VALUES ($1, 'Test hub', $2, $3, true)
			ON CONFLICT (zone_id, name) DO UPDATE SET status = 'active' WHERE hubs.is_test RETURNING id::text`, z.id, z.lat+0.002, z.lng+0.002).Scan(&hub)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("a real hub in %s is called Test hub, so test data will not touch it", z.name)
		}
		if err != nil {
			return errors.New(target.Safe(err))
		}
		for i, id := range ids {
			for n := 1; n <= 5; n++ {
				code := fmt.Sprintf("TEST-HUB-%d-%d%02d", zi+1, i+1, n)
				if _, err := tx.Exec(ctx, `INSERT INTO lots (code, item_id, qty, expires_at, hub_id, is_test) VALUES ($1, $2, 10, now() + interval '30 days', $3, true)
					ON CONFLICT (code) DO UPDATE SET expires_at = now() + interval '30 days'
					WHERE lots.is_test AND NOT EXISTS (SELECT 1 FROM scooter_stock WHERE lot_id = lots.id)
						AND NOT EXISTS (SELECT 1 FROM restock_lots WHERE lot_id = lots.id AND released_at IS NULL)`, code, id, hub); err != nil {
					return errors.New(target.Safe(err))
				}
			}
		}
		fmt.Fprintf(out, "test hub ready in %s\n", z.name)
	}
	return nil
}

// testStaff are a hub lead and a hub staff member at the first test zone's test hub, so the partner
// app can be tried on staging with the test code.
var testStaff = []struct{ phone, name, role string }{
	{"+919800000091", "Test Hub Lead", "hub_lead"},
	{"+919800000092", "Test Hub Staff", "hub_staff"},
}

// addStaff makes the test hub staff once. A real person with one of these numbers is never touched.
func addStaff(ctx context.Context, tx pgx.Tx, out io.Writer, target dbtarget.Target) error {
	var hub string
	err := tx.QueryRow(ctx, `SELECT h.id::text FROM hubs h JOIN zones z ON z.id = h.zone_id WHERE h.is_test AND z.is_test ORDER BY z.name LIMIT 1`).Scan(&hub)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("no test hub: run hubs first")
	}
	if err != nil {
		return errors.New(target.Safe(err))
	}
	for _, st := range testStaff {
		var person string
		var isNew bool
		err := tx.QueryRow(ctx, "SELECT id::text FROM people WHERE phone = $1", st.phone).Scan(&person)
		if errors.Is(err, pgx.ErrNoRows) {
			isNew = true
			err = tx.QueryRow(ctx, "INSERT INTO people (phone) VALUES ($1) RETURNING id::text", st.phone).Scan(&person)
		}
		if err != nil {
			return errors.New(target.Safe(err))
		}
		var existing string
		err = tx.QueryRow(ctx, "SELECT id::text FROM hub_staff WHERE person_id = $1", person).Scan(&existing)
		switch {
		case err == nil:
			fmt.Fprintf(out, "kept the test %s %s\n", st.role, st.phone)
			continue
		case !errors.Is(err, pgx.ErrNoRows):
			return errors.New(target.Safe(err))
		case !isNew:
			real, err := realPerson(ctx, tx, person)
			if err != nil {
				return errors.New(target.Safe(err))
			}
			if real {
				return fmt.Errorf("%s belongs to a real person, so test data will not touch it", st.phone)
			}
		}
		var id string
		if err := tx.QueryRow(ctx, "INSERT INTO hub_staff (person_id, hub_id, role, name, is_test) VALUES ($1, $2, $3, $4, true) RETURNING id::text",
			person, hub, st.role, st.name).Scan(&id); err != nil {
			return errors.New(target.Safe(err))
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: audit.ActorSystem, Action: "staff.added", EntityType: "hub_staff", EntityID: id, Reason: "test data"}); err != nil {
			return errors.New(target.Safe(err))
		}
		fmt.Fprintf(out, "added the test %s %s\n", st.role, st.phone)
	}
	return nil
}

// realPerson is true when a person is used by anything but test data: a customer, a rider, or a
// person who is not active.
func realPerson(ctx context.Context, tx pgx.Tx, person string) (bool, error) {
	var real bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM customers WHERE person_id = $1) OR EXISTS (SELECT 1 FROM riders WHERE person_id = $1)
		OR EXISTS (SELECT 1 FROM people WHERE id = $1 AND status <> 'active')`, person).Scan(&real)
	return real, err
}
