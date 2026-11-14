package mockserver

import (
	"net/http"
	"time"
)

// The console's read of quarantined stock (issue 235, part B3). The mock keeps nothing: it answers a
// fixed pair of lots, one a rider quarantined on a scooter and one a hub lead quarantined at a hub.

func mockQuarantinedLots(now time.Time) []map[string]any {
	return []map[string]any{
		{"lot_id": "lot_mock0001", "lot_code": "LOT-B-0101", "item": map[string]any{"id": "item_chai", "name": "Masala chai"}, "qty": 8,
			"source": "rider", "reason": "temperature", "quarantined_at": stamp(now.Add(-25 * time.Minute)),
			"scooter": map[string]any{"id": "scr_mock01", "code": "KA01AB1234"}, "rider": map[string]any{"id": "rdr_mock0001", "name": "Ravi"}, "hub": nil,
			"order_ids": []string{"ord_mock0001"}},
		{"lot_id": "lot_mock0002", "lot_code": "LOT-C-0201", "item": map[string]any{"id": "item_burger", "name": "Veg burger"}, "qty": 12,
			"source": "hub", "reason": "damaged", "quarantined_at": stamp(now.Add(-26 * time.Hour)),
			"scooter": nil, "rider": nil, "hub": map[string]any{"id": "hub_mock01", "name": "Indiranagar hub"},
			"order_ids": []string{}},
	}
}

// quarantineRoutes is listQuarantinedStock.
func quarantineRoutes(denied func(*call) bool) []route {
	return []route{
		{method: "GET", path: "/admin/quarantined-stock", handle: func(c *call) {
			if denied(c) {
				return
			}
			q := c.r.URL.Query()
			for _, k := range []string{"from", "to"} {
				if v := q.Get(k); v != "" {
					if _, err := time.Parse(time.RFC3339Nano, v); err != nil {
						c.fail(http.StatusBadRequest, "invalid_request", "Send from and to as times.", false)
						return
					}
				}
			}
			lots := mockQuarantinedLots(c.now)
			if c.scenario == "quarantines_empty" {
				lots = []map[string]any{}
			}
			c.json(200, map[string]any{"lots": lots, "next_cursor": nil})
		}},
	}
}
