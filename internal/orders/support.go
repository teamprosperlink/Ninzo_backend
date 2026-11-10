package orders

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/availability"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/outbox"
)

var ticketTypes = map[string]bool{"missing_item": true, "cold_food": true, "wrong_item": true, "payment": true, "other": true}

// notifyFor is how long a "notify me" request waits for an offer.
const notifyFor = 24 * time.Hour

// MountSupport adds createTicket, getTicket and notifyWhenAvailable.
func (s *Service) MountSupport(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.With(idem).Post("/support/tickets", s.createTicket)
		g.Get("/support/tickets/{ticket_id}", s.getTicket)
		g.With(idem).Post("/availability/notify", s.notifyWhenAvailable)
	})
}

func ticketView(ctx context.Context, q querier, id string) (map[string]any, error) {
	var order *string
	var status string
	if err := q.QueryRow(ctx, "SELECT order_id::text, status FROM support_tickets WHERE id = $1", id).Scan(&order, &status); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, "SELECT sender, text, created_at FROM ticket_messages WHERE ticket_id = $1 ORDER BY created_at, id", id)
	if err != nil {
		return nil, err
	}
	messages, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (map[string]any, error) {
		var from, text string
		var at time.Time
		err := r.Scan(&from, &text, &at)
		return map[string]any{"from": from, "text": text, "sent_at": at.UTC().Format(time.RFC3339Nano)}, err
	})
	if err != nil {
		return nil, err
	}
	var orderValue any
	if order != nil {
		orderValue = *order
	}
	return map[string]any{"id": id, "order_id": orderValue, "status": status, "messages": messages}, nil
}

func (s *Service) createTicket(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body struct {
		OrderID string `json:"order_id"`
		Type    string `json:"type"`
		Text    string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !ticketTypes[body.Type] || body.Text == "" || utf8.RuneCountInString(body.Text) > 2000 ||
		strings.ContainsRune(body.Text, 0) ||
		(body.OrderID != "" && !idPattern.MatchString(body.OrderID)) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send a type and text of up to 2000 characters, and optionally order_id.", false)
		return
	}
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ctx := r.Context()
		if err := identity.CheckSession(ctx, tx, actor); err != nil {
			return err
		}
		hasPhone, err := customer(ctx, tx, actor.ID)
		if err != nil {
			return err
		}
		if !hasPhone {
			return &refusal{"phone_required", "Verify a phone first."}
		}
		var order any
		if body.OrderID != "" {
			if err := owned(ctx, tx, body.OrderID, actor.ID); err != nil {
				return err
			}
			order = body.OrderID
		}
		var id string
		if err := tx.QueryRow(ctx, "INSERT INTO support_tickets (customer_id, order_id, type) VALUES ($1, $2, $3) RETURNING id::text",
			actor.ID, order, body.Type).Scan(&id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO ticket_messages (ticket_id, sender, text) VALUES ($1, 'customer', $2)", id, body.Text); err != nil {
			return err
		}
		if err := outbox.Add(ctx, tx, "support.ticket_created", map[string]any{"ticket_id": id, "type": body.Type}); err != nil {
			return err
		}
		t, err := ticketView(ctx, tx, id)
		if err != nil {
			return err
		}
		reply = map[string]any{"ticket": t, "server_time": serverTime()}
		return idempotency.Save(ctx, tx, http.StatusCreated, reply)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusCreated, reply)
}

func (s *Service) getTicket(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	id := chi.URLParam(r, "ticket_id")
	if err := s.readable(r, actor); err != nil {
		s.fail(w, r, err)
		return
	}
	var mine bool
	if idPattern.MatchString(id) {
		if err := s.Pool.QueryRow(r.Context(), "SELECT EXISTS (SELECT 1 FROM support_tickets WHERE id = $1 AND customer_id = $2)", id, actor.ID).Scan(&mine); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if !mine {
		s.fail(w, r, errNotFound)
		return
	}
	t, err := ticketView(r.Context(), s.Pool, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"ticket": t, "server_time": serverTime()})
}

func (s *Service) notifyWhenAvailable(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body struct {
		GateID string `json:"gate_id"`
		ItemID string `json:"item_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !idPattern.MatchString(body.GateID) || (body.ItemID != "" && !idPattern.MatchString(body.ItemID)) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send gate_id, and optionally item_id.", false)
		return
	}
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ctx := r.Context()
		if err := identity.CheckSession(ctx, tx, actor); err != nil {
			return err
		}
		hasPhone, err := customer(ctx, tx, actor.ID)
		if err != nil {
			return err
		}
		if !hasPhone {
			return &refusal{"phone_required", "Verify a phone first."}
		}
		var found bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM gates WHERE id = $1 AND customer_id = $2)", body.GateID, actor.ID).Scan(&found); err != nil {
			return err
		}
		if found && body.ItemID != "" {
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM menu_items WHERE id = $1 AND status = 'active')", body.ItemID).Scan(&found); err != nil {
				return err
			}
		}
		if !found {
			return errNotFound
		}
		var item any
		if body.ItemID != "" {
			item = body.ItemID
		}
		// Asking again for the same gate and item keeps the one open request.
		if _, err := tx.Exec(ctx, `INSERT INTO availability_notifications (customer_id, gate_id, item_id, expires_at) VALUES ($1, $2, $3, $4)
			ON CONFLICT DO NOTHING`, actor.ID, body.GateID, item, s.clock().Add(notifyFor)); err != nil {
			return err
		}
		reply = map[string]any{"server_time": serverTime()}
		return idempotency.Save(ctx, tx, http.StatusCreated, reply)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusCreated, reply)
}

// pushed are the stages a customer is told about by push. Push follows customer_stage, never the
// true state (contracts/README.md).
var pushed = map[string]bool{"payment_failed": true, "no_scooter": true, "rider_accepted": true, "timer_running": true, "arrived": true, "handed_over": true, "ended": true}

// Notify is the background pass for pushes: one per customer stage change, and "notify me"
// requests whose gate has an offer again. It only queues them; the outbox worker sends them.
func (s *Service) Notify(ctx context.Context, now time.Time) (int, error) {
	n, err := s.sweep(ctx, "queuing a stage push", `SELECT id::text FROM orders WHERE seq > notified_seq AND updated_at > $2::timestamptz - interval '1 day'
		AND NOT (id::text = ANY($1::text[])) ORDER BY updated_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`,
		now, func(ctx context.Context, tx pgx.Tx, id string) error {
			order, err := view(ctx, tx, s, id, now)
			if err != nil {
				return err
			}
			stage, _ := order["customer_stage"].(string)
			var last *string
			var customerID string
			if err := tx.QueryRow(ctx, "SELECT notified_stage, customer_id::text FROM orders WHERE id = $1", id).Scan(&last, &customerID); err != nil {
				return err
			}
			if pushed[stage] && (last == nil || *last != stage) {
				if err := outbox.Add(ctx, tx, "push.customer", map[string]any{"customer_id": customerID, "order_id": id, "stage": stage, "seq": order["seq"]}); err != nil {
					return err
				}
			}
			_, err = tx.Exec(ctx, "UPDATE orders SET notified_seq = seq, notified_stage = $2 WHERE id = $1", id, stage)
			return err
		})
	if err != nil {
		return n, err
	}
	m, err := s.sweep(ctx, "checking a notify-me request", `SELECT id::text FROM availability_notifications WHERE sent_at IS NULL AND expires_at > $2
		AND NOT (id::text = ANY($1::text[])) ORDER BY coalesce(checked_at, '-infinity'), created_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`,
		now, func(ctx context.Context, tx pgx.Tx, id string) error {
			var customerID, gateID string
			var item *string
			var lat, lng float64
			if err := tx.QueryRow(ctx, `SELECT a.customer_id::text, a.gate_id::text, a.item_id::text, g.lat, g.lng
				FROM availability_notifications a JOIN gates g ON g.id = a.gate_id WHERE a.id = $1`, id).Scan(&customerID, &gateID, &item, &lat, &lng); err != nil {
				return err
			}
			res, err := availability.At(ctx, tx, lat, lng, now)
			if err != nil {
				return err
			}
			if !offered(res, item) {
				// Checked: it goes to the back, so every request takes its turn.
				_, err := tx.Exec(ctx, "UPDATE availability_notifications SET checked_at = $2 WHERE id = $1", id, now)
				return err
			}
			if err := outbox.Add(ctx, tx, "push.customer", map[string]any{"customer_id": customerID, "gate_id": gateID, "stage": "available"}); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, "UPDATE availability_notifications SET sent_at = $2 WHERE id = $1", id, now)
			return err
		})
	return n + m, err
}

// PushTopics are the outbox topics the push worker takes. Support events wait for the console.
var PushTopics = []string{"push.customer", "push.rider"}

// Pusher sends a push to one device. The log pusher stands in until FCM is connected.
type Pusher interface {
	Push(ctx context.Context, platform, token, topic string, payload map[string]any) error
}

// LogPusher sends nothing. It logs that a push would go out, without the token.
type LogPusher struct{ Log *slog.Logger }

// Push implements Pusher.
func (p LogPusher) Push(_ context.Context, platform, _, topic string, payload map[string]any) error {
	p.Log.Info("push stand-in: would send", "platform", platform, "topic", topic, "stage", payload["stage"])
	return nil
}

// Deliver is the outbox handler: a push goes to every device of its customer or rider. Other
// topics are support events, which the console will read; they are marked delivered.
func (s *Service) Deliver(p Pusher) outbox.Handler {
	return func(ctx context.Context, e outbox.Event) error {
		var actorType string
		switch e.Topic {
		case "push.customer":
			actorType = identity.TypeCustomer
		case "push.rider":
			actorType = identity.TypeRider
		default:
			return errors.New("orders: not a push topic: " + e.Topic) // Never consume another worker's events.
		}
		var payload map[string]any
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			return nil // Unreadable: retrying will not help.
		}
		id, _ := payload[actorType+"_id"].(string)
		rows, err := s.Pool.Query(ctx, "SELECT platform, push_token FROM devices WHERE actor_type = $1 AND actor_id = $2", actorType, id)
		if err != nil {
			return err
		}
		type device struct{ platform, token string }
		devices, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (device, error) {
			var d device
			return d, r.Scan(&d.platform, &d.token)
		})
		if err != nil {
			return err
		}
		var failed error
		for _, d := range devices {
			if err := p.Push(ctx, d.platform, d.token, e.Topic, payload); err != nil {
				failed = errors.Join(failed, err)
			}
		}
		return failed
	}
}

// offered says whether an offer is in reach, and the item too when one was asked for.
func offered(res availability.Result, item *string) bool {
	if !res.Offer {
		return false
	}
	if item == nil {
		return true
	}
	for _, l := range res.Lines {
		if l.Item.ID == *item && l.QtyLeft > 0 {
			return true
		}
	}
	return false
}
