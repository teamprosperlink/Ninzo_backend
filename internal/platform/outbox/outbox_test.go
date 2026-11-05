package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func add(t *testing.T, pool *pgxpool.Pool, topic string, payload any) {
	t.Helper()
	ctx := context.Background()
	if err := database.InTx(ctx, pool, func(tx pgx.Tx) error { return Add(ctx, tx, topic, payload) }); err != nil {
		t.Fatal(err)
	}
}

func scalar(t *testing.T, pool *pgxpool.Pool, query string, dest any) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), query).Scan(dest); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func TestAddCommitsAndRollsBackWithTheChange(t *testing.T) {
	pool := dbtest.Fresh(t)
	ctx := context.Background()
	failed := errors.New("the change failed")
	err := database.InTx(ctx, pool, func(tx pgx.Tx) error {
		if err := Add(ctx, tx, "order.paid", map[string]any{"id": 1}); err != nil {
			return err
		}
		return failed
	})
	if !errors.Is(err, failed) {
		t.Fatalf("got %v", err)
	}
	var n int
	scalar(t, pool, "SELECT count(*) FROM outbox_events", &n)
	if n != 0 {
		t.Errorf("%d events survived a rollback, so a push could go out for a change that never happened", n)
	}
	add(t, pool, "order.paid", map[string]any{"id": 2})
	scalar(t, pool, "SELECT count(*) FROM outbox_events", &n)
	if n != 1 {
		t.Errorf("%d events after a commit", n)
	}
}

func TestAddRefusesSecrets(t *testing.T) {
	pool := dbtest.Fresh(t)
	ctx := context.Background()
	err := database.InTx(ctx, pool, func(tx pgx.Tx) error {
		return Add(ctx, tx, "t", map[string]any{"data": map[string]any{"otp": "123456"}})
	})
	if err == nil || strings.Contains(err.Error(), "123456") {
		t.Errorf("got %v", err)
	}
	if err := database.InTx(ctx, pool, func(tx pgx.Tx) error { return Add(ctx, tx, "", 1) }); err == nil {
		t.Error("a missing topic must be refused")
	}
}

func TestDeliveryMarksDoneAndDoesNotRepeat(t *testing.T) {
	pool := dbtest.Fresh(t)
	add(t, pool, "a", map[string]any{"n": 1})
	add(t, pool, "b", map[string]any{"n": 2})
	var got []Event
	w := &Worker{Pool: pool, Log: quiet, Handle: func(_ context.Context, e Event) error {
		got = append(got, e)
		return nil
	}}
	ctx := context.Background()
	n, err := w.RunOnce(ctx)
	if err != nil || n != 2 {
		t.Fatalf("n %d, err %v", n, err)
	}
	if got[0].Topic != "a" || got[1].Topic != "b" || got[0].Attempts != 1 {
		t.Errorf("events: %+v", got)
	}
	var payload map[string]int
	if err := json.Unmarshal(got[1].Payload, &payload); err != nil || payload["n"] != 2 {
		t.Errorf("payload: %s, %v", got[1].Payload, err)
	}
	if n, err = w.RunOnce(ctx); err != nil || n != 0 {
		t.Errorf("a second run delivered %d, %v", n, err)
	}
	var undelivered int
	scalar(t, pool, "SELECT count(*) FROM outbox_events WHERE delivered_at IS NULL", &undelivered)
	if undelivered != 0 {
		t.Errorf("%d not marked delivered", undelivered)
	}
}

func TestFailureRetriesLaterAndIsNeverLost(t *testing.T) {
	pool := dbtest.Fresh(t)
	add(t, pool, "a", map[string]any{"phone": "+919876543210"})
	var logs bytes.Buffer
	calls := 0
	w := &Worker{Pool: pool, Log: slog.New(slog.NewTextHandler(&logs, nil)), Handle: func(context.Context, Event) error {
		calls++
		return errors.New("push failed")
	}}
	ctx := context.Background()
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := w.RunOnce(ctx); n != 0 || calls != 1 {
		t.Errorf("it retried at once: claimed %d, calls %d", n, calls)
	}
	var waiting bool
	scalar(t, pool, "SELECT next_attempt_at > now() + interval '1 second' AND next_attempt_at < now() + interval '6 seconds' AND delivered_at IS NULL FROM outbox_events", &waiting)
	if !waiting {
		t.Error("the retry is not scheduled about 5 seconds away")
	}
	if strings.Contains(logs.String(), "9876543210") {
		t.Errorf("the log holds personal data: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "push failed") {
		t.Error("the failure must reach the logs")
	}

	// When the retry is due, it runs, and success ends it.
	if _, err := pool.Exec(ctx, "UPDATE outbox_events SET next_attempt_at = now()"); err != nil {
		t.Fatal(err)
	}
	w.Handle = func(_ context.Context, e Event) error {
		if e.Attempts != 2 {
			t.Errorf("attempts: %d", e.Attempts)
		}
		return nil
	}
	if n, err := w.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("retry: %d, %v", n, err)
	}
	var delivered bool
	scalar(t, pool, "SELECT delivered_at IS NOT NULL FROM outbox_events", &delivered)
	if !delivered {
		t.Error("not delivered after the retry")
	}
}

func TestPanicCountsAsFailure(t *testing.T) {
	pool := dbtest.Fresh(t)
	add(t, pool, "a", 1)
	add(t, pool, "b", 2)
	var delivered []string
	w := &Worker{Pool: pool, Log: quiet, Handle: func(_ context.Context, e Event) error {
		if e.Topic == "a" {
			panic("boom")
		}
		delivered = append(delivered, e.Topic)
		return nil
	}}
	if n, err := w.RunOnce(context.Background()); err != nil || n != 2 {
		t.Fatalf("%d, %v", n, err)
	}
	if len(delivered) != 1 || delivered[0] != "b" {
		t.Errorf("one bad event must not block the next: %v", delivered)
	}
	var pending int
	scalar(t, pool, "SELECT count(*) FROM outbox_events WHERE delivered_at IS NULL AND topic = 'a'", &pending)
	if pending != 1 {
		t.Errorf("the panicking event was lost: %d", pending)
	}
}

func TestWorkersNeverShareAnEvent(t *testing.T) {
	pool := dbtest.Fresh(t)
	const events = 40
	for i := 0; i < events; i++ {
		add(t, pool, "t", i)
	}
	var mu sync.Mutex
	seen := map[int64]int{}
	handle := func(_ context.Context, e Event) error {
		mu.Lock()
		seen[e.ID]++
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		return nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := &Worker{Pool: pool, Log: quiet, Handle: handle, Batch: 5}
			for {
				n, err := w.RunOnce(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()
	if len(seen) != events {
		t.Errorf("delivered %d of %d", len(seen), events)
	}
	for id, times := range seen {
		if times != 1 {
			t.Errorf("event %d delivered %d times", id, times)
		}
	}
}

func TestADeadWorkersEventIsPickedUpAfterTheLease(t *testing.T) {
	pool := dbtest.Fresh(t)
	add(t, pool, "a", 1)
	ctx := context.Background()
	w := &Worker{Pool: pool, Log: quiet, Handle: func(context.Context, Event) error { return nil }}
	// Claim as a worker that dies before delivering.
	if _, ok, err := w.claim(ctx); err != nil || !ok {
		t.Fatalf("claim: %v, %v", ok, err)
	}
	if n, _ := w.RunOnce(ctx); n != 0 {
		t.Error("another worker took an event still under lease")
	}
	if _, err := pool.Exec(ctx, "UPDATE outbox_events SET next_attempt_at = now()"); err != nil {
		t.Fatal(err)
	}
	if n, err := w.RunOnce(ctx); err != nil || n != 1 {
		t.Errorf("after the lease: %d, %v", n, err)
	}
}

func TestAnEventIsClaimedOnlyWhenItsTurnComes(t *testing.T) {
	pool := dbtest.Fresh(t)
	add(t, pool, "a", 1)
	add(t, pool, "b", 2)
	w := &Worker{Pool: pool, Log: quiet, Handle: func(ctx context.Context, e Event) error {
		if e.Topic == "a" {
			// While the first is slow, the second holds no lease, so no other worker can repeat it
			// and its own lease has not started running out.
			var attempts int
			if err := pool.QueryRow(ctx, "SELECT attempts FROM outbox_events WHERE topic = 'b'").Scan(&attempts); err != nil {
				t.Error(err)
			}
			if attempts != 0 {
				t.Errorf("the second event was claimed while the first ran: %d", attempts)
			}
		}
		return nil
	}}
	if n, err := w.RunOnce(context.Background()); err != nil || n != 2 {
		t.Errorf("%d, %v", n, err)
	}
}

func TestAStaleFailureCannotShortenAnotherWorkersLease(t *testing.T) {
	pool := dbtest.Fresh(t)
	add(t, pool, "a", 1)
	ctx := context.Background()
	w := &Worker{Pool: pool, Log: quiet, Handle: func(ctx context.Context, _ Event) error {
		// Another worker claims the event while this slow one is still running.
		if _, err := pool.Exec(ctx, "UPDATE outbox_events SET attempts = attempts + 1, next_attempt_at = now() + interval '50 seconds'"); err != nil {
			t.Error(err)
		}
		return errors.New("failed late")
	}}
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var kept bool
	scalar(t, pool, "SELECT next_attempt_at > now() + interval '40 seconds' FROM outbox_events", &kept)
	if !kept {
		t.Error("a stale failure shortened the other worker's lease, so the event could repeat")
	}
}

func TestLeaseOutlastsAHandler(t *testing.T) {
	if lease <= handlerTimeout {
		t.Errorf("lease %s, handler timeout %s", lease, handlerTimeout)
	}
}

func TestBackoffGrowsAndStops(t *testing.T) {
	for attempts, want := range map[int]time.Duration{
		0: 5 * time.Second, 1: 5 * time.Second, 2: 10 * time.Second, 3: 20 * time.Second,
		10: 2560 * time.Second, 11: maxBackoff, 12: maxBackoff, 1000: maxBackoff,
	} {
		if got := backoff(attempts); got != want {
			t.Errorf("attempt %d: %s, want %s", attempts, got, want)
		}
	}
}

func TestRunStopsAndDelivers(t *testing.T) {
	pool := dbtest.Fresh(t)
	add(t, pool, "a", 1)
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &Worker{Pool: pool, Log: quiet, Poll: 10 * time.Millisecond, Handle: func(context.Context, Event) error {
		calls.Add(1)
		return nil
	}}
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()
	for i := 0; i < 200 && calls.Load() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Errorf("calls: %d", calls.Load())
	}
	add(t, pool, "b", 2)
	for i := 0; i < 200 && calls.Load() < 2; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() != 2 {
		t.Errorf("a later event was not delivered: %d", calls.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestShutdownStillRecordsADeliveryThatWorked(t *testing.T) {
	pool := dbtest.Fresh(t)
	add(t, pool, "a", 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &Worker{Pool: pool, Log: quiet, Handle: func(context.Context, Event) error {
		cancel() // Shutdown begins while the delivery finishes.
		return nil
	}}
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var delivered bool
	scalar(t, pool, "SELECT delivered_at IS NOT NULL FROM outbox_events", &delivered)
	if !delivered {
		t.Error("a delivery that worked was not recorded, so it would repeat")
	}
}
