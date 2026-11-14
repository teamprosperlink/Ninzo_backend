package mockserver

import (
	"encoding/base64"
	"net/http"
	"net/mail"
	"regexp"
	"time"
	"unicode/utf8"
)

// This file holds the rider operations added so the finished rider app (the APK) works against
// the mock unchanged. The call-by-call list is docs/engineering/rider_apk_compatibility.md
// and the Issue is 235. Every route here is mock only: the server follows in part B.

const (
	// mockMaxBreak is the console's longest break (max_break_s) in the mock.
	mockMaxBreak = 15 * time.Minute
	// maxDocumentBytes is the largest photo, once decoded. The contract caps data_base64 at 933336 characters, which is exactly this many bytes.
	maxDocumentBytes = 700000
	// mockGateCode is the code printed at the sample gate.
	mockGateCode = "GATE-4821"
)

// conflictFor fails with a 409 when the scenario is one of the codes, and says so.
func conflictFor(c *call, codes ...string) bool {
	for _, code := range codes {
		if c.scenario == code {
			c.fail(http.StatusConflict, code, "Sample rejection: "+code, false)
			return true
		}
	}
	return false
}

// validChecklist is true for no checklist, or a list of up to 20 distinct codes.
// riderOtpRequest is requestOtp for the rider app. A phone that is not an Indian mobile number gets 400 invalid_phone, which the app shows as its invalid number screen.
func riderOtpRequest(c *call) {
	switch c.scenario {
	case "otp_resend_limit", "rate_limited":
		otpRequest(c)
		return
	}
	var body map[string]any
	if err := decodeJSON(c.r, &body); err != nil {
		c.fail(http.StatusBadRequest, "invalid_request", "The body must be JSON.", false)
		return
	}
	phone, present := body["phone"]
	if !present || phone == nil || phone == "" {
		c.fail(http.StatusBadRequest, "invalid_request", "Missing field: phone", false)
		return
	}
	if p, ok := phone.(string); !ok || !indianMobile.MatchString(p) {
		c.fail(http.StatusBadRequest, "invalid_phone", "That is not an Indian mobile number.", false)
		return
	}
	c.json(200, map[string]any{"request_id": "otp_mock01", "expires_in_s": 300, "resend_after_s": 30})
}

var indianMobile = regexp.MustCompile(`^\+91[6-9][0-9]{9}$`)

func validChecklist(v any) bool {
	if v == nil {
		return true
	}
	list, ok := v.([]any)
	if !ok || len(list) > 20 {
		return false
	}
	seen := map[string]bool{}
	for _, item := range list {
		code, ok := item.(string)
		if !ok || code == "" || seen[code] {
			return false
		}
		seen[code] = true
	}
	return true
}

// updateRider is PATCH /riders/me. It accepts our fields (name, preferred_area) and the rider app's (email, area).
func updateRider(c *call) {
	bad := func(msg string) { c.fail(http.StatusBadRequest, "invalid_request", msg, false) }
	var body map[string]any
	if err := decodeJSON(c.r, &body); err != nil {
		bad("The body must be JSON.")
		return
	}
	name, preferred, email, area := "Ravi Kumar", any(nil), any(nil), any(nil)
	_, hasName := body["name"]
	_, hasPreferred := body["preferred_area"]
	_, hasEmail := body["email"]
	_, hasArea := body["area"]
	if !hasName && !hasPreferred && !hasEmail && !hasArea {
		bad("Send at least one of name, preferred_area, email and area.")
		return
	}
	if hasPreferred && hasArea {
		bad("Send preferred_area or area, not both.")
		return
	}
	if hasName {
		s, ok := body["name"].(string)
		if !ok || utf8.RuneCountInString(s) < 1 || utf8.RuneCountInString(s) > 80 {
			bad("name is 1 to 80 characters.")
			return
		}
		name = s
	}
	if hasPreferred {
		s, ok := body["preferred_area"].(string)
		if !ok || utf8.RuneCountInString(s) > 80 {
			bad("preferred_area is up to 80 characters.")
			return
		}
		preferred = s
	}
	if hasEmail && body["email"] != nil {
		s, ok := body["email"].(string)
		if _, err := mail.ParseAddress(s); !ok || err != nil || len(s) > 254 {
			bad("email must be an address of up to 254 characters.")
			return
		}
		email = s
	}
	if hasArea && body["area"] != nil {
		a, ok := body["area"].(map[string]any)
		if !ok {
			bad("area is a place with name, city, lat and lng.")
			return
		}
		n, _ := a["name"].(string)
		city, cityOK := a["city"].(string)
		lat, latOK := a["lat"].(float64)
		lng, lngOK := a["lng"].(float64)
		if utf8.RuneCountInString(n) < 1 || utf8.RuneCountInString(n) > 80 || !cityOK || utf8.RuneCountInString(city) > 80 ||
			!latOK || !lngOK || lat < -90 || lat > 90 || lng < -180 || lng > 180 {
			bad("area needs a name of 1 to 80 characters, a city, a lat of -90 to 90 and a lng of -180 to 180.")
			return
		}
		area, preferred = map[string]any{"name": n, "city": city, "lat": lat, "lng": lng}, n
	}
	rider := sampleRider(c.now, name, preferred)
	rider["email"], rider["area"] = email, area
	c.json(200, map[string]any{"rider": rider})
}

func sampleDocument(kind, side string, now time.Time) map[string]any {
	return map[string]any{"document": kind, "side": side, "content_type": "image/jpeg", "size_bytes": 182340, "uploaded_at": stamp(now.Add(-48 * time.Hour))}
}

func riderAppSettings() map[string]any {
	choice := func(code, label, hint, check string) map[string]any {
		m := map[string]any{"code": code, "label": label}
		if hint != "" {
			m["hint"] = hint
		}
		if check != "" {
			m["check"] = check
		}
		return m
	}
	return map[string]any{
		"stock_request_ask_before_s": 3600, "stock_request_reminder_before_s": 1800, "shift_ending_soon_before_s": 900,
		"break_extension_s": 900, "not_at_gate_after_s": 30, "handover_code_length": 4, "cancel_pay_cap": money(4000),
		"verification_within_s": 86400, "timer_length_s": 90, "max_break_s": int(mockMaxBreak.Seconds()),
		"arrive_m": 100, "almost_there_m": 350, "away_from_spot_m": 400, "low_stock_qty": 2, "scooter_capacity_qty": 24,
		"min_phone_battery_pct": 20, "min_scooter_battery_pct": 30, "low_scooter_battery_pct": 15, "ride_speed_kmh": 18,
		"break_options_s": []int{300, 600, 900}, "add_time_options_s": []int{900, 1800, 3600},
		"skip_reasons": []map[string]any{choice("too_far", "Too far", "", ""), choice("not_ready", "Not ready", "", "")},
		"problem_types": []map[string]any{
			choice("gate_closed", "Gate is closed", "Tell the customer you are waiting.", ""),
			choice("breakdown", "Scooter broke down", "Move to a safe place first.", ""),
			choice("customer_absent", "Customer is not at the gate", "", ""),
		},
		"checklist": []map[string]any{
			choice("helmet", "Helmet on", "", "manual"), choice("phone_battery", "Phone charged", "", "phone_battery"),
			choice("scooter_battery", "Scooter charged", "", "scooter_battery"), choice("location", "Location is on", "", "location"),
		},
	}
}

func sampleShiftPlan(now time.Time) map[string]any {
	return map[string]any{"id": "plan_mock01", "date": now.UTC().Format("2006-01-02"), "node_id": "node_mock01",
		"starts_at": stamp(now.Add(2 * time.Hour)), "ends_at": stamp(now.Add(8 * time.Hour)), "stock_reply": "pending", "replied_at": nil,
		"packing": "not_started", "ready_at": nil}
}

// riderAPKRoutes are the operations the rider app uses that the first rider contract did not have.
func riderAPKRoutes() []route {
	return []route{
		{method: "POST", path: "/riders/me/shifts/online", handle: func(c *call) {
			if conflictFor(c, "no_open_shift") {
				return
			}
			c.json(200, map[string]any{"shift": sampleShift(c.now, false)})
		}},
		{method: "POST", path: "/riders/me/shifts/break", handle: func(c *call) {
			if conflictFor(c, "no_open_shift", "order_in_progress", "offer_open") {
				return
			}
			body, ok := needsBody(c, "length_s")
			if !ok {
				return
			}
			length, isNumber := body["length_s"].(float64)
			if !isNumber || length != float64(int(length)) || length < 60 || length > 3600 {
				c.fail(http.StatusBadRequest, "invalid_request", "length_s is 60 to 3600.", false)
				return
			}
			// The break counts from its start, and never lasts longer than the console's max_break_s.
			start := c.now
			if c.scenario == "on_break" {
				start = c.now.Add(-5 * time.Minute)
			}
			span := time.Duration(length) * time.Second
			if span > mockMaxBreak {
				span = mockMaxBreak
			}
			shift := sampleShift(c.now, false)
			setBreak(shift, start, start.Add(span))
			c.json(200, map[string]any{"shift": shift})
		}},
		{method: "POST", path: "/riders/me/shifts/break/end", handle: func(c *call) {
			if conflictFor(c, "no_open_shift") {
				return
			}
			c.json(200, map[string]any{"shift": sampleShift(c.now, false)})
		}},
		{method: "POST", path: "/riders/me/shifts/planned-end", handle: func(c *call) {
			if conflictFor(c, "no_open_shift") {
				return
			}
			body, ok := needsBody(c, "planned_end_at")
			if !ok {
				return
			}
			// The server also refuses an end more than 16 hours after the shift began. The mock only refuses the past.
			raw, _ := body["planned_end_at"].(string)
			at, err := time.Parse(time.RFC3339, raw)
			if err != nil || !at.After(c.now) {
				c.fail(http.StatusBadRequest, "invalid_request", "planned_end_at must be a time in the future.", false)
				return
			}
			shift := sampleShift(c.now, false)
			shift["planned_end_at"] = stamp(at)
			c.json(200, map[string]any{"shift": shift})
		}},
		{method: "GET", path: "/riders/me/documents", handle: func(c *call) {
			c.json(200, map[string]any{"documents": []map[string]any{sampleDocument("aadhaar", "front", c.now), sampleDocument("drivingLicence", "front", c.now)}})
		}},
		{method: "POST", path: "/riders/me/documents", handle: func(c *call) {
			if conflictFor(c, "already_verified") {
				return
			}
			body, ok := needsBody(c, "document", "side", "content_type", "data_base64")
			if !ok {
				return
			}
			kinds := map[string]bool{"aadhaar": true, "drivingLicence": true, "vehicleRc": true, "selfie": true, "bank": true}
			sides := map[string]bool{"front": true, "back": true, "photo": true}
			types := map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true}
			kind, _ := body["document"].(string)
			side, _ := body["side"].(string)
			contentType, _ := body["content_type"].(string)
			data, _ := body["data_base64"].(string)
			photo, err := base64.StdEncoding.DecodeString(data)
			if !kinds[kind] || !sides[side] || !types[contentType] || err != nil || len(photo) == 0 || len(photo) > maxDocumentBytes {
				c.fail(http.StatusBadRequest, "invalid_request", "Send a known document and side, a jpeg, png or webp, and base64 of up to 700 KB.", false)
				return
			}
			c.json(201, map[string]any{"document": map[string]any{"document": kind, "side": side, "content_type": contentType, "size_bytes": len(photo), "uploaded_at": stamp(c.now)}})
		}},
		{method: "GET", path: "/riders/me/settings", handle: func(c *call) {
			c.json(200, map[string]any{"settings": riderAppSettings()})
		}},
		{method: "GET", path: "/riders/me/help", handle: func(c *call) {
			if c.scenario == "help_empty" {
				c.json(200, map[string]any{"faq": []map[string]any{}})
				return
			}
			c.json(200, map[string]any{"faq": []map[string]any{
				{"id": "faq_mock01", "question": "How do I start a shift?", "answer": "Open the app, pick your spot and tap Start shift."},
				{"id": "faq_mock02", "question": "What if the gate is closed?", "answer": "Tap Report a problem, then Gate is closed. Support will help."},
				{"id": "faq_mock03", "question": "When am I paid?", "answer": "Your earnings for the day show under Earnings."},
			}})
		}},
		{method: "POST", path: "/riders/me/feedback", handle: func(c *call) {
			body, ok := needsBody(c, "message")
			if !ok {
				return
			}
			if m, isString := body["message"].(string); !isString || utf8.RuneCountInString(m) > 2000 {
				c.fail(http.StatusBadRequest, "invalid_request", "message is 1 to 2000 characters.", false)
				return
			}
			c.json(201, map[string]any{"feedback_id": "fbk_mock01"})
		}},
		// The scooter calls this with the key of that one scooter. The mock's key for the scooter NZ-014 is scooter-key-NZ-014.
		{method: "POST", path: "/scooters/{code}/telemetry", open: true, noIdempotent: true, handle: func(c *call) {
			if c.r.Header.Get("X-Telemetry-Key") != "scooter-key-"+c.param("code") {
				c.fail(http.StatusUnauthorized, "unauthorized", "Send the key of this scooter in X-Telemetry-Key.", false)
				return
			}
			if c.param("code") != "NZ-014" {
				c.fail(http.StatusNotFound, "not_found", "No such scooter.", false)
				return
			}
			body, ok := needsBody(c, "battery_pct", "at")
			if !ok {
				return
			}
			pct, isNumber := body["battery_pct"].(float64)
			rawAt, _ := body["at"].(string)
			at, err := time.Parse(time.RFC3339, rawAt)
			if !isNumber || pct != float64(int(pct)) || pct < 0 || pct > 100 || err != nil || at.After(c.now.Add(time.Minute)) {
				c.fail(http.StatusBadRequest, "invalid_request", "battery_pct is 0 to 100, and at is a time that is not ahead of the server.", false)
				return
			}
			c.json(202, map[string]any{})
		}},
		{method: "POST", path: "/riders/me/stock/{lot_id}/quarantine", handle: func(c *call) {
			if conflictFor(c, "no_open_shift", "lot_reserved", "lot_not_on_scooter") {
				return
			}
			if c.scenario == "lot_of_another_rider" {
				c.fail(http.StatusNotFound, "not_found", "No such lot on your scooter.", false)
				return
			}
			body, ok := needsBody(c, "reason")
			if !ok {
				return
			}
			if r, _ := body["reason"].(string); r != "temperature" && r != "damaged" && r != "expired" && r != "other" {
				c.fail(http.StatusBadRequest, "invalid_request", "reason is temperature, damaged, expired or other.", false)
				return
			}
			if n, isString := body["note"].(string); body["note"] != nil && (!isString || utf8.RuneCountInString(n) > 500) {
				c.fail(http.StatusBadRequest, "invalid_request", "note is up to 500 characters.", false)
				return
			}
			c.json(200, map[string]any{"lot_id": c.param("lot_id"), "quarantined_at": stamp(c.now)})
		}},
		// Checking the gate code changes nothing and never decides arrival, which comes from the rider's location.
		{method: "POST", path: "/orders/{order_id}/gate-check", noIdempotent: true, handle: func(c *call) {
			body, ok := needsBody(c, "method", "value")
			if !ok {
				return
			}
			method, _ := body["method"].(string)
			value, isString := body["value"].(string)
			if (method != "qr" && method != "code") || !isString || utf8.RuneCountInString(value) > 64 {
				c.fail(http.StatusBadRequest, "invalid_request", "method is qr or code, and value is 1 to 64 characters.", false)
				return
			}
			c.json(200, map[string]any{"checked": true, "matched": value == mockGateCode && c.scenario != "gate_code_wrong"})
		}},
		{method: "GET", path: "/riders/me/shift-plan", handle: func(c *call) {
			var plan any // Shifts are open-ended, so most days have no plan.
			if c.scenario == "shift_plan_today" || c.scenario == "shift_plan_closed" {
				plan = sampleShiftPlan(c.now)
			}
			c.json(200, map[string]any{"plan": plan})
		}},
		{method: "POST", path: "/riders/me/shift-plan/reply", handle: func(c *call) {
			if conflictFor(c, "shift_plan_closed") {
				return
			}
			body, ok := needsBody(c, "reply")
			if !ok {
				return
			}
			reply, _ := body["reply"].(string)
			if reply != "coming" && reply != "not_today" {
				c.fail(http.StatusBadRequest, "invalid_request", "reply is coming or not_today.", false)
				return
			}
			if c.scenario != "shift_plan_today" {
				c.fail(http.StatusNotFound, "not_found", "There is no plan for today.", false)
				return
			}
			plan := sampleShiftPlan(c.now)
			plan["stock_reply"], plan["replied_at"] = reply, stamp(c.now)
			c.json(200, map[string]any{"plan": plan})
		}},
	}
}
