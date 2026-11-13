package mockserver

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"
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
	"order_cancelled":         "The order was cancelled while packing, on the stream too. Ready and waiting at the gate fail with order_cancelled",
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
	"reassigned":              "This rider no longer has the order, reason reassigned, on the stream too. Waiting at the gate fails with order_reassigned",
	"otp_not_available":       "The order is at ARRIVED with otp_available false, on the stream too",
	"event_rejected":          "POST events rejects every event with wrong_otp",
	"socket_drop":             "The stream drops after two events",
	"token_expiring":          "The stream closes with 4401 after two events, unless the app sends a new auth frame",
	"no_open_shift":           "Break start, break end, the hand stock count, go online, planned end, the two break aliases and quarantine fail with no_open_shift",
	"order_in_progress":       "Break start fails with order_in_progress, because the rider has a live order",
	"on_break":                "The rider is on a break: GET state shows it on the shift, and break start fails with on_break",
	"restock_not_cancellable": "Restock cancel fails with restock_not_cancellable, because the hub has accepted",
	"not_arrived":             "Waiting at the gate fails with not_arrived, because the rider is not at the gate yet",
	"rider_waiting":           "The order is at ARRIVED with waiting_since set, on the stream too",
	"shift_on_other_phone":    "The shift's device_id is another phone's, so the app shows the open on another phone screen",
	"documents_rejected":      "GET verification failed, with a reason for each rejected document. Shift start fails with rider_not_verified",
	"offer_open":              "Break start fails with offer_open, because an offer is waiting for an answer",
	"unknown_device":          "Shift start fails with 400 unknown_device, because the device_id was not registered by this rider",
	"idempotency_in_progress": "Any write with an Idempotency-Key returns a retryable 409, with Retry-After",
	// For the finished rider app (Issue 235, docs/engineering/rider_apk_compatibility.md).
	"sign_in_paused":       "Verifying a code fails with 429 otp_attempts_exceeded and Retry-After 30, the pause the rider app shows",
	"masked_call_works":    "Calling the customer returns the masked number. Without this scenario it is call_not_available, until a provider is chosen",
	"lot_not_on_scooter":   "Quarantine fails with lot_not_on_scooter",
	"lot_of_another_rider": "Quarantine fails with 404, because the lot is on another rider's scooter",
	"gate_code_wrong":      "The gate check answers checked true and matched false. Arrival is still by location",
	"lot_reserved":         "Quarantine fails with lot_reserved, because part of the lot is held for an order",
	"shift_plan_today":     "GET shift plan has today's plan, made by operations. Without this scenario the plan is null",
	"shift_plan_closed":    "Answering the shift plan fails with shift_plan_closed, because the hub started packing",
	"already_verified":     "Uploading a document fails with already_verified",
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
		"otp_available": state == "ARRIVED", "handed_over_at": nil, "order_number": "A2931", "waiting_since": nil,
		"total": money(24900), "payment_method": "prepaid", // The rider app reads these (Issue 235).
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
	case "rider_waiting":
		offer("open")
		order("RIDER_ACCEPTED", nil)
		order("PACKED", nil)
		order("ARRIVED", func(o map[string]any) { o["waiting_since"] = stamp(now) })
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
		"total": money(24900), "payment_method": "prepaid", // The rider app reads these (Issue 235).
	}
}

func sampleShift(now time.Time, ended bool) map[string]any {
	s := map[string]any{"id": "shf_mock01", "node_id": "node_mock01", "started_at": stamp(now.Add(-time.Hour)), "ended_at": nil,
		"node_assigned_at": stamp(now.Add(-time.Hour)), "scooter": map[string]any{"id": "scr_mock01", "plate": "NZ-014", "battery_pct": nil},
		"break_started_at": nil, "break_ends_at": nil, "device_id": "dev_mock01",
		// The rider app's names for the same facts (Issue 235).
		"online_at": stamp(now.Add(-55 * time.Minute)), "planned_end_at": nil, "break": nil}
	if ended {
		s["ended_at"] = stamp(now)
	}
	return s
}

// setBreak puts a break on a shift in both forms the contract carries: our two fields and the rider app's break object.
func setBreak(s map[string]any, from, to time.Time) {
	s["break_started_at"], s["break_ends_at"] = stamp(from), stamp(to)
	s["break"] = map[string]any{"started_at": stamp(from), "ends_at": stamp(to)}
}

// shiftFor is the open shift a scenario describes: on a break, or started from another phone.
func shiftFor(scenario string, now time.Time) map[string]any {
	s := sampleShift(now, false)
	switch scenario {
	case "on_break":
		setBreak(s, now.Add(-5*time.Minute), now.Add(10*time.Minute))
	case "shift_on_other_phone":
		s["device_id"] = "dev_other01"
	}
	return s
}

// onBreak is the sample shift just after a break began, with the console's longest break of 15 minutes.
func onBreak(now time.Time) map[string]any {
	s := sampleShift(now, false)
	setBreak(s, now, now.Add(15*time.Minute))
	return s
}

// secondPage reports whether the cursor asks for the second page. The mock has two pages, and the cursor "page_2" is the second.
func secondPage(c *call) bool { return c.r.URL.Query().Get("cursor") == "page_2" }

// validDay refuses a query date that is not YYYY-MM-DD. An absent date is fine.
func validDay(c *call, name string) bool {
	if v := c.r.URL.Query().Get(name); v != "" {
		if _, err := time.Parse("2006-01-02", v); err != nil {
			c.fail(http.StatusBadRequest, "invalid_request", name+" must be YYYY-MM-DD.", false)
			return false
		}
	}
	return true
}

// stockCount compares a hand count with the sample stock of 6 burgers and 4 chai on the scooter.
func stockCount(c *call) {
	var body struct {
		Lines []struct {
			ItemID string `json:"item_id"`
			Qty    *int   `json:"qty"`
		} `json:"lines"`
		Reason string `json:"reason"`
	}
	if err := decodeJSON(c.r, &body); err != nil || len(body.Lines) == 0 || body.Reason == "" || utf8.RuneCountInString(body.Reason) > 200 {
		c.fail(http.StatusBadRequest, "invalid_request", "Send lines and a reason of up to 200 characters.", false)
		return
	}
	expected := map[string]int{"item_burger": 6, "item_chai": 4}
	counted, mismatches, seen := []map[string]any{}, 0, map[string]bool{}
	for _, l := range body.Lines {
		if seen[l.ItemID] {
			c.fail(http.StatusBadRequest, "invalid_request", "Each item_id can appear once.", false)
			return
		}
		seen[l.ItemID] = true
		if l.ItemID == "" || l.Qty == nil || *l.Qty < 0 {
			c.fail(http.StatusBadRequest, "invalid_request", "Each line needs an item_id and a qty of 0 or more.", false)
			return
		}
		if expected[l.ItemID] != *l.Qty {
			mismatches++
		}
		counted = append(counted, map[string]any{"item_id": l.ItemID, "expected": expected[l.ItemID], "counted": *l.Qty})
	}
	c.json(200, map[string]any{"counted": counted, "mismatches": mismatches})
}

func sampleShiftEntry(id string, now time.Time, ago time.Duration, open bool) map[string]any {
	e := map[string]any{"id": id, "node_id": "node_mock01", "started_at": stamp(now.Add(-ago)), "ended_at": stamp(now.Add(-ago + 4*time.Hour)),
		"online_s": 13200, "break_s": 1200, "orders_handed_over": 9,
		// What the rider app reads of a shift (Issue 235). orders repeats orders_handed_over.
		"online_at": stamp(now.Add(-ago + 5*time.Minute)), "planned_end_at": nil, "orders": 9, "portions": 14, "avg_delivery_s": 142}
	if open {
		e["ended_at"], e["online_s"], e["break_s"], e["orders_handed_over"] = nil, 3300, 300, 2
		e["orders"], e["portions"] = 2, 3
	}
	return e
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
	routes := []route{
		{method: "POST", path: "/auth/otp/request", open: true, noIdempotent: true, handle: riderOtpRequest},
		{method: "POST", path: "/auth/otp/verify", open: true, noIdempotent: true, handle: func(c *call) {
			if c.scenario == "sign_in_paused" {
				// The rider app reads the code and Retry-After and shows the pause (Issue 235).
				c.w.Header().Set("Retry-After", "30")
				c.json(429, map[string]any{"code": "otp_attempts_exceeded", "message": "Too many wrong codes. Try again shortly.", "retryable": true})
				return
			}
			otpVerify(c, riderSessionFields(c))
		}},
		{method: "POST", path: "/auth/refresh", open: true, noIdempotent: true, handle: refresh},
		{method: "POST", path: "/auth/signout", noIdempotent: true, handle: func(c *call) { c.json(200, map[string]any{}) }},
		{method: "GET", path: "/riders/me", handle: func(c *call) {
			if c.scenario == "account_blocked" {
				accountBlocked(c)
				return
			}
			c.json(200, map[string]any{"rider": sampleRider(c.now, "Ravi Kumar", nil)})
		}},
		{method: "PATCH", path: "/riders/me", handle: func(c *call) {
			updateRider(c)
		}},
		{method: "GET", path: "/riders/me/notifications", handle: func(c *call) {
			ago := func(m int) string { return stamp(c.now.Add(-time.Duration(m) * time.Minute)) }
			items, next := []map[string]any{
				{"id": "ntf_mock03", "kind": "offer", "title": "New order", "body": "A burger is waiting for you.", "order_id": orderID, "created_at": ago(5)},
				{"id": "ntf_mock02", "kind": "restock", "title": "Restock ready", "body": "Your cassette at Hub 2 is ready.", "order_id": nil, "created_at": ago(90)},
			}, any("page_2")
			if secondPage(c) {
				items, next = []map[string]any{{"id": "ntf_mock01", "kind": "shift", "title": "Shift started", "body": "You are online at Node 7.", "order_id": nil, "created_at": ago(180)}}, nil
			}
			c.json(200, map[string]any{"notifications": items, "next_cursor": next})
		}},
		{method: "GET", path: "/riders/me/shifts", handle: func(c *call) {
			if !validDay(c, "from") || !validDay(c, "to") {
				return
			}
			if from, to := c.r.URL.Query().Get("from"), c.r.URL.Query().Get("to"); from != "" && to != "" && from > to {
				c.fail(http.StatusBadRequest, "invalid_request", "from must not be after to.", false)
				return
			}
			// The rider app asks with days (Issue 235). It does not page, so it gets every shift in one reply.
			if raw := c.r.URL.Query().Get("days"); raw != "" {
				days, err := strconv.Atoi(raw)
				q := c.r.URL.Query()
				if err != nil || days < 1 || days > 31 || q.Get("from") != "" || q.Get("to") != "" {
					c.fail(http.StatusBadRequest, "invalid_request", "days is 1 to 31 and cannot be sent with from or to.", false)
					return
				}
				c.json(200, map[string]any{"shifts": []map[string]any{sampleShiftEntry("shf_mock01", c.now, time.Hour, true), sampleShiftEntry("shf_mock00", c.now, 30*time.Hour, false), sampleShiftEntry("shf_mock99", c.now, 54*time.Hour, false)}, "next_cursor": nil})
				return
			}
			items, next := []map[string]any{sampleShiftEntry("shf_mock01", c.now, time.Hour, true), sampleShiftEntry("shf_mock00", c.now, 30*time.Hour, false)}, any("page_2")
			if secondPage(c) {
				items, next = []map[string]any{sampleShiftEntry("shf_mock99", c.now, 54*time.Hour, false)}, nil
			}
			c.json(200, map[string]any{"shifts": items, "next_cursor": next})
		}},
		{method: "GET", path: "/riders/me/shifts/{shift_id}/summary", handle: func(c *call) {
			id := c.param("shift_id")
			ago := map[string]time.Duration{"shf_mock01": time.Hour, "shf_mock00": 30 * time.Hour, "shf_mock99": 54 * time.Hour}[id]
			if ago == 0 {
				c.fail(http.StatusNotFound, "not_found", "No such shift for this rider.", false)
				return
			}
			entry := sampleShiftEntry(id, c.now, ago, id == "shf_mock01")
			sold := []map[string]any{{"item_id": "item_burger", "name": "Burger", "qty": 8}, {"item_id": "item_chai", "name": "Masala chai", "qty": 3}}
			returned, earned := []map[string]any{{"item_id": "item_burger", "name": "Burger", "qty": 2}}, 36000
			if entry["ended_at"] == nil { // An open shift counts up to now, and nothing has come back yet.
				sold, returned, earned = sold[:1], []map[string]any{}, 8000
			}
			c.json(200, map[string]any{"shift_id": id, "started_at": entry["started_at"], "ended_at": entry["ended_at"],
				"online_s": entry["online_s"], "break_s": entry["break_s"], "orders_handed_over": entry["orders_handed_over"], "avg_delivery_s": 142,
				"items_sold": sold, "items_returned": returned, "earnings": money(earned)})
		}},
		{method: "POST", path: "/riders/me/breaks/start", handle: func(c *call) {
			if offerError(c, "no_open_shift", "order_in_progress", "offer_open", "on_break") {
				return
			}
			c.json(200, map[string]any{"shift": onBreak(c.now)})
		}},
		{method: "POST", path: "/riders/me/breaks/end", handle: func(c *call) {
			if offerError(c, "no_open_shift") {
				return
			}
			c.json(200, map[string]any{"shift": sampleShift(c.now, false)})
		}},
		{method: "POST", path: "/riders/me/stock/count", handle: func(c *call) {
			if offerError(c, "no_open_shift") {
				return
			}
			stockCount(c)
		}},
		{method: "POST", path: "/restock/{request_id}/cancel", handle: func(c *call) {
			if offerError(c, "restock_not_cancellable") {
				return
			}
			c.json(200, map[string]any{"request": sampleRestock("cancelled", c.now, false)})
		}},
		{method: "POST", path: "/orders/{order_id}/waiting", handle: func(c *call) {
			if c.scenario == "reassigned" {
				c.fail(409, "order_reassigned", "This order is no longer yours.", false)
				return
			}
			if offerError(c, "not_arrived", "order_cancelled") {
				return
			}
			o := riderOrder("ARRIVED", 5, c.now)
			o["waiting_since"] = stamp(c.now)
			c.json(200, map[string]any{"order": o})
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
			c.json(200, map[string]any{"seq": story.seq, "shift": shiftFor(c.scenario, c.now), "offers": offers, "current_order": current, "restock_request": request})
		}},
		{method: "GET", path: "/riders/me/verification", handle: func(c *call) {
			status, reason := map[string]string{"verification_pending": "pending", "verification_failed": "failed", "verification_expired": "expired",
				"documents_rejected": "failed"}[c.scenario], any(nil)
			if status == "" {
				status = "verified"
			}
			if status == "failed" {
				reason = "The document name did not match."
			}
			documents := []map[string]any{}
			if c.scenario == "documents_rejected" {
				documents = []map[string]any{
					{"type": "aadhaar", "status": "rejected", "reason": "name_mismatch"},
					{"type": "driving_licence", "status": "rejected", "reason": "photo_retake"},
					{"type": "rc", "status": "verified", "reason": nil},
					{"type": "insurance", "status": "pending", "reason": nil},
				}
			}
			c.json(200, map[string]any{"status": status, "reason": reason, "documents": documents})
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
			case "verification_pending", "verification_failed", "verification_expired", "documents_rejected":
				c.fail(409, "rider_not_verified", "Verification is not complete.", false)
				return
			}
			if offerError(c, "rider_not_approved", "shift_limit_reached") {
				return
			}
			if body, ok := needsBody(c, "node_id"); ok {
				id, _ := body["device_id"].(string)
				if body["take_over"] == true && id == "" {
					c.fail(400, "invalid_request", "take_over needs a device_id.", false)
					return
				}
				if !validChecklist(body["checklist"]) {
					c.fail(400, "invalid_request", "checklist is a list of up to 20 codes, each once.", false)
					return
				}
				if c.scenario == "unknown_device" {
					c.fail(400, "unknown_device", "That device is not registered for this rider.", false)
					return
				}
				if c.scenario == "shift_already_open" && body["take_over"] != true {
					c.fail(409, "shift_already_open", "Sample rejection: shift_already_open", false)
					return
				}
				shift := sampleShift(c.now, false)
				if id != "" {
					shift["device_id"] = id
				}
				c.json(200, map[string]any{"shift": shift})
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
			// The owner's rule: no masked call until a provider is chosen. The scenario shows the success shape.
			if c.scenario != "masked_call_works" {
				c.fail(409, "call_not_available", "Masked calls are not available yet.", false)
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
			hub := mockHub()
			hub["manager_phone"] = "+919845012345" // For Call hub manager in the rider app (Issue 235).
			c.json(200, map[string]any{"hubs": []map[string]any{hub}, "items": []map[string]any{{"item_id": "item_burger", "name": "Burger"}}})
		}},
		{method: "GET", path: "/restock/{request_id}", handle: func(c *call) {
			c.json(200, map[string]any{"request": sampleRestock("packing", c.now, c.scenario == "hub_short")})
		}},
		{method: "POST", path: "/riders/me/problems", handle: func(c *call) {
			if body, ok := needsBody(c, "type"); ok {
				if t, _ := body["type"].(string); !reportProblemTypes[t] {
					c.fail(400, "invalid_request", "type is breakdown, accident, gate_closed, customer_absent or unsafe_route.", false)
					return
				}
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
	return append(routes, riderAPKRoutes()...)
}

// reportProblemTypes is the enum of reportProblem type in the contract. The settings may offer only these.
var reportProblemTypes = map[string]bool{"breakdown": true, "accident": true, "gate_closed": true, "customer_absent": true, "unsafe_route": true}

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

// sampleRider is the signed-in rider. preferredArea is nil until the rider has set one.
func sampleRider(now time.Time, name string, preferredArea any) map[string]any {
	return map[string]any{"id": "rdr_mock0001", "name": name, "status": "active", "zone_id": "zone_mock01", "preferred_area": preferredArea,
		"created_at": stamp(now.Add(-30 * 24 * time.Hour)), "approved_at": stamp(now.Add(-29 * 24 * time.Hour)),
		// For the rider app (Issue 235). A rider who has set no email and no place shows null.
		"phone": "+919876543210", "email": nil, "area": nil}
}

// mockHub is the sample hub reference, with its place.
func mockHub() map[string]any {
	return map[string]any{"id": "hub_mock01", "name": "Hub 2", "lat": 12.9352, "lng": 77.6245}
}
