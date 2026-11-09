package orders

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
)

// The background passes run inside the customer API for now (decision 0013). Each one takes rows
// with SKIP LOCKED, so two copies of the API never do the same work, and takes the order's row
// lock, the same one a customer request takes.
const batch = 50

// RunJobs runs Tick every so often until ctx ends.
func (s *Service) RunJobs(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.Tick(ctx, s.clock()); err != nil && ctx.Err() == nil {
				s.Log.Error("orders: background pass failed", "error", err.Error())
			}
		}
	}
}

// Tick ends unpaid orders whose hold is over, giving back their stock, and raises seq when an
// order's cancellable changes with time, so apps that watch seq see it. It returns how many
// orders it changed.
func (s *Service) Tick(ctx context.Context, now time.Time) (int, error) {
	changed := 0
	for {
		n := 0
		err := database.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT id::text, state FROM orders WHERE state IN ('PAYMENT_PENDING', 'PAYMENT_FAILED') AND hold_until <= $1
				ORDER BY hold_until, id LIMIT $2 FOR UPDATE SKIP LOCKED`, now, batch)
			if err != nil {
				return err
			}
			type due struct{ id, state string }
			all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (due, error) {
				var d due
				return d, r.Scan(&d.id, &d.state)
			})
			if err != nil {
				return err
			}
			for _, d := range all {
				if err := end(ctx, tx, d.id, d.state, "TIMED_OUT", "timed_out", "system", nil, "payment hold ended"); err != nil {
					return err
				}
			}
			n = len(all)
			return nil
		})
		if err != nil {
			return changed, err
		}
		changed += n
		if n < batch {
			break
		}
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
