package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

// approvalLife is how long a change waits before it lapses (decision 0015).
const approvalLife = 24 * time.Hour

// Applier applies an approved change. A failure code means it no longer applies (such as
// stale_version) and nothing changed; an error is a fault.
type Applier func(ctx context.Context, tx pgx.Tx, change json.RawMessage, by string, now time.Time) (failure string, err error)

// Kinds are the changes that wait for approval, by kind. Each part of the console adds its own.
type Kinds map[string]Applier

// lapse ends pending approvals past their time.
func lapse(ctx context.Context, tx pgx.Tx, now time.Time) error {
	_, err := tx.Exec(ctx, "UPDATE approvals SET status = 'lapsed', decided_at = $1 WHERE status = 'pending' AND expires_at <= $1", now)
	return err
}

// RequestChange records a change waiting for a second person. One per thing at a time.
func RequestChange(ctx context.Context, tx pgx.Tx, u User, kind, target, summary string, change any, reason string, now time.Time) (string, error) {
	if !validReason(reason) {
		return "", errInvalid
	}
	if err := lapse(ctx, tx, now); err != nil {
		return "", err
	}
	raw, err := json.Marshal(change)
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, "SAVEPOINT request_change"); err != nil {
		return "", err
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO approvals (kind, target, summary, change, requested_by, request_reason, requested_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id::text`, kind, target, summary, raw, u.ID, strings.TrimSpace(reason), now, now.Add(approvalLife)).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT request_change"); err != nil {
			return "", err
		}
		return "", &refusal{"approval_pending", "A change to this is already waiting for approval."}
	}
	if err != nil {
		return "", err
	}
	return id, audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: u.ID, Action: "approval.requested", EntityType: "approval", EntityID: id,
		After: map[string]any{"kind": kind, "target": target}, Reason: reason})
}

// ApprovalView is one approval (contract: Approval).
func ApprovalView(ctx context.Context, q querier, id string) (map[string]any, error) {
	var kind, status, summary, reqBy, reqName, reqReason string
	var change json.RawMessage
	var reqAt, expires time.Time
	var decBy, decName, decReason, failure *string
	var decAt *time.Time
	err := q.QueryRow(ctx, `SELECT a.kind, a.status, a.summary, a.change, a.requested_by::text, ru.name, a.request_reason, a.requested_at, a.expires_at,
			a.decided_by::text, du.name, a.decided_at, a.decision_reason, a.failure_code
		FROM approvals a JOIN console_users ru ON ru.id = a.requested_by LEFT JOIN console_users du ON du.id = a.decided_by WHERE a.id = $1`, id).
		Scan(&kind, &status, &summary, &change, &reqBy, &reqName, &reqReason, &reqAt, &expires, &decBy, &decName, &decAt, &decReason, &failure)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	var decided any
	if decBy != nil {
		decided = map[string]any{"id": *decBy, "name": *decName}
	}
	return map[string]any{"id": id, "kind": kind, "status": status, "summary": summary, "change": change,
		"requested_by": map[string]any{"id": reqBy, "name": reqName}, "requested_at": reqAt.UTC().Format(time.RFC3339Nano), "request_reason": reqReason,
		"decided_by": decided, "decided_at": stamp(decAt), "decision_reason": decReason, "failure_code": failure, "expires_at": expires.UTC().Format(time.RFC3339Nano)}, nil
}

// MountApprovals adds listApprovals, approveChange, rejectChange and cancelChange.
func (s *Service) MountApprovals(r chi.Router, kinds Kinds, signedIn ...func(http.Handler) http.Handler) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.With(s.Require("approvals.read", false)).Get("/admin/approvals", s.listApprovals)
		g.With(s.Require("approvals.decide", false), idem).Post("/admin/approvals/{approval_id}/approve", func(w http.ResponseWriter, r *http.Request) { s.approve(w, r, kinds) })
		g.With(s.Require("approvals.decide", false), idem).Post("/admin/approvals/{approval_id}/reject", s.reject)
		g.With(s.Require("approvals.read", false), idem).Post("/admin/approvals/{approval_id}/cancel", s.cancel)
	})
}

var approvalStates = map[string]bool{"pending": true, "approved": true, "rejected": true, "failed": true, "cancelled": true, "lapsed": true}

func (s *Service) listApprovals(w http.ResponseWriter, r *http.Request) {
	p, ok := ReadPage(r)
	status := r.URL.Query().Get("status")
	if !ok || (status != "" && !approvalStates[status]) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Bad status, cursor or limit.", false)
		return
	}
	if err := s.lapseAll(r.Context()); err != nil {
		s.unavailable(w, r, err)
		return
	}
	if !p.HasAfter {
		p.At, p.ID = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), "ffffffff-ffff-ffff-ffff-ffffffffffff"
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT id::text, requested_at FROM approvals WHERE ($1 = '' OR status = $1) AND (requested_at, id) < ($2, $3::uuid)
		ORDER BY requested_at DESC, id DESC LIMIT $4`, status, p.At, p.ID, p.Limit+1)
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
		v, err := ApprovalView(r.Context(), s.Pool, x.id)
		if err != nil {
			s.unavailable(w, r, err)
			return
		}
		out = append(out, v)
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"approvals": out, "next_cursor": next, "server_time": serverTime()})
}

// lockApproval reads a pending approval under its lock, lapsing it if its time is up.
func lockApproval(ctx context.Context, tx pgx.Tx, id string, now time.Time) (kind, requester string, change json.RawMessage, err error) {
	if !idPattern.MatchString(id) {
		return "", "", nil, errNotFound
	}
	var status string
	var expires time.Time
	err = tx.QueryRow(ctx, "SELECT kind, status, requested_by::text, change, expires_at FROM approvals WHERE id = $1 FOR UPDATE", id).Scan(&kind, &status, &requester, &change, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil, errNotFound
	}
	if err != nil {
		return "", "", nil, err
	}
	if status == "pending" && !now.Before(expires) {
		if _, err := tx.Exec(ctx, "UPDATE approvals SET status = 'lapsed', decided_at = $2 WHERE id = $1", id, now); err != nil {
			return "", "", nil, err
		}
		status = "lapsed"
	}
	if status != "pending" {
		return "", "", nil, &refusal{"not_pending", "This change is no longer waiting."}
	}
	return kind, requester, change, nil
}

var codePattern = regexp.MustCompile(`^[a-z_]{3,60}$`)

// lapseAll ends pending approvals past their time, in its own statement, so a refusal that follows
// never undoes it.
func (s *Service) lapseAll(ctx context.Context) error {
	_, err := s.Pool.Exec(ctx, "UPDATE approvals SET status = 'lapsed', decided_at = $1 WHERE status = 'pending' AND expires_at <= $1", s.clock())
	return err
}

func (s *Service) approve(w http.ResponseWriter, r *http.Request, kinds Kinds) {
	id := chi.URLParam(r, "approval_id")
	if err := s.lapseAll(r.Context()); err != nil {
		s.unavailable(w, r, err)
		return
	}
	s.change(w, r, http.StatusOK, func(ctx context.Context, tx pgx.Tx, u User) (map[string]any, error) {
		now := s.clock()
		kind, requester, change, err := lockApproval(ctx, tx, id, now)
		if err != nil {
			return nil, err
		}
		if requester == u.ID {
			return nil, &refusal{"own_change", "Someone else must approve your change."}
		}
		apply := kinds[kind]
		if apply == nil {
			return nil, &refusal{"not_allowed", "This kind of change cannot be approved yet."}
		}
		if _, err := tx.Exec(ctx, "SAVEPOINT apply_change"); err != nil {
			return nil, err
		}
		failure, err := apply(ctx, tx, change, u.ID, now)
		if err != nil {
			return nil, err
		}
		status := "approved"
		var code any
		if failure != "" {
			if !codePattern.MatchString(failure) {
				failure = "not_applied"
			}
			if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT apply_change"); err != nil {
				return nil, err
			}
			status, code = "failed", failure
		}
		if _, err := tx.Exec(ctx, "UPDATE approvals SET status = $2, decided_by = $3, decided_at = $4, failure_code = $5 WHERE id = $1", id, status, u.ID, now, code); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: u.ID, Action: "approval." + status, EntityType: "approval", EntityID: id,
			After: map[string]any{"kind": kind, "failure_code": code}}); err != nil {
			return nil, err
		}
		v, err := ApprovalView(ctx, tx, id)
		return map[string]any{"approval": v}, err
	})
}

func (s *Service) reject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !validReason(body.Reason) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send a reason.", false)
		return
	}
	s.decide(w, r, "rejected", body.Reason, false)
}

func (s *Service) cancel(w http.ResponseWriter, r *http.Request) {
	s.decide(w, r, "cancelled", "", true)
}

func (s *Service) decide(w http.ResponseWriter, r *http.Request, to, reason string, ownOnly bool) {
	id := chi.URLParam(r, "approval_id")
	if err := s.lapseAll(r.Context()); err != nil {
		s.unavailable(w, r, err)
		return
	}
	s.change(w, r, http.StatusOK, func(ctx context.Context, tx pgx.Tx, u User) (map[string]any, error) {
		now := s.clock()
		if ownOnly {
			var requester string
			err := tx.QueryRow(ctx, "SELECT requested_by::text FROM approvals WHERE id = $1", id).Scan(&requester)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && requester != u.ID) || !idPattern.MatchString(id) {
				return nil, errNotFound // Only the person who asked can cancel; to anyone else it is not theirs.
			}
			if err != nil {
				return nil, err
			}
		}
		if _, _, _, err := lockApproval(ctx, tx, id, now); err != nil {
			return nil, err
		}
		var why any
		if reason != "" {
			why = strings.TrimSpace(reason)
		}
		if _, err := tx.Exec(ctx, "UPDATE approvals SET status = $2, decided_by = $3, decided_at = $4, decision_reason = $5 WHERE id = $1", id, to, u.ID, now, why); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: u.ID, Action: "approval." + to, EntityType: "approval", EntityID: id, Reason: reason}); err != nil {
			return nil, err
		}
		v, err := ApprovalView(ctx, tx, id)
		return map[string]any{"approval": v}, err
	})
}
