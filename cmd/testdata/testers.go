package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/dbtarget"
)

// extraRiders are more made-up test riders, so several testers can use staging at once without
// taking each other's rider (#175). They are in the range staging keeps for test data.
var extraRiders = []struct{ phone, name string }{
	{"+919800000081", "Test Rider 2"},
	{"+919800000082", "Test Rider 3"},
	{"+919800000083", "Test Rider 4"},
}

// extraScooters is how many more scooters each test zone's node gets, after fleet's two.
const extraScooters = 2

// addTesters adds the extra test riders, approved in the first test zone, and the extra scooters at
// each test zone's node. Running it again changes nothing, and a real person is never touched. Run
// zones and fleet first.
func addTesters(ctx context.Context, tx pgx.Tx, out io.Writer, target dbtarget.Target) error {
	rows, err := tx.Query(ctx, `SELECT z.id::text, z.name, n.id::text FROM zones z
		JOIN staging_nodes n ON n.zone_id = z.id AND n.is_test AND n.name = 'Test node'
		WHERE z.is_test AND z.area IS NOT NULL ORDER BY z.name`)
	if err != nil {
		return errors.New(target.Safe(err))
	}
	type zone struct{ id, name, node string }
	zones, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (zone, error) {
		var z zone
		return z, r.Scan(&z.id, &z.name, &z.node)
	})
	if err != nil {
		return errors.New(target.Safe(err))
	}
	if len(zones) == 0 {
		return errors.New("no test zones with a test node: run zones and fleet first")
	}
	for _, z := range zones {
		for n := 3; n < 3+extraScooters; n++ {
			code := fmt.Sprintf("TEST-SC-%s-%d", strings.ToUpper(strings.Fields(z.name)[0]), n)
			if _, err := tx.Exec(ctx, `INSERT INTO scooters (code, zone_id, node_id, is_test) VALUES ($1, $2, $3, true)
				ON CONFLICT (code) DO NOTHING`, code, z.id, z.node); err != nil {
				return errors.New(target.Safe(err))
			}
		}
		fmt.Fprintf(out, "extra scooters ready in %s\n", z.name)
	}
	first := zones[0].id
	for _, tr := range extraRiders {
		var person string
		var isNew bool
		err := tx.QueryRow(ctx, "SELECT id::text FROM people WHERE phone = $1", tr.phone).Scan(&person)
		if errors.Is(err, pgx.ErrNoRows) {
			isNew = true
			err = tx.QueryRow(ctx, "INSERT INTO people (phone) VALUES ($1) RETURNING id::text", tr.phone).Scan(&person)
		}
		if err != nil {
			return errors.New(target.Safe(err))
		}
		var rider string
		err = tx.QueryRow(ctx, "SELECT id::text FROM riders WHERE person_id = $1", person).Scan(&rider)
		switch {
		case err == nil:
			fmt.Fprintf(out, "kept the test rider %s\n", tr.phone)
			continue
		case !errors.Is(err, pgx.ErrNoRows):
			return errors.New(target.Safe(err))
		case !isNew:
			real, err := realPerson(ctx, tx, person)
			if err != nil {
				return errors.New(target.Safe(err))
			}
			if real {
				return fmt.Errorf("%s belongs to a real person, so test data will not touch it", tr.phone)
			}
		}
		if err := tx.QueryRow(ctx, "INSERT INTO riders (person_id, name, status, zone_id) VALUES ($1, $2, 'active', $3) RETURNING id::text",
			person, tr.name, first).Scan(&rider); err != nil {
			return errors.New(target.Safe(err))
		}
		if _, err := tx.Exec(ctx, "INSERT INTO rider_verifications (rider_id, status, method, reason) VALUES ($1, 'verified', 'manual_upload', 'test data')", rider); err != nil {
			return errors.New(target.Safe(err))
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: audit.ActorSystem, Action: "rider.approved", EntityType: "rider", EntityID: rider, Reason: "test data"}); err != nil {
			return errors.New(target.Safe(err))
		}
		fmt.Fprintf(out, "added the test rider %s\n", tr.phone)
	}
	return nil
}
