package restock

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
)

// ErrNotExpirable means the request is past the point where it can be ended by hand: it is ready or
// further on, or it has already ended.
var ErrNotExpirable = errors.New("restock: not expirable")

// expirable are the states the console may end: everything before the cassette is ready, and a
// partial fill that still waits on the rider's answer (expireRestock in the console contract).
var expirable = map[string]bool{"confirmed": true, "hub_accepted": true, "packing": true, "partial": true}

// ExpireTx ends a request on the console's order (expireRestock, issue 149), exactly as it ends
// when it expires on its own: the state becomes expired, what was packed goes back to the hub's
// shelf, an open shortfall is refused, the rider's seq rises with a push and a notification, and
// so does the hub's. It locks the request first. A request that is not in an expirable state is
// ErrNotExpirable; an unknown id is ErrNotFound. The caller saves the reply for the
// Idempotency-Key.
func ExpireTx(ctx context.Context, tx pgx.Tx, id string, now time.Time, consoleUser, reason string) error {
	if !idPattern.MatchString(id) {
		return errNotFound
	}
	var rider, state string
	var draft bool
	err := tx.QueryRow(ctx, `SELECT rider_id::text, state, source = 'suggested' AND accepted_by IS NULL AND slot_at IS NULL
		FROM restock_requests WHERE id = $1 FOR UPDATE`, id).Scan(&rider, &state, &draft)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if !expirable[state] {
		return ErrNotExpirable
	}
	if err := endRequest(ctx, tx, id, "expired", "expired_by_console", now); err != nil {
		return err
	}
	if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: consoleUser, Action: "restock.expired_by_console", EntityType: "restock_request", EntityID: id,
		Before: map[string]any{"state": state}, After: map[string]any{"state": "expired"}, Reason: reason}); err != nil {
		return err
	}
	if err := bumpRider(ctx, tx, rider, id); err != nil {
		return err
	}
	if draft {
		// A confirmed request that began as a suggestion looks like the rider's draft to bumpRider,
		// but the hub has seen it since it was confirmed, so the hub's seq rises here (as in CancelTx).
		_, err = tx.Exec(ctx, `INSERT INTO hub_state (hub_id, seq) SELECT hub_id, 1 FROM restock_requests WHERE id = $1
			ON CONFLICT (hub_id) DO UPDATE SET seq = hub_state.seq + 1, updated_at = now()`, id)
	}
	return err
}

// endRequest is what every expiry does to a locked request, whoever starts it: the state moves, the
// lots it packed go back on the hub's shelf, and an open shortfall is refused. The caller audits
// and tells the rider. A given reason replaces the request's own; with none, the request keeps its own.
func endRequest(ctx context.Context, tx pgx.Tx, id, to, reason string, now time.Time) error {
	var why any
	if reason != "" {
		why = reason
	}
	if _, err := tx.Exec(ctx, "UPDATE restock_requests SET state = $2, reason = coalesce($4, reason), state_changed_at = $3, updated_at = now() WHERE id = $1",
		id, to, now, why); err != nil {
		return err
	}
	if err := release(ctx, tx, id, now); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "UPDATE shortfalls SET status = 'refused', decided_at = $2 WHERE request_id = $1 AND status = 'open'", id, now)
	return err
}
