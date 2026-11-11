// Package notify keeps the list of what was pushed to a rider (listRiderNotifications). Every place
// that queues a push.rider event writes the matching row here, so the list and the pushes never
// drift apart. A row never holds a customer's phone or address: callers pass only a title and
// body written for the rider, and the order's id.
package notify

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/outbox"
)

// Retention is how many days a notification is kept.
const Retention = 30

const (
	maxTitle = 120
	maxBody  = 400
)

// Record writes one notification for a rider inside the caller's transaction. kind is an open
// list; the app knows offer, restock, relocation, shift and support. orderID is empty when the
// notification is not about an order.
func Record(ctx context.Context, tx pgx.Tx, rider, kind, title, body, orderID string) error {
	kind = strings.TrimSpace(kind)
	if rider == "" || kind == "" || strings.TrimSpace(title) == "" {
		return errors.New("notify: a rider, kind and title are required")
	}
	var order any
	if orderID != "" {
		order = orderID
	}
	_, err := tx.Exec(ctx, `INSERT INTO rider_notifications (rider_id, kind, title, body, order_id) VALUES ($1, $2, $3, $4, $5)`,
		rider, kind, clip(title, maxTitle), clip(body, maxBody), order)
	return err
}

// Push queues a push.rider event and records the notification, for code that has no push of its
// own to attach the record to.
func Push(ctx context.Context, tx pgx.Tx, rider, kind, title, body, orderID string, extra map[string]any) error {
	payload := map[string]any{"rider_id": rider, "kind": kind}
	for k, v := range extra {
		payload[k] = v
	}
	if err := outbox.Add(ctx, tx, "push.rider", payload); err != nil {
		return err
	}
	return Record(ctx, tx, rider, kind, title, body, orderID)
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}
