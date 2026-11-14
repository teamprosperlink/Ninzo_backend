package riderapp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

// MaxFeedbackRunes is the longest feedback message, as the contract says.
const MaxFeedbackRunes = 2000

// Service is the rider app's simple calls. Now is for tests; nil means the real clock.
type Service struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
	Now  func() time.Time
}

func (s *Service) clock() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func serverTime() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// refusal is a 409 the contract names.
type refusal struct{ code, message string }

func (r *refusal) Error() string { return "riderapp: " + r.code }

var (
	errBlocked  = errors.New("riderapp: blocked")
	errNotFound = errors.New("riderapp: not found")
)

// MountRider adds getRiderSettings, getRiderHelp, sendRiderFeedback, getShiftPlan and replyToShiftPlan.
func (s *Service) MountRider(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.Get("/riders/me/settings", s.getSettings)
		g.Get("/riders/me/help", s.getHelp)
		g.With(idem).Post("/riders/me/feedback", s.sendFeedback)
		g.Get("/riders/me/shift-plan", s.getShiftPlan)
		g.With(idem).Post("/riders/me/shift-plan/reply", s.replyToShiftPlan)
	})
}

func (s *Service) fail(w http.ResponseWriter, r *http.Request, err error) {
	var no *refusal
	switch {
	case errors.As(err, &no):
		httpserver.WriteError(w, http.StatusConflict, no.code, no.message, false)
	case errors.Is(err, identity.ErrSessionInvalid):
		httpserver.WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in again.", false)
	case errors.Is(err, errBlocked):
		identity.WriteBlocked(w)
	case errors.Is(err, errNotFound):
		httpserver.WriteError(w, http.StatusNotFound, "not_found", "Not found.", false)
	default:
		s.Log.Error("riderapp: request failed", "request_id", httpserver.RequestID(r.Context()), "error", err.Error())
		httpserver.WriteError(w, http.StatusInternalServerError, "internal_error", "Try again shortly.", true)
	}
}

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// readRider returns the rider's zone, or errBlocked for a blocked person. A suspended rider may still
// read and write here: none of it is work.
func readRider(ctx context.Context, q querier, id string) (zone string, err error) {
	var z *string
	var personStatus string
	err = q.QueryRow(ctx, "SELECT r.zone_id::text, p.status FROM riders r JOIN people p ON p.id = r.person_id WHERE r.id = $1", id).Scan(&z, &personStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", identity.ErrSessionInvalid
	}
	if err != nil {
		return "", err
	}
	if personStatus != "active" {
		return "", errBlocked
	}
	if z != nil {
		zone = *z
	}
	return zone, nil
}

// live is the sign-in check for a read: an ended session must not keep reading.
func (s *Service) live(ctx context.Context, a identity.Actor) (zone string, err error) {
	ok, err := identity.SessionLive(ctx, s.Pool, a)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", identity.ErrSessionInvalid
	}
	return readRider(ctx, s.Pool, a.ID)
}

// getSettings is built from the console's settings for the rider's zone (the global ones with none).
func (s *Service) getSettings(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	zone, err := s.live(r.Context(), actor)
	var eff geo.Effective
	if err == nil {
		eff, err = geo.LoadSettings(r.Context(), s.Pool, zone)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"settings": Settings(eff), "server_time": serverTime()})
}

// getHelp lists the active rider articles in the order operations set. With none written the list is empty.
func (s *Service) getHelp(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	if _, err := s.live(r.Context(), actor); err != nil {
		s.fail(w, r, err)
		return
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT id::text, question, answer FROM help_articles
		WHERE audience = 'rider' AND active ORDER BY sort, created_at, id`)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	faq, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (map[string]any, error) {
		var id, q, a string
		if err := row.Scan(&id, &q, &a); err != nil {
			return nil, err
		}
		return map[string]any{"id": id, "question": q, "answer": a}, nil
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if faq == nil {
		faq = []map[string]any{}
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"faq": faq, "server_time": serverTime()})
}

// cleanMessage trims the text and refuses control characters other than line breaks and tabs.
func cleanMessage(s string) (string, bool) {
	s = strings.TrimSpace(s)
	n := utf8.RuneCountInString(s)
	if n < 1 || n > MaxFeedbackRunes || !utf8.ValidString(s) {
		return "", false
	}
	if strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' }) {
		return "", false
	}
	return s, true
}

// sendFeedback keeps what the rider wrote for the team to read in the console. The audit trail holds
// its length only, never the words.
func (s *Service) sendFeedback(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body struct {
		Message *string `json:"message"`
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&body); err != nil || dec.More() || body.Message == nil {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send message, 1 to 2000 characters.", false)
		return
	}
	message, ok := cleanMessage(*body.Message)
	if !ok {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send message, 1 to 2000 characters.", false)
		return
	}
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ctx := r.Context()
		if err := identity.CheckSession(ctx, tx, actor); err != nil {
			return err
		}
		if _, err := readRider(ctx, tx, actor.ID); err != nil {
			return err
		}
		var id string
		if err := tx.QueryRow(ctx, "INSERT INTO rider_feedback (rider_id, message) VALUES ($1, $2) RETURNING id::text", actor.ID, message).Scan(&id); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeRider, ActorID: actor.ID, Action: "rider.feedback_sent",
			EntityType: "rider_feedback", EntityID: id, After: map[string]any{"length": utf8.RuneCountInString(message)}}); err != nil {
			return err
		}
		reply = map[string]any{"feedback_id": id, "server_time": serverTime()}
		return idempotency.Save(ctx, tx, http.StatusCreated, reply)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusCreated, reply)
}

// PlanColumns are the columns ScanPlan reads, from shift_plans (alias p when joined).
const PlanColumns = `p.id::text, p.plan_date::text, p.node_id::text, p.starts_at, p.ends_at, p.stock_reply, p.replied_at, p.packing, p.ready_at`

func stampOf(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// ScanPlan reads PlanColumns as the contract's ShiftPlan.
func ScanPlan(row pgx.Row) (map[string]any, error) {
	var id, date, reply, packing string
	var node *string
	var starts, ends time.Time
	var replied, ready *time.Time
	if err := row.Scan(&id, &date, &node, &starts, &ends, &reply, &replied, &packing, &ready); err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "date": date, "node_id": node, "starts_at": starts.UTC().Format(time.RFC3339Nano),
		"ends_at": ends.UTC().Format(time.RFC3339Nano), "stock_reply": reply, "replied_at": stampOf(replied), "packing": packing, "ready_at": stampOf(ready)}, nil
}

// getShiftPlan returns today's plan (India time), or null when operations made none, which is the usual day.
func (s *Service) getShiftPlan(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	if _, err := s.live(r.Context(), actor); err != nil {
		s.fail(w, r, err)
		return
	}
	plan, err := ScanPlan(s.Pool.QueryRow(r.Context(), `SELECT `+PlanColumns+` FROM shift_plans p
		WHERE p.rider_id = $1 AND p.plan_date = ($2::timestamptz AT TIME ZONE 'Asia/Kolkata')::date`, actor.ID, s.clock()))
	if errors.Is(err, pgx.ErrNoRows) {
		httpserver.WriteJSON(w, http.StatusOK, map[string]any{"plan": nil, "server_time": serverTime()})
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"plan": plan, "server_time": serverTime()})
}

// replyToShiftPlan records coming or not_today on today's plan. It can change until the hub starts
// packing. Nothing else happens: no shift starts, ends or is blocked by the answer.
func (s *Service) replyToShiftPlan(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body struct {
		Reply *string `json:"reply"`
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&body); err != nil || dec.More() || body.Reply == nil || (*body.Reply != "coming" && *body.Reply != "not_today") {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "reply is coming or not_today.", false)
		return
	}
	answer := *body.Reply
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		ctx := r.Context()
		if err := identity.CheckSession(ctx, tx, actor); err != nil {
			return err
		}
		if _, err := readRider(ctx, tx, actor.ID); err != nil {
			return err
		}
		now := s.clock().Truncate(time.Microsecond)
		var id, before, packing string
		err := tx.QueryRow(ctx, `SELECT p.id::text, p.stock_reply, p.packing FROM shift_plans p
			WHERE p.rider_id = $1 AND p.plan_date = ($2::timestamptz AT TIME ZONE 'Asia/Kolkata')::date FOR UPDATE`, actor.ID, now).Scan(&id, &before, &packing)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		if packing != "not_started" {
			return &refusal{"shift_plan_closed", "The hub has started packing, so the answer can no longer change."}
		}
		// The same answer again keeps the first time it was given.
		if before != answer {
			if _, err := tx.Exec(ctx, "UPDATE shift_plans SET stock_reply = $2, replied_at = $3 WHERE id = $1", id, answer, now); err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeRider, ActorID: actor.ID, Action: "shift_plan.replied",
				EntityType: "shift_plan", EntityID: id, Before: map[string]any{"stock_reply": before}, After: map[string]any{"stock_reply": answer}}); err != nil {
				return err
			}
		}
		plan, err := ScanPlan(tx.QueryRow(ctx, "SELECT "+PlanColumns+" FROM shift_plans p WHERE p.id = $1", id))
		if err != nil {
			return err
		}
		reply = map[string]any{"plan": plan, "server_time": serverTime()}
		return idempotency.Save(ctx, tx, http.StatusOK, reply)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, reply)
}
