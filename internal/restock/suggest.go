package restock

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

// refusal is a 409 the contract names.
type refusal struct{ code, message string }

func (r *refusal) Error() string { return "restock: " + r.code }

// RefusalOf returns the contract's code and message when err is a refusal.
func RefusalOf(err error) (code, message string, ok bool) {
	var no *refusal
	if errors.As(err, &no) {
		return no.code, no.message, true
	}
	return "", "", false
}

var errInvalid = errors.New("restock: invalid request")

// ErrInvalid lets the offline batch tell bad quantities from other errors.
var ErrInvalid = errInvalid

// Line is one item and quantity a rider asks for.
type Line struct {
	ItemID string `json:"item_id"`
	Qty    int    `json:"qty"`
}

// MountSuggestions adds listRestockSuggestions, confirmRestock and declineRestock.
func (s *Service) MountSuggestions(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.Get("/riders/me/restock-suggestions", s.listSuggestions)
		g.With(idem).Post("/restock/{request_id}/confirm", s.confirm)
		g.With(idem).Post("/restock/{request_id}/decline", s.decline)
	})
}

func (s *Service) failTx(w http.ResponseWriter, r *http.Request, err error) {
	if code, msg, ok := RefusalOf(err); ok {
		httpserver.WriteError(w, http.StatusConflict, code, msg, false)
		return
	}
	if errors.Is(err, errInvalid) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send each item once, with a qty from 1 to 500.", false)
		return
	}
	s.fail(w, r, err)
}

func (s *Service) listSuggestions(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	if _, err := s.rider(r, actor); err != nil {
		s.fail(w, r, err)
		return
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT id::text, expires_at FROM restock_requests WHERE rider_id = $1 AND state = 'suggested' AND expires_at > $2
		ORDER BY created_at DESC, id`, actor.ID, s.clock())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	type sug struct {
		id      string
		expires time.Time
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (sug, error) {
		var x sug
		return x, r.Scan(&x.id, &x.expires)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, x := range list {
		v, err := View(r.Context(), s.Pool, x.id)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		out = append(out, map[string]any{"id": x.id, "hub": v["hub"], "items": v["items"], "expires_at": x.expires.UTC().Format(time.RFC3339Nano)})
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"suggestions": out, "server_time": serverTime()})
}

// lockRequest takes a request's row lock, for its own rider only.
func lockRequest(ctx context.Context, tx pgx.Tx, id, rider string) (string, *time.Time, error) {
	if !idPattern.MatchString(id) {
		return "", nil, errNotFound
	}
	var state string
	var expires *time.Time
	err := tx.QueryRow(ctx, "SELECT state, expires_at FROM restock_requests WHERE id = $1 AND rider_id = $2 FOR UPDATE", id, rider).Scan(&state, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, errNotFound
	}
	return state, expires, err
}

// hubHas says whether the hub holds enough free units for every line.
func hubHas(ctx context.Context, tx pgx.Tx, request string, now time.Time) (bool, error) {
	var short bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM restock_lines w JOIN restock_requests q ON q.id = w.request_id WHERE w.request_id = $1 AND w.qty > (
			SELECT coalesce(sum(l.qty), 0) FROM lots l WHERE l.hub_id = q.hub_id AND l.item_id = w.item_id AND l.status = 'ok' AND l.temp_status = 'ok'
				AND l.expires_at > $2 AND NOT EXISTS (SELECT 1 FROM scooter_stock ss WHERE ss.lot_id = l.id)
				AND NOT EXISTS (SELECT 1 FROM restock_lots x WHERE x.lot_id = l.id AND x.released_at IS NULL)))`, request, now).Scan(&short)
	return !short, err
}

// ConfirmTx confirms a rider's suggestion, with any quantities they changed: the core of
// confirmRestock, shared with the offline batch. A repeat returns the request again.
func (s *Service) ConfirmTx(ctx context.Context, tx pgx.Tx, rider, id string, override []Line, save bool) (map[string]any, error) {
	now := s.clock()
	state, expires, err := lockRequest(ctx, tx, id, rider)
	if err != nil {
		return nil, err
	}
	if state != "suggested" {
		if state == "confirmed" || (state != "expired" && state != "cancelled" && state != "closed") {
			v, err := View(ctx, tx, id)
			return s.reply(ctx, tx, v, err, save)
		}
		return nil, &refusal{"suggestion_expired", "This suggestion has ended."}
	}
	if expires != nil && !now.Before(*expires) {
		return nil, &refusal{"suggestion_expired", "This suggestion has ended."}
	}
	if len(override) > 0 {
		seen := map[string]bool{}
		for _, l := range override {
			if !idPattern.MatchString(l.ItemID) || l.Qty < 1 || l.Qty > 500 || seen[l.ItemID] {
				return nil, errInvalid
			}
			seen[l.ItemID] = true
			var active bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM menu_items WHERE id = $1 AND status = 'active')", l.ItemID).Scan(&active); err != nil {
				return nil, err
			}
			if !active {
				return nil, errInvalid
			}
		}
		if _, err := tx.Exec(ctx, "DELETE FROM restock_lines WHERE request_id = $1", id); err != nil {
			return nil, err
		}
		for _, l := range override {
			if _, err := tx.Exec(ctx, "INSERT INTO restock_lines (request_id, item_id, qty) VALUES ($1, $2, $3)", id, l.ItemID, l.Qty); err != nil {
				return nil, err
			}
		}
	}
	var onShift bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM shifts sh JOIN restock_requests q ON q.scooter_id = sh.scooter_id
		WHERE q.id = $1 AND sh.rider_id = $2 AND sh.ended_at IS NULL)`, id, rider).Scan(&onShift); err != nil {
		return nil, err
	}
	if !onShift {
		return nil, &refusal{"suggestion_expired", "Your shift with this scooter has ended."}
	}
	ok, err := hubHas(ctx, tx, id, now)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &refusal{"hub_out_of_stock", "The hub does not have enough of these items."}
	}
	var open bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM restock_requests WHERE scooter_id = (SELECT scooter_id FROM restock_requests WHERE id = $1)
		AND id <> $1 AND state = ANY($2))`, id, Open).Scan(&open); err != nil {
		return nil, err
	}
	if open {
		return nil, &refusal{"restock_already_open", "This scooter already has a restock in progress."}
	}
	if _, err := tx.Exec(ctx, "UPDATE restock_requests SET state = 'confirmed', state_changed_at = $2, updated_at = now() WHERE id = $1", id, now); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, &refusal{"restock_already_open", "This scooter already has a restock in progress."}
		}
		return nil, err
	}
	if err := bumpRider(ctx, tx, rider, id); err != nil {
		return nil, err
	}
	if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeRider, ActorID: rider, Action: "restock.confirmed", EntityType: "restock_request", EntityID: id}); err != nil {
		return nil, err
	}
	v, err := View(ctx, tx, id)
	return s.reply(ctx, tx, v, err, save)
}

func (s *Service) reply(ctx context.Context, tx pgx.Tx, v map[string]any, err error, save bool) (map[string]any, error) {
	if err != nil {
		return nil, err
	}
	out := map[string]any{"request": v, "server_time": serverTime()}
	if !save {
		return out, nil
	}
	return out, idempotency.Save(ctx, tx, http.StatusOK, out)
}

// DeclineTx declines a suggestion. It is drafted again at the next stock check.
func (s *Service) DeclineTx(ctx context.Context, tx pgx.Tx, rider, id, reason string, save bool) (map[string]any, error) {
	state, _, err := lockRequest(ctx, tx, id, rider)
	if err != nil {
		return nil, err
	}
	switch state {
	case "cancelled":
	case "suggested":
		var why any
		if reason != "" {
			why = reason
		}
		if _, err := tx.Exec(ctx, "UPDATE restock_requests SET state = 'cancelled', reason = $2, state_changed_at = $3, updated_at = now() WHERE id = $1",
			id, why, s.clock()); err != nil {
			return nil, err
		}
		if err := bumpRider(ctx, tx, rider, id); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeRider, ActorID: rider, Action: "restock.declined", EntityType: "restock_request", EntityID: id}); err != nil {
			return nil, err
		}
	default:
		return nil, &refusal{"suggestion_expired", "This suggestion has ended."}
	}
	v, err := View(ctx, tx, id)
	return s.reply(ctx, tx, v, err, save)
}

func (s *Service) confirm(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body struct {
		Override []Line `json:"quantity_override"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send a JSON body, with quantity_override if you changed anything.", false)
		return
	}
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		if err := s.check(r, tx, actor); err != nil {
			return err
		}
		var err error
		reply, err = s.ConfirmTx(r.Context(), tx, actor.ID, chi.URLParam(r, "request_id"), body.Override, true)
		return err
	})
	if err != nil {
		s.failTx(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, reply)
}

func (s *Service) decline(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || utf8.RuneCountInString(body.Reason) > 200 {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send a reason of up to 200 characters, or none.", false)
		return
	}
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		if err := s.check(r, tx, actor); err != nil {
			return err
		}
		var err error
		reply, err = s.DeclineTx(r.Context(), tx, actor.ID, chi.URLParam(r, "request_id"), body.Reason, true)
		return err
	})
	if err != nil {
		s.failTx(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, reply)
}

// check runs the session and rider checks inside a change.
func (s *Service) check(r *http.Request, tx pgx.Tx, a identity.Actor) error {
	if err := identity.CheckSession(r.Context(), tx, a); err != nil {
		return err
	}
	var status, person string
	err := tx.QueryRow(r.Context(), "SELECT r.status, p.status FROM riders r JOIN people p ON p.id = r.person_id WHERE r.id = $1", a.ID).Scan(&status, &person)
	if errors.Is(err, pgx.ErrNoRows) {
		return identity.ErrSessionInvalid
	}
	if err == nil && (person != "active" || status == "suspended") {
		err = errBlocked
	}
	return err
}

// Suggest is the stock check: for each scooter on shift that is running low, it drafts one
// suggestion to bring the low items back up, from the nearest active hub in its zone. A scooter
// with a suggestion or a request in progress, or one that had a suggestion end within the check
// interval, is left alone. It also expires suggestions past their time.
func (s *Service) Suggest(ctx context.Context, now time.Time) (int, error) {
	if err := database.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE restock_requests SET state = 'expired', state_changed_at = $1, updated_at = now()
			WHERE state = 'suggested' AND expires_at <= $1 RETURNING id::text, rider_id::text`, now)
		if err != nil {
			return err
		}
		type ended struct{ id, rider string }
		list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ended, error) {
			var e ended
			return e, r.Scan(&e.id, &e.rider)
		})
		if err != nil {
			return err
		}
		for _, e := range list {
			if err := bumpRider(ctx, tx, e.rider, e.id); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return 0, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT sh.scooter_id::text FROM shifts sh JOIN riders r ON r.id = sh.rider_id AND r.status = 'active'
		JOIN scooters c ON c.id = sh.scooter_id AND c.status = 'active' WHERE sh.ended_at IS NULL ORDER BY sh.scooter_id`)
	if err != nil {
		return 0, err
	}
	scooters, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	made := 0
	for _, scooter := range scooters {
		err := database.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
			ok, err := s.suggestFor(ctx, tx, scooter, now)
			if ok {
				made++
			}
			return err
		})
		if err != nil && ctx.Err() == nil {
			s.Log.Error("restock: a stock check failed", "scooter_id", scooter, "error", err.Error())
		}
	}
	return made, nil
}

func (s *Service) suggestFor(ctx context.Context, tx pgx.Tx, scooter string, now time.Time) (bool, error) {
	var rider, zone string
	var lat, lng *float64
	err := tx.QueryRow(ctx, `SELECT sh.rider_id::text, c.zone_id::text, n.lat, n.lng FROM scooters c JOIN shifts sh ON sh.scooter_id = c.id AND sh.ended_at IS NULL
		LEFT JOIN staging_nodes n ON n.id = c.node_id WHERE c.id = $1 FOR UPDATE OF c SKIP LOCKED`, scooter).Scan(&rider, &zone, &lat, &lng)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	settings, err := geo.LoadSettings(ctx, tx, zone)
	if err != nil {
		return false, err
	}
	low, _ := settings.Get("restock_low_qty")
	target, _ := settings.Get("restock_target_qty")
	ttl, _ := settings.Get("restock_suggestion_ttl_s")
	every, _ := settings.Get("restock_check_s")
	var busy bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM restock_requests WHERE scooter_id = $1 AND (state = 'suggested' OR state = ANY($2)
			OR (state IN ('expired', 'cancelled', 'received', 'closed') AND state_changed_at > $3)))`,
		scooter, Open, now.Add(-time.Duration(every)*time.Second)).Scan(&busy); err != nil {
		return false, err
	}
	if busy {
		return false, nil
	}
	// The nearest active hub in the zone. With no waiting point, the first by name.
	var hub string
	err = tx.QueryRow(ctx, `SELECT id::text FROM hubs WHERE zone_id = $1 AND status = 'active'
		ORDER BY NOT EXISTS (SELECT 1 FROM hub_staff s WHERE s.hub_id = hubs.id AND s.status = 'active'),
			CASE WHEN $2::float8 IS NULL THEN 0 ELSE (lat - $2) * (lat - $2) + (lng - $3) * (lng - $3) END, name, id LIMIT 1`, zone, lat, lng).Scan(&hub)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // No hub in this zone yet.
	}
	if err != nil {
		return false, err
	}
	// Free units on the scooter (reserved units are promised to orders), and free units at the hub.
	rows, err := tx.Query(ctx, `SELECT m.id::text,
			coalesce((SELECT sum(ss.on_hand - ss.reserved) FROM scooter_stock ss JOIN lots l ON l.id = ss.lot_id
				WHERE ss.scooter_id = $1 AND l.item_id = m.id AND l.status = 'ok' AND l.temp_status = 'ok' AND l.expires_at > $2), 0)::int,
			coalesce((SELECT sum(l.qty) FROM lots l WHERE l.hub_id = $3 AND l.item_id = m.id AND l.status = 'ok' AND l.temp_status = 'ok'
				AND l.expires_at > $2 AND NOT EXISTS (SELECT 1 FROM scooter_stock ss WHERE ss.lot_id = l.id)
				AND NOT EXISTS (SELECT 1 FROM restock_lots x WHERE x.lot_id = l.id AND x.released_at IS NULL)), 0)::int
		FROM menu_items m WHERE m.status = 'active' ORDER BY m.sort_order, m.id`, scooter, now, hub)
	if err != nil {
		return false, err
	}
	want := []Line{}
	var item string
	var free, atHub int
	if _, err := pgx.ForEachRow(rows, []any{&item, &free, &atHub}, func() error {
		if free < low && target > free && atHub > 0 {
			want = append(want, Line{ItemID: item, Qty: min(target-free, atHub)})
		}
		return nil
	}); err != nil {
		return false, err
	}
	if len(want) == 0 {
		return false, nil
	}
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO restock_requests (rider_id, scooter_id, hub_id, source, state, expires_at, state_changed_at)
		VALUES ($1, $2, $3, 'suggested', 'suggested', $4, $5) RETURNING id::text`, rider, scooter, hub, now.Add(time.Duration(ttl)*time.Second), now).Scan(&id); err != nil {
		return false, err
	}
	for _, l := range want {
		if _, err := tx.Exec(ctx, "INSERT INTO restock_lines (request_id, item_id, qty) VALUES ($1, $2, $3)", id, l.ItemID, l.Qty); err != nil {
			return false, err
		}
	}
	return true, bumpRider(ctx, tx, rider, id)
}
