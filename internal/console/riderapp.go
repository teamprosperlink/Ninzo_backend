package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
	"github.com/teamprosperlink/Ninzo_backend/internal/riderapp"
)

// The rider app's help articles, the feedback riders send and the day's shift plans (issue 235,
// part B2). Help and plans are written by operations; feedback is only read.

// helpSlug is the short name of an article, kept so ops can find it again. It never shows to riders.
var helpSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,59}$`)

const (
	maxQuestionRunes = 200
	maxAnswerRunes   = 4000
	maxPlanDaysAhead = 60
)

// india is the zone's local time. Plans are made per day in it, as the rider app asks for today's plan.
var india = time.FixedZone("IST", 5*3600+1800)

// MountRiderApp adds the help article calls, listRiderFeedback and the shift plan calls. Every write
// needs an Idempotency-Key and is audited.
func (s *Service) MountRiderApp(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		help := s.Require("help.manage", false)
		g.With(help).Get("/admin/help-articles", s.listHelpArticles)
		g.With(help, idem).Post("/admin/help-articles", s.createHelpArticle)
		g.With(help, idem).Patch("/admin/help-articles/{article_id}", s.updateHelpArticle)
		g.With(help, idem).Post("/admin/help-articles/{article_id}/deactivate", s.deactivateHelpArticle)
		g.With(s.Require("tickets.manage", false)).Get("/admin/rider-feedback", s.listRiderFeedback)
		plans := s.Require("shift_plans.manage", false)
		g.With(plans).Get("/admin/shift-plans", s.listShiftPlans)
		g.With(plans, idem).Post("/admin/shift-plans", s.createShiftPlan)
	})
}

// text trims and checks one line or paragraph: valid UTF-8, within the length, and no control
// characters (a paragraph may hold line breaks).
func text(s string, max int, paragraph bool) (string, bool) {
	s = strings.TrimSpace(s)
	n := utf8.RuneCountInString(s)
	if n < 1 || n > max || !utf8.ValidString(s) {
		return "", false
	}
	if strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) && !(paragraph && (r == '\n' || r == '\r' || r == '\t'))
	}) {
		return "", false
	}
	return s, true
}

const helpColumns = `a.id::text, a.audience, a.slug, a.question, a.answer, a.sort, a.active, a.created_at, a.updated_at, u.id::text, u.name`

func scanHelp(row pgx.Row) (map[string]any, error) {
	var id, audience, slug, question, answer, by, byName string
	var sort int
	var active bool
	var created, updated time.Time
	if err := row.Scan(&id, &audience, &slug, &question, &answer, &sort, &active, &created, &updated, &by, &byName); err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "audience": audience, "slug": slug, "question": question, "answer": answer, "sort": sort, "active": active,
		"created_at": created.UTC().Format(time.RFC3339Nano), "updated_at": updated.UTC().Format(time.RFC3339Nano),
		"updated_by": map[string]any{"id": by, "name": byName}}, nil
}

const helpFrom = ` FROM help_articles a JOIN console_users u ON u.id = a.updated_by`

func (s *Service) listHelpArticles(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Pool.Query(r.Context(), "SELECT "+helpColumns+helpFrom+" ORDER BY a.audience, a.sort, a.created_at, a.id")
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (map[string]any, error) { return scanHelp(row) })
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	if list == nil {
		list = []map[string]any{}
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"articles": list, "server_time": serverTime()})
}

func (s *Service) createHelpArticle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Audience *string `json:"audience"`
		Slug     string  `json:"slug"`
		Question string  `json:"question"`
		Answer   string  `json:"answer"`
		Sort     *int    `json:"sort"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || dec.More() {
		badBody(w, "Send slug, question and answer.")
		return
	}
	question, ok1 := text(body.Question, maxQuestionRunes, false)
	answer, ok2 := text(body.Answer, maxAnswerRunes, true)
	sort := 100
	if body.Sort != nil {
		sort = *body.Sort
	}
	if !ok1 || !ok2 || !helpSlug.MatchString(body.Slug) || sort < 0 || sort > 10000 || (body.Audience != nil && *body.Audience != "rider") {
		badBody(w, "Check the fields against the contract.")
		return
	}
	s.change(w, r, http.StatusCreated, func(ctx context.Context, tx pgx.Tx, u User) (map[string]any, error) {
		var id string
		err := tx.QueryRow(ctx, `INSERT INTO help_articles (audience, slug, question, answer, sort, updated_by) VALUES ('rider', $1, $2, $3, $4, $5)
			RETURNING id::text`, body.Slug, question, answer, sort, u.ID).Scan(&id)
		if uniqueViolation(err) {
			return nil, &refusal{"slug_taken", "An article already has that slug."}
		}
		if err != nil {
			return nil, err
		}
		article, err := scanHelp(tx.QueryRow(ctx, "SELECT "+helpColumns+helpFrom+" WHERE a.id = $1", id))
		if err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: u.ID, Action: "help.created", EntityType: "help_article", EntityID: id,
			After: map[string]any{"slug": body.Slug, "sort": sort, "active": true}}); err != nil {
			return nil, err
		}
		return map[string]any{"article": article}, nil
	})
}

// changeHelp runs an update or deactivation of one article: it locks the row, writes the new values
// and audits the before and after.
func (s *Service) changeHelp(w http.ResponseWriter, r *http.Request, action, reason string, set func(cur map[string]any) (question, answer string, sort int, active bool)) {
	id := strings.ToLower(chi.URLParam(r, "article_id"))
	s.change(w, r, http.StatusOK, func(ctx context.Context, tx pgx.Tx, u User) (map[string]any, error) {
		if !idPattern.MatchString(id) {
			return nil, errNotFound
		}
		before, err := scanHelp(tx.QueryRow(ctx, "SELECT "+helpColumns+helpFrom+" WHERE a.id = $1 FOR UPDATE OF a", id))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errNotFound
		}
		if err != nil {
			return nil, err
		}
		question, answer, sort, active := set(before)
		// Nothing to change: the article comes back as it is, with no new update time and no audit row.
		if question == before["question"] && answer == before["answer"] && sort == before["sort"] && active == before["active"] {
			return map[string]any{"article": before}, nil
		}
		if _, err := tx.Exec(ctx, `UPDATE help_articles SET question = $2, answer = $3, sort = $4, active = $5, updated_at = now(), updated_by = $6 WHERE id = $1`,
			id, question, answer, sort, active, u.ID); err != nil {
			return nil, err
		}
		after, err := scanHelp(tx.QueryRow(ctx, "SELECT "+helpColumns+helpFrom+" WHERE a.id = $1", id))
		if err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: u.ID, Action: action, EntityType: "help_article", EntityID: id,
			Before: map[string]any{"question": before["question"], "sort": before["sort"], "active": before["active"]},
			After:  map[string]any{"question": after["question"], "sort": after["sort"], "active": after["active"]}, Reason: reason}); err != nil {
			return nil, err
		}
		return map[string]any{"article": after}, nil
	})
}

func (s *Service) updateHelpArticle(w http.ResponseWriter, r *http.Request) {
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		badBody(w, "Send a JSON object.")
		return
	}
	var body struct {
		Question *string `json:"question"`
		Answer   *string `json:"answer"`
		Sort     *int    `json:"sort"`
		Active   *bool   `json:"active"`
		Reason   string  `json:"reason"`
	}
	buf, _ := json.Marshal(raw)
	dec := json.NewDecoder(strings.NewReader(string(buf)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || nullOn(raw, "question", "answer", "sort", "active", "reason") || !validReason(body.Reason) {
		badBody(w, "Check the fields against the contract.")
		return
	}
	var question, answer string
	var ok bool
	if body.Question != nil {
		if question, ok = text(*body.Question, maxQuestionRunes, false); !ok {
			badBody(w, "question is 1 to 200 characters.")
			return
		}
	}
	if body.Answer != nil {
		if answer, ok = text(*body.Answer, maxAnswerRunes, true); !ok {
			badBody(w, "answer is 1 to 4000 characters.")
			return
		}
	}
	if body.Sort != nil && (*body.Sort < 0 || *body.Sort > 10000) {
		badBody(w, "sort is 0 to 10000.")
		return
	}
	if body.Question == nil && body.Answer == nil && body.Sort == nil && body.Active == nil {
		badBody(w, "Send at least one field to change.")
		return
	}
	s.changeHelp(w, r, "help.updated", strings.TrimSpace(body.Reason), func(cur map[string]any) (string, string, int, bool) {
		q, a, so, ac := cur["question"].(string), cur["answer"].(string), cur["sort"].(int), cur["active"].(bool)
		if body.Question != nil {
			q = question
		}
		if body.Answer != nil {
			a = answer
		}
		if body.Sort != nil {
			so = *body.Sort
		}
		if body.Active != nil {
			ac = *body.Active
		}
		return q, a, so, ac
	})
}

func (s *Service) deactivateHelpArticle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&body); err != nil || dec.More() || !validReason(body.Reason) {
		badBody(w, "Send a reason of 3 to 500 characters.")
		return
	}
	s.changeHelp(w, r, "help.deactivated", strings.TrimSpace(body.Reason), func(cur map[string]any) (string, string, int, bool) {
		return cur["question"].(string), cur["answer"].(string), cur["sort"].(int), false
	})
}

// ---- Feedback ----

func (s *Service) listRiderFeedback(w http.ResponseWriter, r *http.Request) {
	p, ok := ReadPage(r)
	rider := strings.ToLower(r.URL.Query().Get("rider_id"))
	if !ok || (rider != "" && !idPattern.MatchString(rider)) {
		badBody(w, "Bad rider_id, cursor or limit.")
		return
	}
	if !p.HasAfter {
		p.At, p.ID = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), "ffffffff-ffff-ffff-ffff-ffffffffffff"
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT f.id::text, r.id::text, coalesce(r.name, ''), f.message, f.created_at
		FROM rider_feedback f JOIN riders r ON r.id = f.rider_id
		WHERE ($1 = '' OR f.rider_id::text = $1) AND (f.created_at, f.id) < ($2, $3::uuid)
		ORDER BY f.created_at DESC, f.id DESC LIMIT $4`, rider, p.At, p.ID, p.Limit+1)
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	type row struct {
		id, riderID, name, message string
		at                         time.Time
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.id, &x.riderID, &x.name, &x.message, &x.at)
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
	out := make([]map[string]any, 0, len(list))
	for _, x := range list {
		out = append(out, map[string]any{"id": x.id, "rider": map[string]any{"id": x.riderID, "name": x.name}, "message": x.message,
			"created_at": x.at.UTC().Format(time.RFC3339Nano)})
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"feedback": out, "next_cursor": next, "server_time": serverTime()})
}

// ---- Shift plans ----

const planFrom = ` FROM shift_plans p JOIN riders r ON r.id = p.rider_id`

// scanAdminPlan is the plan the rider sees, with the rider and when it was made.
func scanAdminPlan(row pgx.Row) (map[string]any, error) {
	var riderID, riderName string
	var created time.Time
	var plan map[string]any
	var id, date, reply, packing string
	var node *string
	var starts, ends time.Time
	var replied, ready *time.Time
	if err := row.Scan(&id, &date, &node, &starts, &ends, &reply, &replied, &packing, &ready, &riderID, &riderName, &created); err != nil {
		return nil, err
	}
	stamp := func(t *time.Time) any {
		if t == nil {
			return nil
		}
		return t.UTC().Format(time.RFC3339Nano)
	}
	plan = map[string]any{"id": id, "date": date, "node_id": node, "starts_at": starts.UTC().Format(time.RFC3339Nano), "ends_at": ends.UTC().Format(time.RFC3339Nano),
		"stock_reply": reply, "replied_at": stamp(replied), "packing": packing, "ready_at": stamp(ready),
		"rider": map[string]any{"id": riderID, "name": riderName}, "created_at": created.UTC().Format(time.RFC3339Nano)}
	return plan, nil
}

const adminPlanColumns = riderapp.PlanColumns + `, r.id::text, coalesce(r.name, ''), p.created_at`

func (s *Service) listShiftPlans(w http.ResponseWriter, r *http.Request) {
	p, ok := ReadPage(r)
	q := r.URL.Query()
	rider, date := strings.ToLower(q.Get("rider_id")), q.Get("date")
	if _, err := time.Parse("2006-01-02", date); date != "" && err != nil {
		ok = false
	}
	if !ok || (rider != "" && !idPattern.MatchString(rider)) {
		badBody(w, "Bad rider_id, date, cursor or limit.")
		return
	}
	if !p.HasAfter {
		p.At, p.ID = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), "ffffffff-ffff-ffff-ffff-ffffffffffff"
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT `+adminPlanColumns+planFrom+`
		WHERE ($1 = '' OR p.rider_id::text = $1) AND ($2 = '' OR p.plan_date::text = $2) AND (p.created_at, p.id) < ($3, $4::uuid)
		ORDER BY p.created_at DESC, p.id DESC LIMIT $5`, rider, date, p.At, p.ID, p.Limit+1)
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (map[string]any, error) { return scanAdminPlan(row) })
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	var next any
	if len(list) > p.Limit {
		list = list[:p.Limit]
		created, _ := time.Parse(time.RFC3339Nano, list[len(list)-1]["created_at"].(string))
		next = Cursor(created, list[len(list)-1]["id"].(string))
	}
	if list == nil {
		list = []map[string]any{}
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"plans": list, "next_cursor": next, "server_time": serverTime()})
}

// createShiftPlan makes one rider's plan for one day. Plans are informational: shifts stay open-ended,
// and nothing starts, ends or schedules a shift by one.
func (s *Service) createShiftPlan(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RiderID  string  `json:"rider_id"`
		Date     string  `json:"date"`
		NodeID   *string `json:"node_id"`
		StartsAt string  `json:"starts_at"`
		EndsAt   string  `json:"ends_at"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || dec.More() {
		badBody(w, "Send rider_id, date, starts_at and ends_at.")
		return
	}
	body.RiderID = strings.ToLower(body.RiderID)
	date, err1 := time.ParseInLocation("2006-01-02", body.Date, india)
	starts, err2 := time.Parse(time.RFC3339Nano, body.StartsAt)
	ends, err3 := time.Parse(time.RFC3339Nano, body.EndsAt)
	if err1 != nil || err2 != nil || err3 != nil || !idPattern.MatchString(body.RiderID) || (body.NodeID != nil && !idPattern.MatchString(*body.NodeID)) ||
		!ends.After(starts) || ends.Sub(starts) > 24*time.Hour ||
		starts.In(india).Format("2006-01-02") != body.Date {
		badBody(w, "Send a rider_id, a date, and a start on that day (India time) with an end after it, up to 24 hours later.")
		return
	}
	var node any
	if body.NodeID != nil {
		node = strings.ToLower(*body.NodeID)
	}
	s.change(w, r, http.StatusCreated, func(ctx context.Context, tx pgx.Tx, u User) (map[string]any, error) {
		today := time.Now().In(india).Format("2006-01-02")
		last := time.Now().In(india).AddDate(0, 0, maxPlanDaysAhead).Format("2006-01-02")
		if body.Date < today || body.Date > last {
			return nil, errInvalid // A plan is for today or a day to come, up to 60 days ahead.
		}
		var zone *string
		if err := tx.QueryRow(ctx, "SELECT zone_id::text FROM riders WHERE id = $1", body.RiderID).Scan(&zone); errors.Is(err, pgx.ErrNoRows) {
			return nil, errNotFound
		} else if err != nil {
			return nil, err
		}
		if node != nil {
			var nodeZone string
			err := tx.QueryRow(ctx, "SELECT zone_id::text FROM staging_nodes WHERE id = $1", node).Scan(&nodeZone)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && (zone == nil || *zone != nodeZone)) {
				return nil, errInvalid // A node that does not exist, or is in another zone than the rider's.
			}
			if err != nil {
				return nil, err
			}
		}
		var id string
		err := tx.QueryRow(ctx, `INSERT INTO shift_plans (rider_id, plan_date, node_id, starts_at, ends_at, created_by) VALUES ($1, $2::date, $3, $4, $5, $6)
			RETURNING id::text`, body.RiderID, date.Format("2006-01-02"), node, starts, ends, u.ID).Scan(&id)
		if uniqueViolation(err) {
			return nil, &refusal{"shift_plan_exists", "That rider already has a plan for that day."}
		}
		if err != nil {
			return nil, err
		}
		plan, err := scanAdminPlan(tx.QueryRow(ctx, "SELECT "+adminPlanColumns+planFrom+" WHERE p.id = $1", id))
		if err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeConsole, ActorID: u.ID, Action: "shift_plan.created", EntityType: "shift_plan", EntityID: id,
			After: map[string]any{"rider_id": body.RiderID, "date": body.Date, "node_id": node}}); err != nil {
			return nil, err
		}
		return map[string]any{"plan": plan}, nil
	})
}
