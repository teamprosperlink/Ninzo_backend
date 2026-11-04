package mockserver

import (
	"net/http"
	"time"
)

// customerScenarios lists every scenario, with what it shows. The first group comes
// from the customer KT doc. The rest come from the owner's flow in decision 0007.
var customerScenarios = map[string]string{
	"no_scooter":          "No scooter available. GET order is at no_scooter with a longer ETA on offer. Availability offers nothing",
	"no_scooter_no_offer": "No scooter available, and no longer ETA on offer",
	"offer_timeout":       "No rider took the offer in time: GET order is at no_scooter",
	"offer_accepted":      "A rider was accepted: GET order is at rider_accepted",
	"payment_declined":    "GET order is at payment_failed, so the customer can retry",
	"payment_captured":    "Payment confirmed, inside the 5-second cancel window",
	"handed_over":         "GET order is at handed_over",
	"socket_drop":         "The stream drops after two events. GET order still works",
	"last_unit_taken":     "POST order fails with sold_out, and availability shows one left",
	"gate_not_verified":   "POST order fails with gate_not_verified. Availability offers nothing",
	"zone_offer_off":      "POST order fails with no_offer. Availability offers nothing",

	"after_cancel_window":    "Payment confirmed, and the 5-second window has passed, so cancel is refused",
	"cancel_too_late":        "POST cancel fails with cancel_not_allowed",
	"max_wait_passed":        "wait_deadline has passed, so the customer can leave with a refund",
	"rider_already_near":     "The rider was already in the zone. The stream has a shorter wait",
	"rider_inside_perimeter": "The rider was already inside 100 m, so there is no timer: rider_accepted, then arrived",
	"timer_running":          "GET order is at timer_running",
	"timer_missed":           "The timer ended before 100 m. timer_missed is true, with a credit remedy",
	"signal_lost":            "The timer ended, and the rider's signal is lost, so the customer has the OTP",
	"arrived":                "GET order is at arrived, with the handover OTP",
	"cancelled":              "GET order has ended, cancelled by the customer, and refunded",
	"timed_out":              "GET order has ended, timed out, and refunded",
	"failed":                 "GET order has ended after a failure, and refunded",
	"gate_change_refused":    "PATCH gate fails with gate_change_not_allowed",
	"eta_offer_expired":      "POST accept-eta fails with eta_offer_expired",
	"payment_retry_refused":  "POST payment retry fails with payment_retry_not_allowed",
	"no_refund":              "GET refund returns 404",
	"invalid_otp":            "POST verify fails with invalid_otp",
	"otp_expired":            "POST verify fails with otp_expired",
	"otp_resend_limit":       "POST OTP request fails with otp_resend_limit",
	"out_of_service_area":    "POST gate fails with out_of_service_area",
	"duplicate_gate":         "POST gate fails with duplicate_gate",
	"call_not_available":     "POST call fails with call_not_available",
	"rate_limited":           "POST OTP request returns 429",
	"server_error":           "POST order returns a retryable 500",
	"service_unavailable":    "POST order returns a retryable 503",
}

const (
	orderID      = "ord_mock0001"
	cancelWindow = 5 * time.Second
	maxWait      = 5 * time.Minute
)

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
	return map[string]any{"id": "gate_mock01", "label": "Main gate", "lat": 12.9716, "lng": 77.5946, "entry_notes": "Near the security desk", "verified": true}
}

// customerOrder builds an order that follows the stage table in contracts/README.md.
func customerOrder(stage string, seq int, now time.Time, opts ...func(map[string]any)) map[string]any {
	paidAt := now.Add(-time.Minute)
	o := map[string]any{
		"id": orderID, "seq": seq, "customer_stage": stage, "ended_reason": nil,
		"gate":  map[string]any{"id": "gate_mock01", "label": "Main gate"},
		"items": []map[string]any{{"item_id": "item_burger", "name": "Burger", "qty": 1, "unit_price": money(14900)}},
		"total": money(14900), "payment_status": "paid", "payment_request": nil,
		"cancellable": false, "cancel_until": stamp(paidAt.Add(cancelWindow)), "wait_deadline": nil,
		"eta_source": "placeholder", "eta_s": nil, "eta_offer": nil, "rider": nil,
		"timer":  map[string]any{"timer_started_at": nil, "timer_duration_s": nil, "timer_stopped_at": nil, "timer_missed": false},
		"remedy": nil, "handover_otp": nil, "created_at": stamp(now.Add(-2 * time.Minute)),
	}
	rider := map[string]any{"first_name": "Ravi"}
	switch stage {
	case "payment":
		o["payment_status"], o["cancellable"], o["cancel_until"] = "pending", true, nil
		o["payment_request"] = paymentRequest()
	case "payment_failed":
		o["payment_status"], o["cancellable"], o["cancel_until"] = "failed", true, nil
	case "looking_for_rider":
		o["cancel_until"] = stamp(now.Add(cancelWindow))
		o["cancellable"] = true
		o["wait_deadline"] = stamp(now.Add(maxWait))
	case "no_scooter":
		o["cancellable"] = true
		o["wait_deadline"] = stamp(now.Add(2 * time.Minute))
		o["eta_offer"] = map[string]any{"id": "eta_mock01", "eta_s": 600, "expires_at": stamp(now.Add(2 * time.Minute))}
	case "rider_accepted":
		o["rider"] = rider
		o["wait_deadline"] = stamp(paidAt.Add(maxWait))
		o["eta_source"], o["eta_s"] = "measured", 150
	case "timer_running":
		o["rider"] = rider
		o["eta_source"], o["eta_s"] = "measured", 60
		o["timer"] = map[string]any{"timer_started_at": stamp(now.Add(-10 * time.Second)), "timer_duration_s": 90, "timer_stopped_at": nil, "timer_missed": false}
	case "arrived":
		o["rider"] = rider
		o["eta_source"] = "unavailable"
		o["handover_otp"] = "4821"
		o["timer"] = map[string]any{"timer_started_at": stamp(now.Add(-70 * time.Second)), "timer_duration_s": 90, "timer_stopped_at": stamp(now.Add(-5 * time.Second)), "timer_missed": false}
	case "handed_over":
		o["rider"] = rider
		o["eta_source"] = "unavailable"
		o["timer"] = map[string]any{"timer_started_at": stamp(now.Add(-3 * time.Minute)), "timer_duration_s": 90, "timer_stopped_at": stamp(now.Add(-2 * time.Minute)), "timer_missed": false}
	case "ended":
		o["eta_source"] = "unavailable"
		o["ended_reason"] = "cancelled"
		o["payment_status"] = "refunded"
	}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

func paymentRequest() map[string]any {
	return map[string]any{"razorpay_key_id": "rzp_test_mockkey", "razorpay_order_id": "order_mock0001", "amount": money(14900)}
}

// orderForScenario picks the order GET returns.
func orderForScenario(scenario string, now time.Time) map[string]any {
	switch scenario {
	case "payment_declined":
		return customerOrder("payment_failed", 2, now)
	case "no_scooter", "offer_timeout":
		return customerOrder("no_scooter", 3, now)
	case "no_scooter_no_offer":
		return customerOrder("no_scooter", 3, now, func(o map[string]any) { o["eta_offer"] = nil })
	case "offer_accepted", "rider_already_near":
		return customerOrder("rider_accepted", 3, now)
	case "after_cancel_window":
		return customerOrder("looking_for_rider", 2, now, func(o map[string]any) {
			o["cancellable"] = false
			o["cancel_until"] = stamp(now.Add(-time.Minute))
		})
	case "max_wait_passed":
		return customerOrder("looking_for_rider", 3, now, func(o map[string]any) {
			o["cancel_until"] = stamp(now.Add(-6 * time.Minute))
			o["wait_deadline"] = stamp(now.Add(-time.Minute))
		})
	case "timer_running":
		return customerOrder("timer_running", 4, now)
	case "timer_missed":
		return customerOrder("timer_running", 5, now, func(o map[string]any) {
			o["eta_source"], o["eta_s"] = "unavailable", nil
			o["timer"] = map[string]any{"timer_started_at": stamp(now.Add(-2 * time.Minute)), "timer_duration_s": 90, "timer_stopped_at": nil, "timer_missed": true}
			o["remedy"] = map[string]any{"type": "credit", "amount": money(5000)}
		})
	case "signal_lost":
		return customerOrder("timer_running", 5, now, func(o map[string]any) {
			o["eta_source"], o["eta_s"] = "unavailable", nil
			o["timer"] = map[string]any{"timer_started_at": stamp(now.Add(-2 * time.Minute)), "timer_duration_s": 90, "timer_stopped_at": nil, "timer_missed": true}
			o["handover_otp"] = "4821"
			o["remedy"] = map[string]any{"type": "credit", "amount": money(5000)}
		})
	case "arrived":
		return customerOrder("arrived", 5, now)
	case "rider_inside_perimeter":
		return customerOrder("arrived", 4, now, func(o map[string]any) {
			o["timer"] = map[string]any{"timer_started_at": nil, "timer_duration_s": nil, "timer_stopped_at": nil, "timer_missed": false}
		})
	case "handed_over":
		return customerOrder("handed_over", 6, now)
	case "cancelled":
		return customerOrder("ended", 3, now)
	case "timed_out":
		return customerOrder("ended", 4, now, func(o map[string]any) { o["ended_reason"] = "timed_out" })
	case "failed":
		return customerOrder("ended", 5, now, func(o map[string]any) {
			o["ended_reason"] = "failed"
			o["rider"] = map[string]any{"first_name": "Ravi"}
		})
	}
	return customerOrder("looking_for_rider", 2, now)
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
			otpVerify(c, "customer_id", "cus_mock0001")
		}},
		{method: "POST", path: "/auth/refresh", open: true, noIdempotent: true, handle: refresh},
		{method: "GET", path: "/me", handle: func(c *call) {
			c.json(200, map[string]any{"customer": map[string]any{"id": "cus_mock0001", "phone": "+919876543210", "name": "Test Customer"}})
		}},
		{method: "PATCH", path: "/me", handle: func(c *call) {
			body, ok := needsBody(c, "name")
			if !ok {
				return
			}
			c.json(200, map[string]any{"customer": map[string]any{"id": "cus_mock0001", "phone": "+919876543210", "name": body["name"]}})
		}},
		{method: "POST", path: "/me/devices", handle: registerDevice},
		{method: "DELETE", path: "/me/devices/{device_id}", handle: func(c *call) { c.json(200, map[string]any{}) }},
		{method: "GET", path: "/gates", handle: func(c *call) {
			c.json(200, map[string]any{"gates": []map[string]any{sampleGate()}})
		}},
		{method: "POST", path: "/gates", handle: func(c *call) {
			switch c.scenario {
			case "out_of_service_area":
				c.fail(400, "out_of_service_area", "This pin is outside the service area.", false)
				return
			case "duplicate_gate":
				c.fail(409, "duplicate_gate", "This gate is already saved.", false)
				return
			}
			body, ok := needsBody(c, "label", "lat", "lng")
			if !ok {
				return
			}
			g := sampleGate()
			g["id"], g["label"], g["lat"], g["lng"], g["verified"] = "gate_mock02", body["label"], body["lat"], body["lng"], false
			g["entry_notes"] = body["entry_notes"]
			c.json(201, map[string]any{"gate": g})
		}},
		{method: "GET", path: "/menu/items", handle: func(c *call) {
			c.json(200, map[string]any{"items": []map[string]any{menuItem("item_burger", "Burger", 14900), menuItem("item_chai", "Masala chai", 4900)}})
		}},
		{method: "GET", path: "/menu/items/{item_id}", handle: func(c *call) {
			switch c.param("item_id") {
			case "item_burger":
				c.json(200, map[string]any{"item": menuItem("item_burger", "Burger", 14900)})
			case "item_chai":
				c.json(200, map[string]any{"item": menuItem("item_chai", "Masala chai", 4900)})
			default:
				c.fail(404, "not_found", "No such item.", false)
			}
		}},
		{method: "GET", path: "/availability", handle: availability},
		{method: "POST", path: "/availability/notify", handle: func(c *call) {
			if _, ok := needsBody(c, "gate_id"); ok {
				c.json(201, map[string]any{})
			}
		}},
		{method: "GET", path: "/orders", handle: func(c *call) {
			c.json(200, map[string]any{"orders": []map[string]any{customerOrder("handed_over", 6, c.now)}, "next_cursor": nil})
		}},
		{method: "POST", path: "/orders", handle: createOrder},
		{method: "GET", path: "/orders/{order_id}", handle: func(c *call) {
			if c.param("order_id") == "ord_missing" {
				c.fail(404, "not_found", "No such order.", false)
				return
			}
			c.json(200, map[string]any{"order": orderForScenario(c.scenario, c.now)})
		}},
		{method: "POST", path: "/orders/{order_id}/cancel", handle: func(c *call) {
			if c.scenario == "cancel_too_late" || c.scenario == "after_cancel_window" {
				c.fail(409, "cancel_not_allowed", "The cancel window has closed.", false)
				return
			}
			c.json(200, map[string]any{"order": customerOrder("ended", 3, c.now)})
		}},
		{method: "PATCH", path: "/orders/{order_id}/gate", handle: func(c *call) {
			if c.scenario == "gate_change_refused" {
				c.fail(409, "gate_change_not_allowed", "The address cannot change after payment.", false)
				return
			}
			if _, ok := needsBody(c, "gate_id"); ok {
				c.json(200, map[string]any{"order": customerOrder("payment", 1, c.now), "payment": paymentRequest()})
			}
		}},
		{method: "POST", path: "/orders/{order_id}/payment/retry", handle: func(c *call) {
			if c.scenario == "payment_retry_refused" {
				c.fail(409, "payment_retry_not_allowed", "This order is not at payment_failed.", false)
				return
			}
			c.json(200, map[string]any{"order": customerOrder("payment", 3, c.now), "payment": paymentRequest()})
		}},
		{method: "POST", path: "/orders/{order_id}/accept-eta", handle: func(c *call) {
			if c.scenario == "eta_offer_expired" {
				c.fail(409, "eta_offer_expired", "This offer has expired.", false)
				return
			}
			if _, ok := needsBody(c, "eta_offer_id"); ok {
				c.json(200, map[string]any{"order": customerOrder("looking_for_rider", 4, c.now, func(o map[string]any) {
					o["cancellable"] = false
					o["cancel_until"] = stamp(c.now.Add(-time.Minute))
					o["wait_deadline"] = stamp(c.now.Add(10 * time.Minute))
				})})
			}
		}},
		{method: "POST", path: "/orders/{order_id}/call", handle: func(c *call) {
			if c.scenario == "call_not_available" {
				c.fail(409, "call_not_available", "Calling is available from rider_accepted.", false)
				return
			}
			c.json(200, map[string]any{"phone": "+911140000000", "expires_at": stamp(c.now.Add(5 * time.Minute))})
		}},
		{method: "GET", path: "/orders/{order_id}/refund", handle: func(c *call) {
			if c.scenario == "no_refund" {
				c.fail(404, "not_found", "This order has no refund.", false)
				return
			}
			c.json(200, map[string]any{"refund": map[string]any{
				"reason_code": "customer_cancelled", "method": "original_payment_method", "status": "pending",
				"amount": money(14900), "expected_by": stamp(c.now.Add(5 * 24 * time.Hour)),
			}})
		}},
		{method: "POST", path: "/support/tickets", handle: func(c *call) {
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

func otpVerify(c *call, idField, id string) {
	body, ok := needsBody(c, "request_id", "otp")
	if !ok {
		return
	}
	switch {
	case c.scenario == "invalid_otp" || body["otp"] != TestOTP:
		c.fail(400, "invalid_otp", "That code is wrong.", false)
	case c.scenario == "otp_expired":
		c.fail(400, "otp_expired", "That code has expired.", false)
	default:
		c.json(200, map[string]any{"access_token": "mock-access-token", "refresh_token": "mock-refresh-token", "expires_in_s": 900, idField: id})
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
	if c.r.URL.Query().Get("gate_id") == "" {
		c.fail(400, "invalid_request", "Missing gate_id.", false)
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
		c.json(201, map[string]any{"order": customerOrder("payment", 1, c.now), "payment": paymentRequest()})
	}
}
