package notify

import (
	"context"

	"github.com/jackc/pgx/v5/pgconn"
)

// Execer is what Purge needs: a pool or a transaction.
type Execer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// Purge deletes notifications older than the retention period and returns how many went.
func Purge(ctx context.Context, db Execer) (int64, error) {
	tag, err := db.Exec(ctx, "DELETE FROM rider_notifications WHERE created_at < now() - make_interval(days => $1)", Retention)
	return tag.RowsAffected(), err
}
