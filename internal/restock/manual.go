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
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

// uncollected is how long after arrive_by a packed cassette waits before it expires and its lots
// go back to the hub's shelf.
const uncollected = 30 * time.Minute

// MountRequests adds createRestock and respondToShortfall.
func (s *Service) MountRequests(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.With(idem).Post("/restock", s.create)
		g.With(idem).Post("/restock/{request_id}/shortfall-response", s.respond)
	})
}

func checkLines(ctx context.Context, tx pgx.Tx, lines []Line) error {
	if len(lines) < 1 || len(lines) > 20 {
		return errInvalid
	}
	seen := map[string]bool{}
	for _, l := range lines {
		if !idPattern.MatchString(l.ItemID) || l.Qty < 1 || l.Qty > 500 || seen[l.ItemID] {
			return errInvalid
		}
		seen[l.ItemID] = true
		var active bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM menu_items WHERE id = $1 AND status = 'active')", l.ItemID).Scan(&active); err != nil {
			return err
		}
		if !active {
			return errInvalid
		}
	}
	return nil
}

// release puts a request's packed lots that are still at the hub back on the shelf.
func release(ctx context.Context, tx pgx.Tx, id string, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE restock_lots x SET released_at = $2 WHERE x.request_id = $1 AND x.released_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM scooter_stock ss WHERE ss.lot_id = x.lot_id)`, id, now)
	return err
}

// CreateTx is the core of createRestock: a manual request for units the system cannot see are
// spoiled or damaged. A second request merges into the scooter's request while the hub has not
// started on it; it replaces a suggestion.
func (s *Service) CreateTx(ctx context.Context, tx pgx.Tx, rider, hub string, lines []Line, reason string, save bool) (map[string]any, error) {
	now := s.clock()
	if !idPattern.MatchString(hub) || utf8.RuneCountInString(reason) > 200 {
		return nil, errInvalid
	}
	if err := checkLines(ctx, tx, lines); err != nil {
		return nil, err
	}
	var scooter string
	err := tx.QueryRow(ctx, `SELECT sh.scooter_id::text FROM shifts sh JOIN scooters c ON c.id = sh.scooter_id
		JOIN hubs h ON h.zone_id = c.zone_id AND h.id = $2 AND h.status = 'active'
		WHERE sh.rider_id = $1 AND sh.ended_at IS NULL FOR UPDATE OF c`, rider, hub).Scan(&scooter)
	if errors.Is(err, pgx.ErrNoRows) {
		// No shift, or no such hub in the rider's zone.
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	var open, openHub, openState string
	err = tx.QueryRow(ctx, "SELECT id::text, hub_id::text, state FROM restock_requests WHERE scooter_id = $1 AND state = ANY($2) FOR UPDATE",
		scooter, Open).Scan(&open, &openHub, &openState)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		open = ""
	case err != nil:
		return nil, err
	case openState != "confirmed" || openHub != hub:
		return nil, &refusal{"restock_already_open", "This scooter's restock is already with the hub. Ask again once it is collected."}
	}
	if _, err := tx.Exec(ctx, "UPDATE restock_requests SET state = 'cancelled', state_changed_at = $2, updated_at = now() WHERE scooter_id = $1 AND state = 'suggested'",
		scooter, now); err != nil {
		return nil, err
	}
	var why any
	if reason != "" {
		why = reason
	}
	id := open
	if id == "" {
		if err := tx.QueryRow(ctx, `INSERT INTO restock_requests (rider_id, scooter_id, hub_id, source, state, reason, state_changed_at)
			VALUES ($1, $2, $3, 'manual', 'confirmed', $4, $5) RETURNING id::text`, rider, scooter, hub, why, now).Scan(&id); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return nil, &refusal{"restock_already_open", "This scooter already has a restock in progress."}
			}
			return nil, err
		}
	}
	for _, l := range lines {
		if _, err := tx.Exec(ctx, `INSERT INTO restock_lines (request_id, item_id, qty) VALUES ($1, $2, $3)
			ON CONFLICT (request_id, item_id) DO UPDATE SET qty = least(restock_lines.qty + EXCLUDED.qty, 500)`, id, l.ItemID, l.Qty); err != nil {
			return nil, err
		}
	}
	ok, err := hubHas(ctx, tx, id, now)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &refusal{"hub_out_of_stock", "The hub does not have enough of these items."}
	}
	if err := bumpRider(ctx, tx, rider, id); err != nil {
		return nil, err
	}
	if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeRider, ActorID: rider, Action: "restock.requested", EntityType: "restock_request", EntityID: id}); err != nil {
		return nil, err
	}
	v, err := View(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"request": v, "server_time": serverTime()}
	if save {
		return out, idempotency.Save(ctx, tx, http.StatusCreated, out)
	}
	return out, nil
}

// RespondTx is the core of respondToShortfall. Accepting a partial fill takes what was packed;
// refusing it cancels the request and puts its lots back. Accepting another hub moves the whole
// request there as a new one, and refusing takes what was packed here.
func (s *Service) RespondTx(ctx context.Context, tx pgx.Tx, rider, id string, accept, save bool) (map[string]any, error) {
	now := s.clock()
	state, _, err := lockRequest(ctx, tx, id, rider)
	if err != nil {
		return nil, err
	}
	var shortfall, action string
	var other *string
	err = tx.QueryRow(ctx, "SELECT id::text, action, other_hub_id::text FROM shortfalls WHERE request_id = $1 AND status = 'open' FOR UPDATE", id).Scan(&shortfall, &action, &other)
	if errors.Is(err, pgx.ErrNoRows) || state != "partial" {
		return nil, &refusal{"shortfall_not_open", "There is no shortfall waiting for an answer."}
	}
	if err != nil {
		return nil, err
	}
	decision := "refused"
	if accept {
		decision = "accepted"
	}
	if _, err := tx.Exec(ctx, "UPDATE shortfalls SET status = $2, decided_at = $3 WHERE id = $1", shortfall, decision, now); err != nil {
		return nil, err
	}
	var packed bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM restock_lots WHERE request_id = $1 AND released_at IS NULL)", id).Scan(&packed); err != nil {
		return nil, err
	}
	shown := id // The request the answer returns: the follow-up, when one is opened.
	next := "ready"
	switch {
	case action == "other_hub" && accept:
		next = "cancelled" // The whole request moves to the other hub, below.
	case action == "partial" && !accept:
		next = "cancelled"
	case action == "substitute" && accept:
		next = "packing" // The hub packs the substitute, then seals again.
	case !packed:
		next = "closed" // Nothing was packed, so there is nothing to collect.
	}
	if next != "ready" && next != "packing" {
		if err := release(ctx, tx, id, now); err != nil {
			return nil, err
		}
	}
	if next == "packing" {
		if err := takeSubstitute(ctx, tx, id); err != nil {
			return nil, err
		}
	}
	// This request leaves the one-open rule before its follow-up enters it.
	if _, err := tx.Exec(ctx, "UPDATE restock_requests SET state = $2, state_changed_at = $3, updated_at = now() WHERE id = $1", id, next, now); err != nil {
		return nil, err
	}
	if action == "other_hub" && accept {
		var follow string
		if err := tx.QueryRow(ctx, `INSERT INTO restock_requests (rider_id, scooter_id, hub_id, source, state, reason, follow_of, state_changed_at)
			SELECT rider_id, scooter_id, $2, source, 'confirmed', reason, id, $3 FROM restock_requests WHERE id = $1 RETURNING id::text`, id, *other, now).Scan(&follow); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO restock_lines (request_id, item_id, qty) SELECT $2, item_id, qty FROM restock_lines WHERE request_id = $1", id, follow); err != nil {
			return nil, err
		}
		shown = follow
	}
	bumped := []string{id}
	if shown != id {
		bumped = append(bumped, shown)
	}
	if err := bumpInHubOrder(ctx, tx, rider, bumped...); err != nil {
		return nil, err
	}
	if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeRider, ActorID: rider, Action: "restock.shortfall_" + decision, EntityType: "restock_request", EntityID: id,
		After: map[string]any{"action": action, "state": next}}); err != nil {
		return nil, err
	}
	v, err := View(ctx, tx, shown)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"request": v, "server_time": serverTime()}
	if save {
		return out, idempotency.Save(ctx, tx, http.StatusOK, out)
	}
	return out, nil
}

func (s *Service) create(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body struct {
		HubID  string `json:"hub_id"`
		Items  []Line `json:"items"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send hub_id and items.", false)
		return
	}
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		if err := s.check(r, tx, actor); err != nil {
			return err
		}
		var err error
		reply, err = s.CreateTx(r.Context(), tx, actor.ID, body.HubID, body.Items, body.Reason, true)
		return err
	})
	if err != nil {
		s.failTx(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusCreated, reply)
}

func (s *Service) respond(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body struct {
		Accept *bool `json:"accept"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Accept == nil {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send accept: true or false.", false)
		return
	}
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		if err := s.check(r, tx, actor); err != nil {
			return err
		}
		var err error
		reply, err = s.RespondTx(r.Context(), tx, actor.ID, chi.URLParam(r, "request_id"), *body.Accept, true)
		return err
	})
	if err != nil {
		s.failTx(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, reply)
}

// expireUncollected is ExpireUncollected, kept on the simulator for its tests.
func (sim *Simulator) expireUncollected(ctx context.Context, now time.Time) error {
	return sim.Service.ExpireUncollected(ctx, now)
}

// ExpireUncollected ends packed cassettes nobody came for, long after arrive_by, and puts their
// unscanned lots back on the shelf. A cassette the rider started on is received for what was taken.
// Each cassette has its own transaction, so one failure never holds up the rest.
func (s *Service) ExpireUncollected(ctx context.Context, now time.Time) error {
	skip := []string{}
	for i := 0; i < 200; i++ {
		done := false
		var id string
		err := database.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
			var rider, state string
			err := tx.QueryRow(ctx, `SELECT id::text, rider_id::text, state FROM restock_requests
				WHERE state IN ('ready', 'partial', 'picked_up') AND arrive_by IS NOT NULL AND arrive_by + make_interval(secs => $2) <= $1
					AND NOT (id::text = ANY($3::text[]))
				ORDER BY arrive_by, id LIMIT 1 FOR UPDATE SKIP LOCKED`, now, uncollected.Seconds(), skip).Scan(&id, &rider, &state)
			if errors.Is(err, pgx.ErrNoRows) {
				done = true
				return nil
			}
			if err != nil {
				return err
			}
			to := "expired"
			if state == "picked_up" {
				to = "received"
			}
			if _, err := tx.Exec(ctx, "UPDATE restock_requests SET state = $2, state_changed_at = $3, updated_at = now() WHERE id = $1", id, to, now); err != nil {
				return err
			}
			if err := release(ctx, tx, id, now); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "UPDATE shortfalls SET status = 'refused', decided_at = $2 WHERE request_id = $1 AND status = 'open'", id, now); err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, audit.Event{ActorType: audit.ActorSystem, Action: "restock.uncollected", EntityType: "restock_request", EntityID: id,
				After: map[string]any{"state": to}}); err != nil {
				return err
			}
			return bumpRider(ctx, tx, rider, id)
		})
		if err != nil {
			if id == "" || ctx.Err() != nil {
				return err
			}
			s.Log.Error("restock: expiring a cassette failed", "request_id", id, "error", err.Error())
			skip = append(skip, id)
			continue
		}
		if done {
			return nil
		}
	}
	return nil
}

// takeSubstitute changes the lines for an accepted substitute: each short line keeps what was
// packed (a line with nothing packed goes), and the substitute is asked for in the missing units.
// The hub then packs the substitute and seals again; it seals what it has if it runs short.
func takeSubstitute(ctx context.Context, tx pgx.Tx, id string) error {
	var sub string
	var missing int
	if err := tx.QueryRow(ctx, `SELECT s.substitute_item_id::text, (SELECT coalesce(sum(qty_missing), 0) FROM shortfall_lines WHERE request_id = s.request_id)::int
		FROM shortfalls s WHERE s.request_id = $1`, id).Scan(&sub, &missing); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE restock_lines w SET qty = w.filled_qty FROM shortfall_lines f
		WHERE f.request_id = w.request_id AND f.item_id = w.item_id AND w.request_id = $1 AND coalesce(w.filled_qty, 0) > 0`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM restock_lines w USING shortfall_lines f
		WHERE f.request_id = w.request_id AND f.item_id = w.item_id AND w.request_id = $1 AND coalesce(w.filled_qty, 0) = 0`, id); err != nil {
		return err
	}
	if missing < 1 {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO restock_lines (request_id, item_id, qty) VALUES ($1, $2, $3)
		ON CONFLICT (request_id, item_id) DO UPDATE SET qty = least(restock_lines.qty + EXCLUDED.qty, 500)`, id, sub, min(missing, 500))
	return err
}
