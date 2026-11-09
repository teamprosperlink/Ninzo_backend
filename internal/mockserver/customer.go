package mockserver

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// customerScenarios lists every scenario, with what it shows. The first group comes
// from the customer KT doc. The rest come from the owner's flow in decision 0007.
var customerScenarios = map[string]string{
	"invalid_google_token":     "Google sign-in fails with invalid_google_token",
	"account_blocked":          "Every sign-in, phone verification, change of sign-in method and GET me fail with account_blocked",
	"phone_not_verified":       "GET me has no phone. Orders, gates, notify requests and tickets fail with phone_required",
	"phone_already_registered": "Phone verification fails with phone_already_registered",
	"sign_in_method_google":    "Phone sign-in fails with sign_in_method_mismatch, unless switch_to_phone is true",
	"sign_in_method_unchanged": "Changing sign-in method fails with sign_in_method_unchanged",
	"google_account_in_use":    "Changing to Google fails with google_account_in_use",
	"phone_already_set":        "Phone verification fails with phone_already_set",
	"no_scooter":               "No scooter available. The order is at no_scooter, with a longer ETA on offer. Availability offers nothing",
	"no_scooter_no_offer":      "No scooter available, and no longer ETA on offer",
	"offer_timeout":            "No rider took the offer in time, so the order is at no_scooter",
	"offer_accepted":           "The customer is shown rider_accepted",
	"payment_declined":         "The order is at payment_failed, so the customer can retry",
	"payment_captured":         "Payment confirmed, inside the 5-second cancel window. This is the same as no scenario",
	"handed_over":              "The order is at handed_over",
	"socket_drop":              "The stream drops after two events, with no close frame. GET still works",
	"last_unit_taken":          "POST order fails with sold_out, and availability shows one left",
	"gate_not_verified":        "POST order fails with gate_not_verified. Availability offers nothing",
	"zone_offer_off":           "POST order fails with no_offer. Availability offers nothing",

	"awaiting_payment":         "The order is at payment, so checkout can resume and the gate can change",
	"after_cancel_window":      "Payment confirmed, and the 5-second window has passed, so a cancel is refused",
	"cancel_too_late":          "The same as after_cancel_window",
	"max_wait_passed":          "wait_deadline has passed, so the customer can leave with a refund",
	"rider_already_near":       "The rider was already in the zone. The stream reaches rider_accepted one event sooner",
	"rider_inside_perimeter":   "The rider was already inside 100 m, so there is no timer: rider_accepted, then arrived",
	"timer_running":            "The order is at timer_running",
	"timer_missed":             "The timer ended before 100 m. timer_missed is true, with a credit remedy",
	"signal_lost":              "The timer ended, and the rider's signal is lost, so the customer has the OTP",
	"arrived":                  "The order is at arrived, with the handover OTP",
	"cancelled":                "The order has ended, cancelled by the customer inside the window, and refunded",
	"cancelled_after_deadline": "The order has ended, cancelled after wait_deadline, and refunded",
	"timed_out":                "The order has ended, timed out, and refunded",
	"failed":                   "The order has ended after a failure, and refunded",
	"temperature_failed":       "The order has ended after a failed temperature check, and refunded",
	"missing_food":             "The order was handed over, but food was missing, so it is refunded",
	"eta_offer_expired":        "POST accept-eta fails with eta_offer_expired, at no_scooter",
	"invalid_otp":              "POST verify fails with invalid_otp",
	"otp_expired":              "POST verify fails with otp_expired",
	"otp_resend_limit":         "POST OTP request fails with otp_resend_limit",
	"out_of_service_area":      "POST gate fails with out_of_service_area, and the location check says not served",
	"duplicate_gate":           "POST gate fails with duplicate_gate",
	"gate_limit":               "POST gate fails with gate_limit",
	"rate_limited":             "POST OTP request returns 429",
	"server_error":             "POST order returns a retryable 500",
	"service_unavailable":      "POST order returns a retryable 503",
	"idempotency_in_progress":  "Any write with an Idempotency-Key returns a retryable 409, with Retry-After",
	"token_expiring":           "The stream closes with 4401 after two events, unless the app sends a new auth frame",
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func money(amount int) map[string]any {
	return map[string]any{"amount_minor": amount, "currency": "INR"}
}

func menuItem(id, name string, price int) map[string]any {
	return map[string]any{
		"id": id, "name": name, "description": "Sample item for the mock.", "price": money(price),
		"allergens": []string{"gluten"}, "photo_url": nil,
	}
}

func sampleGate() map[string]any {
	return map[string]any{"id": "gate_mock01", "label": "Main gate", "lat": 12.935, "lng": 77.625, "entry_notes": "Near the security desk", "verified": true}
}

func needsBody(c *call, fields ...string) (map[string]any, bool) {
	body := map[string]any{}
	if err := decodeJSON(c.r, &body); err != nil {
		c.fail(http.StatusBadRequest, "invalid_request", "The body must be JSON.", false)
		return nil, false
	}
	for _, f := range fields {
		if v, ok := body[f]; !ok || v == nil || v == "" {
			c.fail(http.StatusBadRequest, "invalid_request", "Missing field: "+f, false)
			return nil, false
		}
	}
	return body, true
}

func customerRoutes() []route {
	return []route{
		{method: "POST", path: "/auth/otp/request", open: true, noIdempotent: true, handle: otpRequest},
		{method: "POST", path: "/auth/otp/verify", open: true, noIdempotent: true, handle: func(c *call) {
			if c.scenario == "sign_in_method_google" {
				body, ok := needsBody(c, "request_id", "otp")
				if !ok {
					return
				}
				if body["switch_to_phone"] != true {
					c.json(409, map[string]any{"code": "sign_in_method_mismatch", "message": "This account signs in another way.", "retryable": false, "sign_in_method": "google"})
					return
				}
				c.json(200, sessionReply(map[string]any{"customer_id": "cus_mock0001", "phone_verified": true}))
				return
			}
			otpVerify(c, map[string]any{"customer_id": "cus_mock0001", "phone_verified": true})
		}},
		{method: "POST", path: "/me/sign-in-method", noIdempotent: true, handle: changeSignInMethod},
		{method: "POST", path: "/auth/google", open: true, noIdempotent: true, handle: googleSignIn},
		{method: "POST", path: "/me/phone/otp", noIdempotent: true, handle: otpRequest},
		{method: "POST", path: "/me/phone/verify", noIdempotent: true, handle: verifyPhone},
		{method: "POST", path: "/auth/refresh", open: true, noIdempotent: true, handle: refresh},
		{method: "POST", path: "/auth/signout", noIdempotent: true, handle: func(c *call) { c.json(200, map[string]any{}) }},
		{method: "GET", path: "/me", handle: func(c *call) {
			if c.scenario == "account_blocked" {
				accountBlocked(c)
				return
			}
			var phone any = "+919876543210"
			if c.scenario == "phone_not_verified" {
				phone = nil
			}
			c.json(200, map[string]any{"customer": map[string]any{"id": "cus_mock0001", "phone": phone, "name": "Test Customer", "email": nil, "sign_in_method": mockMethod(c)}})
		}},
		{method: "PATCH", path: "/me", handle: func(c *call) {
			body, ok := needsBody(c)
			if !ok {
				return
			}
			_, hasName := body["name"]
			_, hasEmail := body["email"]
			if !hasName && !hasEmail {
				c.fail(400, "invalid_request", "Send name or email.", false)
				return
			}
			if v, isText := body["name"].(string); hasName && (!isText || strings.TrimSpace(v) == "") {
				c.fail(400, "invalid_request", "That name is not valid.", false)
				return
			}
			if v, isText := body["email"].(string); hasEmail && body["email"] != nil && (!isText || !strings.Contains(v, "@")) {
				c.fail(400, "invalid_request", "That email is not valid.", false)
				return
			}
			name := body["name"]
			if !hasName {
				name = "Test Customer"
			}
			c.json(200, map[string]any{"customer": map[string]any{"id": "cus_mock0001", "phone": "+919876543210", "name": name, "email": body["email"], "sign_in_method": mockMethod(c)}})
		}},
		{method: "POST", path: "/me/devices", handle: registerDevice},
		{method: "DELETE", path: "/me/devices/{device_id}", handle: func(c *call) { c.json(200, map[string]any{}) }},
		{method: "GET", path: "/gates", handle: func(c *call) {
			if c.scenario == "account_blocked" {
				accountBlocked(c)
				return
			}
			c.json(200, map[string]any{"gates": []map[string]any{sampleGate()}})
		}},
		{method: "POST", path: "/gates", handle: func(c *call) {
			if phoneRequired(c) {
				return
			}
			switch c.scenario {
			case "out_of_service_area":
				c.fail(400, "out_of_service_area", "This pin is outside the service area.", false)
				return
			case "duplicate_gate":
				c.fail(409, "duplicate_gate", "This gate is already saved.", false)
				return
			case "gate_limit":
				c.fail(409, "gate_limit", "Too many saved gates.", false)
				return
			case "account_blocked":
				accountBlocked(c)
				return
			}
			body, ok := needsBody(c, "label", "lat", "lng")
			if !ok {
				return
			}
			g := sampleGate()
			g["id"], g["label"], g["lat"], g["lng"], g["verified"] = "gate_mock02", body["label"], body["lat"], body["lng"], true
			g["entry_notes"] = body["entry_notes"]
			c.json(201, map[string]any{"gate": g})
		}},
		{method: "GET", path: "/locations/check", handle: func(c *call) {
			lat, err1 := strconv.ParseFloat(c.r.URL.Query().Get("lat"), 64)
			lng, err2 := strconv.ParseFloat(c.r.URL.Query().Get("lng"), 64)
			if err1 != nil || err2 != nil || lat < -90 || lat > 90 || lng < -180 || lng > 180 {
				c.fail(400, "invalid_request", "Send lat and lng.", false)
				return
			}
			if c.scenario == "out_of_service_area" {
				c.json(200, map[string]any{"in_service_area": false, "zone_id": nil})
				return
			}
			c.json(200, map[string]any{"in_service_area": true, "zone_id": "zone_mock01"})
		}},
		{method: "GET", path: "/menu/items", handle: func(c *call) {
			c.json(200, map[string]any{"items": []map[string]any{menuItem("item_burger", "Burger", 14900), menuItem("item_chai", "Masala chai", 4900)}})
		}},
		{method: "GET", path: "/menu/items/{item_id}", handle: func(c *call) {
			switch c.param("item_id") {
			case "item_missing":
				c.fail(404, "not_found", "No such item.", false)
			case "item_chai":
				c.json(200, map[string]any{"item": menuItem("item_chai", "Masala chai", 4900)})
			default:
				c.json(200, map[string]any{"item": menuItem("item_burger", "Burger", 14900)})
			}
		}},
		{method: "GET", path: "/availability", handle: availability},
		{method: "POST", path: "/availability/notify", handle: func(c *call) {
			if phoneRequired(c) {
				return
			}
			if _, ok := needsBody(c, "gate_id"); ok {
				c.json(201, map[string]any{})
			}
		}},
		{method: "GET", path: "/orders", handle: func(c *call) {
			c.json(200, map[string]any{"orders": []map[string]any{currentOrder("handed_over", c.now)}, "next_cursor": nil})
		}},
		{method: "POST", path: "/orders", handle: func(c *call) {
			if phoneRequired(c) {
				return
			}
			createOrder(c)
		}},
		{method: "POST", path: "/test/payments/{order_id}", noIdempotent: true, handle: func(c *call) {
			body, ok := needsBody(c, "result")
			if !ok {
				return
			}
			o := currentOrder(c.scenario, c.now)
			if body["result"] == "failed" {
				o["customer_stage"], o["payment_status"] = "payment_failed", "failed"
			} else {
				o["customer_stage"], o["payment_status"] = "looking_for_rider", "paid"
			}
			c.json(200, map[string]any{"order": o})
		}},
		{method: "GET", path: "/orders/{order_id}", handle: func(c *call) {
			if c.param("order_id") == "ord_missing" {
				c.fail(404, "not_found", "No such order.", false)
				return
			}
			c.json(200, map[string]any{"order": currentOrder(c.scenario, c.now)})
		}},
		{method: "POST", path: "/orders/{order_id}/cancel", handle: func(c *call) {
			order := currentOrder(c.scenario, c.now)
			if order["ended_reason"] == "cancelled" {
				c.json(200, map[string]any{"order": order})
				return
			}
			if order["cancellable"] != true {
				c.fail(409, "cancel_not_allowed", "This order cannot be cancelled now.", false)
				return
			}
			c.json(200, map[string]any{"order": cancelledFrom(order)})
		}},
		{method: "PATCH", path: "/orders/{order_id}/gate", handle: func(c *call) {
			order := currentOrder(c.scenario, c.now)
			if order["customer_stage"] != "payment" {
				c.fail(409, "gate_change_not_allowed", "The address cannot change after payment.", false)
				return
			}
			if _, ok := needsBody(c, "gate_id"); ok {
				c.json(200, map[string]any{"order": order, "payment": paymentRequest()})
			}
		}},
		{method: "POST", path: "/orders/{order_id}/payment/retry", handle: func(c *call) {
			order := currentOrder(c.scenario, c.now)
			if order["customer_stage"] != "payment_failed" {
				c.fail(409, "payment_retry_not_allowed", "This order is not at payment_failed.", false)
				return
			}
			next := currentOrder("awaiting_payment", c.now)
			next["seq"] = order["seq"].(int) + 1
			c.json(200, map[string]any{"order": next, "payment": paymentRequest()})
		}},
		{method: "POST", path: "/orders/{order_id}/accept-eta", handle: func(c *call) {
			order := currentOrder(c.scenario, c.now)
			if order["customer_stage"] != "no_scooter" {
				c.fail(409, "eta_offer_not_available", "There is no longer ETA on offer.", false)
				return
			}
			if c.scenario == "eta_offer_expired" {
				c.fail(409, "eta_offer_expired", "This offer has expired.", false)
				return
			}
			if _, ok := needsBody(c, "eta_offer_id"); ok {
				next := currentOrder("after_cancel_window", c.now)
				next["seq"] = order["seq"].(int) + 1
				next["wait_deadline"] = stamp(c.now.Add(10 * time.Minute))
				c.json(200, map[string]any{"order": next})
			}
		}},
		{method: "POST", path: "/orders/{order_id}/call", handle: func(c *call) {
			if currentOrder(c.scenario, c.now)["rider"] == nil {
				c.fail(409, "call_not_available", "Calling is available from rider_accepted.", false)
				return
			}
			c.json(200, map[string]any{"phone": "+911140000000", "expires_at": stamp(c.now.Add(5 * time.Minute))})
		}},
		{method: "GET", path: "/orders/{order_id}/refund", handle: func(c *call) {
			reason := storyFor(c.scenario).refund
			if reason == "" {
				c.fail(404, "not_found", "This order has no refund.", false)
				return
			}
			c.json(200, map[string]any{"refund": map[string]any{
				"reason_code": reason, "method": "original_payment_method", "status": "pending",
				"amount": money(14900), "expected_by": stamp(c.now.Add(5 * 24 * time.Hour)),
			}})
		}},
		{method: "POST", path: "/support/tickets", handle: func(c *call) {
			if phoneRequired(c) {
				return
			}
			if _, ok := needsBody(c, "type", "text"); ok {
				c.json(201, map[string]any{"ticket": sampleTicket(c.now)})
			}
		}},
		{method: "GET", path: "/support/tickets/{ticket_id}", handle: func(c *call) {
			c.json(200, map[string]any{"ticket": sampleTicket(c.now)})
		}},
		{method: "POST", path: "/webhooks/razorpay", open: true, noIdempotent: true, handle: func(c *call) {
			if c.r.Header.Get("X-Razorpay-Signature") == "" {
				c.fail(401, "unauthorized", "Missing signature.", false)
				return
			}
			c.json(200, map[string]any{})
		}},
	}
}

func sampleTicket(now time.Time) map[string]any {
	return map[string]any{"id": "tkt_mock01", "order_id": orderID, "status": "open", "messages": []map[string]any{
		{"from": "customer", "text": "My food was cold.", "sent_at": stamp(now.Add(-time.Minute))},
	}}
}

func otpRequest(c *call) {
	switch c.scenario {
	case "otp_resend_limit":
		c.fail(429, "otp_resend_limit", "Too many codes requested.", true)
		return
	case "rate_limited":
		c.fail(429, "rate_limited", "Too many requests.", true)
		return
	}
	if _, ok := needsBody(c, "phone"); ok {
		c.json(200, map[string]any{"request_id": "otp_mock01", "expires_in_s": 300, "resend_after_s": 30})
	}
}

func otpVerify(c *call, session map[string]any) {
	body, ok := needsBody(c, "request_id", "otp")
	if !ok {
		return
	}
	switch {
	case c.scenario == "invalid_otp" || body["otp"] != TestOTP:
		c.fail(400, "invalid_otp", "That code is wrong.", false)
	case c.scenario == "otp_expired":
		c.fail(400, "otp_expired", "That code has expired.", false)
	case c.scenario == "account_blocked":
		accountBlocked(c)
	default:
		c.json(200, sessionReply(session))
	}
}

func refresh(c *call) {
	body, ok := needsBody(c, "refresh_token")
	if !ok {
		return
	}
	if body["refresh_token"] == "mock-revoked-token" {
		c.fail(401, "unauthorized", "Sign in again.", false)
		return
	}
	c.json(200, map[string]any{"access_token": "mock-access-token", "refresh_token": "mock-refresh-token", "expires_in_s": 900})
}

func registerDevice(c *call) {
	if _, ok := needsBody(c, "platform", "push_token"); ok {
		c.json(201, map[string]any{"device_id": "dev_mock01"})
	}
}

func availability(c *call) {
	q := c.r.URL.Query()
	_, latErr := strconv.ParseFloat(q.Get("lat"), 64)
	_, lngErr := strconv.ParseFloat(q.Get("lng"), 64)
	pin := q.Get("lat") != "" || q.Get("lng") != ""
	if (q.Get("gate_id") != "" && pin) || (q.Get("gate_id") == "" && (latErr != nil || lngErr != nil)) {
		c.fail(400, "invalid_request", "Send gate_id, or lat and lng, not both.", false)
		return
	}
	body := map[string]any{"offer_available": true, "unavailable_reason": nil, "stale_at": stamp(c.now.Add(30 * time.Second))}
	reason := map[string]string{"no_scooter": "no_coverage", "no_scooter_no_offer": "no_coverage", "zone_offer_off": "zone_off", "gate_not_verified": "gate_not_verified"}[c.scenario]
	switch {
	case reason != "":
		body["offer_available"], body["unavailable_reason"], body["items"] = false, reason, []map[string]any{}
	case c.scenario == "last_unit_taken":
		body["items"] = []map[string]any{{"item": menuItem("item_burger", "Burger", 14900), "qty_left": 1}}
	default:
		body["items"] = []map[string]any{{"item": menuItem("item_burger", "Burger", 14900), "qty_left": 4}, {"item": menuItem("item_chai", "Masala chai", 4900), "qty_left": 7}}
	}
	c.json(200, body)
}

func createOrder(c *call) {
	switch c.scenario {
	case "last_unit_taken":
		c.fail(409, "sold_out", "The last unit was taken.", false)
		return
	case "gate_not_verified":
		c.fail(409, "gate_not_verified", "This gate is not verified.", false)
		return
	case "zone_offer_off":
		c.fail(409, "no_offer", "The offer is off in this zone.", false)
		return
	case "server_error":
		c.fail(500, "internal_error", "A sample server error.", true)
		return
	case "service_unavailable":
		c.fail(503, "service_unavailable", "A sample outage.", true)
		return
	}
	if _, ok := needsBody(c, "gate_id", "items"); ok {
		c.json(201, map[string]any{"order": currentOrder("awaiting_payment", c.now), "payment": paymentRequest()})
	}
}

// sessionReply is a signed-in reply: the sample tokens plus the role's own fields.
func sessionReply(fields map[string]any) map[string]any {
	reply := map[string]any{"access_token": "mock-access-token", "refresh_token": "mock-refresh-token", "expires_in_s": 900}
	for k, v := range fields {
		reply[k] = v
	}
	return reply
}

func accountBlocked(c *call) {
	c.json(403, map[string]any{"code": "account_blocked", "message": "This account is blocked.", "retryable": false, "missing_permission": "account_active"})
}

func googleSignIn(c *call) {
	if _, ok := needsBody(c, "id_token"); !ok {
		return
	}
	switch c.scenario {
	case "invalid_google_token":
		c.fail(401, "invalid_google_token", "Google could not confirm this sign-in.", false)
	case "account_blocked":
		accountBlocked(c)
	default:
		c.json(200, sessionReply(map[string]any{"customer_id": "cus_mock0001", "phone_verified": c.scenario != "phone_not_verified"}))
	}
}

func verifyPhone(c *call) {
	switch c.scenario {
	case "phone_already_registered":
		if _, ok := needsBody(c, "request_id", "otp"); ok {
			c.fail(409, "phone_already_registered", "This number already has an account. Log in with phone and OTP.", false)
		}
	case "phone_already_set":
		if _, ok := needsBody(c, "request_id", "otp"); ok {
			c.fail(409, "phone_already_set", "This account already has a different verified phone.", false)
		}
	default:
		otpVerify(c, map[string]any{"customer_id": "cus_mock0001", "phone_verified": true})
	}
}

// mockMethod is the customer's sign-in method. Only a Google customer can have no phone.
func mockMethod(c *call) string {
	if c.scenario == "phone_not_verified" || c.scenario == "sign_in_method_google" {
		return "google"
	}
	return "phone"
}

func changeSignInMethod(c *call) {
	body, ok := needsBody(c, "method")
	if !ok {
		return
	}
	switch body["method"] {
	case "google":
		if v, has := body["id_token"]; !has || v == "" {
			c.fail(400, "invalid_request", "Send id_token.", false)
			return
		}
	case "phone":
		if body["request_id"] == nil || body["otp"] == nil {
			c.fail(400, "invalid_request", "Send request_id and otp.", false)
			return
		}
	default:
		c.fail(400, "invalid_request", "Send a sign-in method that is on.", false)
		return
	}
	switch c.scenario {
	case "sign_in_method_unchanged":
		c.fail(409, "sign_in_method_unchanged", "This account already uses that sign-in method.", false)
	case "google_account_in_use":
		c.fail(409, "google_account_in_use", "This Google account is used by another account.", false)
	case "phone_not_verified":
		c.fail(409, "phone_required", "Verify a phone first.", false)
	case "phone_already_set":
		c.fail(409, "phone_already_set", "This account has a different verified phone.", false)
	case "invalid_google_token":
		c.fail(401, "invalid_google_token", "Google could not confirm this sign-in.", false)
	case "account_blocked":
		accountBlocked(c)
	default:
		c.json(200, sessionReply(map[string]any{"customer_id": "cus_mock0001", "phone_verified": true}))
	}
}

func phoneRequired(c *call) bool {
	if c.scenario != "phone_not_verified" {
		return false
	}
	c.fail(409, "phone_required", "Verify a phone first.", false)
	return true
}
