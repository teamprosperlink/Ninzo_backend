package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

// MountReviews adds the audit log, arrival reviews and support alerts.
func (s *Service) MountReviews(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.With(s.Require("audit.read", false)).Get("/admin/audit", s.listAudit)
		g.With(s.Require("reviews.read", false)).Get("/admin/arrival-reviews", s.listReviews)
		g.With(s.Require("reviews.decide", false), idem).Post("/admin/arrival-reviews/{review_id}/decision", s.decideReview)
		g.With(s.Require("alerts.read", false)).Get("/admin/support-alerts", s.listAlerts)
	})
}

// idPage reads a cursor that is a row id (audit and outbox rows have growing integer ids), newest
// first.
func idPage(r *http.Request) (limit int, before int64, ok bool) {
	limit, before = 50, int64(1)<<62
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			return 0, 0, false
		}
		limit = n
	}
	if v := r.URL.Query().Get("cursor"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			return 0, 0, false
		}
		before = n
	}
	return limit, before, true
}

// secretKeys never leave the audit log, whatever a row holds: the contract promises no password
// hashes, TOTP secrets or full phone numbers.
var secretKeys = []string{"password", "secret", "totp", "phone", "refresh_token", "access_token", "push_token", "subject", "email"}

// secretExact are keys hidden only when they are the whole key, as a short word inside another
// key (otp_released_at) is harmless.
var secretExact = []string{"otp", "code", "token"}

func scrub(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			lower := strings.ToLower(k)
			hidden := false
			for _, s := range secretKeys {
				if strings.Contains(lower, s) && lower != "phone_last4" {
					hidden = true
				}
			}
			for _, s := range secretExact {
				if lower == s {
					hidden = true
				}
			}
			if hidden {
				out[k] = "(hidden)"
				continue
			}
			out[k] = scrub(val)
		}
		return out
	case []any:
		for i := range x {
			x[i] = scrub(x[i])
		}
		return x
	}
	return v
}

func (s *Service) listAudit(w http.ResponseWriter, r *http.Request) {
	limit, before, ok := idPage(r)
	q := r.URL.Query()
	var from, to *time.Time
	for _, p := range []struct {
		key string
		dst **time.Time
	}{{"from", &from}, {"to", &to}} {
		if v := q.Get(p.key); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				ok = false
			}
			*p.dst = &t
		}
	}
	actor := q.Get("actor_id")
	if len(actor) > 64 || len(q.Get("action")) > 80 || len(q.Get("entity_type")) > 40 {
		ok = false
	}
	if actor != "" && !idPattern.MatchString(actor) {
		actor = "00000000-0000-0000-0000-000000000000" // Not an id: nobody acted as it.
	}
	if !ok {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Check the filters, cursor and limit.", false)
		return
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT id, actor_type, actor_id::text, action, entity_type, entity_id, before, after, reason, created_at
		FROM audit_events WHERE id < $1
			AND ($2::timestamptz IS NULL OR created_at >= $2) AND ($3::timestamptz IS NULL OR created_at < $3)
			AND (NULLIF($4, '')::uuid IS NULL OR actor_id = NULLIF($4, '')::uuid)
			AND ($5 = '' OR action = $5) AND ($6 = '' OR entity_type = $6)
		ORDER BY id DESC LIMIT $7`, before, from, to, actor, q.Get("action"), q.Get("entity_type"), limit+1)
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	out := []map[string]any{}
	var id int64
	var actorType, action, entityType, entityID string
	var actorID, reason *string
	var beforeJ, afterJ []byte
	var at time.Time
	if _, err := pgx.ForEachRow(rows, []any{&id, &actorType, &actorID, &action, &entityType, &entityID, &beforeJ, &afterJ, &reason, &at}, func() error {
		decode := func(raw []byte) any {
			if raw == nil {
				return nil
			}
			var v any
			if json.Unmarshal(raw, &v) != nil {
				return nil
			}
			if _, isMap := v.(map[string]any); !isMap {
				return map[string]any{"value": scrub(v)} // The contract's before and after are objects.
			}
			return scrub(v)
		}
		out = append(out, map[string]any{"id": strconv.FormatInt(id, 10), "actor_type": actorType, "actor_id": actorID, "action": action,
			"entity_type": entityType, "entity_id": entityID, "before": decode(beforeJ), "after": decode(afterJ), "reason": reason,
			"at": at.UTC().Format(time.RFC3339Nano)})
		return nil
	}); err != nil {
		s.unavailable(w, r, err)
		return
	}
	var next any
	if len(out) > limit {
		out = out[:limit]
		next = out[limit-1]["id"]
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"events": out, "next_cursor": next, "server_time": serverTime()})
}

// ReviewView is one arrival review (contract: ArrivalReview).
func ReviewView(ctx context.Context, q querier, id string) (map[string]any, error) {
	var order, status string
	var lat, lng float64
	var device, due time.Time
	var decided *time.Time
	var by, why *string
	err := q.QueryRow(ctx, `SELECT a.order_id::text, a.status, a.lat, a.lng, a.device_time, a.due_at, a.decided_at, cu.name, a.decision_reason
		FROM arrival_reviews a LEFT JOIN console_users cu ON cu.id = a.decided_by WHERE a.id = $1`, id).
		Scan(&order, &status, &lat, &lng, &device, &due, &decided, &by, &why)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "order_id": order, "status": status, "lat": lat, "lng": lng, "device_time": device.UTC().Format(time.RFC3339Nano),
		"due_at": due.UTC().Format(time.RFC3339Nano), "decided_at": stamp(decided), "decided_by": by, "decision_reason": why}, nil
}

// listReviews: open ones first, soonest due; then the rest, newest first.
func (s *Service) listReviews(w http.ResponseWriter, r *http.Request) {
	p, ok := ReadPage(r)
	status := r.URL.Query().Get("status")
	if !ok || (status != "" && status != "open" && status != "arrived_in_time" && status != "late" && status != "timed_out") {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Bad status, cursor or limit.", false)
		return
	}
	if !p.HasAfter {
		p.At, p.ID = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC), "00000000-0000-0000-0000-000000000000"
	}
	// One order key for paging: open ones by due time, the rest after them by newest first.
	rows, err := s.Pool.Query(r.Context(), `SELECT id::text, sort_at FROM (
			SELECT id, CASE WHEN status = 'open' THEN due_at - interval '100 years' ELSE timestamptz '3000-01-01' - (created_at - timestamptz '2000-01-01') END AS sort_at
			FROM arrival_reviews WHERE ($1 = '' OR status = $1)) x
		WHERE (sort_at, id) > ($2, $3::uuid)
		ORDER BY sort_at, id LIMIT $4`, status, p.At, p.ID, p.Limit+1)
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	type row struct {
		id string
		at time.Time
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.id, &x.at)
	})
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	var next any
	if len(list) > p.Limit {
		list = list[:p.Limit]
		next = Cursor(list[len(list)-1].at, list[len(list)-1].id)
	}
	out := []map[string]any{}
	for _, x := range list {
		v, err := ReviewView(r.Context(), s.Pool, x.id)
		if err != nil {
			s.unavailable(w, r, err)
			return
		}
		out = append(out, v)
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"reviews": out, "next_cursor": next, "server_time": serverTime()})
}

// decideReview settles an open review. arrived_in_time stops the timer at the rider's evidence, so
// no credit is owed; late keeps the miss, and the background pass then pays its credit.
func (s *Service) decideReview(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || (body.Decision != "arrived_in_time" && body.Decision != "late") || !validReason(body.Reason) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send decision and a reason.", false)
		return
	}
	id := chi.URLParam(r, "review_id")
	s.change(w, r, http.StatusOK, func(ctx context.Context, tx pgx.Tx, u User) (map[string]any, error) {
		if !idPattern.MatchString(id) {
			return nil, errNotFound
		}
		// The order first, as the background passes lock it before its review.
		var order string
		if err := tx.QueryRow(ctx, "SELECT order_id::text FROM arrival_reviews WHERE id = $1", id).Scan(&order); errors.Is(err, pgx.ErrNoRows) {
			return nil, errNotFound
		} else if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, "SELECT 1 FROM orders WHERE id = $1 FOR UPDATE", order); err != nil {
			return nil, err
		}
		var status string
		var device, due time.Time
		if err := tx.QueryRow(ctx, "SELECT status, device_time, due_at FROM arrival_reviews WHERE id = $1 FOR UPDATE", id).Scan(&status, &device, &due); err != nil {
			return nil, err
		}
		if status != "open" || !s.clock().Before(due) {
			return nil, &refusal{"review_closed", "This review is already closed, or its time ran out."}
		}
		if _, err := tx.Exec(ctx, "UPDATE arrival_reviews SET status = $2, decided_at = now(), decided_by = $3, decision_reason = $4 WHERE id = $1",
			id, body.Decision, u.ID, body.Reason); err != nil {
			return nil, err
		}
		var state string
		if err := tx.QueryRow(ctx, "SELECT state FROM orders WHERE id = $1", order).Scan(&state); err != nil {
			return nil, err
		}
		if body.Decision == "arrived_in_time" {
			// A credit already given to the customer cannot be taken back here.
			var applied bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM remedies WHERE order_id = $1 AND type = 'credit' AND status = 'applied')", order).Scan(&applied); err != nil {
				return nil, err
			}
			if applied {
				return nil, &refusal{"credit_applied", "The missed-timer credit was already given; settle it by hand."}
			}
			if _, err := tx.Exec(ctx, "UPDATE remedies SET status = 'cancelled' WHERE order_id = $1 AND type = 'credit' AND status = 'owed'", order); err != nil {
				return nil, err
			}
		}
		if body.Decision == "arrived_in_time" {
			// The rider reached the perimeter in time: the timer stops at that moment and the miss
			// is cleared, so no credit is paid. The customer's screen refreshes through seq.
			if _, err := tx.Exec(ctx, `UPDATE orders SET timer_missed = false, timer_stopped_at = CASE WHEN timer_stopped_at IS NULL OR timer_stopped_at > $2 THEN $2 ELSE timer_stopped_at END,
				seq = seq + 1, updated_at = now() WHERE id = $1`, order, device); err != nil {
				return nil, err
			}
		}
		// The order's own history shows the decision; its state does not change.
		if _, err := tx.Exec(ctx, `INSERT INTO order_events (order_id, from_state, to_state, actor_type, actor_id, reason) VALUES ($1, $2, $2, $3, $4, $5)`,
			order, state, identity.TypeConsole, u.ID, "arrival review: "+strings.ReplaceAll(body.Decision, "_", " ")); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: u.ID, Action: "arrival_review." + body.Decision, EntityType: "order",
			EntityID: order, After: map[string]any{"review_id": id, "decision": body.Decision}, Reason: body.Reason}); err != nil {
			return nil, err
		}
		v, err := ReviewView(ctx, tx, id)
		return map[string]any{"review": v}, err
	})
}

// listAlerts lists what the system raised for a person to look at, newest first.
func (s *Service) listAlerts(w http.ResponseWriter, r *http.Request) {
	limit, before, ok := idPage(r)
	if !ok {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Bad cursor or limit.", false)
		return
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT id, coalesce(payload->>'reason', 'unknown'), payload->>'order_id', payload->>'rider_id', created_at
		FROM outbox_events WHERE topic = 'support.alert' AND id < $1 ORDER BY id DESC LIMIT $2`, before, limit+1)
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	out := []map[string]any{}
	var id int64
	var kind string
	var order, rider *string
	var at time.Time
	if _, err := pgx.ForEachRow(rows, []any{&id, &kind, &order, &rider, &at}, func() error {
		for _, p := range []**string{&order, &rider} {
			if *p != nil && !idPattern.MatchString(**p) {
				*p = nil
			}
		}
		out = append(out, map[string]any{"id": strconv.FormatInt(id, 10), "kind": kind, "order_id": order, "rider_id": rider, "created_at": at.UTC().Format(time.RFC3339Nano)})
		return nil
	}); err != nil {
		s.unavailable(w, r, err)
		return
	}
	var next any
	if len(out) > limit {
		out = out[:limit]
		next = out[limit-1]["id"]
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"alerts": out, "next_cursor": next, "server_time": serverTime()})
}
