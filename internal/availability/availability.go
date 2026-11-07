// Package availability works out what a customer can really get at a place: items on a scooter
// whose rider is on shift, within reach under the zone's settings, and on sale at this hour.
package availability

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teamprosperlink/Ninzo_backend/internal/catalog"
	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

// staleAfter tells the app when to ask again.
const staleAfter = 30 * time.Second

var idPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// india sets the selling hours. India has no daylight saving, so a fixed offset is exact.
var india = time.FixedZone("IST", 5*3600+1800)

// Reasons, when nothing is on offer. The contract keeps this an open list.
const (
	ReasonOutOfArea  = "out_of_service_area"
	ReasonZoneOff    = "zone_off"
	ReasonNoCoverage = "no_coverage"
	ReasonSoldOut    = "sold_out"
)

// Line is one item in reach, and how many are left.
type Line struct {
	Item    catalog.Item `json:"item"`
	QtyLeft int          `json:"qty_left"`
}

// Result is what can be offered at a place now.
type Result struct {
	Offer  bool
	Reason string
	Lines  []Line
}

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// reachable lists scooters a rider on an open shift has, with the rider and person active, whose
// node is within reach. Rider GPS replaces the node's position in v0.5.0.
const reachable = `
	SELECT s.id FROM scooters s
	JOIN shifts sh ON sh.scooter_id = s.id AND sh.ended_at IS NULL
	JOIN riders r ON r.id = sh.rider_id AND r.status = 'active' AND r.zone_id = s.zone_id
	JOIN people p ON p.id = r.person_id AND p.status = 'active'
	JOIN staging_nodes n ON n.id = s.node_id
	WHERE s.zone_id = $1 AND s.status = 'active'
		AND extensions.ST_DWithin(
			extensions.ST_SetSRID(extensions.ST_MakePoint(n.lng, n.lat), 4326)::extensions.geography,
			extensions.ST_SetSRID(extensions.ST_MakePoint($3, $2), 4326)::extensions.geography, $4)`

// At works out what can be offered at a point. now sets the selling hours.
func At(ctx context.Context, q querier, lat, lng float64, now time.Time) (Result, error) {
	none := Result{Lines: []Line{}}
	zone, found, err := geo.ZoneAt(ctx, q, lat, lng)
	if err != nil {
		return Result{}, err
	}
	if !found {
		// ZoneAt finds only active zones, so tell a paused zone apart from no zone at all.
		var paused bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM zones WHERE status <> 'active' AND area IS NOT NULL
			AND extensions.ST_Covers(area, extensions.ST_SetSRID(extensions.ST_MakePoint($2, $1), 4326)))`, lat, lng).Scan(&paused); err != nil {
			return Result{}, err
		}
		none.Reason = ReasonOutOfArea
		if paused {
			none.Reason = ReasonZoneOff
		}
		return none, nil
	}
	settings, err := geo.LoadSettings(ctx, q, zone)
	if err != nil {
		return Result{}, err
	}
	budget, _ := settings.Get("sweet_spot_budget_s")
	speed, _ := settings.Get("straight_line_speed_kmh")
	radius, _ := settings.Get("max_search_radius_m")
	// The furthest a scooter can be: the nearer of the widest search and what the budget allows.
	reach := math.Min(float64(radius), float64(speed)/3.6*float64(budget))

	var scooters int
	if err := q.QueryRow(ctx, "SELECT count(*) FROM ("+reachable+") r", zone, lat, lng, reach).Scan(&scooters); err != nil {
		return Result{}, err
	}
	if scooters == 0 {
		none.Reason = ReasonNoCoverage
		return none, nil
	}

	local := now.In(india)
	weekday := (int(local.Weekday()) + 6) % 7 // Monday is 0.
	minute := local.Hour()*60 + local.Minute()
	rows, err := q.Query(ctx, `
		WITH reach AS (`+reachable+`),
		per_scooter AS (
			SELECT ss.scooter_id, l.item_id, sum(ss.on_hand - ss.reserved)::int AS qty
			FROM scooter_stock ss JOIN reach ON reach.id = ss.scooter_id JOIN lots l ON l.id = ss.lot_id
			WHERE l.status = 'ok' AND l.temp_status = 'ok' AND l.expires_at > now()
			GROUP BY ss.scooter_id, l.item_id
		),
		-- One order comes from one scooter, so the most that can be promised is the best single
		-- scooter's quantity, not the total across scooters.
		stock AS (SELECT item_id, max(qty) AS qty FROM per_scooter GROUP BY item_id)
		SELECT m.id::text, m.name, m.description, m.price_paise, m.currency, m.allergens, m.photo_url, st.qty
		FROM stock st JOIN menu_items m ON m.id = st.item_id
		WHERE st.qty > 0 AND m.status = 'active'
			AND (NOT EXISTS (SELECT 1 FROM availability_rules ar WHERE ar.item_id = m.id AND (ar.zone_id IS NULL OR ar.zone_id = $1))
				OR EXISTS (SELECT 1 FROM availability_rules ar WHERE ar.item_id = m.id AND (ar.zone_id IS NULL OR ar.zone_id = $1)
					AND (ar.weekdays >> $5::int) & 1 = 1 AND $6::int >= ar.start_minute AND $6::int < ar.end_minute))
		ORDER BY m.sort_order, m.name, m.id`, zone, lat, lng, reach, weekday, minute)
	if err != nil {
		return Result{}, err
	}
	lines, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Line, error) {
		var l Line
		err := row.Scan(&l.Item.ID, &l.Item.Name, &l.Item.Description, &l.Item.Price.AmountMinor, &l.Item.Price.Currency,
			&l.Item.Allergens, &l.Item.PhotoURL, &l.QtyLeft)
		if l.Item.Allergens == nil {
			l.Item.Allergens = []string{}
		}
		return l, err
	})
	if err != nil {
		return Result{}, err
	}
	if len(lines) == 0 {
		none.Reason = ReasonSoldOut
		return none, nil
	}
	return Result{Offer: true, Lines: lines}, nil
}

// Service serves getAvailability.
type Service struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
	now  func() time.Time
}

// MountCustomer adds getAvailability.
func (s *Service) MountCustomer(r chi.Router, signedIn ...func(http.Handler) http.Handler) {
	r.Group(func(g chi.Router) {
		g.Use(signedIn...)
		g.Get("/availability", s.get)
	})
}

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

var (
	errNotFound = errors.New("availability: no such gate")
	errBlocked  = errors.New("availability: blocked")
)

func (s *Service) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, identity.ErrSessionInvalid):
		httpserver.WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in again.", false)
	case errors.Is(err, errBlocked):
		identity.WriteBlocked(w)
	case errors.Is(err, errNotFound):
		httpserver.WriteError(w, http.StatusNotFound, "not_found", "No such gate.", false)
	default:
		s.Log.Error("availability: request failed", "request_id", httpserver.RequestID(r.Context()), "error", err.Error())
		httpserver.WriteError(w, http.StatusInternalServerError, "internal_error", "Try again shortly.", true)
	}
}

// get answers for a saved gate, or for a pin with no gate saved, so a customer can look before
// giving a phone (decision 0011).
func (s *Service) get(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	query := r.URL.Query()
	gateID := query.Get("gate_id")
	var lat, lng float64
	switch {
	case gateID != "" && (query.Has("lat") || query.Has("lng")):
		httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send gate_id, or lat and lng, not both.", false)
		return
	case gateID != "":
		if !idPattern.MatchString(gateID) {
			s.fail(w, r, errNotFound)
			return
		}
	default:
		var err1, err2 error
		lat, err1 = strconv.ParseFloat(query.Get("lat"), 64)
		lng, err2 = strconv.ParseFloat(query.Get("lng"), 64)
		if err1 != nil || err2 != nil || math.IsNaN(lat) || math.IsNaN(lng) || math.Abs(lat) > 90 || math.Abs(lng) > 180 {
			httpserver.WriteError(w, http.StatusBadRequest, "invalid_request", "Send gate_id, or lat and lng.", false)
			return
		}
	}
	live, err := identity.SessionLive(r.Context(), s.Pool, actor)
	if err == nil && !live {
		err = identity.ErrSessionInvalid
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var active bool
	if err := s.Pool.QueryRow(r.Context(), `SELECT p.status = 'active' FROM customers c JOIN people p ON p.id = c.person_id WHERE c.id = $1`, actor.ID).Scan(&active); err != nil || !active {
		if errors.Is(err, pgx.ErrNoRows) {
			err = identity.ErrSessionInvalid
		} else if err == nil {
			err = errBlocked
		}
		s.fail(w, r, err)
		return
	}
	if gateID != "" {
		var verified bool
		err := s.Pool.QueryRow(r.Context(), "SELECT lat, lng, verified FROM gates WHERE id = $1 AND customer_id = $2", gateID, actor.ID).Scan(&lat, &lng, &verified)
		if errors.Is(err, pgx.ErrNoRows) {
			err = errNotFound
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if !verified {
			reply(w, Result{Reason: "gate_not_verified", Lines: []Line{}}, s.clock())
			return
		}
	}
	res, err := At(r.Context(), s.Pool, lat, lng, s.clock())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	reply(w, res, s.clock())
}

func reply(w http.ResponseWriter, res Result, now time.Time) {
	var reason any
	if !res.Offer {
		reason = res.Reason
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{
		"offer_available": res.Offer, "unavailable_reason": reason, "items": res.Lines,
		"stale_at": now.Add(staleAfter).UTC().Format(time.RFC3339Nano), "server_time": now.UTC().Format(time.RFC3339Nano),
	})
}
