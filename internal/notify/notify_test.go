package notify

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
)

func rider(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var person, id string
	if err := pool.QueryRow(context.Background(), "INSERT INTO people (phone) VALUES ('+919800000901') RETURNING id::text").Scan(&person); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), "INSERT INTO riders (person_id) VALUES ($1) RETURNING id::text", person).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRecordKeepsARowAndRefusesAnEmptyOne(t *testing.T) {
	pool := dbtest.Fresh(t)
	id := rider(t, pool)
	ctx := context.Background()
	err := database.InTx(ctx, pool, func(tx pgx.Tx) error {
		if err := Record(ctx, tx, id, "restock", "Restock ready", "Collect it.", ""); err != nil {
			return err
		}
		// Overlong text is cut to the column's limits, never refused: a push must not fail a change.
		return Record(ctx, tx, id, "shift", strings.Repeat("t", 500), strings.Repeat("b", 900), "")
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, "SELECT count(*) FROM rider_notifications WHERE rider_id = $1 AND order_id IS NULL", id); n != 2 {
		t.Errorf("%d rows", n)
	}
	if n := count(t, pool, "SELECT count(*) FROM rider_notifications WHERE char_length(title) <= 120 AND char_length(body) <= 400"); n != 2 {
		t.Error("long text was not cut")
	}
	for _, bad := range [][3]string{{"", "k", "t"}, {id, "", "t"}, {id, "k", "  "}} {
		err := database.InTx(ctx, pool, func(tx pgx.Tx) error { return Record(ctx, tx, bad[0], bad[1], bad[2], "b", "") })
		if err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestPushQueuesTheEventAndTheRowTogether(t *testing.T) {
	pool := dbtest.Fresh(t)
	id := rider(t, pool)
	ctx := context.Background()
	if err := database.InTx(ctx, pool, func(tx pgx.Tx) error {
		return Push(ctx, tx, id, "shift", "Your break is over", "Offers can come again.", "", map[string]any{"shift_id": "s1"})
	}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, "SELECT count(*) FROM outbox_events WHERE topic = 'push.rider' AND payload->>'rider_id' = $1 AND payload->>'kind' = 'shift' AND payload->>'shift_id' = 's1'", id); n != 1 {
		t.Errorf("%d events", n)
	}
	if n := count(t, pool, "SELECT count(*) FROM rider_notifications WHERE rider_id = $1 AND kind = 'shift'", id); n != 1 {
		t.Errorf("%d rows", n)
	}
	// A failed change leaves neither.
	err := database.InTx(ctx, pool, func(tx pgx.Tx) error {
		if err := Push(ctx, tx, id, "shift", "Gone", "Gone.", "", nil); err != nil {
			return err
		}
		return context.Canceled
	})
	if err == nil || count(t, pool, "SELECT count(*) FROM rider_notifications WHERE title = 'Gone'") != 0 || count(t, pool, "SELECT count(*) FROM outbox_events") != 1 {
		t.Error("a rolled-back change left a push or a row")
	}
}

func TestPurgeDeletesWhatIsOlderThanThirtyDays(t *testing.T) {
	pool := dbtest.Fresh(t)
	id := rider(t, pool)
	ctx := context.Background()
	for _, age := range []string{"1 day", "29 days", "31 days", "90 days"} {
		if _, err := pool.Exec(ctx, "INSERT INTO rider_notifications (rider_id, kind, title, body, created_at) VALUES ($1, 'shift', 'n', 'b', now() - $2::interval)", id, age); err != nil {
			t.Fatal(err)
		}
	}
	n, err := Purge(ctx, pool)
	if err != nil || n != 2 {
		t.Fatalf("purged %d, %v", n, err)
	}
	if left := count(t, pool, "SELECT count(*) FROM rider_notifications"); left != 2 {
		t.Errorf("%d left", left)
	}
}
