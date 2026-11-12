package restock

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
)

// ExpireTx (issue 149) ends a request by hand exactly as it ends on its own.

// ended is everything an expiry leaves behind that the rider, the hub and the shelf can see.
type ended struct {
	state                                     string
	liveLots, releasedLots, refusedShortfalls int
	riderSeq, hubSeq, requestSeq              int
	notes, pushes                             int
	noteTitle                                 string
}

func (f *fixture) snapshot(id string) ended {
	f.t.Helper()
	e := ended{state: f.state(id)}
	e.liveLots = f.count("SELECT count(*) FROM restock_lots WHERE request_id = $1 AND released_at IS NULL", id)
	e.releasedLots = f.count("SELECT count(*) FROM restock_lots WHERE request_id = $1 AND released_at IS NOT NULL", id)
	e.refusedShortfalls = f.count("SELECT count(*) FROM shortfalls WHERE request_id = $1 AND status = 'refused'", id)
	e.riderSeq, e.hubSeq = f.riderSeq(), f.hubSeq()
	e.requestSeq = f.count("SELECT seq::int FROM restock_requests WHERE id = $1", id)
	e.notes = f.count("SELECT count(*) FROM rider_notifications WHERE rider_id = $1", f.rider)
	e.pushes = f.count("SELECT count(*) FROM outbox_events WHERE topic = 'push.rider'")
	f.pool.QueryRow(context.Background(), "SELECT coalesce(max(title), '') FROM rider_notifications WHERE rider_id = $1", f.rider).Scan(&e.noteTitle)
	return e
}

// packedPartial is a partial fill that holds a packed lot and an open shortfall, whose cassette is
// long past arrive_by.
func (f *fixture) packedPartial() string {
	f.t.Helper()
	f.withRequests()
	lot := f.hubLot(f.hub, fmt.Sprintf("HUB-X-%d", time.Now().UnixNano()%1e9), f.chai, 10, time.Hour)
	id := f.request(f.chai, 4)
	f.exec("UPDATE restock_requests SET state = 'partial', arrive_by = now() - interval '2 hours', state_changed_at = now() - interval '2 hours' WHERE id = $1", id)
	f.exec("INSERT INTO restock_lots (request_id, lot_id) VALUES ($1, $2)", id, lot)
	f.exec("INSERT INTO shortfalls (request_id, action) VALUES ($1, 'partial')", id)
	return id
}

func (f *fixture) expireByHand(id, reason string) error {
	f.t.Helper()
	return database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		return ExpireTx(context.Background(), tx, id, time.Now(), "00000000-0000-0000-0000-0000000000aa", reason)
	})
}

func TestExpiringByHandLeavesWhatAnExpiryOnItsOwnLeaves(t *testing.T) {
	var byHand, onItsOwn ended
	{
		f := newFixture(t)
		id := f.packedPartial()
		before := f.snapshot(id)
		if err := f.expireByHand(id, "the hub has gone home"); err != nil {
			t.Fatal(err)
		}
		byHand = f.snapshot(id)
		if byHand.riderSeq != before.riderSeq+1 || byHand.hubSeq != before.hubSeq+1 || byHand.requestSeq != before.requestSeq+1 || byHand.pushes != before.pushes+1 || byHand.notes != before.notes+1 {
			t.Errorf("before %+v, after %+v", before, byHand)
		}
		if f.count("SELECT count(*) FROM restock_requests WHERE id = $1 AND reason = 'expired_by_console'", id) != 1 {
			t.Error("the request does not say it was ended by the console")
		}
	}
	{
		f := newFixture(t)
		id := f.packedPartial()
		if err := f.svc.ExpireUncollected(context.Background(), time.Now()); err != nil {
			t.Fatal(err)
		}
		onItsOwn = f.snapshot(id)
	}
	if byHand != onItsOwn {
		t.Errorf("by hand %+v\non its own %+v", byHand, onItsOwn)
	}
	if byHand.state != "expired" || byHand.liveLots != 0 || byHand.releasedLots != 1 || byHand.refusedShortfalls != 1 || byHand.noteTitle != "Restock ended" {
		t.Errorf("%+v", byHand)
	}
}

func TestAConfirmedRequestExpiresByHandAsItDoesWhenNoHubTakesIt(t *testing.T) {
	seen := [2]ended{}
	for i := 0; i < 2; i++ {
		f := newFixture(t)
		f.withRequests()
		id := f.request(f.chai, 4)
		f.exec("UPDATE restock_requests SET state_changed_at = now() - interval '2 hours' WHERE id = $1", id)
		if i == 0 {
			if err := f.expireByHand(id, "no one answers"); err != nil {
				t.Fatal(err)
			}
		} else if err := f.svc.ExpireUnaccepted(context.Background(), time.Now()); err != nil {
			t.Fatal(err)
		}
		seen[i] = f.snapshot(id)
	}
	if seen[0] != seen[1] {
		t.Errorf("by hand %+v\non its own %+v", seen[0], seen[1])
	}
}

func TestExpireTxRefusesWhatIsNotExpirable(t *testing.T) {
	f := newFixture(t)
	f.withRequests()
	id := f.request(f.chai, 4)
	for _, state := range []string{"ready", "picked_up", "received", "closed", "expired", "cancelled"} {
		f.exec("UPDATE restock_requests SET state = $2 WHERE id = $1", id, state)
		if err := f.expireByHand(id, "not allowed here"); !errors.Is(err, ErrNotExpirable) {
			t.Errorf("%s: %v", state, err)
		}
		if f.state(id) != state {
			t.Errorf("%s: the state changed", state)
		}
	}
	for _, id := range []string{"", "nope", "00000000-0000-0000-0000-000000000001"} {
		if err := f.expireByHand(id, "no such request"); !errors.Is(err, ErrNotFound) {
			t.Errorf("%q: %v", id, err)
		}
	}
}

// A second caller waits for the first and then finds the request already ended: the request is
// locked before its state is read.
func TestASecondExpiryWaitsForTheFirstAndIsRefused(t *testing.T) {
	f := newFixture(t)
	id := f.packedPartial()
	ctx := context.Background()
	first, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(ctx)
	if err := ExpireTx(ctx, first, id, time.Now(), "00000000-0000-0000-0000-0000000000aa", "the first"); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		result <- database.InTx(ctx, f.pool, func(tx pgx.Tx) error {
			return ExpireTx(ctx, tx, id, time.Now(), "00000000-0000-0000-0000-0000000000bb", "the second")
		})
	}()
	select {
	case err := <-result:
		t.Fatalf("the second did not wait for the first: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrNotExpirable) {
		t.Errorf("the second found the request ended, so it is not expirable: %v", err)
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'restock.expired_by_console' AND entity_id = $1", id); n != 1 {
		t.Errorf("%d audit rows", n)
	}
}
