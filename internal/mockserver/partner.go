package mockserver

import (
	"fmt"
	"net/http"
	"time"
)

// partnerScenarios lists every hub scenario, from the partner KT doc.
var partnerScenarios = map[string]string{
	"new_request":                "A new request arrives on the live channel and joins the queue",
	"arrival_changed":            "The rider's arrival time changes, on the live channel too",
	"request_taken":              "Another staff member accepted the first request, on the live channel too. Accept fails with request_taken",
	"request_withdrawn":          "The rider withdrew the first request, on the live channel too. Accept fails with request_withdrawn",
	"empty_queue":                "No requests at the hub",
	"hub_closed":                 "The hub is paused: hub_open is false and accept fails with hub_closed",
	"wrong_lot":                  "Scan fails with wrong_lot; the request stays open",
	"lot_expired":                "Scan fails with lot_expired",
	"lot_quarantined":            "Scan fails with lot_quarantined",
	"lot_already_packed":         "Scan fails with lot_already_packed",
	"temp_alert":                 "Scan fails with temp_alert and the lot, so the app opens the quarantine prompt",
	"duplicate_scan":             "Scan succeeds with duplicate true and no second count",
	"partial_fill":               "The pick list has a line with no lots; ready fails with shortfall_needed until a shortfall is recorded",
	"substitute_accepted":        "The rider accepted the substitute: the request is back at packing with a line for it, on the live channel too",
	"substitute_offered":         "The request has a substitute shortfall waiting for the rider",
	"substitute_refused":         "The rider refused the substitute, on the live channel too",
	"shortfall_already_recorded": "Shortfall fails with shortfall_already_recorded",
	"lots_mismatch":              "Ready fails with lots_mismatch",
	"already_ready":              "The request is already ready; ready again changes nothing",
	"not_ready":                  "The label fails with not_ready",
	"not_hub_lead":               "A hub_staff member: quarantine fails with 403",
	"lot_on_scooter":             "Quarantine fails with lot_on_scooter",
	"network_down":               "Scan returns a retryable 503. The queued copy can be sent later",
	"event_rejected":             "POST events rejects every event with wrong_lot",
	"account_blocked":            "Sign-in and GET staff fail with account_blocked",
	"socket_drop":                "The live channel drops after two events",
	"token_expiring":             "The live channel closes with 4401 after two events, unless the app sends a new auth frame",
	"idempotency_in_progress":    "Any write with an Idempotency-Key returns a retryable 409, with Retry-After",
}

const (
	hubID      = "hub_mock01"
	newID      = "rst_mock01" // New to the hub.
	packingID  = "rst_mock02" // Being packed.
	readyID    = "rst_mock03" // Sealed and waiting for the rider.
	arrivingID = "rst_mock04" // Arrives in the new_request scenario.
)

func hubLine(item, name string, qty, picked int) map[string]any {
	return map[string]any{"item_id": item, "name": name, "qty": qty, "picked_qty": picked}
}

func hubRequest(id, state string, seq int, now time.Time) map[string]any {
	r := map[string]any{
		"id": id, "state": state, "seq": seq, "scooter_code": "SC-0" + id[len(id)-1:],
		"items":       []map[string]any{hubLine("item_burger", "Burger", 4, 0), hubLine("item_chai", "Masala chai", 6, 0)},
		"lot_numbers": []string{}, "slot_at": nil, "rider_arrival_at": nil, "accepted_by": nil, "shortfall": nil,
		"created_at": stamp(now.Add(-10 * time.Minute)),
	}
	if state != "confirmed" && state != "cancelled" {
		r["slot_at"], r["rider_arrival_at"], r["accepted_by"] = stamp(now.Add(2*time.Minute)), stamp(now.Add(12*time.Minute)), "Asha"
	}
	switch state {
	case "packing":
		r["items"] = []map[string]any{hubLine("item_burger", "Burger", 4, 5), hubLine("item_chai", "Masala chai", 6, 0)}
		r["lot_numbers"] = []string{"LOT-B-0101"}
	case "ready", "partial":
		r["items"] = []map[string]any{hubLine("item_burger", "Burger", 4, 5), hubLine("item_chai", "Masala chai", 6, 10)}
		r["lot_numbers"] = []string{"LOT-B-0101", "LOT-C-0201"}
	}
	return r
}

func sampleShortfall(action, status string) map[string]any {
	s := map[string]any{"action": action, "lines": []map[string]any{{"item_id": "item_chai", "qty_missing": 6}}, "substitute_item_id": nil, "other_hub": nil, "status": status}
	switch action {
	case "substitute":
		s["substitute_item_id"] = "item_lemon_tea"
	case "other_hub":
		s["other_hub"] = map[string]any{"id": "hub_mock02", "name": "Indiranagar hub"}
	}
	return s
}

// hubStory is a scenario's queue and the live events that led to it.
type hubStory struct {
	requests []map[string]any
	events   []streamEvent
	seq      int
}

func hubStoryFor(scenario string, now time.Time) hubStory {
	var h hubStory
	add := func(kind string, r map[string]any) {
		h.seq++
		r["seq"] = h.seq
		h.events = append(h.events, streamEvent{kind: kind, seq: h.seq, data: r})
	}
	if scenario == "empty_queue" {
		return h
	}
	first := hubRequest(newID, "confirmed", 1, now)
	add("request_new", first)
	add("request_new", hubRequest(packingID, "packing", 1, now))
	add("request_new", hubRequest(readyID, "ready", 1, now))
	switch scenario {
	case "new_request":
		add("request_new", hubRequest(arrivingID, "confirmed", 1, now))
	case "arrival_changed":
		changed := hubRequest(packingID, "packing", 1, now)
		changed["rider_arrival_at"] = stamp(now.Add(25 * time.Minute))
		add("request_changed", changed)
	case "request_taken":
		taken := hubRequest(newID, "hub_accepted", 1, now)
		taken["accepted_by"] = "Imran"
		add("request_changed", taken)
	case "request_withdrawn":
		add("request_changed", hubRequest(newID, "cancelled", 1, now))
	case "substitute_offered", "substitute_refused", "substitute_accepted":
		short := hubRequest(packingID, "partial", 1, now)
		short["items"] = []map[string]any{hubLine("item_burger", "Burger", 4, 5), hubLine("item_chai", "Masala chai", 6, 0)}
		short["lot_numbers"] = []string{"LOT-B-0101"}
		short["shortfall"] = sampleShortfall("substitute", "open")
		add("request_changed", short)
		if scenario == "substitute_accepted" {
			back := hubRequest(packingID, "packing", 1, now)
			back["items"] = []map[string]any{hubLine("item_burger", "Burger", 4, 5), hubLine("item_chai", "Masala chai", 6, 0), hubLine("item_lemon_tea", "Lemon tea", 6, 0)}
			back["lot_numbers"] = short["lot_numbers"]
			back["shortfall"] = sampleShortfall("substitute", "accepted")
			add("request_changed", back)
		}
		if scenario == "substitute_refused" {
			refused := hubRequest(packingID, "partial", 1, now)
			refused["items"], refused["lot_numbers"] = short["items"], short["lot_numbers"]
			refused["shortfall"] = sampleShortfall("substitute", "refused")
			add("request_changed", refused)
		}
	case "already_ready":
		add("request_changed", hubRequest(newID, "ready", 1, now))
	}
	// The queue is each request's latest version, in first-seen order. Ended ones leave it.
	latest := map[string]map[string]any{}
	order := []string{}
	for _, e := range h.events {
		r := e.data.(map[string]any)
		id := r["id"].(string)
		if _, seen := latest[id]; !seen {
			order = append(order, id)
		}
		latest[id] = r
	}
	for _, id := range order {
		if r := latest[id]; r["state"] != "cancelled" {
			h.requests = append(h.requests, r)
		}
	}
	return h
}

// hubRequestFor is GET one request: its latest version in the scenario. Any other id is answered
// as the first request, so every sample id works; an empty queue has none.
func hubRequestFor(c *call, id string) (map[string]any, bool) {
	var found, first map[string]any
	for _, e := range hubStoryFor(c.scenario, c.now).events {
		r := e.data.(map[string]any)
		if r["id"] == id {
			found = r
		}
		if r["id"] == newID {
			first = r
		}
	}
	if found == nil && first != nil {
		found = map[string]any{}
		for k, v := range first {
			found[k] = v
		}
		found["id"] = id
	}
	return found, found != nil
}

func pickList(id string, scenario string, now time.Time) map[string]any {
	lot := func(code string, qty int, inHours int) map[string]any {
		return map[string]any{"lot_id": "lot_" + code, "lot_code": code, "qty": qty, "expires_at": stamp(now.Add(time.Duration(inHours) * time.Hour)), "temp_status": "ok"}
	}
	chai := []map[string]any{lot("LOT-C-0201", 10, 20), lot("LOT-C-0202", 10, 44)}
	if scenario == "partial_fill" {
		chai = []map[string]any{}
	}
	lines := []map[string]any{
		{"item_id": "item_burger", "name": "Burger", "qty": 4, "picked_qty": 0, "route_order": 1, "lots": []map[string]any{lot("LOT-B-0101", 5, 6), lot("LOT-B-0102", 5, 30)}},
		{"item_id": "item_chai", "name": "Masala chai", "qty": 6, "picked_qty": 0, "route_order": 2, "lots": chai},
	}
	return map[string]any{"request_id": id, "lines": lines}
}

func hubLot(code, item string, status string, now time.Time) map[string]any {
	return map[string]any{"lot_id": "lot_" + code, "lot_code": code, "item_id": item, "qty": 5, "expires_at": stamp(now.Add(6 * time.Hour)),
		"temp_status": "ok", "status": status, "in_cassette": false}
}

func partnerRoutes() []route {
	fail := func(c *call, codes ...string) bool {
		for _, code := range codes {
			if c.scenario == code {
				c.fail(409, code, "Sample rejection: "+code, false)
				return true
			}
		}
		return false
	}
	request := func(c *call) (map[string]any, bool) {
		r, ok := hubRequestFor(c, c.param("request_id"))
		if !ok {
			c.fail(404, "not_found", "No such request at this hub.", false)
		}
		return r, ok
	}
	return []route{
		{method: "POST", path: "/auth/otp/request", open: true, noIdempotent: true, handle: otpRequest},
		{method: "POST", path: "/auth/otp/verify", open: true, noIdempotent: true, handle: func(c *call) {
			role := "hub_lead"
			if c.scenario == "not_hub_lead" {
				role = "hub_staff"
			}
			otpVerify(c, map[string]any{"staff_id": "stf_mock01", "role": role, "hub_id": hubID})
		}},
		{method: "POST", path: "/auth/refresh", open: true, noIdempotent: true, handle: refresh},
		{method: "POST", path: "/auth/signout", noIdempotent: true, handle: func(c *call) { c.json(200, map[string]any{}) }},
		{method: "GET", path: "/hub/me", handle: func(c *call) {
			if c.scenario == "account_blocked" {
				accountBlocked(c)
				return
			}
			role := "hub_lead"
			if c.scenario == "not_hub_lead" {
				role = "hub_staff"
			}
			c.json(200, map[string]any{"staff": map[string]any{"id": "stf_mock01", "name": "Asha", "role": role,
				"hub": map[string]any{"id": hubID, "name": "Koramangala hub"}, "hub_open": c.scenario != "hub_closed"}})
		}},
		{method: "GET", path: "/hub/restock", handle: func(c *call) {
			state := c.r.URL.Query().Get("state")
			if _, ok := map[string]bool{"": true, "confirmed": true, "hub_accepted": true, "packing": true, "ready": true, "partial": true, "picked_up": true,
				"received": true, "closed": true, "expired": true, "cancelled": true}[state]; !ok {
				c.fail(400, "invalid_request", "Unknown state.", false)
				return
			}
			out := []map[string]any{}
			for _, r := range hubStoryFor(c.scenario, c.now).requests {
				if state == "" || r["state"] == state {
					out = append(out, r)
				}
			}
			c.json(200, map[string]any{"requests": out, "seq": hubStoryFor(c.scenario, c.now).seq})
		}},
		{method: "GET", path: "/hub/restock/{request_id}", handle: func(c *call) {
			if r, ok := request(c); ok {
				c.json(200, map[string]any{"request": r})
			}
		}},
		{method: "POST", path: "/hub/restock/{request_id}/accept", handle: func(c *call) {
			if c.scenario == "hub_closed" {
				c.fail(409, "hub_closed", "The hub is paused.", false)
				return
			}
			if fail(c, "request_taken", "request_withdrawn") {
				return
			}
			if r, ok := request(c); ok {
				if r["state"] == "confirmed" {
					r = hubRequest(r["id"].(string), "hub_accepted", r["seq"].(int)+1, c.now)
				}
				c.json(200, map[string]any{"request": r})
			}
		}},
		{method: "GET", path: "/hub/restock/{request_id}/pick-list", handle: func(c *call) {
			if _, ok := request(c); ok {
				c.json(200, pickList(c.param("request_id"), c.scenario, c.now))
			}
		}},
		{method: "POST", path: "/hub/restock/{request_id}/scan", handle: func(c *call) {
			if c.scenario == "network_down" {
				c.fail(503, "service_unavailable", "A sample outage. Keep the scan queued.", true)
				return
			}
			sampleLot := map[string]any{"lot_id": "lot_LOT-C-0203", "lot_code": "LOT-C-0203", "qty": 10, "expires_at": stamp(c.now.Add(20 * time.Hour)), "temp_status": "ok"}
			switch c.scenario {
			case "temp_alert":
				sampleLot["temp_status"] = "alert"
				c.json(409, map[string]any{"code": "temp_alert", "message": "This lot failed its temperature check. Quarantine it.", "retryable": false, "lot": sampleLot})
				return
			case "wrong_lot":
				c.json(409, map[string]any{"code": "wrong_lot", "message": "That lot is not on this request.", "retryable": false, "lot": nil})
				return
			case "lot_expired", "lot_quarantined", "lot_already_packed":
				c.json(409, map[string]any{"code": c.scenario, "message": "Sample rejection: " + c.scenario, "retryable": false, "lot": sampleLot})
				return
			}
			body, ok := needsBody(c, "lot_code", "scanned_at")
			if !ok {
				return
			}
			if _, err := time.Parse(time.RFC3339, body["scanned_at"].(string)); err != nil {
				c.fail(400, "invalid_request", "scanned_at must be a date-time.", false)
				return
			}
			if r, ok := request(c); ok {
				code, _ := body["lot_code"].(string)
				packed := r
				if c.scenario != "duplicate_scan" {
					// A new lot in the cassette: a change, so the seq rises.
					packed = hubRequest(r["id"].(string), "packing", r["seq"].(int)+1, c.now)
					packed["lot_numbers"] = append(append([]string{}, r["lot_numbers"].([]string)...), code)
				}
				lot := map[string]any{"lot_id": "lot_" + code, "lot_code": code, "qty": 5, "expires_at": stamp(c.now.Add(6 * time.Hour)), "temp_status": "ok"}
				c.json(200, map[string]any{"lot": lot, "duplicate": c.scenario == "duplicate_scan", "request": packed})
			}
		}},
		{method: "POST", path: "/hub/restock/{request_id}/unscan", handle: func(c *call) {
			if _, ok := needsBody(c, "lot_code"); !ok {
				return
			}
			if r, ok := request(c); ok {
				c.json(200, map[string]any{"request": r})
			}
		}},
		{method: "POST", path: "/hub/restock/{request_id}/shortfall", handle: func(c *call) {
			if fail(c, "shortfall_already_recorded") {
				return
			}
			body, ok := needsBody(c, "lines", "action")
			if !ok {
				return
			}
			action, _ := body["action"].(string)
			if action != "partial" && action != "substitute" && action != "other_hub" {
				c.fail(400, "invalid_request", "action must be partial, substitute or other_hub.", false)
				return
			}
			if r, ok := request(c); ok {
				short := hubRequest(r["id"].(string), "packing", r["seq"].(int)+1, c.now)
				short["shortfall"] = sampleShortfall(action, "open")
				c.json(200, map[string]any{"request": short})
			}
		}},
		{method: "POST", path: "/hub/restock/{request_id}/ready", handle: func(c *call) {
			if c.scenario == "partial_fill" {
				c.fail(409, "shortfall_needed", "A line is short. Record a shortfall first.", false)
				return
			}
			if fail(c, "lots_mismatch") {
				return
			}
			if _, ok := needsBody(c, "lot_numbers"); !ok {
				return
			}
			if r, ok := request(c); ok {
				if r["state"] != "ready" {
					r = hubRequest(r["id"].(string), "ready", r["seq"].(int)+1, c.now)
				}
				c.json(200, map[string]any{"request": r})
			}
		}},
		{method: "GET", path: "/hub/restock/{request_id}/label", handle: func(c *call) {
			if fail(c, "not_ready") {
				return
			}
			if r, ok := request(c); ok {
				c.json(200, map[string]any{"label": map[string]any{"request_code": "R-0" + r["id"].(string)[len(r["id"].(string))-1:], "scooter_code": r["scooter_code"],
					"hub_name": "Koramangala hub", "lot_numbers": []string{"LOT-B-0101", "LOT-C-0201"},
					"items": []map[string]any{{"name": "Burger", "qty": 5}, {"name": "Masala chai", "qty": 10}}, "ready_at": stamp(c.now)}})
			}
		}},
		{method: "GET", path: "/hub/lots", handle: func(c *call) {
			lots := []map[string]any{hubLot("LOT-B-0101", "item_burger", "ok", c.now), hubLot("LOT-C-0201", "item_chai", "ok", c.now), hubLot("LOT-C-0199", "item_chai", "quarantined", c.now)}
			if item := c.r.URL.Query().Get("item_id"); item != "" {
				kept := []map[string]any{}
				for _, l := range lots {
					if l["item_id"] == item {
						kept = append(kept, l)
					}
				}
				lots = kept
			}
			c.json(200, map[string]any{"lots": lots})
		}},
		{method: "POST", path: "/hub/lots/{lot_id}/quarantine", handle: func(c *call) {
			if c.scenario == "not_hub_lead" {
				c.json(403, map[string]any{"code": "forbidden", "message": "Only a hub lead can quarantine a lot.", "retryable": false, "missing_permission": "lot.quarantine"})
				return
			}
			if fail(c, "lot_on_scooter") {
				return
			}
			body, ok := needsBody(c, "reason")
			if !ok {
				return
			}
			if _, known := map[string]bool{"temperature": true, "damaged": true, "expired": true, "contamination": true, "other": true}[body["reason"].(string)]; !known {
				c.fail(400, "invalid_request", "Unknown reason.", false)
				return
			}
			code := c.param("lot_id")
			c.json(200, map[string]any{"lot": hubLot(code, "item_chai", "quarantined", c.now)})
		}},
		{method: "POST", path: "/hub/events", noIdempotent: true, handle: func(c *call) {
			body, ok := needsBody(c, "events")
			if !ok {
				return
			}
			events, _ := body["events"].([]any)
			if len(events) == 0 {
				c.fail(400, "invalid_request", "Send at least one event.", false)
				return
			}
			accepted, rejected := []string{}, []map[string]any{}
			for _, raw := range events {
				e, _ := raw.(map[string]any)
				id, _ := e["event_id"].(string)
				switch {
				case c.scenario == "event_rejected":
					rejected = append(rejected, map[string]any{"id": id, "code": "wrong_lot", "message": "Sample rejection: wrong_lot", "retryable": false})
				case id == "":
					rejected = append(rejected, map[string]any{"id": "", "code": "invalid_event", "message": "Each event needs an event_id.", "retryable": false})
				case map[string]bool{"accept": true, "scan": true, "unscan": true, "shortfall": true, "ready": true, "quarantine": true}[fmt.Sprint(e["type"])]:
					accepted = append(accepted, id)
				default:
					rejected = append(rejected, map[string]any{"id": id, "code": "unknown_event_type", "message": "Unknown event type.", "retryable": false})
				}
			}
			c.json(200, map[string]any{"accepted_ids": accepted, "rejected": rejected})
		}},
	}
}

func (s *Server) hubStream(w http.ResponseWriter, r *http.Request) {
	scenario := scenarioOf(r)
	s.serveStream(w, r, scenario, hubStoryFor(scenario, s.now()).events, false, 0)
}
