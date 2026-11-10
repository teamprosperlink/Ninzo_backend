package hub

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
	"github.com/teamprosperlink/Ninzo_backend/internal/orders"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
	"github.com/teamprosperlink/Ninzo_backend/internal/restock"
)

// The hub's estimates until a hub sets its own slot: packing takes packFor, and the rider should
// come riderAfter the slot. They match the simulator's.
const (
	packFor    = 5 * time.Minute
	riderAfter = 10 * time.Minute
)

var idPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

var errNotFound = errors.New("hub: not found")

// refusal is a 409 the contract names. lot is set for a refused scan.
type refusal struct {
	code, message string
	lot           any
	withLot       bool
}

func (r *refusal) Error() string { return "hub: " + r.code }

func refuse(code, message string) error { return &refusal{code: code, message: message} }

// queueStates are a hub's requests still in progress.
var queueStates = []string{"confirmed", "hub_accepted", "packing", "ready", "partial", "picked_up"}

var allStates = map[string]bool{"confirmed": true, "hub_accepted": true, "packing": true, "ready": true, "partial": true, "picked_up": true,
	"received": true, "closed": true, "expired": true, "cancelled": true}

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func stamp(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// MountQueue adds the restock queue: list, get, accept, pick list, scan and unscan.
func (s *Service) MountQueue(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.Get("/hub/restock", s.list)
		g.Get("/hub/restock/{request_id}", s.get)
		g.Get("/hub/restock/{request_id}/pick-list", s.pickList)
		g.With(idem).Post("/hub/restock/{request_id}/accept", s.accept)
		g.With(idem).Post("/hub/restock/{request_id}/scan", s.scan)
		g.With(idem).Post("/hub/restock/{request_id}/unscan", s.unscan)
	})
}

// View is one request as hub staff see it (contract: HubRequest). It holds no rider details.
func View(ctx context.Context, q querier, id string) (map[string]any, error) {
	var state, scooter string
	var seq int64
	var slot, arrive *time.Time
	var created time.Time
	var acceptedBy *string
	err := q.QueryRow(ctx, `SELECT r.state, r.seq, c.code, r.slot_at, r.arrive_by, r.created_at, s.name FROM restock_requests r
		JOIN scooters c ON c.id = r.scooter_id LEFT JOIN hub_staff s ON s.id = r.accepted_by WHERE r.id = $1`, id).
		Scan(&state, &seq, &scooter, &slot, &arrive, &created, &acceptedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT w.item_id::text, m.name, w.qty, coalesce((SELECT sum(l.qty) FROM restock_lots x JOIN lots l ON l.id = x.lot_id
			WHERE x.request_id = w.request_id AND x.released_at IS NULL AND l.item_id = w.item_id), 0)::int
		FROM restock_lines w JOIN menu_items m ON m.id = w.item_id WHERE w.request_id = $1 ORDER BY m.sort_order, w.item_id`, id)
	if err != nil {
		return nil, err
	}
	items := []map[string]any{}
	var item, name string
	var qty, picked int
	if _, err := pgx.ForEachRow(rows, []any{&item, &name, &qty, &picked}, func() error {
		items = append(items, map[string]any{"item_id": item, "name": name, "qty": qty, "picked_qty": picked})
		return nil
	}); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, "SELECT l.code FROM restock_lots x JOIN lots l ON l.id = x.lot_id WHERE x.request_id = $1 AND x.released_at IS NULL ORDER BY x.created_at, l.code", id)
	if err != nil {
		return nil, err
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	shortfall, err := shortfallView(ctx, q, id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "state": state, "seq": max(seq, 1), "scooter_code": scooter, "items": items, "lot_numbers": codes,
		"slot_at": stamp(slot), "rider_arrival_at": stamp(arrive), "accepted_by": acceptedBy, "shortfall": shortfall,
		"created_at": created.UTC().Format(time.RFC3339Nano)}, nil
}

// shortfallView is the request's shortfall with its short lines.
func shortfallView(ctx context.Context, q querier, id string) (any, error) {
	var action, status string
	var sub, other, otherName *string
	err := q.QueryRow(ctx, `SELECT s.action, s.status, s.substitute_item_id::text, s.other_hub_id::text, h.name FROM shortfalls s
		LEFT JOIN hubs h ON h.id = s.other_hub_id WHERE s.request_id = $1`, id).Scan(&action, &status, &sub, &other, &otherName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, "SELECT item_id::text, qty_missing FROM shortfall_lines WHERE request_id = $1 ORDER BY item_id", id)
	if err != nil {
		return nil, err
	}
	lines := []map[string]any{}
	var item string
	var missing int
	if _, err := pgx.ForEachRow(rows, []any{&item, &missing}, func() error {
		lines = append(lines, map[string]any{"item_id": item, "qty_missing": missing})
		return nil
	}); err != nil {
		return nil, err
	}
	var otherHub any
	if other != nil {
		otherHub = map[string]any{"id": *other, "name": *otherName}
	}
	return map[string]any{"action": action, "lines": lines, "substitute_item_id": sub, "other_hub": otherHub, "status": status}, nil
}

// HubSeq is the hub's seq, raised by every change to one of its requests.
func HubSeq(ctx context.Context, q querier, hub string) (int64, error) {
	var seq int64
	err := q.QueryRow(ctx, "SELECT coalesce((SELECT seq FROM hub_state WHERE hub_id = $1), 0)", hub).Scan(&seq)
	return seq, err
}

// Bump records a change to a request: its seq, its hub's, and the rider's, who sees it too.
func Bump(ctx context.Context, tx pgx.Tx, request string) error {
	var rider string
	if err := tx.QueryRow(ctx, "SELECT rider_id::text FROM restock_requests WHERE id = $1", request).Scan(&rider); err != nil {
		return err
	}
	return restock.BumpRider(ctx, tx, rider, request)
}

func (s *Service) failQueue(w http.ResponseWriter, r *http.Request, err error) {
	var no *refusal
	switch {
	case errors.As(err, &no) && no.withLot:
		httpserver.WriteJSON(w, http.StatusConflict, map[string]any{"code": no.code, "message": no.message, "retryable": false, "lot": no.lot, "server_time": serverTime()})
	case errors.As(err, &no):
		httpserver.WriteError(w, http.StatusConflict, no.code, no.message, false)
	case errors.Is(err, errNotFound):
		httpserver.WriteError(w, http.StatusNotFound, "not_found", "Not found at this hub.", false)
	case errors.Is(err, errInvalid):
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Check the fields against the contract.", false)
	case retryable(err):
		w.Header().Set("Retry-After", "1")
		httpserver.WriteError(w, http.StatusServiceUnavailable, "service_unavailable", "Another change was saved at the same moment. Try again.", true)
	default:
		s.fail(w, r, err)
	}
}

func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	state := r.URL.Query().Get("state")
	if state != "" && !allStates[state] {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Unknown state.", false)
		return
	}
	st, err := s.staff(r, actor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// The seq is read first, so any change after it raises the seq again and the app fetches.
	seq, err := HubSeq(r.Context(), s.Pool, st.HubID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	states := queueStates
	if state != "" {
		states = []string{state}
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT id::text FROM restock_requests WHERE hub_id = $1 AND state = ANY($2)
		ORDER BY state <> 'confirmed', arrive_by NULLS LAST, created_at, id LIMIT 200`, st.HubID, states)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		v, err := View(r.Context(), s.Pool, id)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		out = append(out, v)
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"requests": out, "seq": seq, "server_time": serverTime()})
}

// own reads a request at the staff member's hub. One at another hub is not found.
func own(ctx context.Context, q querier, id, hub string) error {
	if !idPattern.MatchString(id) {
		return errNotFound
	}
	var at string
	err := q.QueryRow(ctx, "SELECT hub_id::text FROM restock_requests WHERE id = $1 AND state <> 'suggested'", id).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && at != hub) {
		return errNotFound
	}
	return err
}

func (s *Service) get(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	st, err := s.staff(r, actor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	id := chi.URLParam(r, "request_id")
	if err := own(r.Context(), s.Pool, id, st.HubID); err != nil {
		s.failQueue(w, r, err)
		return
	}
	v, err := View(r.Context(), s.Pool, id)
	if err != nil {
		s.failQueue(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"request": v, "server_time": serverTime()})
}

// change runs one write on a request at the staff member's hub, under the request's row lock,
// and replies with the request.
func (s *Service) change(w http.ResponseWriter, r *http.Request, act func(ctx context.Context, tx pgx.Tx, st Staff, id, state string) (map[string]any, error)) {
	actor, _ := identity.ActorFrom(r.Context())
	id := chi.URLParam(r, "request_id")
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		if err := identity.CheckSession(r.Context(), tx, actor); err != nil {
			return err
		}
		st, err := staffIn(r.Context(), tx, actor.ID)
		if err != nil {
			return err
		}
		if !idPattern.MatchString(id) {
			return errNotFound
		}
		var state, hub string
		err = tx.QueryRow(r.Context(), "SELECT state, hub_id::text FROM restock_requests WHERE id = $1 AND state <> 'suggested' FOR UPDATE", id).Scan(&state, &hub)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && hub != st.HubID) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		extra, err := act(r.Context(), tx, st, id, state)
		if err != nil {
			return err
		}
		v, err := View(r.Context(), tx, id)
		if err != nil {
			return err
		}
		reply = map[string]any{"request": v, "server_time": serverTime()}
		for k, val := range extra {
			reply[k] = val
		}
		return idempotency.Save(r.Context(), tx, http.StatusOK, reply)
	})
	if err != nil {
		s.failQueue(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, reply)
}

// staffIn reads the staff member inside a change, and refuses one who may not act.
func staffIn(ctx context.Context, tx pgx.Tx, id string) (Staff, error) {
	var st Staff
	var status, person string
	err := tx.QueryRow(ctx, `SELECT s.id::text, s.name, s.role, s.status, p.status, h.id::text, h.name, h.status = 'active'
		FROM hub_staff s JOIN people p ON p.id = s.person_id JOIN hubs h ON h.id = s.hub_id WHERE s.id = $1`, id).
		Scan(&st.ID, &st.Name, &st.Role, &status, &person, &st.HubID, &st.HubName, &st.HubOpen)
	if errors.Is(err, pgx.ErrNoRows) {
		return Staff{}, identity.ErrSessionInvalid
	}
	if err == nil && (person != "active" || status != "active") {
		err = errBlocked
	}
	return st, err
}

// AcceptTx accepts a request for the staff member's hub: one acceptance only. It sets the slot
// and when the rider should come. The same staff member accepting again gets it back.
func AcceptTx(ctx context.Context, tx pgx.Tx, st Staff, id, state string, now time.Time) error {
	var by *string
	if err := tx.QueryRow(ctx, "SELECT accepted_by::text FROM restock_requests WHERE id = $1", id).Scan(&by); err != nil {
		return err
	}
	switch {
	case state == "cancelled" || state == "expired":
		return refuse("request_withdrawn", "The rider withdrew this request.")
	case state != "confirmed" && by != nil && *by == st.ID:
		return nil
	case state != "confirmed":
		return refuse("request_taken", "Someone else has this request.")
	case !st.HubOpen:
		return refuse("hub_closed", "The hub is paused.")
	}
	slot := now.Add(packFor)
	if _, err := tx.Exec(ctx, `UPDATE restock_requests SET state = 'hub_accepted', accepted_by = $2, slot_at = $3, arrive_by = $4, state_changed_at = $5
		WHERE id = $1`, id, st.ID, slot, slot.Add(riderAfter), now); err != nil {
		return err
	}
	if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeStaff, ActorID: st.ID, Action: "restock.hub_accepted", EntityType: "restock_request", EntityID: id}); err != nil {
		return err
	}
	return Bump(ctx, tx, id)
}

func (s *Service) accept(w http.ResponseWriter, r *http.Request) {
	s.change(w, r, func(ctx context.Context, tx pgx.Tx, st Staff, id, state string) (map[string]any, error) {
		return nil, AcceptTx(ctx, tx, st, id, state, time.Now())
	})
}

func (s *Service) pickList(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	st, err := s.staff(r, actor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	id := chi.URLParam(r, "request_id")
	if err := own(r.Context(), s.Pool, id, st.HubID); err != nil {
		s.failQueue(w, r, err)
		return
	}
	var state string
	if err := s.Pool.QueryRow(r.Context(), "SELECT state FROM restock_requests WHERE id = $1", id).Scan(&state); err != nil {
		s.fail(w, r, err)
		return
	}
	switch state {
	case "confirmed":
		s.failQueue(w, r, refuse("not_accepted", "Accept the request first."))
		return
	case "hub_accepted", "packing":
	default:
		s.failQueue(w, r, refuse("request_closed", "This request is no longer being packed."))
		return
	}
	now := time.Now()
	rows, err := s.Pool.Query(r.Context(), `SELECT w.item_id::text, m.name, w.qty, m.sort_order,
			coalesce((SELECT sum(l.qty) FROM restock_lots x JOIN lots l ON l.id = x.lot_id WHERE x.request_id = w.request_id AND x.released_at IS NULL AND l.item_id = w.item_id), 0)::int
		FROM restock_lines w JOIN menu_items m ON m.id = w.item_id WHERE w.request_id = $1`, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	type line struct {
		item, name         string
		qty, order, picked int
		lots               []map[string]any
		soonest            time.Time
	}
	lines, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (line, error) {
		var l line
		return l, row.Scan(&l.item, &l.name, &l.qty, &l.order, &l.picked)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := []map[string]any{}
	for i := range lines {
		l := &lines[i]
		if l.picked >= l.qty {
			continue
		}
		lots, err := freeLots(r.Context(), s.Pool, st.HubID, l.item, now)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		l.lots = lots
		l.soonest = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
		if len(lots) > 0 {
			l.soonest, _ = time.Parse(time.RFC3339Nano, lots[0]["expires_at"].(string))
		}
		out = append(out, map[string]any{"item_id": l.item, "name": l.name, "qty": l.qty, "picked_qty": l.picked, "route_order": l.order, "lots": lots, "_soonest": l.soonest})
	}
	// The line with the soonest expiring lot first; then the shelf route.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := out[j-1], out[j]
			at, bt := a["_soonest"].(time.Time), b["_soonest"].(time.Time)
			if at.Before(bt) || (at.Equal(bt) && a["route_order"].(int) <= b["route_order"].(int)) {
				break
			}
			out[j-1], out[j] = b, a
		}
	}
	for _, l := range out {
		delete(l, "_soonest")
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"request_id": id, "lines": out, "server_time": serverTime()})
}

// freeLots are the hub's lots of an item that can be picked: ok, not in temperature alert, not
// expired, in no cassette, on no scooter. Soonest expiry first.
func freeLots(ctx context.Context, q querier, hub, item string, now time.Time) ([]map[string]any, error) {
	rows, err := q.Query(ctx, `SELECT l.id::text, l.code, l.qty, l.expires_at, l.temp_status FROM lots l
		WHERE l.hub_id = $1 AND l.item_id = $2 AND l.status = 'ok' AND l.temp_status = 'ok' AND l.expires_at > $3
			AND NOT EXISTS (SELECT 1 FROM restock_lots x WHERE x.lot_id = l.id AND x.released_at IS NULL)
			AND NOT EXISTS (SELECT 1 FROM scooter_stock ss WHERE ss.lot_id = l.id)
		ORDER BY l.expires_at, l.code LIMIT 50`, hub, item, now)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	var id, code, temp string
	var qty int
	var expires time.Time
	_, err = pgx.ForEachRow(rows, []any{&id, &code, &qty, &expires, &temp}, func() error {
		out = append(out, map[string]any{"lot_id": id, "lot_code": code, "qty": qty, "expires_at": expires.UTC().Format(time.RFC3339Nano), "temp_status": temp})
		return nil
	})
	return out, err
}

func lotView(id, code string, qty int, expires time.Time, temp string) map[string]any {
	return map[string]any{"lot_id": id, "lot_code": code, "qty": qty, "expires_at": expires.UTC().Format(time.RFC3339Nano), "temp_status": temp}
}

// ScanTx puts a whole lot into the request's cassette. It returns the lot and whether it was
// already there.
func ScanTx(ctx context.Context, tx pgx.Tx, st Staff, id, state, code string, now time.Time) (map[string]any, bool, error) {
	switch state {
	case "confirmed":
		return nil, false, refuse("not_accepted", "Accept the request first.")
	case "hub_accepted", "packing":
	default:
		return nil, false, refuse("request_closed", "This request is no longer being packed.")
	}
	var lot, item, temp, status string
	var hub *string
	var qty int
	var expires time.Time
	err := tx.QueryRow(ctx, "SELECT id::text, item_id::text, qty, expires_at, temp_status, status, hub_id::text FROM lots WHERE code = $1 FOR UPDATE", code).
		Scan(&lot, &item, &qty, &expires, &temp, &status, &hub)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (hub == nil || *hub != st.HubID)) {
		return nil, false, &refusal{code: "wrong_lot", message: "That is not a lot at this hub.", withLot: true}
	}
	if err != nil {
		return nil, false, err
	}
	view := lotView(lot, code, qty, expires, temp)
	no := func(c, m string) (map[string]any, bool, error) {
		return nil, false, &refusal{code: c, message: m, lot: view, withLot: true}
	}
	var onRequest, here, elsewhere bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM restock_lines WHERE request_id = $1 AND item_id = $2),
			EXISTS (SELECT 1 FROM restock_lots WHERE request_id = $1 AND lot_id = $3 AND released_at IS NULL),
			EXISTS (SELECT 1 FROM restock_lots WHERE request_id <> $1 AND lot_id = $3 AND released_at IS NULL)`, id, item, lot).
		Scan(&onRequest, &here, &elsewhere); err != nil {
		return nil, false, err
	}
	switch {
	case here:
		return view, true, nil
	case !onRequest:
		return no("wrong_lot", "That item is not on this request.")
	case status == "quarantined":
		return no("lot_quarantined", "This lot is quarantined.")
	case !expires.After(now):
		return no("lot_expired", "This lot has expired.")
	case temp != "ok":
		return no("temp_alert", "This lot failed its temperature check. Quarantine it.")
	case elsewhere:
		return no("lot_already_packed", "This lot is in another cassette.")
	}
	if _, err := tx.Exec(ctx, "INSERT INTO restock_lots (request_id, lot_id) VALUES ($1, $2)", id, lot); err != nil {
		return nil, false, err
	}
	if state == "hub_accepted" {
		if _, err := tx.Exec(ctx, "UPDATE restock_requests SET state = 'packing', state_changed_at = $2 WHERE id = $1", id, now); err != nil {
			return nil, false, err
		}
	}
	return view, false, Bump(ctx, tx, id)
}

func (s *Service) scan(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LotCode   string `json:"lot_code"`
		ItemID    string `json:"item_id"`
		ScannedAt string `json:"scanned_at"`
	}
	err := json.NewDecoder(r.Body).Decode(&body)
	_, timeErr := time.Parse(time.RFC3339, body.ScannedAt)
	code := strings.TrimSpace(body.LotCode)
	if err != nil || code == "" || len(code) > 64 || timeErr != nil {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send lot_code and scanned_at.", false)
		return
	}
	s.change(w, r, func(ctx context.Context, tx pgx.Tx, st Staff, id, state string) (map[string]any, error) {
		lot, dup, err := ScanTx(ctx, tx, st, id, state, code, time.Now())
		if err != nil {
			return nil, err
		}
		return map[string]any{"lot": lot, "duplicate": dup}, nil
	})
}

// UnscanTx takes a lot back out of a cassette not yet sealed.
func UnscanTx(ctx context.Context, tx pgx.Tx, st Staff, id, state, code string) error {
	if state != "hub_accepted" && state != "packing" {
		return refuse("request_closed", "This cassette is sealed or the request has ended.")
	}
	tag, err := tx.Exec(ctx, `DELETE FROM restock_lots x USING lots l WHERE x.lot_id = l.id AND x.request_id = $1 AND l.code = $2 AND x.released_at IS NULL`, id, code)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return refuse("not_in_cassette", "That lot is not in this cassette.")
	}
	return Bump(ctx, tx, id)
}

func (s *Service) unscan(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LotCode string `json:"lot_code"`
	}
	code := ""
	if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
		code = strings.TrimSpace(body.LotCode)
	}
	if code == "" || len(code) > 64 {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send lot_code.", false)
		return
	}
	s.change(w, r, func(ctx context.Context, tx pgx.Tx, st Staff, id, state string) (map[string]any, error) {
		return nil, UnscanTx(ctx, tx, st, id, state, code)
	})
}

// Feed is what the hub's live channel reads.
func (s *Service) Feed() orders.HubFeed {
	return orders.HubFeed{
		Hub: func(ctx context.Context, staff string) (string, bool, error) {
			var hub string
			var ok bool
			err := s.Pool.QueryRow(ctx, `SELECT s.hub_id::text, s.status = 'active' AND p.status = 'active' FROM hub_staff s
				JOIN people p ON p.id = s.person_id WHERE s.id = $1`, staff).Scan(&hub, &ok)
			if errors.Is(err, pgx.ErrNoRows) {
				return "", false, nil
			}
			return hub, ok, err
		},
		Seq:  func(ctx context.Context, hub string) (int64, error) { return HubSeq(ctx, s.Pool, hub) },
		View: func(ctx context.Context, id string) (map[string]any, error) { return View(ctx, s.Pool, id) },
	}
}

// retryable is a clash with another change at the same moment: a unique key taken, a deadlock or
// a serialisation failure. Sending again works.
func retryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "23505" || pgErr.Code == "40P01" || pgErr.Code == "40001")
}
