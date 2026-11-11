// Package restock serves a rider's restock requests: the hubs, what the rider asked for, what the
// hub packed, and what it could not fill. Until the partner APIs (v0.8.0), a simulator plays the
// hub staff's part.
package restock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/audit"
	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/outbox"
)

var idPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

var (
	errNotFound = errors.New("restock: not found")
	errBlocked  = errors.New("restock: blocked")
	// ErrNotFound lets the offline batch tell a missing request from other errors.
	ErrNotFound = errNotFound
)

// Open are the states of a request still in progress.
var Open = []string{"confirmed", "hub_accepted", "packing", "ready", "picked_up", "partial"}

// Service serves restock requests.
type Service struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
	now  func() time.Time
}

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func serverTime() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func stamp(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func (s *Service) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, identity.ErrSessionInvalid):
		httpserver.WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in again.", false)
	case errors.Is(err, errBlocked):
		identity.WriteBlocked(w)
	case errors.Is(err, errNotFound):
		httpserver.WriteError(w, http.StatusNotFound, "not_found", "Not found.", false)
	default:
		s.Log.Error("restock: request failed", "request_id", httpserver.RequestID(r.Context()), "error", err.Error())
		httpserver.WriteError(w, http.StatusInternalServerError, "internal_error", "Try again shortly.", true)
	}
}

// rider checks the session is live and the rider may act, and returns their zone.
func (s *Service) rider(r *http.Request, a identity.Actor) (*string, error) {
	live, err := identity.SessionLive(r.Context(), s.Pool, a)
	if err != nil {
		return nil, err
	}
	if !live {
		return nil, identity.ErrSessionInvalid
	}
	var zone *string
	var status, person string
	err = s.Pool.QueryRow(r.Context(), "SELECT r.zone_id::text, r.status, p.status FROM riders r JOIN people p ON p.id = r.person_id WHERE r.id = $1", a.ID).
		Scan(&zone, &status, &person)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, identity.ErrSessionInvalid
	}
	if err == nil && (person != "active" || status == "suspended") {
		err = errBlocked
	}
	return zone, err
}

// MountRider adds getRestockOptions and getRestock.
func (s *Service) MountRider(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.Get("/restock/options", s.options)
		g.Get("/restock/{request_id}", s.get)
	})
}

// View is a restock request as its rider sees it (contract: RestockRequest).
func View(ctx context.Context, q querier, id string) (map[string]any, error) {
	var state, hubID, hubName string
	var hubLat, hubLng float64
	var slot, arrive *time.Time
	err := q.QueryRow(ctx, `SELECT r.state, h.id::text, h.name, h.lat, h.lng, r.slot_at, r.arrive_by FROM restock_requests r JOIN hubs h ON h.id = r.hub_id
		WHERE r.id = $1`, id).Scan(&state, &hubID, &hubName, &hubLat, &hubLng, &slot, &arrive)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT l.item_id::text, m.name, l.qty, l.filled_qty FROM restock_lines l JOIN menu_items m ON m.id = l.item_id
		WHERE l.request_id = $1 ORDER BY m.name, l.item_id`, id)
	if err != nil {
		return nil, err
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (map[string]any, error) {
		var item, name string
		var qty int
		var filled *int
		err := r.Scan(&item, &name, &qty, &filled)
		var f any
		if filled != nil {
			f = *filled
		}
		return map[string]any{"item_id": item, "name": name, "qty": qty, "filled_qty": f}, err
	})
	if err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, "SELECT l.code FROM restock_lots x JOIN lots l ON l.id = x.lot_id WHERE x.request_id = $1 AND x.released_at IS NULL ORDER BY l.code", id)
	if err != nil {
		return nil, err
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	var shortfall any
	var action string
	var subID, subName, otherID, otherName *string
	var otherLat, otherLng, nodeLat, nodeLng *float64
	// The distance is from the node the rider is at now, so it is null with no open shift.
	err = q.QueryRow(ctx, `SELECT s.action, s.substitute_item_id::text, m.name, s.other_hub_id::text, h.name, h.lat, h.lng, n.lat, n.lng FROM shortfalls s
		JOIN restock_requests r ON r.id = s.request_id
		LEFT JOIN menu_items m ON m.id = s.substitute_item_id LEFT JOIN hubs h ON h.id = s.other_hub_id
		LEFT JOIN shifts sh ON sh.rider_id = r.rider_id AND sh.ended_at IS NULL LEFT JOIN staging_nodes n ON n.id = sh.node_id
		WHERE s.request_id = $1 AND s.status = 'open'`, id).Scan(&action, &subID, &subName, &otherID, &otherName, &otherLat, &otherLng, &nodeLat, &nodeLng)
	switch {
	case err == nil:
		var sub, other any
		if subID != nil {
			sub = map[string]any{"item_id": *subID, "name": *subName}
		}
		if otherID != nil {
			var distance any
			if nodeLat != nil && nodeLng != nil && otherLat != nil && otherLng != nil {
				distance = int(math.Round(geo.Metres(*nodeLat, *nodeLng, *otherLat, *otherLng)))
			}
			other = map[string]any{"id": *otherID, "name": *otherName, "lat": *otherLat, "lng": *otherLng, "distance_m": distance}
		}
		shortfall = map[string]any{"action": action, "substitute": sub, "other_hub": other}
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}
	// ready_in_s counts down to the hub's slot. It stops at 0 once the slot has passed.
	var readyIn any
	if slot != nil {
		readyIn = int(math.Max(0, math.Ceil(time.Until(*slot).Seconds())))
	}
	return map[string]any{"id": id, "state": state, "hub": map[string]any{"id": hubID, "name": hubName, "lat": hubLat, "lng": hubLng}, "items": items,
		"shortfall": shortfall, "slot_at": stamp(slot), "arrive_by": stamp(arrive), "lot_numbers": codes, "ready_in_s": readyIn}, nil
}

// Current is the rider's request in progress, for the rider's state, or nil.
func Current(ctx context.Context, q querier, rider string) (any, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT id::text FROM restock_requests WHERE rider_id = $1 AND state = ANY($2) ORDER BY created_at DESC LIMIT 1`, rider, Open).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return View(ctx, q, id)
}

func (s *Service) options(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	zone, err := s.rider(r, actor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	hubs := []map[string]any{}
	if zone != nil {
		rows, err := s.Pool.Query(r.Context(), "SELECT id::text, name, lat, lng FROM hubs WHERE zone_id = $1 AND status = 'active' ORDER BY name, id", *zone)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if hubs, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (map[string]any, error) {
			var id, name string
			var lat, lng float64
			err := r.Scan(&id, &name, &lat, &lng)
			return map[string]any{"id": id, "name": name, "lat": lat, "lng": lng}, err
		}); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	rows, err := s.Pool.Query(r.Context(), "SELECT id::text, name FROM menu_items WHERE status = 'active' ORDER BY sort_order, name, id")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (map[string]any, error) {
		var id, name string
		err := r.Scan(&id, &name)
		return map[string]any{"item_id": id, "name": name}, err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"hubs": hubs, "items": items, "server_time": serverTime()})
}

func (s *Service) get(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	id := chi.URLParam(r, "request_id")
	if _, err := s.rider(r, actor); err != nil {
		s.fail(w, r, err)
		return
	}
	var mine bool
	if idPattern.MatchString(id) {
		if err := s.Pool.QueryRow(r.Context(), "SELECT EXISTS (SELECT 1 FROM restock_requests WHERE id = $1 AND rider_id = $2)", id, actor.ID).Scan(&mine); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if !mine {
		s.fail(w, r, errNotFound)
		return
	}
	v, err := View(r.Context(), s.Pool, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"request": v, "server_time": serverTime()})
}

// bumpRider raises the rider's seq and pushes, so the rider app sees the request moved.
// bumpInHubOrder bumps several of a rider's requests in hub order, so two changes that touch the
// same two hubs take their hub_state rows in the same order.
func bumpInHubOrder(ctx context.Context, tx pgx.Tx, rider string, requests ...string) error {
	rows, err := tx.Query(ctx, "SELECT id::text FROM restock_requests WHERE id = ANY($1::uuid[]) ORDER BY hub_id, id", requests)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := bumpRider(ctx, tx, rider, id); err != nil {
			return err
		}
	}
	return nil
}

// BumpRider records a change the rider sees to a request, and the hub with it.
func BumpRider(ctx context.Context, tx pgx.Tx, rider, request string) error {
	return bumpRider(ctx, tx, rider, request)
}

func bumpRider(ctx context.Context, tx pgx.Tx, rider, request string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO rider_state (rider_id, seq) VALUES ($1, 1)
		ON CONFLICT (rider_id) DO UPDATE SET seq = rider_state.seq + 1, updated_at = now()`, rider); err != nil {
		return err
	}
	// The hub sees every change to its requests too: the request's seq and the hub's rise with it.
	// A suggestion, or one declined or expired without being confirmed, is the rider's draft, so
	// the hub's seq does not move for it.
	var hub string
	var draft bool
	if err := tx.QueryRow(ctx, `UPDATE restock_requests SET seq = seq + 1, updated_at = now() WHERE id = $1
		RETURNING hub_id::text, source = 'suggested' AND state IN ('suggested', 'expired', 'cancelled') AND accepted_by IS NULL AND slot_at IS NULL`, request).Scan(&hub, &draft); err != nil {
		return err
	}
	if !draft {
		if _, err := tx.Exec(ctx, `INSERT INTO hub_state (hub_id, seq) VALUES ($1, 1)
			ON CONFLICT (hub_id) DO UPDATE SET seq = hub_state.seq + 1, updated_at = now()`, hub); err != nil {
			return err
		}
	}
	return outbox.Add(ctx, tx, "push.rider", map[string]any{"rider_id": rider, "kind": "restock", "request_id": request})
}

// move changes a locked request's state.
func move(ctx context.Context, tx pgx.Tx, id, to string, now time.Time) (string, error) {
	var rider string
	err := tx.QueryRow(ctx, "UPDATE restock_requests SET state = $2, state_changed_at = $3, updated_at = now() WHERE id = $1 RETURNING rider_id::text",
		id, to, now).Scan(&rider)
	return rider, err
}

// Tick is one pass of the real restock jobs: the stock check that drafts suggestions, and the
// expiry of cassettes nobody collected. It never acts for hub staff.
func (s *Service) Tick(ctx context.Context, now time.Time) {
	if _, err := s.Suggest(ctx, now); err != nil && ctx.Err() == nil {
		s.Log.Error("restock: stock check failed", "error", err.Error())
	}
	if err := s.ExpireUncollected(ctx, now); err != nil && ctx.Err() == nil {
		s.Log.Error("restock: expiring cassettes failed", "error", err.Error())
	}
	if err := s.ExpireUnaccepted(ctx, now); err != nil && ctx.Err() == nil {
		s.Log.Error("restock: expiring unaccepted requests failed", "error", err.Error())
	}
}

// ExpireUnaccepted ends confirmed requests no hub accepted within the zone's restock_accept_s, so
// the scooter can ask again elsewhere. Nothing is packed yet, so nothing goes back on a shelf. Each
// request has its own transaction.
func (s *Service) ExpireUnaccepted(ctx context.Context, now time.Time) error {
	skip := []string{}
	for i := 0; i < 200; i++ {
		done := false
		var id string
		err := database.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
			var rider, zone string
			var since time.Time
			err := tx.QueryRow(ctx, `SELECT r.id::text, r.rider_id::text, h.zone_id::text, r.state_changed_at FROM restock_requests r JOIN hubs h ON h.id = r.hub_id
				WHERE r.state = 'confirmed' AND r.state_changed_at <= $1::timestamptz - interval '2 minutes' AND NOT (r.id::text = ANY($2::text[]))
				ORDER BY r.state_changed_at, r.id LIMIT 1 FOR UPDATE OF r SKIP LOCKED`, now, skip).Scan(&id, &rider, &zone, &since)
			if errors.Is(err, pgx.ErrNoRows) {
				done = true
				return nil
			}
			if err != nil {
				return err
			}
			settings, err := geo.LoadSettings(ctx, tx, zone)
			if err != nil {
				return err
			}
			limit, _ := settings.Get("restock_accept_s")
			if now.Sub(since) < time.Duration(limit)*time.Second {
				skip = append(skip, id) // Not due yet in this zone.
				return nil
			}
			if _, err := tx.Exec(ctx, "UPDATE restock_requests SET state = 'expired', reason = coalesce(reason, 'no hub accepted it'), state_changed_at = $2, updated_at = now() WHERE id = $1",
				id, now); err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, audit.Event{ActorType: audit.ActorSystem, Action: "restock.unaccepted", EntityType: "restock_request", EntityID: id}); err != nil {
				return err
			}
			return bumpRider(ctx, tx, rider, id)
		})
		if err != nil {
			if id == "" || ctx.Err() != nil {
				return err
			}
			s.Log.Error("restock: expiring an unaccepted request failed", "request_id", id, "error", err.Error())
			skip = append(skip, id)
			continue
		}
		if done {
			return nil
		}
	}
	return nil
}

// RunJobs runs Tick every so often until ctx ends. A failure or panic is logged, never fatal.
func (s *Service) RunJobs(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			func() {
				defer func() {
					if p := recover(); p != nil {
						s.Log.Error("restock: jobs panicked", "panic", fmt.Sprint(p))
					}
				}()
				s.Tick(ctx, s.clock())
			}()
		}
	}
}

// Simulator plays the hub staff, for tests only since the hub APIs (v0.8.0 part F). At a hub with no
// active staff, it accepts a confirmed request, packs it,
// and marks it ready, packing whole lots from the hub's own stock, earliest expiry first. What the
// hub cannot fill becomes a shortfall: another hub in the zone that has it, or a partial fill.
type Simulator struct {
	Service     *Service
	AcceptAfter time.Duration // From confirmed to hub_accepted. 20 seconds.
	PackAfter   time.Duration // From hub_accepted to packing. 20 seconds.
	ReadyAfter  time.Duration // From packing to ready. 60 seconds.
	ArriveIn    time.Duration // The rider should arrive this long after ready. 10 minutes.
}

func (sim *Simulator) defaults() {
	if sim.AcceptAfter == 0 {
		sim.AcceptAfter = 20 * time.Second
	}
	if sim.PackAfter == 0 {
		sim.PackAfter = 20 * time.Second
	}
	if sim.ReadyAfter == 0 {
		sim.ReadyAfter = time.Minute
	}
	if sim.ArriveIn == 0 {
		sim.ArriveIn = 10 * time.Minute
	}
}

// Run checks stock and moves requests along every so often until ctx ends. A failure is logged,
// never fatal.
func (sim *Simulator) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			func() {
				defer func() {
					if p := recover(); p != nil {
						sim.Service.Log.Error("restock: simulator panicked")
					}
				}()
				now := sim.Service.clock()
				if _, err := sim.Service.Suggest(ctx, now); err != nil && ctx.Err() == nil {
					sim.Service.Log.Error("restock: stock check failed", "error", err.Error())
				}
				if _, err := sim.Step(ctx, now); err != nil && ctx.Err() == nil {
					sim.Service.Log.Error("restock: simulator failed", "error", err.Error())
				}
				if err := sim.expireUncollected(ctx, now); err != nil && ctx.Err() == nil {
					sim.Service.Log.Error("restock: expiring cassettes failed", "error", err.Error())
				}
			}()
		}
	}
}

// Step moves every request that is due by one state, each in its own transaction under its row
// lock. It returns how many it moved.
func (sim *Simulator) Step(ctx context.Context, now time.Time) (int, error) {
	sim.defaults()
	moved := 0
	for _, step := range []struct {
		from  string
		after time.Duration
	}{{"packing", sim.ReadyAfter}, {"hub_accepted", sim.PackAfter}, {"confirmed", sim.AcceptAfter}} {
		skip := []string{}
		for i := 0; i < 200; i++ {
			done := false
			var id string
			err := database.InTx(ctx, sim.Service.Pool, func(tx pgx.Tx) error {
				err := tx.QueryRow(ctx, `SELECT id::text FROM restock_requests WHERE state = $1 AND state_changed_at <= $2
					AND NOT (id::text = ANY($3::text[]))
					AND NOT EXISTS (SELECT 1 FROM hub_staff s WHERE s.hub_id = restock_requests.hub_id AND s.status = 'active')
					ORDER BY state_changed_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`, step.from, now.Add(-step.after), skip).Scan(&id)
				if errors.Is(err, pgx.ErrNoRows) {
					done = true
					return nil
				}
				if err != nil {
					return err
				}
				return sim.advance(ctx, tx, id, step.from, now)
			})
			if err != nil {
				if id == "" || ctx.Err() != nil {
					return moved, err
				}
				sim.Service.Log.Error("restock: moving a request failed", "request_id", id, "error", err.Error())
				skip = append(skip, id)
				continue
			}
			if done {
				break
			}
			moved++
		}
	}
	return moved, nil
}

func (sim *Simulator) advance(ctx context.Context, tx pgx.Tx, id, from string, now time.Time) error {
	switch from {
	case "confirmed":
		// The hub gives a slot when the packing will be done, and when the rider should come.
		slot := now.Add(sim.PackAfter + sim.ReadyAfter)
		var rider string
		if err := tx.QueryRow(ctx, `UPDATE restock_requests SET state = 'hub_accepted', state_changed_at = $2, slot_at = $3, arrive_by = $4, updated_at = now()
			WHERE id = $1 RETURNING rider_id::text`, id, now, slot, slot.Add(sim.ArriveIn)).Scan(&rider); err != nil {
			return err
		}
		return bumpRider(ctx, tx, rider, id)
	case "hub_accepted":
		rider, err := move(ctx, tx, id, "packing", now)
		if err != nil {
			return err
		}
		return bumpRider(ctx, tx, rider, id)
	}
	short, err := pack(ctx, tx, id, now)
	if err != nil {
		return err
	}
	to := "ready"
	if short {
		to = "partial"
	}
	rider, err := move(ctx, tx, id, to, now)
	if err != nil {
		return err
	}
	return bumpRider(ctx, tx, rider, id)
}

// pack puts whole lots from the hub into the cassette for each line, earliest expiry first, and
// records what was filled. Lots are whole, so filled_qty can be more than qty; below qty is short. Lots are locked, so two hubs or passes never pack the same one. It
// returns whether any line was short, and then records the shortfall.
func pack(ctx context.Context, tx pgx.Tx, id string, now time.Time) (bool, error) {
	var hub, zone string
	if err := tx.QueryRow(ctx, "SELECT r.hub_id::text, h.zone_id::text FROM restock_requests r JOIN hubs h ON h.id = r.hub_id WHERE r.id = $1", id).Scan(&hub, &zone); err != nil {
		return false, err
	}
	// One packer at a hub at a time, as one person packs at a bench. Lots are then read with no
	// skipping, so a free lot is never missed.
	if _, err := tx.Exec(ctx, "SELECT 1 FROM hubs WHERE id = $1 FOR UPDATE", hub); err != nil {
		return false, err
	}
	rows, err := tx.Query(ctx, "SELECT item_id::text, qty FROM restock_lines WHERE request_id = $1 ORDER BY item_id", id)
	if err != nil {
		return false, err
	}
	type line struct {
		item string
		qty  int
	}
	lines, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (line, error) {
		var l line
		return l, r.Scan(&l.item, &l.qty)
	})
	if err != nil {
		return false, err
	}
	shortItems, shortQty := []string{}, []int{}
	for _, l := range lines {
		rows, err := tx.Query(ctx, `SELECT l.id::text, l.qty FROM lots l
			WHERE l.hub_id = $1 AND l.item_id = $2 AND l.status = 'ok' AND l.temp_status = 'ok' AND l.expires_at > $3
				AND NOT EXISTS (SELECT 1 FROM scooter_stock ss WHERE ss.lot_id = l.id)
				AND NOT EXISTS (SELECT 1 FROM restock_lots x WHERE x.lot_id = l.id AND x.released_at IS NULL)
			ORDER BY l.expires_at, l.id`, hub, l.item, now)
		if err != nil {
			return false, err
		}
		type lot struct {
			id  string
			qty int
		}
		lots, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (lot, error) {
			var x lot
			return x, r.Scan(&x.id, &x.qty)
		})
		if err != nil {
			return false, err
		}
		filled := 0
		for _, x := range lots {
			if filled >= l.qty {
				break
			}
			if _, err := tx.Exec(ctx, "INSERT INTO restock_lots (request_id, lot_id) VALUES ($1, $2)", id, x.id); err != nil {
				return false, err
			}
			filled += x.qty
		}
		if _, err := tx.Exec(ctx, "UPDATE restock_lines SET filled_qty = $3 WHERE request_id = $1 AND item_id = $2", id, l.item, filled); err != nil {
			return false, err
		}
		if filled < l.qty {
			shortItems = append(shortItems, l.item)
			shortQty = append(shortQty, l.qty-filled)
		}
	}
	if len(shortItems) == 0 {
		return false, nil
	}
	// Another hub in the zone that has every short item is offered; otherwise a partial fill.
	var other *string
	err = tx.QueryRow(ctx, `SELECT h.id::text FROM hubs h WHERE h.zone_id = $1 AND h.id <> $2 AND h.status = 'active'
			AND NOT EXISTS (SELECT 1 FROM unnest($3::uuid[], $5::int[]) AS want(item, qty) WHERE want.qty > (
				SELECT coalesce(sum(l.qty), 0) FROM lots l WHERE l.hub_id = h.id AND l.item_id = want.item AND l.status = 'ok' AND l.temp_status = 'ok'
					AND l.expires_at > $4
					AND NOT EXISTS (SELECT 1 FROM scooter_stock ss WHERE ss.lot_id = l.id)
					AND NOT EXISTS (SELECT 1 FROM restock_lots x WHERE x.lot_id = l.id AND x.released_at IS NULL)))
		ORDER BY h.name, h.id LIMIT 1`, zone, hub, shortItems, now, shortQty).Scan(&other)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	action := "partial"
	if other != nil {
		action = "other_hub"
	}
	if _, err := tx.Exec(ctx, "INSERT INTO shortfalls (request_id, action, other_hub_id) VALUES ($1, $2, $3)", id, action, other); err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO shortfall_lines (request_id, item_id, qty_missing) SELECT $1, item, least(qty, 500) FROM unnest($2::uuid[], $3::int[]) AS short(item, qty)`,
		id, shortItems, shortQty)
	return true, err
}
