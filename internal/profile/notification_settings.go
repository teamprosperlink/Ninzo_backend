package profile

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

// The settings a customer has before choosing any (issue 178): offers and news are off, support
// replies are on. A customer with no row has these.
const (
	defaultOffersAndNews  = false
	defaultSupportReplies = true
)

// settingsReply is the contract's NotificationSettingsResponse. order_updates is always true and
// is not stored, because order pushes are part of the service.
func settingsReply(offersAndNews, supportReplies bool) map[string]any {
	return map[string]any{
		"settings": map[string]any{
			"order_updates": true, "offers_and_news": offersAndNews, "support_replies": supportReplies,
		},
		"server_time": serverTime(),
	}
}

// readNotificationSettings returns a customer's settings, or the defaults when there is no row.
func readNotificationSettings(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, customerID string) (offersAndNews, supportReplies bool, err error) {
	offersAndNews, supportReplies = defaultOffersAndNews, defaultSupportReplies
	err = q.QueryRow(ctx, "SELECT offers_and_news, support_replies FROM customer_notification_settings WHERE customer_id = $1", customerID).
		Scan(&offersAndNews, &supportReplies)
	if err == pgx.ErrNoRows {
		err = nil
	}
	return offersAndNews, supportReplies, err
}

func (s *Service) getNotificationSettings(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	if !s.requireLive(w, r, actor) {
		return
	}
	if _, err := readCustomer(r.Context(), s.Pool, actor.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	offers, support, err := readNotificationSettings(r.Context(), s.Pool, actor.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, settingsReply(offers, support))
}

// updateNotificationSettings changes offers_and_news, support_replies or both. Any other key is a
// 400, and order_updates too, even as true, because it cannot be changed.
func (s *Service) updateNotificationSettings(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body == nil {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "The body must be a JSON object.", false)
		return
	}
	var offers, support *bool
	for key, raw := range body {
		var v bool
		switch {
		case key == "order_updates":
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "order_updates cannot be changed.", false)
			return
		case key != "offers_and_news" && key != "support_replies":
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send offers_and_news or support_replies.", false)
			return
		case bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &v) != nil:
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", key+" must be true or false.", false)
			return
		case key == "offers_and_news":
			offers = &v
		default:
			support = &v
		}
	}
	if offers == nil && support == nil {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send offers_and_news, support_replies or both.", false)
		return
	}
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		if err := identity.CheckSession(r.Context(), tx, actor); err != nil {
			return err
		}
		if _, err := readCustomer(r.Context(), tx, actor.ID); err != nil {
			return err
		}
		// A kind left out keeps its stored value, or its default when there is no row yet.
		var gotOffers, gotSupport bool
		if err := tx.QueryRow(r.Context(), `INSERT INTO customer_notification_settings AS s (customer_id, offers_and_news, support_replies)
			VALUES ($1, coalesce($2::boolean, $4::boolean), coalesce($3::boolean, $5::boolean))
			ON CONFLICT (customer_id) DO UPDATE SET offers_and_news = coalesce($2::boolean, s.offers_and_news),
				support_replies = coalesce($3::boolean, s.support_replies), updated_at = now()
			RETURNING offers_and_news, support_replies`,
			actor.ID, offers, support, defaultOffersAndNews, defaultSupportReplies).Scan(&gotOffers, &gotSupport); err != nil {
			return err
		}
		// The kinds and their new values: switches are not personal, and the history is worth keeping.
		after := map[string]any{}
		if offers != nil {
			after["offers_and_news"] = gotOffers
		}
		if support != nil {
			after["support_replies"] = gotSupport
		}
		if err := audit.Record(r.Context(), tx, audit.Event{
			ActorType: identity.TypeCustomer, ActorID: actor.ID, Action: "customer.notification_settings_updated", EntityType: "customer", EntityID: actor.ID,
			After: after,
		}); err != nil {
			return err
		}
		reply = settingsReply(gotOffers, gotSupport)
		return idempotency.Save(r.Context(), tx, http.StatusOK, reply)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, reply)
}
