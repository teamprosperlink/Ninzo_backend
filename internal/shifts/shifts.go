// Package shifts serves a rider's working day: starting and ending a shift, the staging nodes, and
// loading stock onto the scooter.
package shifts

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/idempotency"
)

var idPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// refusal is a 409 the contract names.
type refusal struct{ code, message string }

func (r *refusal) Error() string { return "shifts: " + r.code }

var (
	errNotFound = errors.New("shifts: not found")
	errBlocked  = errors.New("shifts: blocked")
)

func refuse(code, message string) error { return &refusal{code, message} }

// Service serves the rider's shift and stock.
type Service struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
}

// MountRider adds startShift, endShift, listNodes, scanStock and getStock.
func (s *Service) MountRider(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	idem := idempotency.Middleware(s.Pool, idempotency.Options{Actor: identity.ActorID, Log: s.Log})
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.With(idem).Post("/riders/me/shifts/start", s.startShift)
		g.With(idem).Post("/riders/me/shifts/end", s.endShift)
		g.Get("/nodes", s.listNodes)
		g.With(idem).Post("/riders/me/stock/scan", s.scanStock)
		g.Get("/riders/me/stock", s.getStock)
	})
}

func serverTime() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

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
		s.Log.Error("shifts: request failed", "request_id", httpserver.RequestID(r.Context()), "error", err.Error())
		httpserver.WriteError(w, http.StatusInternalServerError, "internal_error", "Try again shortly.", true)
	}
}

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

type rider struct {
	status string
	zoneID *string
}

// liveRider is readRider for a read with no lock: an ended session must not keep reading.
func liveRider(ctx context.Context, q querier, a identity.Actor) (rider, error) {
	live, err := identity.SessionLive(ctx, q, a)
	if err != nil {
		return rider{}, err
	}
	if !live {
		return rider{}, identity.ErrSessionInvalid
	}
	return readRider(ctx, q, a.ID)
}

// readRider refuses a blocked person or a suspended rider, as account_blocked.
func readRider(ctx context.Context, q querier, id string) (rider, error) {
	var rd rider
	var personStatus string
	err := q.QueryRow(ctx, `SELECT r.status, r.zone_id::text, p.status FROM riders r JOIN people p ON p.id = r.person_id
		WHERE r.id = $1`, id).Scan(&rd.status, &rd.zoneID, &personStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return rd, identity.ErrSessionInvalid
	}
	if err == nil && (personStatus != "active" || rd.status == "suspended") {
		err = errBlocked
	}
	return rd, err
}

// bump raises the rider's seq, so the app knows something it can see changed.
func bump(ctx context.Context, tx pgx.Tx, riderID string) error {
	_, err := tx.Exec(ctx, `INSERT INTO rider_state (rider_id, seq) VALUES ($1, 1)
		ON CONFLICT (rider_id) DO UPDATE SET seq = rider_state.seq + 1, updated_at = now()`, riderID)
	return err
}

type shift struct {
	ID        string  `json:"id"`
	NodeID    string  `json:"node_id"`
	StartedAt string  `json:"started_at"`
	EndedAt   *string `json:"ended_at"`
}

// openShift returns the rider's open shift and its scooter, locked.
func openShift(ctx context.Context, tx pgx.Tx, riderID string) (shift, string, bool, error) {
	var sh shift
	var scooter string
	var started time.Time
	err := tx.QueryRow(ctx, `SELECT id::text, node_id::text, scooter_id::text, started_at FROM shifts
		WHERE rider_id = $1 AND ended_at IS NULL FOR UPDATE`, riderID).Scan(&sh.ID, &sh.NodeID, &scooter, &started)
	if errors.Is(err, pgx.ErrNoRows) {
		return sh, "", false, nil
	}
	sh.StartedAt = stamp(started)
	return sh, scooter, err == nil, err
}

func (s *Service) startShift(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body struct {
		NodeID string `json:"node_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !idPattern.MatchString(body.NodeID) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send node_id.", false)
		return
	}
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		if err := identity.CheckSession(r.Context(), tx, actor); err != nil {
			return err
		}
		rd, err := readRider(r.Context(), tx, actor.ID)
		if err != nil {
			return err
		}
		if rd.status != "active" {
			return refuse("rider_not_approved", "Ninzo has not approved this rider yet.")
		}
		var verified bool
		if err := tx.QueryRow(r.Context(), `SELECT coalesce((SELECT status = 'verified' AND (expires_at IS NULL OR expires_at > now())
			FROM rider_verifications WHERE rider_id = $1 ORDER BY created_at DESC, (status = 'verified'), id LIMIT 1), false)`, actor.ID).Scan(&verified); err != nil {
			return err
		}
		if !verified {
			return refuse("rider_not_verified", "Documents are not verified.")
		}
		var nodeZone string
		err = tx.QueryRow(r.Context(), "SELECT zone_id::text FROM staging_nodes WHERE id = $1", body.NodeID).Scan(&nodeZone)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (rd.zoneID == nil || *rd.zoneID != nodeZone)) {
			return errNotFound // A node in another zone is not this rider's to use.
		}
		if err != nil {
			return err
		}
		if _, _, open, err := openShift(r.Context(), tx, actor.ID); err != nil || open {
			if err == nil {
				err = refuse("shift_already_open", "A shift is already open.")
			}
			return err
		}
		// A scooter is left holding this rider only if something went wrong. Free it, or this rider
		// could never start again.
		if _, err := tx.Exec(r.Context(), `UPDATE scooters SET current_rider_id = NULL, updated_at = now()
			WHERE current_rider_id = $1 AND NOT EXISTS (SELECT 1 FROM shifts WHERE scooter_id = scooters.id AND ended_at IS NULL)`, actor.ID); err != nil {
			return err
		}
		// Any free scooter waiting at this node. Locked rows are skipped, so two riders at once
		// never get the same one.
		var scooter string
		err = tx.QueryRow(r.Context(), `SELECT id::text FROM scooters
			WHERE node_id = $1 AND status = 'active' AND current_rider_id IS NULL
			ORDER BY code FOR UPDATE SKIP LOCKED LIMIT 1`, body.NodeID).Scan(&scooter)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("no_scooter_at_node", "No free scooter is waiting at this node.")
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), "UPDATE scooters SET current_rider_id = $2, updated_at = now() WHERE id = $1", scooter, actor.ID); err != nil {
			return err
		}
		var sh shift
		var started time.Time
		if err := tx.QueryRow(r.Context(), `INSERT INTO shifts (rider_id, node_id, scooter_id) VALUES ($1, $2, $3)
			RETURNING id::text, node_id::text, started_at`, actor.ID, body.NodeID, scooter).Scan(&sh.ID, &sh.NodeID, &started); err != nil {
			return err
		}
		sh.StartedAt = stamp(started)
		if err := bump(r.Context(), tx, actor.ID); err != nil {
			return err
		}
		if err := audit.Record(r.Context(), tx, audit.Event{
			ActorType: identity.TypeRider, ActorID: actor.ID, Action: "shift.started", EntityType: "shift", EntityID: sh.ID,
			After: map[string]any{"node_id": body.NodeID, "scooter_id": scooter},
		}); err != nil {
			return err
		}
		reply = map[string]any{"shift": sh, "server_time": serverTime()}
		return idempotency.Save(r.Context(), tx, http.StatusOK, reply)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, reply)
}

// endShift frees the scooter. Its stock stays on it, for the next rider.
func (s *Service) endShift(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var reply map[string]any
	err := database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		if err := identity.CheckSession(r.Context(), tx, actor); err != nil {
			return err
		}
		// A suspended rider can still end a shift, so their scooter is freed.
		sh, scooter, open, err := openShift(r.Context(), tx, actor.ID)
		if err != nil {
			return err
		}
		if !open {
			return refuse("no_open_shift", "No shift is open.")
		}
		if err := handBack(r.Context(), tx, actor.ID); err != nil {
			return err
		}
		var ended time.Time
		if err := tx.QueryRow(r.Context(), "UPDATE shifts SET ended_at = now() WHERE id = $1 RETURNING ended_at", sh.ID).Scan(&ended); err != nil {
			return err
		}
		e := stamp(ended)
		sh.EndedAt = &e
		if _, err := tx.Exec(r.Context(), "UPDATE scooters SET current_rider_id = NULL, updated_at = now() WHERE id = $1", scooter); err != nil {
			return err
		}
		if err := bump(r.Context(), tx, actor.ID); err != nil {
			return err
		}
		if err := audit.Record(r.Context(), tx, audit.Event{
			ActorType: identity.TypeRider, ActorID: actor.ID, Action: "shift.ended", EntityType: "shift", EntityID: sh.ID,
		}); err != nil {
			return err
		}
		reply = map[string]any{"shift": sh, "server_time": serverTime()}
		return idempotency.Save(r.Context(), tx, http.StatusOK, reply)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, reply)
}

type node struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	FullnessPct int     `json:"fullness_pct"`
	Lat         float64 `json:"lat"`
	Lng         float64 `json:"lng"`
}

func (s *Service) listNodes(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	zone := r.URL.Query().Get("zone_id")
	if !idPattern.MatchString(zone) {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send zone_id.", false)
		return
	}
	rd, err := liveRider(r.Context(), s.Pool, actor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Only the rider's own zone. Other zones' nodes, and how full they are, are not theirs to see.
	if rd.zoneID == nil || *rd.zoneID != zone {
		s.fail(w, r, errNotFound)
		return
	}
	// Fullness counts scooters parked there with no rider on them.
	rows, err := s.Pool.Query(r.Context(), `SELECT n.id::text, n.name, n.lat, n.lng,
			least(100, (100 * (SELECT count(*) FROM scooters c WHERE c.node_id = n.id AND c.current_rider_id IS NULL AND c.status = 'active')) / n.capacity)::int
		FROM staging_nodes n WHERE n.zone_id = $1 ORDER BY n.name, n.id`, zone)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	nodes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (node, error) {
		var n node
		err := row.Scan(&n.ID, &n.Name, &n.Lat, &n.Lng, &n.FullnessPct)
		return n, err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if nodes == nil {
		nodes = []node{}
	}
	var assigned *string
	if err := s.Pool.QueryRow(r.Context(), "SELECT (SELECT node_id::text FROM shifts WHERE rider_id = $1 AND ended_at IS NULL)", actor.ID).Scan(&assigned); err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"nodes": nodes, "assigned_node_id": assigned, "relocation": nil, "server_time": serverTime()})
}

type unit struct {
	LotID      string `json:"lot_id"`
	ItemID     string `json:"item_id"`
	Expiry     string `json:"expiry"`
	TempStatus string `json:"temp_status"`
}

// scanStock loads a whole lot onto the rider's scooter.
func (s *Service) scanStock(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	var body struct {
		LotCode   string `json:"lot_code"`
		ScannedAt string `json:"scanned_at"`
	}
	err := json.NewDecoder(r.Body).Decode(&body)
	// scanned_at is the device's time. It must be sent, but the server's time is the official one.
	_, timeErr := time.Parse(time.RFC3339, body.ScannedAt)
	if err != nil || strings.TrimSpace(body.LotCode) == "" || len(body.LotCode) > 64 || timeErr != nil {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send lot_code and scanned_at.", false)
		return
	}
	code := strings.TrimSpace(body.LotCode)
	var reply map[string]any
	err = database.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		if err := identity.CheckSession(r.Context(), tx, actor); err != nil {
			return err
		}
		if _, err := readRider(r.Context(), tx, actor.ID); err != nil {
			return err
		}
		var err error
		reply, err = ScanTx(r.Context(), tx, actor.ID, code, true)
		return err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, reply)
}

// ScanTx loads a whole lot onto the rider's scooter: the core of scanStock, shared with the
// offline batch. A refusal is an error RefusalOf can read.
func ScanTx(ctx context.Context, tx pgx.Tx, riderID, code string, save bool) (reply map[string]any, err error) {
	err = func() error {
		_, scooter, open, err := openShift(ctx, tx, riderID)
		if err != nil {
			return err
		}
		if !open {
			return refuse("no_open_shift", "Start a shift first.")
		}
		var u unit
		var qty int
		var status string
		var expires time.Time
		var expired bool
		// Locked, so one lot cannot go onto two scooters at once.
		err = tx.QueryRow(ctx, `SELECT id::text, item_id::text, qty, status, temp_status, expires_at, expires_at <= now()
			FROM lots WHERE code = $1 FOR UPDATE`, code).Scan(&u.LotID, &u.ItemID, &qty, &status, &u.TempStatus, &expires, &expired)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("bad_scan", "That code is not a known lot.")
		}
		if err != nil {
			return err
		}
		u.Expiry = stamp(expires)
		switch {
		case status == "quarantined":
			return refuse("lot_quarantined", "This lot is quarantined.")
		case expired:
			return refuse("lot_expired", "This lot has expired.")
		}
		var loaded bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM scooter_stock WHERE lot_id = $1)", u.LotID).Scan(&loaded); err != nil {
			return err
		}
		if loaded {
			return refuse("lot_already_on_scooter", "This lot is already on a scooter.")
		}
		if _, err := tx.Exec(ctx, "INSERT INTO scooter_stock (scooter_id, lot_id, on_hand) VALUES ($1, $2, $3)", scooter, u.LotID, qty); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO stock_ledger (scooter_id, lot_id, delta_on_hand, reason, actor_type, actor_id)
			VALUES ($1, $2, $3, 'scan', 'rider', $4)`, scooter, u.LotID, qty, riderID); err != nil {
			return err
		}
		if err := bump(ctx, tx, riderID); err != nil {
			return err
		}
		reply = map[string]any{"unit": u, "server_time": serverTime()}
		if !save {
			return nil
		}
		return idempotency.Save(ctx, tx, http.StatusOK, reply)
	}()
	return reply, err
}

// RefusalOf returns the contract's code and message when err is a refusal.
func RefusalOf(err error) (code, message string, ok bool) {
	var no *refusal
	if errors.As(err, &no) {
		return no.code, no.message, true
	}
	return "", "", false
}

type stockLot struct {
	LotID      string `json:"lot_id"`
	ItemID     string `json:"item_id"`
	Qty        int    `json:"qty"`
	Expiry     string `json:"expiry"`
	TempStatus string `json:"temp_status"`
}

// getStock lists what is on the rider's scooter now. With no shift there is no scooter, so nothing.
func (s *Service) getStock(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	if _, err := liveRider(r.Context(), s.Pool, actor); err != nil {
		s.fail(w, r, err)
		return
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT l.id::text, l.item_id::text, ss.on_hand - ss.reserved, l.expires_at, l.temp_status
		FROM shifts sh JOIN scooter_stock ss ON ss.scooter_id = sh.scooter_id JOIN lots l ON l.id = ss.lot_id
		WHERE sh.rider_id = $1 AND sh.ended_at IS NULL AND ss.on_hand > 0 ORDER BY l.expires_at, l.id`, actor.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	lots, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (stockLot, error) {
		var l stockLot
		var expires time.Time
		err := row.Scan(&l.LotID, &l.ItemID, &l.Qty, &expires, &l.TempStatus)
		l.Expiry = stamp(expires)
		return l, err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if lots == nil {
		lots = []stockLot{}
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"lots": lots, "server_time": serverTime()})
}

// handBack refuses to end a shift while the rider has an accepted order, and gives back an open
// offer, so the order goes to another rider. The order's lock is taken before the offer's, as
// every change to an order does, and the offer is read again under it.
func handBack(ctx context.Context, tx pgx.Tx, rider string) error {
	var offer, order, state string
	err := tx.QueryRow(ctx, "SELECT id::text, order_id::text FROM offers WHERE rider_id = $1 AND released_at IS NULL", rider).Scan(&offer, &order)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "SELECT 1 FROM orders WHERE id = $1 FOR UPDATE", order); err != nil {
		return err
	}
	var released *time.Time
	if err := tx.QueryRow(ctx, "SELECT state, released_at FROM offers WHERE id = $1 FOR UPDATE", offer).Scan(&state, &released); err != nil {
		return err
	}
	if released != nil {
		return nil
	}
	if state == "accepted" {
		return refuse("order_in_progress", "Finish the current order first.")
	}
	if _, err := tx.Exec(ctx, `UPDATE offers SET state = 'expired', ended_reason = 'reassigned', final_state = 'OFFERED', released_at = now(), updated_at = now()
		WHERE id = $1`, offer); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, "UPDATE orders SET state = 'PAYMENT_SECURED', updated_at = now() WHERE id = $1 AND state = 'OFFERED'", order)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		_, err = tx.Exec(ctx, `INSERT INTO order_events (order_id, from_state, to_state, actor_type, actor_id, reason)
			VALUES ($1, 'OFFERED', 'PAYMENT_SECURED', 'rider', $2, 'shift ended')`, order, rider)
	}
	return err
}
