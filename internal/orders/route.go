package orders

import (
	"errors"
	"math"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/httpserver"
)

// getRoute is getRoute (#156): until the routing engine (OSRM, decided), the route is a straight
// line from the rider's last position to the gate, and its time is a placeholder, so the app shows
// no time. The order must be one this rider accepted.
func (s *Service) getRoute(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.ActorFrom(r.Context())
	if err := s.riderReadable(r, actor); err != nil {
		s.fail(w, r, err)
		return
	}
	id := chi.URLParam(r, "order_id")
	if !idPattern.MatchString(id) {
		s.fail(w, r, errNotFound)
		return
	}
	var gateLat, gateLng float64
	var fromLat, fromLng *float64
	err := s.Pool.QueryRow(r.Context(), `SELECT h.lat, h.lng,
			coalesce((SELECT p.lat FROM location_pings p WHERE p.rider_id = $2 ORDER BY p.received_at DESC, p.id DESC LIMIT 1), n.lat),
			coalesce((SELECT p.lng FROM location_pings p WHERE p.rider_id = $2 ORDER BY p.received_at DESC, p.id DESC LIMIT 1), n.lng)
		FROM orders o JOIN gate_history h ON h.id = o.gate_history_id
		JOIN offers f ON f.order_id = o.id AND f.rider_id = $2 AND f.state = 'accepted'
		LEFT JOIN scooters sc ON sc.id = f.scooter_id LEFT JOIN staging_nodes n ON n.id = sc.node_id
		WHERE o.id = $1 LIMIT 1`, id, actor.ID).Scan(&gateLat, &gateLng, &fromLat, &fromLng)
	if errors.Is(err, pgx.ErrNoRows) {
		err = errNotFound
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	points := [][2]float64{{gateLat, gateLng}}
	if fromLat != nil && fromLng != nil {
		points = [][2]float64{{*fromLat, *fromLng}, {gateLat, gateLng}}
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"polyline": encodePolyline(points), "eta_source": "placeholder", "eta_s": nil, "server_time": serverTime()})
}

// encodePolyline is Google's encoded polyline format, five decimal places, which map libraries read.
func encodePolyline(points [][2]float64) string {
	var b strings.Builder
	var prevLat, prevLng int64
	put := func(v int64) {
		v <<= 1
		if v < 0 {
			v = ^v
		}
		for v >= 0x20 {
			b.WriteByte(byte((0x20 | (v & 0x1f)) + 63))
			v >>= 5
		}
		b.WriteByte(byte(v + 63))
	}
	for _, p := range points {
		lat, lng := int64(math.Round(p[0]*1e5)), int64(math.Round(p[1]*1e5))
		put(lat - prevLat)
		put(lng - prevLng)
		prevLat, prevLng = lat, lng
	}
	return b.String()
}
