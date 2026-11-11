package mockserver

import (
	"encoding/json"
	"net/http"
	"time"
)

// riderScenarios lists every rider scenario, from the rider KT doc and decision 0007.
var riderScenarios = map[string]string{
	"account_blocked":         "Sign-in and GET rider fail with account_blocked",
	"rider_not_approved":      "An applicant: shift start fails with rider_not_approved",
	"offer_arrives":           "An open offer. This is the same as no scenario",
	"on_delivery":             "The rider has an order, packed and on the way",
	"offer_expired":           "The offer has expired, on the stream too. Accept and decline fail with offer_expired",
	"offer_taken":             "Another rider took the offer, on the stream too. Accept fails with offer_taken",
	"stale_fencing_token":     "Accept, decline and Ready fail with stale_fencing_token",
	"rider_moving":            "Ready fails with rider_moving, because the rider is moving",
	"order_cancelled":         "The order was cancelled while packing, on the stream too. Ready fails with order_cancelled",
	"route_unavailable":       "GET route returns 503, so the app keeps the last route",
	"wrong_otp":               "Handover fails with wrong_otp",
	"rider_not_near":          "Handover fails with rider_not_near",
	"partial_quantity":        "Handover succeeds. Send a quantity below the ordered one",
	"network_down":            "Handover returns a retryable 503. The queued copy can be replayed",
	"restock_suggestion":      "GET restock suggestions has a drafted request",
	"suggestion_expired":      "Restock confirm fails with suggestion_expired",
	"hub_short":               "The restock has a short fill with a substitute, on the stream too",
	"hub_out_of_stock":        "Restock confirm and create fail with hub_out_of_stock",
	"verification_pending":    "GET verification is pending. Shift start fails with rider_not_verified",
	"verification_failed":     "GET verification failed. Shift start fails with rider_not_verified",
	"verification_expired":    "GET verification expired. Shift start fails with rider_not_verified",
	"shift_limit_reached":     "Shift start fails with shift_limit_reached",
	"shift_already_open":      "Shift start fails with shift_already_open",
	"rider_suspended":         "Shift start returns 403",
	"lot_expired":             "Stock scan fails with lot_expired",
	"bad_scan":                "Stock scan fails with bad_scan",
	"temperature_alert":       "Stock scan returns temp_status alert",
	"relocation_suggested":    "GET nodes has a relocation suggestion",
	"node_full":               "GET nodes shows a full node",
	"no_nodes":                "GET nodes returns an empty list",
	"reassigned":              "This rider no longer has the order, reason reassigned, on the stream too",
	"otp_not_available":       "The order is at ARRIVED with otp_available false, on the stream too",
	"event_rejected":          "POST events rejects every event with wrong_otp",
	"socket_drop":             "The stream drops after two events",
	"token_expiring":          "The stream closes with 4401 after two events, unless the app sends a new auth frame",
	"idempotency_in_progress": "Any write with an Idempotency-Key returns a retryable 409, with Retry-After",
}

// riderStory is one scenario's events, with what GET returns.
type riderStory struct {
	events  []streamEvent
	getAt   int // The event GET state reflects. -1 means the last.
	offers  []map[string]any
	order   map[string]any
	restock map[string]any
	seq     int
}

func riderOrder(state string, seq int, now time.Time) map[string]any {
	o := map[string]any{
		"id": orderID, "offer_id": "off_mock01", "fencing_token": "ft_mock01", "state": state, "seq": seq, "ended_reason": nil,
		"items": []map[string]any{{"item_id": "item_burger", "name": "Burger", "qty": 1}},
		"gate":  riderGate(), "timer_label": "1.5 mins", "rider_accepted_at": stamp(now.Add(-time.Minute)),
		"otp_available": state == "ARRIVED", "handed_over_at": nil, "order_number": "A2931",
	}
	if state == "HANDED_OVER" {
		o["handed_over_at"] = stamp(now)
	}
	return o
}

// riderStoryFor builds a scenario's events in order. GET state and the stream both come from these.
func riderStoryFor(scenario string, now time.Time) riderStory {
	var story riderStory
	offer := func(state string) {
		story.seq++
		o := sampleOffer(state, now)
		story.offers = nil
		if state == "open" {
			story.offers = []map[string]any{o}
		}
		story.events = append(story.events, streamEvent{kind: "offer", seq: story.seq, data: o})
	}
	order := func(state string, change func(map[string]any)) {
		story.seq++
		o := riderOrder(state, story.seq, now)
		if change != nil {
			change(o)
		}
		story.offers, story.order = nil, o
		story.events = append(story.events, streamEvent{kind: "order_state", seq: story.seq, data: o})
	}
	restock := func(state string, short bool) {
		story.seq++
		r := sampleRestock(state, now, short)
		story.restock = r
		story.events = append(story.events, streamEvent{kind: "restock", seq: story.seq, data: r})
	}
	story.getAt = -1
	switch scenario {
	case "offer_expired":
		offer("open")
		offer("expired")
	case "offer_taken":
		offer("open")
		offer("taken")
	case "order_cancelled":
		offer("open")
		order("RIDER_ACCEPTED", nil)
		order("CANCELLED", func(o map[string]any) { o["ended_reason"] = "cancelled" })
	case "reassigned":
		offer("open")
		order("RIDER_ACCEPTED", nil)
		order("RIDER_ACCEPTED", func(o map[string]any) { o["ended_reason"] = "reassigned" })
	case "on_delivery":
		offer("open")
		order("RIDER_ACCEPTED", nil)
		order("PACKED", nil)
	case "otp_not_available":
		offer("open")
		order("RIDER_ACCEPTED", nil)
		order("PACKED", nil)
		order("ARRIVED", func(o map[string]any) { o["otp_available"] = false })
	case "hub_short":
		restock("confirmed", false)
		restock("partial", true)
	default:
		offer("open")
		story.getAt = 0
		first := story.offers
		order("RIDER_ACCEPTED", nil)
		order("PACKED", nil)
		order("TIMER_RUNNING", nil)
		order("ARRIVED", nil)
		order("HANDED_OVER", nil)
		restock("ready", false)
		story.offers, story.order, story.restock = first, nil, nil
		story.seq = 1
	}
	return story
}

func riderGate() map[string]any {
	return map[string]any{"id": "gate_mock01", "label": "Main gate", "lat": 12.9716, "lng": 77.5946, "entry_notes": "Near the security desk"}
}

func sampleOffer(state string, now time.Time) map[string]any {
	var orderRef any
	if state == "accepted" {
		orderRef = orderID
	}
	return map[string]any{
		"id": "off_mock01", "order_id": orderRef, "items": []map[string]any{{"item_id": "item_burger", "name": "Burger", "qty": 1}},
		"gate": riderGate(), "expires_at": stamp(now.Add(20 * time.Second)), "state": state, "fencing_token": "ft_mock01", "order_number": "A2931",
	}
}

func sampleShift(now time.Time, ended bool) map[string]any {
	s := map[string]any{"id": "shf_mock01", "node_id": "node_mock01", "started_at": stamp(now.Add(-time.Hour)), "ended_at": nil,
		"node_assigned_at": stamp(now.Add(-time.Hour)), "scooter": map[string]any{"id": "scr_mock01", "plate": "NZ-014", "battery_pct": nil}}
	if ended {
		s["ended_at"] = stamp(now)
	}
	return s
}

func sampleRestock(state string, now time.Time, short bool) map[string]any {
	r := map[string]any{
		"id": "rst_mock01", "state": state, "hub": mockHub(),
		"items":     []map[string]any{{"item_id": "item_burger", "name": "Burger", "qty": 4, "filled_qty": nil}},
		"shortfall": nil, "slot_at": stamp(now.Add(10 * time.Minute)), "arrive_by": stamp(now.Add(9 * time.Minute)), "lot_numbers": []string{}, "ready_in_s": 600,
	}
	if short {
		r["state"] = "partial"
		r["items"] = []map[string]any{{"item_id": "item_burger", "name": "Burger", "qty": 4, "filled_qty": 2}}
		r["shortfall"] = map[string]any{"action": "substitute", "substitute": map[string]any{"item_id": "item_chai", "name": "Masala chai"}, "other_hub": nil}
	}
	return r
}

func riderRoutes() []route {
	offerError := func(c *call, codes ...string) bool {
		for _, code := range codes {
			if c.scenario == code {
				c.fail(409, code, "Sample rejection: "+code, false)
				return true
			}
		}
		return false
	}
	return []route{
		{method: "POST", path: "/auth/otp/request", open: true, noIdempotent: true, handle: otpRequest},
		{method: "POST", path: "/auth/otp/verify", open: true, noIdempotent: true, handle: func(c *call) { otpVerify(c, riderSessionFields(c)) }},
		{method: "POST", path: "/auth/refresh", open: true, noIdempotent: true, handle: refresh},
		{method: "POST", path: "/auth/signout", noIdempotent: true, handle: func(c *call) { c.json(200, map[string]any{}) }},
		{method: "GET", path: "/riders/me", handle: func(c *call) {
			if c.scenario == "account_blocked" {
				accountBlocked(c)
				return
			}
			c.json(200, map[string]any{"rider": map[string]any{"id": "rdr_mock0001", "name": "Ravi Kumar", "status": "active", "zone_id": "zone_mock01",
				"created_at": stamp(c.now.Add(-30 * 24 * time.Hour)), "approved_at": stamp(c.now.Add(-29 * 24 * time.Hour))}})
		}},
		{method: "GET", path: "/riders/me/state", handle: func(c *call) {
			story := riderStoryFor(c.scenario, c.now)
			offers := story.offers
			if offers == nil {
				offers = []map[string]any{}
			}
			var current, request any
			if story.order != nil && story.order["ended_reason"] == nil && story.order["state"] != "HANDED_OVER" {
				current = story.order
			}
			if story.restock != nil {
				request = story.restock
			}
			c.json(200, map[string]any{"seq": story.seq, "shift": sampleShift(c.now, false), "offers": offers, "current_order": current, "restock_request": request})
		}},
		{method: "GET", path: "/riders/me/verification", handle: func(c *call) {
			status, reason := map[string]string{"verification_pending": "pending", "verification_failed": "failed", "verification_expired": "expired"}[c.scenario], any(nil)
			if status == "" {
				status = "verified"
			}
			if status == "failed" {
				reason = "The document name did not match."
			}
			c.json(200, map[string]any{"status": status, "reason": reason})
		}},
		{method: "POST", path: "/riders/me/verification/start", handle: func(c *call) {
			if _, ok := needsBody(c, "method"); ok {
				c.json(200, map[string]any{"redirect_url": "https://example.invalid/mock-verification", "upload_url": nil})
			}
		}},
		{method: "POST", path: "/riders/me/devices", handle: registerDevice},
		{method: "DELETE", path: "/riders/me/devices/{device_id}", handle: func(c *call) { c.json(200, map[string]any{}) }},
		{method: "POST", path: "/riders/me/shifts/start", handle: func(c *call) {
			switch c.scenario {
			case "rider_suspended":
				c.json(403, map[string]any{"code": "forbidden", "message": "This rider is suspended.", "retryable": false, "missing_permission": "shift.start"})
				return
			case "verification_pending", "verification_failed", "verification_expired":
				c.fail(409, "rider_not_verified", "Verification is not complete.", false)
				return
			}
			if offerError(c, "rider_not_approved", "shift_limit_reached", "shift_already_open") {
				return
			}
			if _, ok := needsBody(c, "node_id"); ok {
				c.json(200, map[string]any{"shift": sampleShift(c.now, false)})
			}
		}},
		{method: "POST", path: "/riders/me/shifts/end", handle: func(c *call) {
			c.json(200, map[string]any{"shift": sampleShift(c.now, true)})
		}},
		{method: "GET", path: "/nodes", handle: func(c *call) {
			if c.r.URL.Query().Get("zone_id") == "" {
				c.fail(400, "invalid_request", "Missing zone_id.", false)
				return
			}
			fullness := 40
			if c.scenario == "node_full" {
				fullness = 100
			}
			nodes := []map[string]any{{"id": "node_mock01", "name": "Node 7", "fullness_pct": fullness, "lat": 12.97, "lng": 77.59, "hub_distance_m": 850}}
			var assigned, relocation any = "node_mock01", nil
			if c.scenario == "no_nodes" {
				nodes, assigned = []map[string]any{}, nil
			}
			if c.scenario == "relocation_suggested" {
				relocation = map[string]any{"id": "rel_mock01", "node_id": "node_mock02", "reason": "3 orders expected, nobody nearby", "orders_waiting": 3, "expires_at": stamp(c.now.Add(5 * time.Minute))}
			}
			c.json(200, map[string]any{"nodes": nodes, "assigned_node_id": assigned, "relocation": relocation})
		}},
		{method: "POST", path: "/riders/me/relocations/{relocation_id}/accept", handle: func(c *call) { c.json(200, map[string]any{}) }},
		{method: "POST", path: "/riders/me/stock/scan", handle: func(c *call) {
			if offerError(c, "lot_expired", "bad_scan") {
				return
			}
			if _, ok := needsBody(c, "lot_code", "scanned_at"); !ok {
				return
			}
			temp := "ok"
			if c.scenario == "temperature_alert" {
				temp = "alert"
			}
			c.json(200, map[string]any{"unit": map[string]any{"lot_id": "lot_mock01", "item_id": "item_burger", "expiry": stamp(c.now.Add(4 * time.Hour)), "temp_status": temp}})
		}},
		{method: "GET", path: "/riders/me/stock", handle: func(c *call) {
			c.json(200, map[string]any{"lots": []map[string]any{{"lot_id": "lot_mock01", "item_id": "item_burger", "qty": 6, "expiry": stamp(c.now.Add(4 * time.Hour)), "temp_status": "ok", "image_url": "https://example.invalid/mock/burger.jpg"}}})
		}},
		{method: "GET", path: "/riders/me/offers", handle: func(c *call) {
			offers := riderStoryFor(c.scenario, c.now).offers
			if offers == nil {
				offers = []map[string]any{}
			}
			c.json(200, map[string]any{"offers": offers})
		}},
		{method: "POST", path: "/offers/{offer_id}/accept", handle: func(c *call) {
			if offerError(c, "offer_expired", "offer_taken", "stale_fencing_token") {
				return
			}
			if _, ok := needsBody(c, "fencing_token"); ok {
				c.json(200, map[string]any{"order": riderOrder("RIDER_ACCEPTED", 2, c.now)})
			}
		}},
		{method: "POST", path: "/offers/{offer_id}/decline", handle: func(c *call) {
			if offerError(c, "offer_expired", "stale_fencing_token") {
				return
			}
			if _, ok := needsBody(c, "fencing_token"); ok {
				c.json(200, map[string]any{})
			}
		}},
		{method: "POST", path: "/offers/{offer_id}/ready", handle: func(c *call) {
			if offerError(c, "rider_moving", "order_cancelled", "stale_fencing_token") {
				return
			}
			if _, ok := needsBody(c, "fencing_token"); ok {
				c.json(200, map[string]any{"order": riderOrder("PACKED", 3, c.now)})
			}
		}},
		{method: "GET", path: "/orders/{order_id}", handle: func(c *call) {
			o := riderStoryFor(c.scenario, c.now).order
			if o == nil {
				o = riderOrder("RIDER_ACCEPTED", 2, c.now)
			}
			c.json(200, map[string]any{"order": o})
		}},
		{method: "GET", path: "/orders/{order_id}/route", handle: func(c *call) {
			if c.scenario == "route_unavailable" {
				c.fail(503, "service_unavailable", "No route right now.", true)
				return
			}
			c.json(200, map[string]any{"polyline": "_p~iF~ps|U_ulLnnqC", "eta_source": "measured", "eta_s": 150})
		}},
		{method: "POST", path: "/orders/{order_id}/handover", handle: func(c *call) {
			if c.scenario == "network_down" {
				c.fail(503, "service_unavailable", "A sample outage.", true)
				return
			}
			if offerError(c, "wrong_otp", "rider_not_near") {
				return
			}
			if _, ok := needsBody(c, "otp", "quantities", "client_time"); ok {
				c.json(200, map[string]any{"order": riderOrder("HANDED_OVER", 6, c.now)})
			}
		}},
		{method: "POST", path: "/orders/{order_id}/call", handle: func(c *call) {
			if offerError(c, "call_not_available") {
				return
			}
			c.json(200, map[string]any{"phone": "+911140000000", "expires_at": stamp(c.now.Add(5 * time.Minute))})
		}},
		{method: "POST", path: "/riders/me/location", noIdempotent: true, handle: func(c *call) {
			if _, ok := needsBody(c, "lat", "lng", "device_time"); ok {
				c.json(202, map[string]any{"next_ping_s": 3})
			}
		}},
		{method: "POST", path: "/riders/me/events", noIdempotent: true, handle: eventBatch},
		{method: "GET", path: "/riders/me/restock-suggestions", handle: func(c *call) {
			suggestions := []map[string]any{}
			if c.scenario == "restock_suggestion" {
				suggestions = append(suggestions, map[string]any{"id": "rst_mock01", "hub": mockHub(),
					"items": []map[string]any{{"item_id": "item_burger", "name": "Burger", "qty": 4, "filled_qty": nil}}, "expires_at": stamp(c.now.Add(10 * time.Minute))})
			}
			c.json(200, map[string]any{"suggestions": suggestions})
		}},
		{method: "POST", path: "/restock/{request_id}/confirm", handle: func(c *call) {
			if offerError(c, "suggestion_expired", "hub_out_of_stock") {
				return
			}
			c.json(200, map[string]any{"request": sampleRestock("confirmed", c.now, false)})
		}},
		{method: "POST", path: "/restock/{request_id}/decline", handle: func(c *call) {
			c.json(200, map[string]any{"request": sampleRestock("cancelled", c.now, false)})
		}},
		{method: "POST", path: "/restock/{request_id}/shortfall-response", handle: func(c *call) {
			if _, ok := needsBody(c, "accept"); ok {
				c.json(200, map[string]any{"request": sampleRestock("partial", c.now, true)})
			}
		}},
		{method: "POST", path: "/restock", handle: func(c *call) {
			if offerError(c, "hub_out_of_stock") {
				return
			}
			if _, ok := needsBody(c, "hub_id", "items"); ok {
				c.json(201, map[string]any{"request": sampleRestock("confirmed", c.now, false)})
			}
		}},
		{method: "GET", path: "/restock/options", handle: func(c *call) {
			c.json(200, map[string]any{"hubs": []map[string]any{mockHub()}, "items": []map[string]any{{"item_id": "item_burger", "name": "Burger"}}})
		}},
		{method: "GET", path: "/restock/{request_id}", handle: func(c *call) {
			c.json(200, map[string]any{"request": sampleRestock("packing", c.now, c.scenario == "hub_short")})
		}},
		{method: "POST", path: "/riders/me/problems", handle: func(c *call) {
			if _, ok := needsBody(c, "type"); ok {
				c.json(201, map[string]any{"report_id": "rep_mock01"})
			}
		}},
		{method: "GET", path: "/riders/me/earnings", handle: func(c *call) {
			date := c.r.URL.Query().Get("date")
			if _, err := time.Parse("2006-01-02", date); err != nil {
				c.fail(400, "invalid_request", "date must be YYYY-MM-DD.", false)
				return
			}
			c.json(200, map[string]any{"date": date, "orders": 1, "order_earnings": []map[string]any{{"order_id": orderID, "amount": money(4000)}},
				"incentives": money(1000), "total": money(7000), "repositioning": money(2000), "repositioning_seconds": 600})
		}},
	}
}

func eventBatch(c *call) {
	var body struct {
		Events []struct {
			EventID string `json:"event_id"`
		} `json:"events"`
	}
	if err := decodeJSON(c.r, &body); err != nil || len(body.Events) == 0 {
		c.fail(http.StatusBadRequest, "invalid_request", "Send at least one event.", false)
		return
	}
	accepted, rejected := []string{}, []map[string]any{}
	for _, e := range body.Events {
		if c.scenario == "event_rejected" {
			rejected = append(rejected, map[string]any{"id": e.EventID, "code": "wrong_otp", "message": "That code is wrong.", "retryable": false})
		} else {
			accepted = append(accepted, e.EventID)
		}
	}
	c.json(200, map[string]any{"accepted_ids": accepted, "rejected": rejected})
}

func decodeJSON(r *http.Request, into any) error {
	return json.NewDecoder(r.Body).Decode(into)
}

func riderSessionFields(c *call) map[string]any {
	status := "active"
	if c.scenario == "rider_not_approved" {
		status = "applicant"
	}
	return map[string]any{"rider_id": "rdr_mock0001", "status": status}
}

// mockHub is the sample hub reference, with its place.
func mockHub() map[string]any {
	return map[string]any{"id": "hub_mock01", "name": "Hub 2", "lat": 12.9352, "lng": 77.6245}
}
