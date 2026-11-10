package orders

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
)

// The background passes run inside the customer API for now (decision 0013). Each one takes rows
// with SKIP LOCKED, so two copies of the API never do the same work, and takes the order's row
// lock, the same one a customer request takes.
// maxPerPass caps one pass, so a backlog is worked through over a few passes.
const maxPerPass = 500

// RunJobs runs Tick every so often until ctx ends. A panic in one pass is logged, and never stops
// the API.
func (s *Service) RunJobs(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.safeTick(ctx)
		}
	}
}

func (s *Service) safeTick(ctx context.Context) {
	defer func() {
		if p := recover(); p != nil {
			s.Log.Error("orders: background pass panicked", "panic", fmt.Sprint(p))
		}
	}()
	now := s.clock()
	if _, err := s.Tick(ctx, now); err != nil && ctx.Err() == nil {
		s.Log.Error("orders: background pass failed", "error", err.Error())
	}
	if _, err := s.Dispatch(ctx, now); err != nil && ctx.Err() == nil {
		s.Log.Error("orders: rider pass failed", "error", err.Error())
	}
	if _, err := s.Notify(ctx, now); err != nil && ctx.Err() == nil {
		s.Log.Error("orders: notification pass failed", "error", err.Error())
	}
}

// Tick ends unpaid orders whose hold is over, giving back their stock, and raises seq when an
// order's cancellable changes with time, so apps that watch seq see it. Each order is ended in its
// own transaction, so one order's stock is all a transaction locks, and an order that fails is
// logged and skipped, never blocking the rest. It returns how many orders it changed.
func (s *Service) Tick(ctx context.Context, now time.Time) (int, error) {
	changed := 0
	skip := []string{}
	for i := 0; i < maxPerPass; i++ {
		var id string
		err := database.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
			var state string
			err := tx.QueryRow(ctx, `SELECT id::text, state FROM orders WHERE state IN ('PAYMENT_PENDING', 'PAYMENT_FAILED') AND hold_until <= $1
				AND NOT (id::text = ANY($2::text[])) ORDER BY hold_until, id LIMIT 1 FOR UPDATE SKIP LOCKED`, now, skip).Scan(&id, &state)
			if err != nil {
				return err
			}
			return end(ctx, tx, id, state, "TIMED_OUT", "timed_out", "system", nil, "payment hold ended")
		})
		if errors.Is(err, pgx.ErrNoRows) {
			break
		}
		if err != nil {
			if id == "" || ctx.Err() != nil {
				return changed, err
			}
			s.Log.Error("orders: ending an unpaid order failed", "order_id", id, "error", err.Error())
			skip = append(skip, id)
			continue
		}
		changed++
	}
	for _, mark := range []string{"cancel_until", "wait_deadline"} {
		tag, err := s.Pool.Exec(ctx, `WITH due AS (
				SELECT id FROM orders WHERE state IN ('PAYMENT_SECURED', 'OFFERED', 'RIDER_ACCEPTED', 'PACKED')
					AND NOT `+mark+`_seen AND `+mark+` <= $1 FOR UPDATE SKIP LOCKED)
			UPDATE orders o SET seq = o.seq + 1, `+mark+`_seen = true, updated_at = now() FROM due WHERE o.id = due.id`, now)
		if err != nil {
			return changed, err
		}
		changed += int(tag.RowsAffected())
	}
	return changed, nil
}

// sweep works through due orders one at a time, each in its own transaction. pick selects one row
// id, with $1 the ids to skip and $2 now, locked with SKIP LOCKED. A row that fails is logged and
// skipped, and a row left unchanged is skipped quietly.
func (s *Service) sweep(ctx context.Context, what, pick string, now time.Time, act func(context.Context, pgx.Tx, string) error) (int, error) {
	changed := 0
	skip := []string{}
	for i := 0; i < maxPerPass; i++ {
		var id string
		err := database.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, pick, skip, now).Scan(&id); err != nil {
				return err
			}
			return act(ctx, tx, id)
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows) && id == "":
			return changed, nil
		case errors.Is(err, errUnchanged):
			skip = append(skip, id)
		case err != nil:
			if id == "" || ctx.Err() != nil {
				return changed, err
			}
			s.Log.Error("orders: "+what+" failed", "id", id, "error", err.Error())
			skip = append(skip, id)
		default:
			changed++
		}
	}
	return changed, nil
}
