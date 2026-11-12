package console

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/orders"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

// livePollEvery is the contract's shortest time between two polls of one zone by one user.
const livePollEvery = 5 * time.Second

// liveOrderStates are the orders "in progress" on the map: paid and not yet handed over or ended.
// Unpaid orders (PAYMENT_PENDING, PAYMENT_FAILED) are not on the street, and from HANDED_OVER the
// food is with the customer. Migration 0047's partial index lists the same states; keep them equal.
const liveOrderStates = `'PAYMENT_SECURED', 'OFFERED', 'NO_SCOOTER', 'RIDER_ACCEPTED', 'PACKED', 'TIMER_RUNNING', 'ARRIVED'`

// liveLimiter remembers when each (user, zone) last polled. It is in memory: console-api is one
// process, and the worst a restart or a second copy can do is let one extra poll through.
type liveLimiter struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// allow records a poll at now and returns 0, or how long the caller must still wait.
func (l *liveLimiter) allow(key string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if last, ok := l.seen[key]; ok && now.Sub(last) < livePollEvery && !now.Before(last) {
		return livePollEvery - now.Sub(last)
	}
	if l.seen == nil {
		l.seen = map[string]time.Time{}
	}
	if len(l.seen) >= 4096 { // Forget the polls that no longer count, so the map cannot grow for ever.
		for k, t := range l.seen {
			if now.Sub(t) >= livePollEvery {
				delete(l.seen, k)
			}
		}
	}
	l.seen[key] = now
	return 0
}

// MountLive adds getLiveZone (v0.10.0 part E): map.read. It only reads, so it is not audited.
func (s *Service) MountLive(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.With(s.Require("map.read", false)).Get("/admin/zones/{zone_id}/live", s.getLiveZone)
	})
}

// The SQL for the four reads. Each is one statement for the whole zone: no query per rider,
// scooter or order.
const (
	// The zone, and the incidents open in it. An incident has no zone of its own: it takes its
	// order's zone, or else its rider's. One with neither (a ticket about no order) is in no zone.
	liveZoneSQL = `SELECT (SELECT count(*) FROM incidents i
			LEFT JOIN orders o ON o.id = i.order_id
			LEFT JOIN riders r ON r.id = i.rider_id
		WHERE i.status IN ('open', 'assigned') AND coalesce(o.zone_id, r.zone_id) = z.id)
	FROM zones z WHERE z.id = $1`

	// Riders on an open shift whose scooter belongs to the zone. The latest ping comes from a
	// lateral join that walks location_pings_rider_idx (rider_id, received_at DESC) one row deep.
	// The flags come from the unique partial indexes on offers and relocations.
	liveRidersSQL = `SELECT r.id::text, r.name, lp.lat, lp.lng, lp.received_at, n.lat, n.lng,
		sh.break_started_at IS NOT NULL,
		coalesce(f.accepted, false), coalesce(f.offered, false),
		EXISTS (SELECT 1 FROM relocations x WHERE x.rider_id = r.id AND x.state = 'accepted')
	FROM shifts sh
	JOIN scooters sc ON sc.id = sh.scooter_id
	JOIN riders r ON r.id = sh.rider_id
	JOIN staging_nodes n ON n.id = sh.node_id
	LEFT JOIN LATERAL (SELECT p.lat, p.lng, p.received_at FROM location_pings p
		WHERE p.rider_id = r.id ORDER BY p.received_at DESC LIMIT 1) lp ON true
	LEFT JOIN LATERAL (SELECT bool_or(o.state = 'accepted') AS accepted, bool_or(o.state = 'open' AND o.expires_at > $2) AS offered
		FROM offers o WHERE o.rider_id = r.id AND o.released_at IS NULL) f ON true
	WHERE sh.ended_at IS NULL AND sc.zone_id = $1
	ORDER BY r.name, r.id`

	liveScootersSQL = `SELECT s.id::text, s.code, s.node_id::text, s.current_rider_id::text,
		coalesce((SELECT sum(st.on_hand) FROM scooter_stock st WHERE st.scooter_id = s.id), 0)::int
	FROM scooters s WHERE s.zone_id = $1
		AND (s.status = 'active' OR EXISTS (SELECT 1 FROM shifts sh WHERE sh.scooter_id = s.id AND sh.ended_at IS NULL))
	ORDER BY s.code, s.id`

	// The gate is the version the order was placed to. Only its position is read: not the label,
	// the entry notes, the customer or the phone.
	liveOrdersSQL = `SELECT o.id::text, o.order_no, o.state, g.lat, g.lng, o.rider_id::text, o.timer_started_at
	FROM orders o JOIN gate_history g ON g.id = o.gate_history_id
	WHERE o.zone_id = $1 AND o.state IN (` + liveOrderStates + `)
	ORDER BY o.created_at, o.id`
)

// riderState picks the one state a rider shows on the map. When several are true the first of
// these wins: on_order (an accepted order not yet handed over), offered (an offer waiting for
// an answer), moving (an accepted relocation), on_break, idle. An order outranks everything
// because it is what the rider is doing and what ops watch; an offer comes next because it is
// time-limited and the rider must answer; a relocation outranks a break because paid moving time
// is running; a break ranks over idle only. The app refuses a break with an order or an offer,
// so several can be true only for a moment. An order and an offer never both hold: a rider has one
// live offer, accepted or open.
func riderState(onOrder, offered, moving, onBreak bool) string {
	switch {
	case onOrder:
		return "on_order"
	case offered:
		return "offered"
	case moving:
		return "moving"
	case onBreak:
		return "on_break"
	}
	return "idle"
}

// LiveZone is the contract's LiveZone for one zone, read in one snapshot. errNotFound is an
// unknown zone.
func LiveZone(ctx context.Context, q reader, zone string, now time.Time) (map[string]any, error) {
	var open int
	if err := q.QueryRow(ctx, liveZoneSQL, zone).Scan(&open); errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	} else if err != nil {
		return nil, err
	}

	riders := []map[string]any{}
	rows, err := q.Query(ctx, liveRidersSQL, zone, now)
	if err != nil {
		return nil, err
	}
	var id, name string
	var pingLat, pingLng *float64
	var pinged *time.Time
	var nodeLat, nodeLng float64
	var onBreak, onOrder, offered, moving bool
	if _, err := pgx.ForEachRow(rows, []any{&id, &name, &pingLat, &pingLng, &pinged, &nodeLat, &nodeLng, &onBreak, &onOrder, &offered, &moving}, func() error {
		// A rider who has not sent a ping yet is shown at the node they started the shift at.
		lat, lng := nodeLat, nodeLng
		if pingLat != nil && pingLng != nil {
			lat, lng = *pingLat, *pingLng
		}
		riders = append(riders, map[string]any{"id": id, "name": name, "lat": lat, "lng": lng, "last_ping_at": stamp(pinged),
			"state": riderState(onOrder, offered, moving, onBreak)})
		return nil
	}); err != nil {
		return nil, err
	}

	scooters := []map[string]any{}
	rows, err = q.Query(ctx, liveScootersSQL, zone)
	if err != nil {
		return nil, err
	}
	var plate string
	var node, rider *string
	var units int
	if _, err := pgx.ForEachRow(rows, []any{&id, &plate, &node, &rider, &units}, func() error {
		scooters = append(scooters, map[string]any{"id": id, "plate": plate, "node_id": nullable(node), "rider_id": nullable(rider), "stock_units": units})
		return nil
	}); err != nil {
		return nil, err
	}

	live := []map[string]any{}
	rows, err = q.Query(ctx, liveOrdersSQL, zone)
	if err != nil {
		return nil, err
	}
	var orderNo int64
	var state string
	var gateLat, gateLng float64
	var timer *time.Time
	if _, err := pgx.ForEachRow(rows, []any{&id, &orderNo, &state, &gateLat, &gateLng, &rider, &timer}, func() error {
		live = append(live, map[string]any{"id": id, "order_number": orders.OrderNumber(orderNo), "state": state,
			"gate": map[string]any{"lat": gateLat, "lng": gateLng}, "rider_id": nullable(rider), "timer_started_at": stamp(timer)})
		return nil
	}); err != nil {
		return nil, err
	}
	return map[string]any{"riders": riders, "scooters": scooters, "orders": live, "incidents_open": open, "server_time": now.UTC().Format(time.RFC3339Nano)}, nil
}

func (s *Service) getLiveZone(w http.ResponseWriter, r *http.Request) {
	zone := strings.ToLower(chi.URLParam(r, "zone_id"))
	if !idPattern.MatchString(zone) {
		httpserver.WriteError(w, http.StatusNotFound, "not_found", "Not found.", false)
		return
	}
	actor, _ := identity.ActorFrom(r.Context())
	now := s.clock()
	if wait := s.live.allow(actor.ID+"/"+zone, now); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int((wait+time.Second-1)/time.Second)))
		httpserver.WriteError(w, http.StatusTooManyRequests, "rate_limited", "Poll the live zone no more often than every 5 seconds.", true)
		return
	}
	// One snapshot, so a rider, their scooter and their order always agree with each other.
	var reply map[string]any
	err := pgx.BeginTxFunc(r.Context(), s.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		reply, err = LiveZone(r.Context(), tx, zone, now)
		return err
	})
	s.write(w, r, http.StatusOK, reply, err)
}
