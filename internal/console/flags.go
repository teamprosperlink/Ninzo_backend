package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

var flagKey = regexp.MustCompile(`^[a-z][a-z0-9_]{1,60}$`)

// FlagOn says whether a feature flag is on. An unknown flag is off.
func FlagOn(ctx context.Context, q querier, key string) (bool, error) {
	var on bool
	err := q.QueryRow(ctx, "SELECT coalesce((SELECT enabled FROM feature_flags WHERE key = $1), false)", key).Scan(&on)
	return on, err
}

// MountFlags adds listFlags and requestFlagChange.
func (s *Service) MountFlags(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.With(s.Require("flags.manage", false)).Get("/admin/flags", s.listFlags)
		g.With(s.Require("flags.manage", false), idem).Put("/admin/flags/{flag_key}", s.requestFlag)
	})
}

func (s *Service) listFlags(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Pool.Query(r.Context(), "SELECT key, enabled, description, updated_at FROM feature_flags ORDER BY key")
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	out := []map[string]any{}
	var key, desc string
	var on bool
	var at time.Time
	if _, err := pgx.ForEachRow(rows, []any{&key, &on, &desc, &at}, func() error {
		out = append(out, map[string]any{"key": key, "enabled": on, "description": desc, "updated_at": at.UTC().Format(time.RFC3339Nano)})
		return nil
	}); err != nil {
		s.unavailable(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"flags": out, "server_time": serverTime()})
}

func (s *Service) requestFlag(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled *bool  `json:"enabled"`
		Reason  string `json:"reason"`
	}
	key := chi.URLParam(r, "flag_key")
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Enabled == nil || !validReason(body.Reason) || !flagKey.MatchString(key) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send enabled and a reason.", false)
		return
	}
	s.change(w, r, http.StatusAccepted, func(ctx context.Context, tx pgx.Tx, u User) (map[string]any, error) {
		var on bool
		err := tx.QueryRow(ctx, "SELECT enabled FROM feature_flags WHERE key = $1", key).Scan(&on)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errNotFound
		}
		if err != nil {
			return nil, err
		}
		state := map[bool]string{true: "on", false: "off"}
		id, err := RequestChange(ctx, tx, u, "flag", key, key+": "+state[*body.Enabled],
			map[string]any{"kind": "flag", "key": key, "enabled_before": on, "enabled_after": *body.Enabled}, body.Reason, s.clock())
		if err != nil {
			return nil, err
		}
		v, err := ApprovalView(ctx, tx, id)
		return map[string]any{"approval": v}, err
	})
}

// ApplyFlag is the flag kind's Applier: it applies only if the flag is still as it was asked about.
func ApplyFlag(ctx context.Context, tx pgx.Tx, change json.RawMessage, by string, now time.Time) (string, error) {
	var c struct {
		Key    string `json:"key"`
		Before bool   `json:"enabled_before"`
		After  bool   `json:"enabled_after"`
	}
	if err := json.Unmarshal(change, &c); err != nil {
		return "", err
	}
	tag, err := tx.Exec(ctx, "UPDATE feature_flags SET enabled = $2, updated_at = $4 WHERE key = $1 AND enabled = $3", c.Key, c.After, c.Before, now)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "stale_version", nil
	}
	return "", audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: by, Action: "flag.changed", EntityType: "flag", EntityID: c.Key,
		Before: map[string]any{"enabled": c.Before}, After: map[string]any{"enabled": c.After}})
}
