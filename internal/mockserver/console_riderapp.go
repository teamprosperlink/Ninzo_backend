package mockserver

import (
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// The console's calls for what the rider app shows (issue 235, part B2): help articles, the feedback riders
// send, and shift plans. The mock keeps nothing: each call answers a fixed sample, shaped like the real reply.

const (
	helpArticleID = "hlp_mock01"
	feedbackID    = "fbk_mock01"
	shiftPlanID   = "plan_mock01"
)

var mockHelpSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,59}$`)

func mockHelpArticle(slug, question, answer string, sort int, active bool, now time.Time) map[string]any {
	return map[string]any{"id": helpArticleID, "audience": "rider", "slug": slug, "question": question, "answer": answer, "sort": sort, "active": active,
		"created_at": stamp(now.Add(-72 * time.Hour)), "updated_at": stamp(now.Add(-time.Hour)), "updated_by": map[string]any{"id": consoleUserID, "name": "Asha K"}}
}

func mockAdminShiftPlan(reply string, now time.Time) map[string]any {
	var replied any
	if reply != "pending" {
		replied = stamp(now.Add(-30 * time.Minute))
	}
	day := now.In(time.FixedZone("IST", 5*3600+1800)).Format("2006-01-02")
	return map[string]any{"id": shiftPlanID, "rider": map[string]any{"id": "rdr_mock0001", "name": "Ravi"}, "date": day, "node_id": "node_mock01",
		"starts_at": stamp(now.Add(2 * time.Hour)), "ends_at": stamp(now.Add(8 * time.Hour)), "stock_reply": reply, "replied_at": replied,
		"packing": "not_started", "ready_at": nil, "created_at": stamp(now.Add(-2 * time.Hour))}
}

// riderAppRoutes are the console operations for the rider app's help, feedback and shift plans.
func riderAppRoutes(denied func(*call) bool, fail func(*call, ...string) bool) []route {
	return []route{
		{method: "GET", path: "/admin/help-articles", handle: func(c *call) {
			if denied(c) {
				return
			}
			articles := []map[string]any{
				mockHelpArticle("gate-closed", "What if the gate is closed?", "Tap Report a problem, then Gate is closed. Support will help.", 10, true, c.now),
				mockHelpArticle("when-paid", "When am I paid?", "Your earnings for the day show under Earnings.", 20, false, c.now),
			}
			articles[1]["id"] = "hlp_mock02"
			if c.scenario == "help_empty" {
				articles = []map[string]any{}
			}
			c.json(200, map[string]any{"articles": articles})
		}},
		{method: "POST", path: "/admin/help-articles", handle: func(c *call) {
			if denied(c) || fail(c, "slug_taken") {
				return
			}
			body, ok := needsBody(c, "slug", "question", "answer")
			if !ok {
				return
			}
			slug, _ := body["slug"].(string)
			question, _ := body["question"].(string)
			answer, _ := body["answer"].(string)
			sort := 100
			if v, has := body["sort"]; has {
				n, isNumber := v.(float64)
				if !isNumber || n != float64(int(n)) || n < 0 || n > 10000 {
					c.fail(http.StatusBadRequest, "invalid_request", "sort is 0 to 10000.", false)
					return
				}
				sort = int(n)
			}
			if !mockHelpSlug.MatchString(slug) || utf8.RuneCountInString(strings.TrimSpace(question)) < 1 || utf8.RuneCountInString(question) > 200 ||
				utf8.RuneCountInString(strings.TrimSpace(answer)) < 1 || utf8.RuneCountInString(answer) > 4000 {
				c.fail(http.StatusBadRequest, "invalid_request", "Check slug, question and answer against the contract.", false)
				return
			}
			c.json(201, map[string]any{"article": mockHelpArticle(slug, question, answer, sort, true, c.now)})
		}},
		{method: "PATCH", path: "/admin/help-articles/{article_id}", handle: func(c *call) {
			if denied(c) {
				return
			}
			body, ok := needsBody(c, "reason")
			if !ok {
				return
			}
			if len(body) < 2 {
				c.fail(http.StatusBadRequest, "invalid_request", "Send at least one field to change.", false)
				return
			}
			article := mockHelpArticle("gate-closed", "What if the gate is closed?", "Tap Report a problem, then Gate is closed. Support will help.", 10, true, c.now)
			if v, is := body["question"].(string); is {
				article["question"] = v
			}
			if v, is := body["answer"].(string); is {
				article["answer"] = v
			}
			if v, is := body["active"].(bool); is {
				article["active"] = v
			}
			c.json(200, map[string]any{"article": article})
		}},
		{method: "POST", path: "/admin/help-articles/{article_id}/deactivate", handle: func(c *call) {
			if denied(c) {
				return
			}
			if _, ok := needsBody(c, "reason"); ok {
				c.json(200, map[string]any{"article": mockHelpArticle("gate-closed", "What if the gate is closed?", "Tap Report a problem, then Gate is closed. Support will help.", 10, false, c.now)})
			}
		}},
		{method: "GET", path: "/admin/rider-feedback", handle: func(c *call) {
			if denied(c) {
				return
			}
			list := []map[string]any{{"id": feedbackID, "rider": map[string]any{"id": "rdr_mock0001", "name": "Ravi"}, "message": "The map is hard to read at night.",
				"created_at": stamp(c.now.Add(-10 * time.Minute))}}
			if c.scenario == "feedback_empty" {
				list = []map[string]any{}
			}
			c.json(200, map[string]any{"feedback": list, "next_cursor": nil})
		}},
		{method: "GET", path: "/admin/shift-plans", handle: func(c *call) {
			if denied(c) {
				return
			}
			plans := []map[string]any{mockAdminShiftPlan("coming", c.now)}
			if c.scenario == "shift_plans_empty" {
				plans = []map[string]any{}
			}
			c.json(200, map[string]any{"plans": plans, "next_cursor": nil})
		}},
		{method: "POST", path: "/admin/shift-plans", handle: func(c *call) {
			if denied(c) || fail(c, "shift_plan_exists") {
				return
			}
			body, ok := needsBody(c, "rider_id", "date", "starts_at", "ends_at")
			if !ok {
				return
			}
			starts, err1 := time.Parse(time.RFC3339Nano, strOf(body["starts_at"]))
			ends, err2 := time.Parse(time.RFC3339Nano, strOf(body["ends_at"]))
			if _, err := time.Parse("2006-01-02", strOf(body["date"])); err != nil || err1 != nil || err2 != nil || !ends.After(starts) || ends.Sub(starts) > 24*time.Hour {
				c.fail(http.StatusBadRequest, "invalid_request", "Send a date, and a start and end up to 24 hours apart.", false)
				return
			}
			plan := mockAdminShiftPlan("pending", c.now)
			plan["date"], plan["starts_at"], plan["ends_at"] = body["date"], stamp(starts), stamp(ends)
			plan["node_id"] = body["node_id"]
			c.json(201, map[string]any{"plan": plan})
		}},
	}
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}
