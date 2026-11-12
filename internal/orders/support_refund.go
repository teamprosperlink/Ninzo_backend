package orders

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// refundStatus is how a refund starts: the test payment moved no money, so its refund is done at
// once; a real provider's is pending until the provider says it is processed.
func refundStatus(provider string) string {
	if provider == "test" {
		return "processed"
	}
	return "pending"
}

// RecordSupportRefund records a refund a person asked for in the console (issue 147), in the same
// refunds table as the automatic ones, so the customer's getRefund and order views read it with no
// special case. The caller holds the order and the paid payment locked, and has checked the amount
// fits (the table's trigger refuses a sum over the payment as a last guard).
//
// (payment_id, reason_code) is unique and an order can have several of these, so the code is
// support_refund_<n>, counted under the payment's lock. It returns the refund's id.
func RecordSupportRefund(ctx context.Context, tx pgx.Tx, orderID, paymentID, provider string, amount int, requestedBy, note string) (string, error) {
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) + 1 FROM refunds WHERE payment_id = $1 AND starts_with(reason_code, 'support_refund_')`, paymentID).Scan(&n); err != nil {
		return "", err
	}
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO refunds (order_id, payment_id, reason_code, amount_paise, status, expected_by, requested_by, note)
		VALUES ($1, $2, $3, $4, $5, now() + interval '7 days', $6, $7) RETURNING id::text`,
		orderID, paymentID, fmt.Sprintf("support_refund_%d", n), amount, refundStatus(provider), requestedBy, note).Scan(&id); err != nil {
		return "", err
	}
	// The customer's order view reads refunds, so it refreshes through seq.
	_, err := tx.Exec(ctx, "UPDATE orders SET seq = seq + 1, updated_at = now() WHERE id = $1", orderID)
	return id, err
}
