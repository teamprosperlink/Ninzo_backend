package mockserver

import (
	"net/http"
	"strconv"
	"time"
)

// The console's read of customer ratings (Issue 265, part A). The mock keeps nothing: it answers a fixed
// set of rated orders and applies the filters to it, so the app developers can see each filter work.

type mockRating struct {
	orderID, number, zone, riderID, riderName string
	stars                                     int
	reason                                    any
	ago                                       time.Duration
}

var mockRatings = []mockRating{
	{orderID: "ord_mock0001", number: "A1001", zone: "zone_mock01", riderID: "rdr_mock0001", riderName: "Ravi", stars: 4, reason: "late", ago: 10 * time.Minute},
	{orderID: "ord_mock0007", number: "A0987", zone: "zone_mock01", riderID: "rdr_mock0003", riderName: "Meena", stars: 5, reason: nil, ago: 3 * time.Hour},
	{orderID: "ord_mock0008", number: "A0975", zone: "zone_mock02", riderID: "rdr_mock0001", riderName: "Ravi", stars: 2, reason: "cold_food", ago: 26 * time.Hour},
}

func (m mockRating) row(now time.Time) map[string]any {
	return map[string]any{"order_id": m.orderID, "order_number": m.number, "zone_id": m.zone,
		"rider": map[string]any{"id": m.riderID, "name": m.riderName}, "stars": m.stars, "reason": m.reason, "rated_at": stamp(now.Add(-m.ago))}
}

// ratingRoutes is listRatings.
func ratingRoutes(denied func(*call) bool) []route {
	return []route{
		{method: "GET", path: "/admin/ratings", handle: func(c *call) {
			if denied(c) {
				return
			}
			q := c.r.URL.Query()
			bad := func(message string) { c.fail(http.StatusBadRequest, "invalid_request", message, false) }
			var from, to time.Time
			for key, into := range map[string]*time.Time{"from": &from, "to": &to} {
				if v := q.Get(key); v != "" {
					t, err := time.Parse(time.RFC3339Nano, v)
					if err != nil {
						bad("Send from and to as times.")
						return
					}
					*into = t
				}
			}
			stars := 0
			if v := q.Get("stars"); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil || n < 1 || n > 5 {
					bad("stars must be a whole number from 1 to 5.")
					return
				}
				stars = n
			}
			if v := q.Get("limit"); v != "" {
				if n, err := strconv.Atoi(v); err != nil || n < 1 || n > 100 {
					bad("limit must be from 1 to 100.")
					return
				}
			}
			rows := []map[string]any{}
			if c.scenario != "ratings_empty" {
				for _, m := range mockRatings {
					at := c.now.Add(-m.ago)
					switch {
					case stars != 0 && m.stars != stars,
						q.Get("zone_id") != "" && m.zone != q.Get("zone_id"),
						q.Get("rider_id") != "" && m.riderID != q.Get("rider_id"),
						!from.IsZero() && at.Before(from),
						!to.IsZero() && !at.Before(to):
						continue
					}
					rows = append(rows, m.row(c.now))
				}
			}
			c.json(200, map[string]any{"ratings": rows, "next_cursor": nil})
		}},
	}
}
