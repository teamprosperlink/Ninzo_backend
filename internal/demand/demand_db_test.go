package demand

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
)

func rows(t *testing.T, pool *pgxpool.Pool, where string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM demand_checks WHERE "+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestACheckIsRecordedOncePerBandAndOutcome(t *testing.T) {
	pool := dbtest.Fresh(t)
	ctx := context.Background()
	at := ist(2026, 11, 13, 19, 5)
	for _, m := range []int{0, 1, 20} { // the app asks again within the band
		if err := Record(ctx, pool, "cust-1", 12.935, 77.625, at.Add(time.Duration(m)*time.Minute), Offered); err != nil {
			t.Fatal(err)
		}
	}
	if n := rows(t, pool, "true"); n != 1 {
		t.Fatalf("%d rows for one customer in one band", n)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(Record(ctx, pool, "cust-1", 12.935, 77.625, at.Add(26*time.Minute), Offered))   // next band
	must(Record(ctx, pool, "cust-1", 12.935, 77.625, at.Add(2*time.Minute), NoCoverage)) // another outcome
	must(Record(ctx, pool, "cust-2", 12.935, 77.625, at, Offered))                       // another customer
	must(Record(ctx, pool, "cust-1", 12.945, 77.625, at, Offered))                       // another cell
	if n := rows(t, pool, "true"); n != 5 {
		t.Fatalf("%d rows, want 5", n)
	}
	// What is stored is the day-salted hash, not the id.
	if n := rows(t, pool, "who = $1 AND day = $2", who("cust-1", SlotOf(at).Day), SlotOf(at).Day.Format("2006-01-02")); n != 4 { // the first band, the next band, no_coverage, another cell
		t.Fatalf("%d of cust-1's rows hold the hash for the day", n)
	}
	if err := Record(ctx, pool, "cust-1", 12.935, 77.625, at, "made_up"); err == nil {
		t.Fatal("an unknown outcome was kept")
	}
	if err := Record(ctx, pool, "", 12.935, 77.625, at, Offered); err == nil {
		t.Fatal("no customer, yet kept")
	}
}

func TestForgetRemovesOnlyThatCustomer(t *testing.T) {
	pool := dbtest.Fresh(t)
	ctx := context.Background()
	now := ist(2026, 11, 13, 19, 0)
	// 200 days back is past the 84-day retention: a prune that has not run yet left it there.
	for _, d := range []int{0, 1, 30, 83, 200} {
		for _, c := range []string{"gone", "stays"} {
			if err := Record(ctx, pool, c, 12.935, 77.625, now.AddDate(0, 0, -d), Offered); err != nil {
				t.Fatal(err)
			}
		}
	}
	n, err := Forget(ctx, pool, "gone")
	if err != nil || n != 5 {
		t.Fatalf("forgot %d: %v", n, err)
	}
	if left := rows(t, pool, "true"); left != 5 {
		t.Fatalf("%d left, want the other customer's 5", left)
	}
}

func TestPruneKeepsTheWindow(t *testing.T) {
	pool := dbtest.Fresh(t)
	ctx := context.Background()
	now := ist(2026, 11, 13, 19, 0)
	for _, d := range []int{0, 84, 85, 200} { // retention 84 days: 84 days ago stays, 85 goes
		if err := Record(ctx, pool, "c", 12.935, 77.625, now.AddDate(0, 0, -d), Offered); err != nil {
			t.Fatal(err)
		}
	}
	n, err := Prune(ctx, pool, now)
	if err != nil || n != 2 {
		t.Fatalf("pruned %d: %v", n, err)
	}
	p := &Pruner{Every: time.Hour}
	if ran, _ := p.Maybe(ctx, pool, now); !ran {
		t.Fatal("first run skipped")
	}
	if ran, _ := p.Maybe(ctx, pool, now.Add(59*time.Minute)); ran {
		t.Fatal("ran again within the hour")
	}
	if ran, _ := p.Maybe(ctx, pool, now.Add(61*time.Minute)); !ran {
		t.Fatal("did not run after the hour")
	}
}

func TestCountsJoinsPaidOrders(t *testing.T) {
	pool := dbtest.Fresh(t)
	ctx := context.Background()
	id := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	at := ist(2026, 11, 13, 19, 5)
	zone := id(`INSERT INTO zones (name, city, status, area) VALUES ('Z', 'Bengaluru', 'active',
		extensions.ST_GeomFromText('MULTIPOLYGON(((77.612 12.925, 77.640 12.925, 77.640 12.945, 77.612 12.945, 77.612 12.925)))', 4326)) RETURNING id::text`)
	node := id("INSERT INTO staging_nodes (zone_id, name, lat, lng, capacity) VALUES ($1, 'N', 12.937, 77.625, 4) RETURNING id::text", zone)
	scooter := id("INSERT INTO scooters (code, zone_id, node_id) VALUES ('SC-1', $1, $2) RETURNING id::text", zone, node)
	order := func(n int, state string, paid any, redacted bool) {
		t.Helper()
		person := id("INSERT INTO people (phone) VALUES ($1) RETURNING id::text", "+91980000060"+string(rune('0'+n)))
		cust := id("INSERT INTO customers (person_id) VALUES ($1) RETURNING id::text", person)
		gate := id("INSERT INTO gates (customer_id, zone_id, label, lat, lng) VALUES ($1, $2, 'G', 12.935, 77.625) RETURNING id::text", cust, zone)
		hist := id("INSERT INTO gate_history (gate_id, version, label, lat, lng, verified, reason) VALUES ($1, 1, 'G', 12.935, 77.625, false, 'test') RETURNING id::text", gate)
		if redacted {
			if _, err := pool.Exec(ctx, "UPDATE gate_history SET redacted_at = now(), label = '(removed)', entry_notes = NULL, lat = round(lat::numeric, 2)::double precision, lng = round(lng::numeric, 2)::double precision WHERE id = $1", hist); err != nil {
				t.Fatalf("cannot mark a gate redacted: %v", err)
			}
		}
		id(`INSERT INTO orders (customer_id, idempotency_key, gate_id, gate_history_id, zone_id, scooter_id, global_settings, state, total_paise, hold_until, paid_at, ended_reason, ended_at)
			VALUES ($1, $2, $3, $4, $5, $6, (SELECT id FROM zone_settings WHERE zone_id IS NULL ORDER BY version DESC LIMIT 1), $7, 9900, now(), $8, $9::text, CASE WHEN $9::text IS NULL THEN NULL ELSE now() END) RETURNING id::text`,
			cust, "k", gate, hist, zone, scooter, state, paid, map[string]any{"TIMED_OUT": "timed_out"}[state])
	}
	order(1, "COMPLETED", at, false)
	order(2, "TIMED_OUT", at, false)        // paid, then failed: still demand
	order(3, "PAYMENT_PENDING", nil, false) // never paid
	order(4, "COMPLETED", at.AddDate(0, 0, -3), false)
	order(5, "COMPLETED", at, true) // the gate was redacted: it no longer says where
	for _, c := range []struct {
		who, outcome string
	}{{"a", Offered}, {"b", NoCoverage}, {"b", SoldOut}, {"c", ZoneOff}, {"d", Offered}, {"d", NoCoverage}} {
		if err := Record(ctx, pool, c.who, 12.935, 77.625, at, c.outcome); err != nil {
			t.Fatal(err)
		}
	}
	counts, err := Counts(ctx, pool, at, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 1 {
		t.Fatalf("%+v", counts)
	}
	c := counts[0]
	// Asked: a, b, d (c asked where the zone was off). Refused: b, d once each.
	if c.Asked != 3 || c.Offered != 2 || c.Refused != 2 || c.Paid != 2 || c.Band != 38 || c.Day.Format("2006-01-02") != "2026-11-13" {
		t.Fatalf("%+v", c)
	}
	if counts, _ = Counts(ctx, pool, at.AddDate(0, 0, -3), at); len(counts) != 2 {
		t.Fatalf("over four days: %+v", counts)
	}
}
