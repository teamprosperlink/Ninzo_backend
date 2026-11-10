// Package outbox saves events in the same transaction as the change that caused them, and
// delivers them afterwards, so a crash can neither lose an event nor send one for a change that
// never happened. Delivery is at least once, so handlers must be safe to run twice. Order is not
// guaranteed, because a failed event retries later.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/sensitive"
)

// Add saves an event. It takes a transaction, so it commits or fails with the change.
func Add(ctx context.Context, tx pgx.Tx, topic string, payload any) error {
	if topic == "" {
		return errors.New("outbox: a topic is required")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("outbox: %w", err)
	}
	if found := sensitive.Find(raw); len(found) > 0 {
		return fmt.Errorf("outbox: refusing secret fields: %s", strings.Join(found, ", "))
	}
	if _, err := tx.Exec(ctx, "INSERT INTO outbox_events (topic, payload) VALUES ($1, $2)", topic, raw); err != nil {
		return fmt.Errorf("outbox: %w", err)
	}
	return nil
}

// Event is one saved event. Handlers can use ID to spot a repeat.
type Event struct {
	ID       int64
	Topic    string
	Payload  json.RawMessage
	Attempts int
}

// Handler delivers one event. An error, or a panic, retries it later.
type Handler func(context.Context, Event) error

const (
	defaultBatch   = 20
	defaultPoll    = time.Second
	handlerTimeout = 30 * time.Second
	// lease must be longer than handlerTimeout, so a slow delivery is not picked up twice. If a
	// worker dies, the event is picked up again when the lease ends.
	lease      = handlerTimeout + 30*time.Second
	maxBackoff = time.Hour
)

// Worker delivers saved events. No transaction is held during delivery, so several workers can
// run, and a slow outside call never holds a database connection.
type Worker struct {
	Pool   *pgxpool.Pool
	Handle Handler
	Log    *slog.Logger
	Batch  int
	Poll   time.Duration
	// Topics, when set, limits the worker to these topics. Others wait for their own worker.
	Topics []string
}

// Run delivers until ctx ends. A database error is logged and tried again, so a brief outage
// does not stop delivery.
func (w *Worker) Run(ctx context.Context) {
	poll := w.Poll
	if poll == 0 {
		poll = defaultPoll
	}
	for {
		n, err := w.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			w.Log.Error("outbox: claiming events failed", "error", err.Error())
		}
		if n > 0 && err == nil {
			continue // There may be more waiting.
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
	}
}

// RunOnce delivers up to Batch events, and returns how many it claimed. Each is claimed only when
// its turn comes, so its lease covers one delivery and never a queue of slow ones.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	batch := w.Batch
	if batch == 0 {
		batch = defaultBatch
	}
	n := 0
	for n < batch && ctx.Err() == nil {
		e, ok, err := w.claim(ctx)
		if err != nil {
			return n, err
		}
		if !ok {
			break
		}
		n++
		w.deliver(ctx, e)
	}
	return n, nil
}

func (w *Worker) claim(ctx context.Context) (Event, bool, error) {
	var e Event
	err := w.Pool.QueryRow(ctx, `UPDATE outbox_events
		SET attempts = attempts + 1, next_attempt_at = now() + make_interval(secs => $1)
		WHERE id = (
			SELECT id FROM outbox_events
			WHERE delivered_at IS NULL AND next_attempt_at <= now()
				AND (cardinality($2::text[]) = 0 OR topic = ANY($2::text[]))
			ORDER BY next_attempt_at, id
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, topic, payload, attempts`, lease.Seconds(), topics(w.Topics)).Scan(&e.ID, &e.Topic, &e.Payload, &e.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, err
	}
	return e, true, nil
}

func topics(t []string) []string {
	if t == nil {
		return []string{}
	}
	return t
}

func (w *Worker) deliver(ctx context.Context, e Event) {
	err := w.call(ctx, e)
	// Not on ctx: a delivery that worked must be recorded even if shutdown has begun.
	save, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err == nil {
		if _, dbErr := w.Pool.Exec(save, "UPDATE outbox_events SET delivered_at = now() WHERE id = $1", e.ID); dbErr != nil {
			w.Log.Error("outbox: delivered but not recorded, so it will repeat", "id", e.ID, "topic", e.Topic, "error", dbErr.Error())
		}
		return
	}
	// The payload is not logged, because it can hold personal data.
	w.Log.Warn("outbox: delivery failed", "id", e.ID, "topic", e.Topic, "attempts", e.Attempts, "error", err.Error())
	// attempts is the guard: if another worker has since claimed this event, its lease stands.
	if _, dbErr := w.Pool.Exec(save, `UPDATE outbox_events SET next_attempt_at = now() + make_interval(secs => $2)
		WHERE id = $1 AND attempts = $3 AND delivered_at IS NULL`,
		e.ID, backoff(e.Attempts).Seconds(), e.Attempts); dbErr != nil {
		w.Log.Error("outbox: could not schedule a retry, so the lease will", "id", e.ID, "topic", e.Topic, "error", dbErr.Error())
	}
}

func (w *Worker) call(ctx context.Context, e Event) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("handler panicked: %v", v)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, handlerTimeout)
	defer cancel()
	return w.Handle(ctx, e)
}

// backoff doubles from 5 seconds, up to an hour. Events are never dropped: a person must look at
// one that keeps failing.
func backoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > 12 {
		return maxBackoff
	}
	d := 5 * time.Second << (attempts - 1)
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}
