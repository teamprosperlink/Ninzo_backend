package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

var errInvalid = errors.New("hub: invalid request")

// ShortLine is one short line of a shortfall.
type ShortLine struct {
	ItemID     string `json:"item_id"`
	QtyMissing int    `json:"qty_missing"`
}

// Shortfall is what the hub records when it cannot fill a request.
type Shortfall struct {
	Action     string      `json:"action"`
	Lines      []ShortLine `json:"lines"`
	Substitute string      `json:"substitute_item_id"`
	OtherHub   string      `json:"other_hub_id"`
	Reason     string      `json:"reason"`
}

// MountSeal adds recordShortfall, markHubRequestReady, getCassetteLabel, listHubLots and quarantineLot.
func (s *Service) MountSeal(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.Get("/hub/restock/{request_id}/label", s.label)
		g.Get("/hub/lots", s.lots)
		g.With(idem).Post("/hub/restock/{request_id}/shortfall", s.shortfall)
		g.With(idem).Post("/hub/restock/{request_id}/ready", s.ready)
		g.With(idem).Post("/hub/lots/{lot_id}/quarantine", s.quarantine)
	})
}

// ShortfallTx records what the hub cannot fill, once per request, for every short line.
func ShortfallTx(ctx context.Context, tx pgx.Tx, st Staff, id, state string, sf Shortfall) error {
	switch state {
	case "confirmed":
		return refuse("not_accepted", "Accept the request first.")
	case "hub_accepted", "packing":
	default:
		return refuse("request_closed", "This cassette is sealed or the request has ended.")
	}
	if len(sf.Lines) < 1 || len(sf.Lines) > 20 || utf8.RuneCountInString(sf.Reason) > 200 {
		return errInvalid
	}
	switch sf.Action {
	case "partial":
		if sf.Substitute != "" || sf.OtherHub != "" {
			return errInvalid
		}
	case "substitute":
		if !idPattern.MatchString(sf.Substitute) || sf.OtherHub != "" {
			return errInvalid
		}
	case "other_hub":
		if !idPattern.MatchString(sf.OtherHub) || sf.Substitute != "" {
			return errInvalid
		}
	default:
		return errInvalid
	}
	var recorded bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM shortfalls WHERE request_id = $1)", id).Scan(&recorded); err != nil {
		return err
	}
	if recorded {
		return refuse("shortfall_already_recorded", "A shortfall is already recorded for this request.")
	}
	seen := map[string]bool{}
	for _, l := range sf.Lines {
		if !idPattern.MatchString(l.ItemID) || l.QtyMissing < 1 || l.QtyMissing > 500 || seen[l.ItemID] {
			return errInvalid
		}
		seen[l.ItemID] = true
		var qty int
		err := tx.QueryRow(ctx, "SELECT qty FROM restock_lines WHERE request_id = $1 AND item_id = $2", id, l.ItemID).Scan(&qty)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && l.QtyMissing > qty) {
			return errInvalid // Not on the request, or more missing than was asked for.
		}
		if err != nil {
			return err
		}
	}
	var sub, other any
	if sf.Action == "substitute" {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM menu_items WHERE id = $1 AND status = 'active')
			AND NOT EXISTS (SELECT 1 FROM restock_lines WHERE request_id = $2 AND item_id = $1)`, sf.Substitute, id).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return errInvalid // A substitute is an active item the rider did not already ask for.
		}
		missing := 0
		for _, l := range sf.Lines {
			missing += l.QtyMissing
		}
		var free int
		if err := tx.QueryRow(ctx, `SELECT coalesce(sum(l.qty), 0)::int FROM lots l WHERE l.hub_id = $1 AND l.item_id = $2 AND l.status = 'ok' AND l.temp_status = 'ok'
			AND l.expires_at > now() AND NOT EXISTS (SELECT 1 FROM restock_lots x WHERE x.lot_id = l.id AND x.released_at IS NULL)
			AND NOT EXISTS (SELECT 1 FROM scooter_stock ss WHERE ss.lot_id = l.id)`, st.HubID, sf.Substitute).Scan(&free); err != nil {
			return err
		}
		if free < missing {
			return refuse("substitute_out_of_stock", "The hub does not have enough of the substitute.")
		}
		sub = sf.Substitute
	}
	if sf.Action == "other_hub" {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM hubs o JOIN hubs h ON h.zone_id = o.zone_id
			WHERE o.id = $1 AND h.id = $2 AND o.id <> h.id AND o.status = 'active')`, sf.OtherHub, st.HubID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return refuse("other_hub_unavailable", "That hub is not open in this zone.")
		}
		other = sf.OtherHub
	}
	var why any
	if sf.Reason != "" {
		why = sf.Reason
	}
	if _, err := tx.Exec(ctx, "INSERT INTO shortfalls (request_id, action, substitute_item_id, other_hub_id, reason, recorded_by) VALUES ($1, $2, $3, $4, $5, $6)",
		id, sf.Action, sub, other, why, st.ID); err != nil {
		return err
	}
	for _, l := range sf.Lines {
		if _, err := tx.Exec(ctx, "INSERT INTO shortfall_lines (request_id, item_id, qty_missing) VALUES ($1, $2, $3)", id, l.ItemID, l.QtyMissing); err != nil {
			return err
		}
	}
	if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeStaff, ActorID: st.ID, Action: "restock.shortfall_recorded", EntityType: "restock_request", EntityID: id,
		After: map[string]any{"action": sf.Action, "lines": len(sf.Lines)}}); err != nil {
		return err
	}
	return Bump(ctx, tx, id)
}

func (s *Service) shortfall(w http.ResponseWriter, r *http.Request) {
	var sf Shortfall
	if err := json.NewDecoder(r.Body).Decode(&sf); err != nil {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send action and lines.", false)
		return
	}
	s.change(w, r, func(ctx context.Context, tx pgx.Tx, st Staff, id, state string) (map[string]any, error) {
		return nil, ShortfallTx(ctx, tx, st, id, state, sf)
	})
}

// ReadyTx seals the cassette with the lots scanned. It becomes ready, or partial while a shortfall
// waits for the rider. Sealing again changes nothing.
func ReadyTx(ctx context.Context, tx pgx.Tx, st Staff, id, state string, lotNumbers []string, now time.Time) error {
	switch state {
	case "confirmed":
		return refuse("not_accepted", "Accept the request first.")
	case "ready", "partial":
		return nil // Already sealed.
	case "hub_accepted", "packing":
	default:
		return refuse("request_closed", "This request has ended.")
	}
	rows, err := tx.Query(ctx, "SELECT l.code FROM restock_lots x JOIN lots l ON l.id = x.lot_id WHERE x.request_id = $1 AND x.released_at IS NULL", id)
	if err != nil {
		return err
	}
	scanned, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	// Every lot scanned, each once, and nothing else.
	mismatch := refuse("lots_mismatch", "The lot numbers do not match the lots scanned.")
	sent := map[string]bool{}
	for _, c := range lotNumbers {
		c = strings.TrimSpace(c)
		if sent[c] {
			return mismatch
		}
		sent[c] = true
	}
	if len(sent) != len(scanned) {
		return mismatch
	}
	for _, c := range scanned {
		if !sent[c] {
			return mismatch
		}
	}
	var bad bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM restock_lots x JOIN lots l ON l.id = x.lot_id
		WHERE x.request_id = $1 AND x.released_at IS NULL AND (l.status <> 'ok' OR l.temp_status <> 'ok'))`, id).Scan(&bad); err != nil {
		return err
	}
	if bad {
		return refuse("lot_quarantined", "A lot in the cassette is quarantined or failed its temperature check. Take it out first.")
	}
	// open: a shortfall waits for the rider. answered: the rider accepted a substitute, so what is
	// packed now is sealed as it is, even if the hub ran short of the substitute.
	var open, answered bool
	if err := tx.QueryRow(ctx, `SELECT coalesce(bool_or(status = 'open'), false), coalesce(bool_or(status = 'accepted' AND action = 'substitute'), false)
		FROM shortfalls WHERE request_id = $1`, id).Scan(&open, &answered); err != nil {
		return err
	}
	if len(scanned) == 0 && !open {
		return refuse("nothing_packed", "Scan a lot, or record a shortfall, first.")
	}
	var short bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM restock_lines w WHERE w.request_id = $1 AND w.qty > coalesce((SELECT sum(l.qty)
			FROM restock_lots x JOIN lots l ON l.id = x.lot_id WHERE x.request_id = w.request_id AND x.released_at IS NULL AND l.item_id = w.item_id), 0))`, id).Scan(&short); err != nil {
		return err
	}
	if short && !open && !answered {
		return refuse("shortfall_needed", "A line is short. Record a shortfall first.")
	}
	// FAIL 2: an open shortfall's lines are what is really short, as packed.
	if open {
		if _, err := tx.Exec(ctx, "DELETE FROM shortfall_lines WHERE request_id = $1", id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO shortfall_lines (request_id, item_id, qty_missing)
			SELECT w.request_id, w.item_id, least(w.qty - p.picked, 500) FROM restock_lines w
			CROSS JOIN LATERAL (SELECT coalesce(sum(l.qty), 0)::int AS picked FROM restock_lots x JOIN lots l ON l.id = x.lot_id
				WHERE x.request_id = w.request_id AND x.released_at IS NULL AND l.item_id = w.item_id) p
			WHERE w.request_id = $1 AND p.picked < w.qty`, id); err != nil {
			return err
		}
		if !short {
			// Packed in full after all: the shortfall is not needed.
			if _, err := tx.Exec(ctx, "DELETE FROM shortfalls WHERE request_id = $1", id); err != nil {
				return err
			}
			open = false
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE restock_lines w SET filled_qty = coalesce((SELECT sum(l.qty) FROM restock_lots x JOIN lots l ON l.id = x.lot_id
		WHERE x.request_id = w.request_id AND x.released_at IS NULL AND l.item_id = w.item_id), 0) WHERE w.request_id = $1`, id); err != nil {
		return err
	}
	to := "ready"
	if open {
		to = "partial"
	}
	if _, err := tx.Exec(ctx, "UPDATE restock_requests SET state = $2, state_changed_at = $3, sealed_at = $3 WHERE id = $1", id, to, now); err != nil {
		return err
	}
	if err := audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeStaff, ActorID: st.ID, Action: "restock.sealed", EntityType: "restock_request", EntityID: id,
		After: map[string]any{"state": to, "lots": len(scanned)}}); err != nil {
		return err
	}
	return Bump(ctx, tx, id)
}

func (s *Service) ready(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LotNumbers []string `json:"lot_numbers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.LotNumbers == nil || len(body.LotNumbers) > 100 {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send lot_numbers, which may be empty after a shortfall.", false)
		return
	}
	for _, c := range body.LotNumbers {
		if c = strings.TrimSpace(c); c == "" || len(c) > 64 {
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Each lot number is 1 to 64 characters.", false)
			return
		}
	}
	s.change(w, r, func(ctx context.Context, tx pgx.Tx, st Staff, id, state string) (map[string]any, error) {
		return nil, ReadyTx(ctx, tx, st, id, state, body.LotNumbers, time.Now())
	})
}

func (s *Service) label(w http.ResponseWriter, r *http.Request) {
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
	var state, scooter string
	var sealed *time.Time
	if err := s.Pool.QueryRow(r.Context(), `SELECT r.state, c.code, coalesce(r.sealed_at, r.state_changed_at) FROM restock_requests r JOIN scooters c ON c.id = r.scooter_id WHERE r.id = $1`, id).
		Scan(&state, &scooter, &sealed); err != nil {
		s.fail(w, r, err)
		return
	}
	if !slices.Contains([]string{"ready", "partial", "picked_up", "received"}, state) {
		s.failQueue(w, r, refuse("not_ready", "Seal the cassette first."))
		return
	}
	v, err := View(r.Context(), s.Pool, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	items := []map[string]any{}
	for _, raw := range v["items"].([]map[string]any) {
		items = append(items, map[string]any{"name": raw["name"], "qty": raw["picked_qty"]})
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"label": map[string]any{
		"request_code": "R-" + strings.ToUpper(strings.ReplaceAll(id, "-", "")[:6]), "scooter_code": scooter, "hub_name": st.HubName,
		"lot_numbers": v["lot_numbers"], "items": items, "ready_at": sealed.UTC().Format(time.RFC3339Nano),
	}, "server_time": serverTime()})
}

// lotView is one lot at the hub (contract: HubLot).
func hubLotView(ctx context.Context, q querier, lot string, now time.Time) (map[string]any, error) {
	var code, item, temp, status string
	var qty int
	var expires time.Time
	var inCassette bool
	err := q.QueryRow(ctx, `SELECT l.code, l.item_id::text, l.qty, l.expires_at, l.temp_status, l.status,
			EXISTS (SELECT 1 FROM restock_lots x JOIN restock_requests r ON r.id = x.request_id WHERE x.lot_id = l.id AND x.released_at IS NULL
				AND r.state IN ('hub_accepted', 'packing', 'ready', 'partial'))
		FROM lots l WHERE l.id = $1`, lot).Scan(&code, &item, &qty, &expires, &temp, &status, &inCassette)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	if status == "ok" && !expires.After(now) {
		status = "expired"
	}
	return map[string]any{"lot_id": lot, "lot_code": code, "item_id": item, "qty": qty, "expires_at": expires.UTC().Format(time.RFC3339Nano),
		"temp_status": temp, "status": status, "in_cassette": inCassette}, nil
}

func (s *Service) lots(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	item := r.URL.Query().Get("item_id")
	if item != "" && !idPattern.MatchString(item) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "item_id is not an id.", false)
		return
	}
	st, err := s.staff(r, actor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT id::text FROM lots WHERE hub_id = $1 AND ($2 = '' OR item_id::text = $2)
		ORDER BY expires_at, code LIMIT 500`, st.HubID, item)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		s.fail(w, r, err)
		return
	}
	now := time.Now()
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		v, err := hubLotView(r.Context(), s.Pool, id, now)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		out = append(out, v)
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"lots": out, "server_time": serverTime()})
}

var quarantineReasons = map[string]bool{"temperature": true, "damaged": true, "expired": true, "contamination": true, "other": true}

// errLeadOnly refuses quarantine to anyone but a hub lead.
var errLeadOnly = errors.New("hub: hub lead only")

// QuarantineTx quarantines a lot at the hub, hub lead only. It leaves any cassette still at the hub.
// The requests it is packed in are locked before the lot, as a scan locks them, so the two never
// deadlock. Quarantining again changes nothing.
func QuarantineTx(ctx context.Context, tx pgx.Tx, st Staff, lot, reason, note string, now time.Time) error {
	if st.Role != "hub_lead" {
		return errLeadOnly
	}
	if !idPattern.MatchString(lot) {
		return errNotFound
	}
	if !quarantineReasons[reason] || utf8.RuneCountInString(note) > 500 {
		return errInvalid
	}
	rows, err := tx.Query(ctx, `SELECT r.id::text FROM restock_lots x JOIN restock_requests r ON r.id = x.request_id
		WHERE x.lot_id = $1 AND x.released_at IS NULL AND r.state IN ('hub_accepted', 'packing', 'ready', 'partial') ORDER BY r.id FOR UPDATE OF r`, lot)
	if err != nil {
		return err
	}
	open, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	var hub *string
	var status string
	var onScooter bool
	err = tx.QueryRow(ctx, `SELECT hub_id::text, status, EXISTS (SELECT 1 FROM scooter_stock WHERE lot_id = l.id) FROM lots l WHERE l.id = $1 FOR UPDATE`, lot).
		Scan(&hub, &status, &onScooter)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if hub == nil || *hub != st.HubID {
		if onScooter {
			// It left a hub. Only a lot that was packed at this hub says so; any other is not found.
			var ours bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM restock_lots x JOIN restock_requests r ON r.id = x.request_id WHERE x.lot_id = $1 AND r.hub_id = $2)", lot, st.HubID).Scan(&ours); err != nil {
				return err
			}
			if ours {
				return refuse("lot_on_scooter", "This lot has already left the hub.")
			}
		}
		return errNotFound
	}
	if status == "quarantined" {
		return nil
	}
	var why any
	if note != "" {
		why = note
	}
	if _, err := tx.Exec(ctx, `UPDATE lots SET status = 'quarantined', quarantine_reason = $2, quarantine_note = $3, quarantined_by = $4, quarantined_at = $5 WHERE id = $1`,
		lot, reason, why, st.ID, now); err != nil {
		return err
	}
	for _, request := range open {
		if _, err := tx.Exec(ctx, "UPDATE restock_lots SET released_at = $3 WHERE request_id = $1 AND lot_id = $2 AND released_at IS NULL", request, lot, now); err != nil {
			return err
		}
		// A sealed cassette's filled quantities follow what is left in it.
		if _, err := tx.Exec(ctx, `UPDATE restock_lines w SET filled_qty = coalesce((SELECT sum(l.qty) FROM restock_lots x JOIN lots l ON l.id = x.lot_id
			WHERE x.request_id = w.request_id AND x.released_at IS NULL AND l.item_id = w.item_id), 0)
			WHERE w.request_id = $1 AND w.filled_qty IS NOT NULL`, request); err != nil {
			return err
		}
		if err := Bump(ctx, tx, request); err != nil {
			return err
		}
	}
	return audit.Record(ctx, tx, audit.Event{ActorType: identity.TypeStaff, ActorID: st.ID, Action: "lot.quarantined", EntityType: "lot", EntityID: lot,
		After: map[string]any{"reason": reason, "taken_out_of": len(open)}})
}

func (s *Service) quarantine(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body struct {
		Reason string `json:"reason"`
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !quarantineReasons[body.Reason] || utf8.RuneCountInString(body.Note) > 500 {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send a reason: temperature, damaged, expired, contamination or other.", false)
		return
	}
	lot := chi.URLParam(r, "lot_id")
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		if err := identity.CheckSession(r.Context(), tx, actor); err != nil {
			return err
		}
		st, err := staffIn(r.Context(), tx, actor.ID)
		if err != nil {
			return err
		}
		now := time.Now()
		if err := QuarantineTx(r.Context(), tx, st, lot, body.Reason, body.Note, now); err != nil {
			return err
		}
		v, err := hubLotView(r.Context(), tx, lot, now)
		if err != nil {
			return err
		}
		reply = map[string]any{"lot": v, "server_time": serverTime()}
		return idempotency.Save(r.Context(), tx, http.StatusOK, reply)
	})
	switch {
	case err == nil:
		httpserver.WriteJSON(w, http.StatusOK, reply)
	case errors.Is(err, errLeadOnly):
		httpserver.WriteJSON(w, http.StatusForbidden, map[string]any{"code": "forbidden", "message": "Only a hub lead can quarantine a lot.", "retryable": false,
			"missing_permission": "lot.quarantine", "server_time": serverTime()})
	default:
		s.failQueue(w, r, err)
	}
}
