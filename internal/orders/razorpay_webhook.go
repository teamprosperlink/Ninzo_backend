package orders

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/outbox"
)

// maxWebhookBody bounds a webhook body; Razorpay's are a few KB.
const maxWebhookBody = 256 << 10

// refundAlertAfter is how many failed sends of one refund raise a support alert (about a day and
// a half of hourly tries after the first few); it keeps being retried after that.
const refundAlertAfter = 40

// MountRazorpayWebhook adds POST /v1/webhooks/razorpay (issue 237; the router is already under /v1).
// It has no user sign-in: the signature is the authentication, checked before anything is read or
// written.
func (s *Service) MountRazorpayWebhook(r chi.Router, p *Razorpay) {
	r.Post("/webhooks/razorpay", func(w http.ResponseWriter, req *http.Request) {
		s.razorpayWebhook(w, req, p)
	})
}

// validSignature checks X-Razorpay-Signature: hex HMAC-SHA256 of the raw body with the webhook
// secret, compared in constant time.
func validSignature(body []byte, signature, secret string) bool {
	if signature == "" || secret == "" {
		return false
	}
	want, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

// rzEvent is the part of a Razorpay webhook we read.
type rzEvent struct {
	Event   string `json:"event"`
	Payload struct {
		Payment struct {
			Entity struct {
				ID      string `json:"id"`
				OrderID string `json:"order_id"`
				Amount  int    `json:"amount"`
				Status  string `json:"status"`
			} `json:"entity"`
		} `json:"payment"`
		Refund struct {
			Entity rzRefund `json:"entity"`
		} `json:"refund"`
	} `json:"payload"`
}

func (s *Service) razorpayWebhook(w http.ResponseWriter, req *http.Request, p *Razorpay) {
	body, err := io.ReadAll(io.LimitReader(req.Body, maxWebhookBody+1))
	if err != nil || len(body) > maxWebhookBody {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if !validSignature(body, req.Header.Get("X-Razorpay-Signature"), p.webhookSecret) {
		s.Log.WarnContext(req.Context(), "razorpay webhook: bad signature")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	eventID := req.Header.Get("X-Razorpay-Event-Id")
	var ev rzEvent
	if eventID == "" || len(eventID) > 100 || json.Unmarshal(body, &ev) != nil || ev.Event == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	now := s.clock()
	err = database.InTx(req.Context(), s.Pool, func(tx pgx.Tx) error {
		// Razorpay sends an event more than once; the first one recorded is the one acted on.
		tag, err := tx.Exec(req.Context(), "INSERT INTO webhook_events (event_id, event) VALUES ($1, $2) ON CONFLICT DO NOTHING", eventID, ev.Event)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		return s.applyRazorpay(req.Context(), tx, ev, now)
	})
	if err != nil {
		// Nothing was kept, so Razorpay's retry will try again.
		s.Log.ErrorContext(req.Context(), "razorpay webhook: not applied", "event", ev.Event, "error", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// alert raises a support alert, which opens a support incident (migration 0044), for money that
// needs a person: a wrong amount, a second capture, a refund that keeps failing.
func alert(ctx context.Context, tx pgx.Tx, orderID, reason string, details map[string]any) error {
	payload := map[string]any{"order_id": orderID, "reason": reason}
	for k, v := range details {
		payload[k] = v
	}
	return outbox.Add(ctx, tx, "support.alert", payload)
}

// refunded tells the customer their refund went back (issue 252), in the transaction that marks it
// processed, so the push is queued once, whichever of the webhook or the sender gets there first.
func refunded(ctx context.Context, tx pgx.Tx, refundID, orderID string) error {
	var customer string
	if err := tx.QueryRow(ctx, "SELECT customer_id::text FROM orders WHERE id = $1", orderID).Scan(&customer); err != nil {
		return err
	}
	return outbox.Add(ctx, tx, "push.customer", map[string]any{"customer_id": customer, "order_id": orderID, "refund_id": refundID, "stage": "refund_processed"})
}

func (s *Service) applyRazorpay(ctx context.Context, tx pgx.Tx, ev rzEvent, now time.Time) error {
	switch ev.Event {
	case "payment.captured", "order.paid", "payment.failed":
		pay := ev.Payload.Payment.Entity
		if pay.OrderID == "" || pay.ID == "" {
			return nil // Not one of ours: a payment with no order.
		}
		var orderID string
		err := tx.QueryRow(ctx, "SELECT order_id::text FROM payments WHERE provider = 'razorpay' AND provider_order_id = $1", pay.OrderID).Scan(&orderID)
		if errors.Is(err, pgx.ErrNoRows) {
			s.Log.WarnContext(ctx, "razorpay webhook: unknown order", "provider_order_id", pay.OrderID)
			return nil
		}
		if err != nil {
			return err
		}
		// The order first, then its payment: the same order as cancel and refunds, so they never
		// wait on each other in a circle.
		if _, err := tx.Exec(ctx, "SELECT 1 FROM orders WHERE id = $1 FOR UPDATE", orderID); err != nil {
			return err
		}
		var paymentRow, status string
		var amount int
		var known *string
		if err := tx.QueryRow(ctx, `SELECT id::text, status, amount_paise, provider_payment_id FROM payments
			WHERE provider = 'razorpay' AND provider_order_id = $1 FOR UPDATE`, pay.OrderID).Scan(&paymentRow, &status, &amount, &known); err != nil {
			return err
		}
		if ev.Event == "payment.failed" {
			// One failed attempt; the customer may still pay on the same Razorpay order.
			return Confirm(ctx, tx, orderID, pay.OrderID, false, now)
		}
		if pay.Amount != amount {
			s.Log.ErrorContext(ctx, "razorpay webhook: amount mismatch", "order_id", orderID, "paid", pay.Amount, "expected", amount)
			return alert(ctx, tx, orderID, "payment_amount_mismatch", map[string]any{"razorpay_payment_id": pay.ID, "paid_paise": pay.Amount, "expected_paise": amount})
		}
		if known != nil && *known != pay.ID {
			// A second payment captured on one Razorpay order (a late authorisation): the order is
			// paid once; this money must go back, which a person does from the Razorpay dashboard.
			return alert(ctx, tx, orderID, "payment_second_capture", map[string]any{"razorpay_payment_id": pay.ID, "paid_paise": pay.Amount})
		}
		if known == nil {
			if _, err := tx.Exec(ctx, "UPDATE payments SET provider_payment_id = $2 WHERE id = $1", paymentRow, pay.ID); err != nil {
				return err
			}
		}
		if status == "failed" {
			// A later attempt on the same Razorpay order succeeded: decide it again as paid.
			if _, err := tx.Exec(ctx, "UPDATE payments SET status = 'created', updated_at = now() WHERE id = $1", paymentRow); err != nil {
				return err
			}
		}
		return Confirm(ctx, tx, orderID, pay.OrderID, true, now)
	case "refund.processed", "refund.failed":
		rf := ev.Payload.Refund.Entity
		next := "processed"
		if ev.Event == "refund.failed" {
			next = "failed"
		}
		// By Razorpay's id, or by ours in the notes when this arrives before the sender's record.
		var refundID, orderID string
		err := tx.QueryRow(ctx, `UPDATE refunds SET status = $3, provider_refund_id = coalesce(provider_refund_id, $1), updated_at = now()
			WHERE status = 'pending' AND (provider_refund_id = $1 OR id::text = $2) RETURNING id::text, order_id::text`, rf.ID, rf.Notes["refund_id"], next).Scan(&refundID, &orderID)
		if errors.Is(err, pgx.ErrNoRows) {
			s.Log.WarnContext(ctx, "razorpay webhook: refund not ours, or already settled", "refund", rf.ID)
			return nil
		}
		if err != nil {
			return err
		}
		if next == "failed" {
			// The money did not go back; a person must act (refund again, or another way).
			return alert(ctx, tx, orderID, "refund_failed", map[string]any{"refund_id": refundID, "razorpay_refund_id": rf.ID})
		}
		return refunded(ctx, tx, refundID, orderID)
	default:
		return nil // Events we did not ask for are kept as seen and ignored.
	}
}

// refundLease is how long a claimed refund is left to its sender before another pass may try it.
const refundLease = 5 * time.Minute

// SendRefunds asks Razorpay to move the money for pending refunds (issue 237). Refunds are written
// inside order transactions as pending; this pass claims up to 10 (a short lease, committed at
// once), calls Razorpay with no transaction open, and records each result in its own transaction.
// A crash between the call and the record is safe: the next try finds the refund by our id.
func (s *Service) SendRefunds(ctx context.Context, p *Razorpay, now time.Time) (int, error) {
	type due struct {
		id, payment, order string
		amount, attempts   int
	}
	var list []due
	err := database.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE refunds r SET next_try_at = $2::timestamptz + make_interval(secs => $3::int)
			FROM payments p
			WHERE p.id = r.payment_id AND r.id IN (
				SELECT r2.id FROM refunds r2 JOIN payments p2 ON p2.id = r2.payment_id
				WHERE r2.status = 'pending' AND r2.provider_refund_id IS NULL AND p2.provider = 'razorpay' AND p2.provider_payment_id IS NOT NULL
					AND (r2.next_try_at IS NULL OR r2.next_try_at <= $2)
				ORDER BY r2.created_at, r2.id LIMIT $1 FOR UPDATE OF r2 SKIP LOCKED)
			RETURNING r.id::text, p.provider_payment_id, r.order_id::text, r.amount_paise, r.attempts`, 10, now, int(refundLease.Seconds()))
		if err != nil {
			return err
		}
		list, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (due, error) {
			var d due
			return d, row.Scan(&d.id, &d.payment, &d.order, &d.amount, &d.attempts)
		})
		return err
	})
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, d := range list {
		made, callErr := p.Refund(ctx, d.payment, d.id, d.amount)
		err := database.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
			if callErr != nil {
				// Wait 1, 2, 4 … minutes, at most an hour; the power is capped before it can overflow.
				var attempts int
				err := tx.QueryRow(ctx, `UPDATE refunds SET attempts = attempts + 1, last_error = left($2, 300),
						next_try_at = $3::timestamptz + make_interval(mins => least(60, power(2, least(attempts, 6)))::int), updated_at = now()
					WHERE id = $1 AND status = 'pending' RETURNING attempts`, d.id, callErr.Error(), now).Scan(&attempts)
				if errors.Is(err, pgx.ErrNoRows) {
					return nil // Settled meanwhile by a webhook.
				}
				if err != nil {
					return err
				}
				if attempts == refundAlertAfter {
					return alert(ctx, tx, d.order, "refund_failing", map[string]any{"refund_id": d.id, "last_error": callErr.Error()})
				}
				return nil
			}
			status := "pending"
			switch made.Status {
			case "processed":
				status = "processed"
			case "failed":
				status = "failed"
			}
			// A webhook may already have settled it by our id; keep its state then.
			tag, err := tx.Exec(ctx, `UPDATE refunds SET provider_refund_id = coalesce(provider_refund_id, $2),
					status = $3, last_error = NULL, updated_at = now() WHERE id = $1 AND status = 'pending'`,
				d.id, made.ID, status)
			if err != nil || tag.RowsAffected() == 0 || status == "pending" {
				return err // A webhook may have settled it already; its state stays.
			}
			if status == "processed" {
				return refunded(ctx, tx, d.id, d.order)
			}
			return alert(ctx, tx, d.order, "refund_failed", map[string]any{"refund_id": d.id, "razorpay_refund_id": made.ID})
		})
		if err != nil {
			return sent, err
		}
		if callErr != nil {
			s.Log.WarnContext(ctx, "razorpay refund: not sent", "refund_id", d.id, "error", callErr.Error())
			continue
		}
		sent++
	}
	return sent, nil
}

// RunRefunds runs SendRefunds every so often until ctx ends.
func (s *Service) RunRefunds(ctx context.Context, p *Razorpay, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.SendRefunds(ctx, p, s.clock()); err != nil && ctx.Err() == nil {
				s.Log.ErrorContext(ctx, "razorpay refund pass failed", "error", err.Error())
			}
		}
	}
}
