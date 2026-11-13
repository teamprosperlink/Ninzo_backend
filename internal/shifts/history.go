package shifts

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/earnings"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

// The cursor is the last shift's start and id, so a new shift never moves a page.
func encodeCursor(t time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(t.UnixMicro(), 10) + "|" + id))
}

func decodeCursor(raw string) (time.Time, string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return time.Time{}, "", false
	}
	micros, id, ok := strings.Cut(string(b), "|")
	n, err := strconv.ParseInt(micros, 10, 64)
	if !ok || err != nil || !idPattern.MatchString(id) {
		return time.Time{}, "", false
	}
	return time.UnixMicro(n), id, true
}

// seconds is a length of time in whole seconds, never below zero.
func seconds(d time.Duration) int64 {
	if d < 0 {
		return 0
	}
	return int64(d / time.Second)
}

// listRiderShifts lists the rider's shifts, newest first. online_s is the length minus the breaks,
// and an open shift is counted up to now.
func (s *Service) listRiderShifts(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	q := r.URL.Query()
	limit := 20
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "limit is from 1 to 100.", false)
			return
		}
		limit = n
	}
	now := s.clock()
	afterAt, afterID := now.Add(time.Hour), "ffffffff-ffff-ffff-ffff-ffffffffffff"
	if raw := q.Get("cursor"); raw != "" {
		var ok bool
		if afterAt, afterID, ok = decodeCursor(raw); !ok {
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "That cursor is not valid.", false)
			return
		}
	}
	// from and to are days in the zone's local time. A shift is in range if it started in them.
	var from, until *time.Time
	if raw := q.Get("from"); raw != "" {
		d, err := time.ParseInLocation(time.DateOnly, raw, earnings.Local)
		if err != nil {
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "from is a date such as 2026-11-10.", false)
			return
		}
		from = &d
	}
	if raw := q.Get("to"); raw != "" {
		d, err := time.ParseInLocation(time.DateOnly, raw, earnings.Local)
		if err != nil {
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "to is a date such as 2026-11-10.", false)
			return
		}
		end := d.AddDate(0, 0, 1)
		until = &end
	}
	if from != nil && until != nil && !from.Before(*until) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "from must not be after to.", false)
		return
	}
	// days is the rider app's way to ask (Issue 235): the last days days in India time, today
	// included, all on one page. It cannot be mixed with from or to.
	byDays := false
	if raw := q.Get("days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxDays || from != nil || until != nil || q.Get("cursor") != "" || q.Has("limit") {
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "days is from 1 to 31, and not with from, to, cursor or limit.", false)
			return
		}
		today := now.In(earnings.Local)
		first := time.Date(today.Year(), today.Month(), today.Day()-(n-1), 0, 0, 0, 0, earnings.Local)
		from, byDays, limit = &first, true, maxDaysShifts
		afterAt, afterID = now.Add(time.Hour), "ffffffff-ffff-ffff-ffff-ffffffffffff"
	}
	if _, err := liveRider(r.Context(), s.Pool, actor); err != nil {
		s.fail(w, r, err)
		return
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT sh.id::text, sh.node_id::text, sh.started_at, sh.ended_at,
			coalesce((SELECT sum(extract(epoch FROM coalesce(b.ended_at, $2::timestamptz) - b.started_at)) FROM shift_breaks b WHERE b.shift_id = sh.id), 0)::bigint,
			d.handed, d.portions, d.avg_s, sh.online_at, sh.planned_end_at
		FROM shifts sh
		CROSS JOIN LATERAL (SELECT count(*) AS handed,
				coalesce(sum((SELECT coalesce(sum(i.qty_handed), 0) FROM order_items i WHERE i.order_id = o.id)), 0)::bigint AS portions,
				avg(extract(epoch FROM o.handed_over_at - o.rider_accepted_at)) FILTER (WHERE o.rider_accepted_at IS NOT NULL) AS avg_s
			FROM orders o WHERE o.rider_id = sh.rider_id AND o.handed_over_at >= sh.started_at
				AND o.handed_over_at <= coalesce(sh.ended_at, $2::timestamptz)) d
		WHERE sh.rider_id = $1 AND (sh.started_at, sh.id) < ($3, $4::uuid)
			AND ($5::timestamptz IS NULL OR sh.started_at >= $5) AND ($6::timestamptz IS NULL OR sh.started_at < $6)
		ORDER BY sh.started_at DESC, sh.id DESC LIMIT $7`, actor.ID, now, afterAt, afterID, from, until, limit+1)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	type entry struct {
		id, node       string
		started        time.Time
		ended          *time.Time
		breaks, handed int64
		portions       int64
		avg            *float64
		online, plan   *time.Time
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (entry, error) {
		var e entry
		err := row.Scan(&e.id, &e.node, &e.started, &e.ended, &e.breaks, &e.handed, &e.portions, &e.avg, &e.online, &e.plan)
		return e, err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var next any
	if len(found) > limit {
		found = found[:limit] // With days this is the 1000-shift ceiling, and there is no next page.
		if !byDays {
			last := found[len(found)-1]
			next = encodeCursor(last.started, last.id)
		}
	}
	out := make([]map[string]any, 0, len(found))
	for _, e := range found {
		end := now
		if e.ended != nil {
			end = *e.ended
		}
		out = append(out, map[string]any{"id": e.id, "node_id": e.node, "started_at": stamp(e.started), "ended_at": optStamp(e.ended),
			"online_s": max(0, seconds(end.Sub(e.started))-e.breaks), "break_s": e.breaks, "orders_handed_over": e.handed,
			"online_at": optStamp(e.online), "planned_end_at": optStamp(e.plan), "orders": e.handed, "portions": e.portions, "avg_delivery_s": avgSeconds(e.avg)})
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"shifts": out, "next_cursor": next, "server_time": serverTime()})
}

// getShiftSummary totals one of the rider's shifts. On an open shift the figures count up to now,
// and items_returned is empty until the shift ends.
func (s *Service) getShiftSummary(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	id := chi.URLParam(r, "shift_id")
	if _, err := liveRider(r.Context(), s.Pool, actor); err != nil {
		s.fail(w, r, err)
		return
	}
	if !idPattern.MatchString(id) {
		s.fail(w, r, errNotFound)
		return
	}
	ctx, now := r.Context(), s.clock()
	var rider string
	var started time.Time
	var ended *time.Time
	var returned []byte
	// Another rider's shift is not found, never forbidden, so ids reveal nothing.
	err := s.Pool.QueryRow(ctx, "SELECT rider_id::text, started_at, ended_at, returned_items FROM shifts WHERE id = $1", id).Scan(&rider, &started, &ended, &returned)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && rider != actor.ID) {
		s.fail(w, r, errNotFound)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	end := now
	if ended != nil {
		end = *ended
	}
	var breaks, handed int64
	var avg *float64
	var paise int64
	if err := s.Pool.QueryRow(ctx, `SELECT
			coalesce((SELECT sum(extract(epoch FROM coalesce(b.ended_at, $3::timestamptz) - b.started_at)) FROM shift_breaks b WHERE b.shift_id = $1), 0)::bigint,
			(SELECT count(*) FROM orders o WHERE o.rider_id = $2 AND o.handed_over_at >= $4 AND o.handed_over_at <= $3),
			(SELECT avg(extract(epoch FROM o.handed_over_at - o.rider_accepted_at)) FROM orders o WHERE o.rider_id = $2
				AND o.handed_over_at >= $4 AND o.handed_over_at <= $3 AND o.rider_accepted_at IS NOT NULL),
			coalesce((SELECT sum(e.amount_paise) FROM earnings e WHERE e.rider_id = $2 AND e.earned_at >= $4 AND e.earned_at <= $3), 0)::bigint`,
		id, actor.ID, end, started).Scan(&breaks, &handed, &avg, &paise); err != nil {
		s.fail(w, r, err)
		return
	}
	// What was sold is what was handed over, not what was ordered.
	rows, err := s.Pool.Query(ctx, `SELECT i.item_id::text, i.name, sum(i.qty_handed)::int FROM order_items i JOIN orders o ON o.id = i.order_id
		WHERE o.rider_id = $1 AND o.handed_over_at >= $2 AND o.handed_over_at <= $3 AND i.qty_handed > 0
		GROUP BY i.item_id, i.name ORDER BY i.name, i.item_id`, actor.ID, started, end)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sold, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (map[string]any, error) {
		var item, name string
		var qty int
		err := row.Scan(&item, &name, &qty)
		return map[string]any{"item_id": item, "name": name, "qty": qty}, err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	back, err := returnedItems(returned, ended != nil)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var avgDelivery any
	if avg != nil {
		avgDelivery = int64(*avg + 0.5)
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"shift_id": id, "started_at": stamp(started), "ended_at": optStamp(ended),
		"online_s": max(0, seconds(end.Sub(started))-breaks), "break_s": breaks, "orders_handed_over": handed, "avg_delivery_s": avgDelivery,
		"items_sold": sold, "items_returned": back, "earnings": map[string]any{"amount_minor": paise, "currency": "INR"}, "server_time": serverTime()})
}

// maxDays is the longest stretch the rider app's days may ask for.
const maxDays = 31

// maxDaysShifts is the most shifts a days reply holds. 31 days of real shifts never come near it.
const maxDaysShifts = 1000

// avgSeconds is an average of seconds, rounded to whole seconds, or null when there was nothing to average.
func avgSeconds(avg *float64) any {
	if avg == nil {
		return nil
	}
	return int64(*avg + 0.5)
}
