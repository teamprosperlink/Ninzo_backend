package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

// maxIncidentNote is the contract's IncidentNote limit.
const maxIncidentNote = 2000

var incidentKinds = map[string]bool{"support_alert": true, "problem_report": true, "arrival_review": true, "ticket": true, "signal_lost": true}

// MountIncidents adds the incident queue (v0.10.0 part C): listIncidents and getIncident
// (incidents.read), and assignIncident, addIncidentNote and closeIncident (incidents.manage).
//
// Incidents are made by triggers (migration 0044), in the same transaction as the support alert,
// problem report, arrival review or ticket they stand for; they are not made here. Every write
// needs an Idempotency-Key, is audited, and saves its reply in the same transaction.
func (s *Service) MountIncidents(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		read, manage := s.Require("incidents.read", false), s.Require("incidents.manage", false)
		g.With(read).Get("/admin/incidents", s.listIncidents)
		g.With(read).Get("/admin/incidents/{incident_id}", s.getIncident)
		g.With(manage, idem).Post("/admin/incidents/{incident_id}/assign", s.assignIncident)
		g.With(manage, idem).Post("/admin/incidents/{incident_id}/notes", s.addIncidentNote)
		g.With(manage, idem).Post("/admin/incidents/{incident_id}/close", s.closeIncident)
	})
}

// incidentSelect reads incidents the way the contract's Incident shows them. It carries the
// ids and the assignee's name only: no phone, address or customer text is ever read here.
const incidentSelect = `SELECT i.id::text, i.kind, i.source_id, i.order_id::text, i.rider_id::text, i.status, i.assigned_to::text, cu.name,
		i.opened_at, i.closed_at, (SELECT count(*) FROM incident_notes n WHERE n.incident_id = i.id)
	FROM incidents i LEFT JOIN console_users cu ON cu.id = i.assigned_to`

type incidentRow struct {
	id, kind, source, status      string
	order, rider, assignee, aname *string
	opened                        time.Time
	closed                        *time.Time
	notes                         int
}

func (x *incidentRow) targets() []any {
	return []any{&x.id, &x.kind, &x.source, &x.order, &x.rider, &x.status, &x.assignee, &x.aname, &x.opened, &x.closed, &x.notes}
}

func (x incidentRow) view() map[string]any {
	return map[string]any{"id": x.id, "kind": x.kind, "source_id": x.source, "order_id": nullable(x.order), "rider_id": nullable(x.rider), "status": x.status,
		"assigned_to": actorOrNil(x.assignee, x.aname), "opened_at": x.opened.UTC().Format(time.RFC3339Nano), "closed_at": stamp(x.closed), "notes_count": x.notes}
}

func nullable(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// IncidentView is one incident (contract: Incident).
func IncidentView(ctx context.Context, q reader, id string) (map[string]any, error) {
	var x incidentRow
	err := q.QueryRow(ctx, incidentSelect+" WHERE i.id = $1", id).Scan(x.targets()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	return x.view(), nil
}

// incidentReply is the contract's IncidentResponse: the incident and its notes, oldest first.
func incidentReply(ctx context.Context, q reader, id string) (map[string]any, error) {
	v, err := IncidentView(ctx, q, id)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT n.id::text, n.text, n.author_id::text, cu.name, n.created_at FROM incident_notes n
		JOIN console_users cu ON cu.id = n.author_id WHERE n.incident_id = $1 ORDER BY n.created_at, n.id`, id)
	if err != nil {
		return nil, err
	}
	notes := []map[string]any{}
	var nid, text, author, name string
	var at time.Time
	if _, err := pgx.ForEachRow(rows, []any{&nid, &text, &author, &name, &at}, func() error {
		notes = append(notes, map[string]any{"id": nid, "text": text, "author": map[string]any{"id": author, "name": name}, "created_at": at.UTC().Format(time.RFC3339Nano)})
		return nil
	}); err != nil {
		return nil, err
	}
	return map[string]any{"incident": v, "notes": notes}, nil
}

func (s *Service) listIncidents(w http.ResponseWriter, r *http.Request) {
	p, ok := ReadPage(r)
	status, kind := r.URL.Query().Get("status"), r.URL.Query().Get("kind")
	if !ok || (status != "" && status != "open" && status != "assigned" && status != "closed") || (kind != "" && !incidentKinds[kind]) {
		badBody(w, "Bad status, kind, cursor or limit.")
		return
	}
	if !p.HasAfter {
		p.At, p.ID = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), "ffffffff-ffff-ffff-ffff-ffffffffffff"
	}
	rows, err := s.Pool.Query(r.Context(), incidentSelect+` WHERE ($1 = '' OR i.status = $1) AND ($2 = '' OR i.kind = $2) AND (i.opened_at, i.id) < ($3, $4::uuid)
		ORDER BY i.opened_at DESC, i.id DESC LIMIT $5`, status, kind, p.At, p.ID, p.Limit+1)
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (incidentRow, error) {
		var x incidentRow
		return x, r.Scan(x.targets()...)
	})
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	var next any
	if len(list) > p.Limit {
		list = list[:p.Limit]
		next = Cursor(list[len(list)-1].opened, list[len(list)-1].id)
	}
	out := make([]map[string]any, 0, len(list))
	for _, x := range list {
		out = append(out, x.view())
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"incidents": out, "next_cursor": next, "server_time": serverTime()})
}

func (s *Service) getIncident(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(chi.URLParam(r, "incident_id"))
	if !idPattern.MatchString(id) {
		httpserver.WriteError(w, http.StatusNotFound, "not_found", "Not found.", false)
		return
	}
	reply, err := incidentReply(r.Context(), s.Pool, id)
	if errors.Is(err, errNotFound) {
		httpserver.WriteError(w, http.StatusNotFound, "not_found", "Not found.", false)
		return
	}
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	reply["server_time"] = serverTime()
	httpserver.WriteJSON(w, http.StatusOK, reply)
}

type lockedIncident struct {
	kind, source, status string
	order                *string
}

// lockIncident locks an incident row for writing. Two people acting on one incident therefore
// run one after the other, and what is read afterwards is final until the transaction ends. A
// closed incident is incident_closed (409); an unknown id is 404.
func lockIncident(ctx context.Context, tx pgx.Tx, id string) (lockedIncident, error) {
	var x lockedIncident
	err := tx.QueryRow(ctx, "SELECT kind, source_id, status, order_id::text FROM incidents WHERE id = $1 FOR UPDATE", id).Scan(&x.kind, &x.source, &x.status, &x.order)
	if errors.Is(err, pgx.ErrNoRows) {
		return x, errNotFound
	}
	if err != nil {
		return x, err
	}
	if x.status == "closed" {
		return x, &refusal{"incident_closed", "This incident is already closed."}
	}
	return x, nil
}

// assignIncident gives an incident to a console user who may take it, or takes the assignment
// away. Assigned and open follow the assignee, so the two never disagree.
func (s *Service) assignIncident(w http.ResponseWriter, r *http.Request) {
	var raw map[string]json.RawMessage
	if !decodeStrict(r, &raw) || raw == nil {
		badBody(w, "Send user_id: a console user id, or null.")
		return
	}
	value, present := raw["user_id"]
	var user *string
	if present && string(value) != "null" {
		var id string
		if json.Unmarshal(value, &id) != nil || !idPattern.MatchString(id) {
			badBody(w, "user_id must be a console user id, or null.")
			return
		}
		id = strings.ToLower(id)
		user = &id
	} else if !present {
		badBody(w, "Send user_id: a console user id, or null.")
		return
	}
	id := strings.ToLower(chi.URLParam(r, "incident_id"))
	s.change(w, r, http.StatusOK, func(ctx context.Context, tx pgx.Tx, u User) (map[string]any, error) {
		if !idPattern.MatchString(id) {
			return nil, errNotFound
		}
		if _, err := lockIncident(ctx, tx, id); err != nil {
			return nil, err
		}
		var before *string
		if err := tx.QueryRow(ctx, "SELECT assigned_to::text FROM incidents WHERE id = $1", id).Scan(&before); err != nil {
			return nil, err
		}
		status := "open"
		if user != nil {
			status = "assigned"
			// Held while the incident is updated, so the person cannot be deactivated in between.
			var active bool
			var role string
			err := tx.QueryRow(ctx, "SELECT active, role FROM console_users WHERE id = $1 FOR SHARE", *user).Scan(&active, &role)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, errNotFound
			}
			if err != nil {
				return nil, err
			}
			if !active || !Can(role, "incidents.manage") {
				return nil, &refusal{"user_cannot_take", "That person is not active or cannot manage incidents."}
			}
		}
		if _, err := tx.Exec(ctx, "UPDATE incidents SET status = $2, assigned_to = $3 WHERE id = $1", id, status, user); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: u.ID, Action: "incident.assigned", EntityType: "incident", EntityID: id,
			Before: map[string]any{"assigned_to": nullable(before)}, After: map[string]any{"assigned_to": nullable(user)}}); err != nil {
			return nil, err
		}
		return incidentReply(ctx, tx, id)
	})
}

// addIncidentNote adds staff-written text to an open incident.
func (s *Service) addIncidentNote(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if !decodeStrict(r, &body) {
		badBody(w, "Send text of 1 to 2000 characters.")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" || utf8.RuneCountInString(text) > maxIncidentNote || strings.ContainsRune(text, 0) {
		badBody(w, "Send text of 1 to 2000 characters.")
		return
	}
	id := strings.ToLower(chi.URLParam(r, "incident_id"))
	s.change(w, r, http.StatusCreated, func(ctx context.Context, tx pgx.Tx, u User) (map[string]any, error) {
		if !idPattern.MatchString(id) {
			return nil, errNotFound
		}
		if _, err := lockIncident(ctx, tx, id); err != nil {
			return nil, err
		}
		var note string
		if err := tx.QueryRow(ctx, "INSERT INTO incident_notes (incident_id, author_id, text) VALUES ($1, $2, $3) RETURNING id::text", id, u.ID, text).Scan(&note); err != nil {
			return nil, err
		}
		// The audit row keeps the id, not the words: the note is in incident_notes.
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: u.ID, Action: "incident.noted", EntityType: "incident", EntityID: id,
			After: map[string]any{"note_id": note}}); err != nil {
			return nil, err
		}
		return incidentReply(ctx, tx, id)
	})
}

// closeIncident closes an incident. A support alert or problem report has no state of its own,
// so closing its incident is what marks it handled: the alert leaves the outbox's due index
// (delivered_at) and the report gets handled_at. A lost signal's incident marks its report and
// every lost-signal alert for the order. A ticket or an arrival review is never changed here:
// while it is open the incident cannot close (source_open), and when it closes through its own
// call the incident closes by itself (trigger).
func (s *Service) closeIncident(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	if !decodeStrict(r, &body) || !validReason(body.Reason) {
		badBody(w, "Send a reason of 3 to 500 characters.")
		return
	}
	reason := strings.TrimSpace(body.Reason)
	id := strings.ToLower(chi.URLParam(r, "incident_id"))
	s.change(w, r, http.StatusOK, func(ctx context.Context, tx pgx.Tx, u User) (map[string]any, error) {
		if !idPattern.MatchString(id) {
			return nil, errNotFound
		}
		x, err := lockIncident(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if err := markHandled(ctx, tx, x); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, "UPDATE incidents SET status = 'closed', closed_at = now(), closed_by = $2, close_reason = $3 WHERE id = $1", id, u.ID, reason); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: u.ID, Action: "incident.closed", EntityType: "incident", EntityID: id,
			Before: map[string]any{"status": x.status}, After: map[string]any{"status": "closed", "kind": x.kind, "source_id": x.source}, Reason: reason}); err != nil {
			return nil, err
		}
		return incidentReply(ctx, tx, id)
	})
}

// markHandled does to the source what closing its incident means.
func markHandled(ctx context.Context, tx pgx.Tx, x lockedIncident) error {
	switch x.kind {
	case "ticket", "arrival_review":
		table := "support_tickets"
		if x.kind == "arrival_review" {
			table = "arrival_reviews"
		}
		var open bool
		if err := tx.QueryRow(ctx, "SELECT status = 'open' FROM "+table+" WHERE id = $1", x.source).Scan(&open); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if open {
			return &refusal{"source_open", "Close the ticket or decide the review first; this incident then closes by itself."}
		}
	case "support_alert":
		_, err := tx.Exec(ctx, "UPDATE outbox_events SET delivered_at = now() WHERE id = $1::bigint AND topic = 'support.alert' AND delivered_at IS NULL", x.source)
		return err
	case "problem_report":
		_, err := tx.Exec(ctx, "UPDATE problem_reports SET handled_at = now() WHERE id = $1::uuid AND handled_at IS NULL", x.source)
		return err
	case "signal_lost":
		if _, err := tx.Exec(ctx, "UPDATE problem_reports SET handled_at = now() WHERE handled_at IS NULL AND (id = $1::uuid OR (order_id = $2::uuid AND type = 'signal_lost'))",
			x.source, x.order); err != nil {
			return err
		}
		if x.order != nil {
			_, err := tx.Exec(ctx, `UPDATE outbox_events SET delivered_at = now() WHERE topic = 'support.alert' AND delivered_at IS NULL
				AND payload->>'reason' = 'signal_lost' AND payload->>'order_id' = $1`, *x.order)
			return err
		}
	}
	return nil
}
