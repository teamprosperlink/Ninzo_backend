package mockserver

import (
	"fmt"
	"math"
	"slices"
	"time"
)

// consoleScenarios lists every console scenario, from the ops console KT doc and decision 0014.
var consoleScenarios = map[string]string{
	"invalid_credentials":       "Login fails with invalid_credentials",
	"account_locked":            "The account is locked after repeated failures: login answers invalid_credentials, as for a wrong password",
	"totp_not_enrolled":         "Login says totp_enrolled false, so the console sets up the authenticator first",
	"must_change_password":      "Login and the session say must_change_password, after a temporary password",
	"invalid_totp":              "The authenticator code fails with invalid_totp",
	"challenge_expired":         "The challenge has expired: enroll and verify fail with challenge_expired",
	"no_permission":             "Every admin call returns 403 with the missing permission",
	"role_ops":                  "The signed-in user is ops, not admin",
	"role_hub_manager":          "The signed-in user is a hub_manager for one hub",
	"approval_pending":          "Settings, kill-switch, flag and refund requests fail with approval_pending; the list shows one waiting",
	"own_change":                "Approving fails with own_change: the same person asked for it",
	"kill_switch_off":           "A zone's offer is off",
	"rider_verification_failed": "The rider review queue has a rider whose verification failed",
	"rider_not_verified":        "Approving a rider fails with not_verified",
	"login_id_taken":            "Adding a user fails with login_id_taken",
	"password_only":             "Login signs in with the password alone (totp_required false, with session), as on staging until pre-launch",
	"last_admin":                "Demoting or deactivating fails with last_admin",
	"name_taken":                "Adding a menu item or hub fails with name_taken",
	"outside_zone":              "A gate edit fails with outside_zone",
	"phone_taken":               "Adding hub staff fails with phone_taken",
	"review_closed":             "Deciding a review fails with review_closed",
	"audit_empty":               "The audit log has nothing for the filters",
	"invalid_setting":           "A settings change fails with invalid_setting",
	"idempotency_in_progress":   "Any write with an Idempotency-Key returns a retryable 409, with Retry-After",
	"password_change_required":  "Every admin call returns 403 password_change_required until the password is changed",
	"not_pending":               "Approve, reject and cancel fail with not_pending",
	"approval_failed":           "The approval list shows one that failed with stale_version",
	"stale_version":             "A settings change fails with stale_version: someone changed them since",
	"totp_attempts_exceeded":    "The authenticator code fails with totp_attempts_exceeded",
	"not_allowed":               "A rider decision fails with not_allowed",
	"rate_limited":              "Sign-in fails with rate_limited",
	"account_locked_user":       "The user list shows a user locked after failed sign-ins",
	"orders_empty":              "The order search finds nothing",
	"refund_needs_approval":     "A refund of any size waits for a second admin: 202 with a pending approval",
	"refund_too_large":          "A refund fails with refund_too_large: more than was paid less earlier refunds",
	"not_paid":                  "A refund, replacement or credit fails with not_paid",
	"item_not_on_order":         "A replacement fails with item_not_on_order",
	"ticket_closed":             "Replying to or closing a ticket fails with ticket_closed",
	"incident_closed":           "Assigning, noting or closing an incident fails with incident_closed",
	"restock_not_expirable":     "Expiring a restock request fails with restock_not_expirable: it is already ready or past it",
	"remedy_exists":             "A credit or replacement fails with remedy_exists: the order already has one of that type",
	"source_open":               "Closing an incident fails with source_open: its ticket or arrival review is still open",
	"user_cannot_take":          "Assigning an incident fails with user_cannot_take: the user is inactive or lacks incidents.manage",
	"poll_too_fast":             "The live zone answers 429 rate_limited: polled faster than every 5 seconds",
}

const consoleUserID = "usr_mock01"

func consoleUser(role string, now time.Time) map[string]any {
	var hub any
	if role == "hub_manager" {
		hub = hubID
	}
	return map[string]any{"id": consoleUserID, "login_id": "asha.k", "name": "Asha K", "role": role, "hub_id": hub, "active": true, "totp_enrolled": true,
		"locked_until": nil, "created_at": stamp(now.Add(-30 * 24 * time.Hour))}
}

// rolePermissions is decision 0015's table.
var rolePermissions = map[string][]string{
	"admin": {"users.manage", "approvals.read", "approvals.decide", "riders.review", "flags.manage", "zones.read", "settings.read", "settings.request",
		"zones.kill_switch", "menu.manage", "gates.read", "gates.manage", "hubs.read", "hubs.manage", "hub_staff.manage", "audit.read", "reviews.read",
		"reviews.decide", "alerts.read", "orders.read", "tickets.manage", "refunds.request", "remedies.issue", "incidents.read", "incidents.manage",
		"hub_ops.read", "hub_ops.manage", "map.read"},
	"ops": {"approvals.read", "riders.review", "zones.read", "settings.read", "settings.request", "zones.kill_switch", "menu.manage", "gates.read",
		"gates.manage", "hubs.read", "hubs.manage", "hub_staff.manage", "reviews.read", "reviews.decide", "alerts.read", "orders.read", "remedies.issue",
		"incidents.read", "incidents.manage", "hub_ops.read", "hub_ops.manage", "map.read"},
	"support": {"zones.read", "gates.read", "reviews.read", "reviews.decide", "alerts.read", "orders.read", "tickets.manage", "refunds.request",
		"remedies.issue", "incidents.read", "incidents.manage", "map.read"},
	"hub_manager": {"hubs.read", "hub_staff.manage", "hub_ops.read"},
}

func userReply(role string, now time.Time) map[string]any {
	return map[string]any{"user": consoleUser(role, now), "permissions": rolePermissions[role]}
}

func consoleRole(c *call) string {
	switch c.scenario {
	case "role_ops":
		return "ops"
	case "role_hub_manager":
		return "hub_manager"
	}
	return "admin"
}

func sampleApproval(kind, status string, now time.Time) map[string]any {
	var by, at, why, failure any
	switch status {
	case "approved", "rejected", "failed":
		by, at = map[string]any{"id": "usr_mock02", "name": "Ravi M"}, stamp(now)
	}
	if status == "rejected" {
		why = "Not during rush hour"
	}
	if status == "failed" {
		failure = "stale_version"
	}
	change := map[string]any{
		"settings":    map[string]any{"kind": "settings", "zone_id": "zone_mock01", "base_version": 3, "diffs": []map[string]any{{"key": "timer_length_s", "before": 90, "after": 120}}},
		"kill_switch": map[string]any{"kind": "kill_switch", "zone_id": "zone_mock01", "offer_enabled_before": true, "offer_enabled_after": false},
		"flag":        map[string]any{"kind": "flag", "key": "eta_offers", "enabled_before": false, "enabled_after": true},
		"refund":      map[string]any{"kind": "refund", "order_id": orderID, "amount": money(30000)},
	}[kind]
	summary := map[string]string{"settings": "Koramangala: timer_length_s 90 to 120", "kill_switch": "Koramangala: turn the 90-second offer off",
		"flag": "eta_offers: on", "refund": "Order ord_mock0001: refund 30000 paise"}[kind]
	return map[string]any{"id": "apr_mock01", "kind": kind, "status": status, "summary": summary, "change": change,
		"requested_by": map[string]any{"id": consoleUserID, "name": "Asha K"}, "requested_at": stamp(now.Add(-5 * time.Minute)), "request_reason": "Rush hour in the zone",
		"decided_by": by, "decided_at": at, "decision_reason": why, "failure_code": failure, "expires_at": stamp(now.Add(24 * time.Hour))}
}

func sampleSettings(zone any) map[string]any {
	settings := []map[string]any{
		{"key": "timer_length_s", "unit": "seconds", "min": 30, "max": 600, "value": 90, "effective": 90, "required": true, "description": "How long the visible timer runs"},
		{"key": "zone_radius_m", "unit": "metres", "min": 50, "max": 5000, "value": nil, "effective": 350, "required": true, "description": "The zone around the customer where the timer can start"},
		{"key": "pay_per_order_paise", "unit": "paise", "min": 0, "max": 100000, "value": 4000, "effective": 4000, "required": true, "description": "What a rider earns for each order handed over"},
	}
	var zoneVersion any
	if zone != nil {
		zoneVersion = 2
	}
	return map[string]any{"zone_id": zone, "global_version": 3, "zone_version": zoneVersion, "settings": settings}
}

func sampleRiderReview(verification string, now time.Time) map[string]any {
	return map[string]any{"rider_id": "rdr_mock0002", "name": "Kiran", "phone_last4": "4321", "status": "applicant", "verification": verification,
		"zone_id": nil, "applied_at": stamp(now.Add(-2 * time.Hour))}
}

func adminMenuItem(id, name string, price int, status string, isVeg bool, now time.Time) map[string]any {
	return map[string]any{"id": id, "name": name, "description": nil, "price": money(price), "status": status, "sort_order": 1, "allergens": []string{},
		"is_veg": isVeg, "photo_url": nil, "updated_at": stamp(now)}
}

func adminGate(version int, now time.Time) map[string]any {
	return map[string]any{"id": "gate_mock01", "zone_id": "zone_mock01", "label": "Tower B main gate", "lat": 12.9352, "lng": 77.6245,
		"entry_notes": "Ask the guard for the delivery lane", "verified": true, "version": version, "updated_at": stamp(now)}
}

func adminHub(status string) map[string]any {
	return map[string]any{"id": hubID, "zone_id": "zone_mock01", "name": "Koramangala hub", "lat": 12.937, "lng": 77.626, "status": status, "staff_count": 2, "is_test": false}
}

func hubStaffMember(role, status string) map[string]any {
	return map[string]any{"id": "stf_mock01", "hub_id": hubID, "name": "Asha", "phone_last4": "0091", "role": role, "status": status}
}

func adminOrderSummary(number, id, state string, now time.Time) map[string]any {
	var ended any
	if state == "COMPLETED" {
		ended = "handed_over"
	}
	return map[string]any{"id": id, "order_number": number, "state": state, "zone_id": "zone_mock01", "customer": map[string]any{"id": "cus_mock0001", "phone_last4": "4321"},
		"rider": map[string]any{"id": "rdr_mock0001", "name": "Ravi"}, "total": money(14900), "created_at": stamp(now.Add(-20 * time.Minute)), "ended_reason": ended}
}

func adminRemedy(kind string, now time.Time) map[string]any {
	var amount, items any
	if kind == "credit" {
		amount = money(5000)
	} else {
		items = []map[string]any{{"item_id": "item_burger", "qty": 1}}
	}
	return map[string]any{"id": "rem_mock01", "kind": kind, "amount": amount, "items": items, "status": "owed", "reason": "Cold food",
		"created_by": map[string]any{"id": consoleUserID, "name": "Asha K"}, "created_at": stamp(now)}
}

func adminOrder(id string, now time.Time) map[string]any {
	o := adminOrderSummary("A1001", id, "COMPLETED", now)
	o["gate"] = map[string]any{"label": "Tower B main gate", "lat": 12.9352, "lng": 77.6245}
	o["items"] = []map[string]any{{"item_id": "item_burger", "name": "Burger", "qty": 1, "unit_price": money(14900)}}
	o["timer"] = map[string]any{"timer_started_at": stamp(now.Add(-12 * time.Minute)), "timer_stopped_at": stamp(now.Add(-11 * time.Minute)), "timer_duration_s": 90,
		"perimeter_m": 100, "timer_missed": false, "arrival_source": "handover"}
	o["remedies"] = []map[string]any{adminRemedy("credit", now)}
	o["refunds"] = []map[string]any{{"id": "rfd_mock01", "order_id": id, "amount": money(5000), "status": "processed", "reason": "Cold food",
		"requested_by": map[string]any{"id": consoleUserID, "name": "Asha K"}, "created_at": stamp(now.Add(-5 * time.Minute))}}
	return o
}

func adminTicket(status string, now time.Time) map[string]any {
	var closedAt any
	if status == "resolved" {
		closedAt = stamp(now)
	}
	return map[string]any{"id": "tkt_mock01", "order_id": orderID, "type": "cold_food", "status": status, "subject": "The food was cold", "opened_at": stamp(now.Add(-8 * time.Minute)),
		"closed_at": closedAt, "close_reason": nil, "messages": []map[string]any{{"id": "msg_mock01", "author_type": "customer", "author_name": nil,
			"text": "The burger arrived cold.", "created_at": stamp(now.Add(-8 * time.Minute))}}}
}

func adminIncident(kind, status string, now time.Time) map[string]any {
	var assigned, closedAt any
	if status == "assigned" || status == "closed" {
		assigned = map[string]any{"id": consoleUserID, "name": "Asha K"}
	}
	if status == "closed" {
		closedAt = stamp(now)
	}
	return map[string]any{"id": "inc_mock01", "kind": kind, "source_id": "alt_mock01", "order_id": orderID, "rider_id": "rdr_mock0001", "status": status,
		"assigned_to": assigned, "opened_at": stamp(now.Add(-6 * time.Minute)), "closed_at": closedAt, "notes_count": 1}
}

func incidentReply(kind, status string, now time.Time) map[string]any {
	return map[string]any{"incident": adminIncident(kind, status, now), "notes": []map[string]any{{"id": "inn_mock01", "text": "Called the rider; the gate was shut.",
		"author": map[string]any{"id": consoleUserID, "name": "Asha K"}, "created_at": stamp(now.Add(-2 * time.Minute))}}}
}

func adminHubOps(hub map[string]any, stuck int, busy bool) map[string]any {
	queue := map[string]any{"confirmed": 0, "hub_accepted": 0, "packing": 0, "partial": 0, "ready": 0, "picked_up": 0}
	if busy {
		queue = map[string]any{"confirmed": 1, "hub_accepted": 2, "packing": 1, "partial": 0, "ready": 1, "picked_up": 0}
	}
	return map[string]any{"hub": hub, "queue": queue, "stuck": stuck, "open_shortfalls": 1, "quarantined_lots": 0, "active_staff": 2}
}

func adminRestock(state string, waited int, ended any, now time.Time) map[string]any {
	return map[string]any{"id": "rst_mock01", "hub_id": hubID, "rider": map[string]any{"id": "rdr_mock0001", "name": "Ravi"}, "state": state, "waiting_s": waited,
		"requested_at": stamp(now.Add(-time.Duration(waited) * time.Second)), "ended_reason": ended}
}

func consoleRoutes() []route {
	denied := func(c *call) bool {
		switch c.scenario {
		case "role_ops", "role_hub_manager":
			if perm := consolePermission(c); perm != "console" && !slices.Contains(rolePermissions[consoleRole(c)], perm) {
				c.json(403, map[string]any{"code": "forbidden", "message": "Your role cannot do this.", "retryable": false, "missing_permission": perm})
				return true
			}
		case "no_permission":
			c.json(403, map[string]any{"code": "forbidden", "message": "Your role cannot do this.", "retryable": false, "missing_permission": consolePermission(c)})
			return true
		case "password_change_required":
			c.json(403, map[string]any{"code": "password_change_required", "message": "Change your temporary password first.", "retryable": false, "missing_permission": "password_changed"})
			return true
		}
		return false
	}
	fail := func(c *call, codes ...string) bool {
		for _, code := range codes {
			if c.scenario == code {
				c.fail(409, code, "Sample rejection: "+code, false)
				return true
			}
		}
		return false
	}
	reason := func(c *call) (map[string]any, bool) { return needsBody(c, "reason") }
	page := func(c *call, key string, items any) {
		c.json(200, map[string]any{key: items, "next_cursor": nil})
	}
	return []route{
		{method: "POST", path: "/auth/login", open: true, noIdempotent: true, handle: func(c *call) {
			if _, ok := needsBody(c, "login_id", "password"); !ok {
				return
			}
			switch c.scenario {
			case "invalid_credentials":
				c.fail(401, "invalid_credentials", "That email or password is wrong.", false)
				return
			case "account_locked":
				c.fail(401, "invalid_credentials", "That email or password is wrong, or the account is locked for a while.", false)
				return
			case "rate_limited":
				c.fail(429, "rate_limited", "Too many requests.", true)
				return
			}
			reply := map[string]any{"challenge_id": "chl_mock01", "totp_enrolled": c.scenario != "totp_not_enrolled", "totp_required": c.scenario != "password_only",
				"must_change_password": c.scenario == "must_change_password", "expires_in_s": 300, "session": nil}
			if c.scenario == "password_only" {
				reply["totp_enrolled"] = false
				reply["session"] = map[string]any{"access_token": "mock-access-token", "refresh_token": "mock-refresh-token", "expires_in_s": 900,
					"user_id": consoleUserID, "role": "admin", "must_change_password": false, "server_time": time.Now().UTC().Format(time.RFC3339Nano)}
			}
			c.json(200, reply)
		}},
		{method: "POST", path: "/auth/totp/enroll", open: true, noIdempotent: true, handle: func(c *call) {
			if _, ok := needsBody(c, "challenge_id"); !ok {
				return
			}
			if fail(c, "challenge_expired") {
				return
			}
			if c.scenario != "totp_not_enrolled" {
				c.fail(409, "totp_already_enrolled", "The authenticator is already set up.", false)
				return
			}
			c.json(200, map[string]any{"secret": "JBSWY3DPEHPK3PXP", "otpauth_uri": "otpauth://totp/Ninzo:asha.k?secret=JBSWY3DPEHPK3PXP&issuer=Ninzo"})
		}},
		{method: "POST", path: "/auth/totp/verify", open: true, noIdempotent: true, handle: func(c *call) {
			if _, ok := needsBody(c, "challenge_id", "code"); !ok {
				return
			}
			switch c.scenario {
			case "invalid_totp":
				c.fail(400, "invalid_totp", "That code is wrong.", false)
				return
			case "challenge_expired":
				c.fail(409, "challenge_expired", "Sign in again.", false)
				return
			case "totp_attempts_exceeded":
				c.fail(429, "totp_attempts_exceeded", "Too many wrong codes. Sign in again.", false)
				return
			}
			c.json(200, sessionReply(map[string]any{"user_id": consoleUserID, "role": consoleRole(c), "must_change_password": c.scenario == "must_change_password"}))
		}},
		{method: "POST", path: "/auth/refresh", open: true, noIdempotent: true, handle: refresh},
		{method: "POST", path: "/auth/signout", noIdempotent: true, handle: func(c *call) { c.json(200, map[string]any{}) }},
		{method: "POST", path: "/me/password", handle: func(c *call) {
			body, ok := needsBody(c, "current_password", "new_password")
			if !ok {
				return
			}
			if p, _ := body["new_password"].(string); len(p) < 12 {
				c.fail(400, "weak_password", "Use at least 12 characters.", false)
				return
			}
			c.json(200, map[string]any{})
		}},
		{method: "GET", path: "/admin/me", handle: func(c *call) { c.json(200, userReply(consoleRole(c), c.now)) }},
		{method: "GET", path: "/admin/users", handle: func(c *call) {
			if !denied(c) {
				locked := consoleUser("support", c.now)
				if c.scenario == "account_locked_user" {
					locked["locked_until"] = stamp(c.now.Add(10 * time.Minute))
				}
				page(c, "users", []map[string]any{consoleUser("admin", c.now), locked})
			}
		}},
		{method: "POST", path: "/admin/users", handle: func(c *call) {
			if denied(c) || fail(c, "login_id_taken") {
				return
			}
			body, ok := needsBody(c, "login_id", "name", "role")
			if !ok {
				return
			}
			role, _ := body["role"].(string)
			if _, known := map[string]bool{"admin": true, "ops": true, "support": true, "hub_manager": true}[role]; !known {
				c.fail(400, "invalid_request", "Unknown role.", false)
				return
			}
			c.json(201, map[string]any{"user": consoleUser(role, c.now), "temporary_password": "Mock-Temp-Password-1"})
		}},
		{method: "PATCH", path: "/admin/users/{user_id}", handle: func(c *call) {
			if denied(c) || fail(c, "last_admin") {
				return
			}
			if _, ok := reason(c); ok {
				c.json(200, userReply("support", c.now))
			}
		}},
		{method: "POST", path: "/admin/users/{user_id}/reset", handle: func(c *call) {
			if denied(c) {
				return
			}
			if _, ok := reason(c); ok {
				c.json(200, map[string]any{"user": consoleUser("support", c.now), "temporary_password": "Mock-Temp-Password-2"})
			}
		}},
		{method: "GET", path: "/admin/approvals", handle: func(c *call) {
			if denied(c) {
				return
			}
			list := []map[string]any{sampleApproval("settings", "approved", c.now)}
			switch c.scenario {
			case "approval_pending":
				list = append([]map[string]any{sampleApproval("kill_switch", "pending", c.now)}, list...)
			case "approval_failed":
				list = append([]map[string]any{sampleApproval("settings", "failed", c.now)}, list...)
			}
			page(c, "approvals", list)
		}},
		{method: "POST", path: "/admin/approvals/{approval_id}/approve", handle: func(c *call) {
			if !denied(c) && !fail(c, "own_change", "not_pending") {
				c.json(200, map[string]any{"approval": sampleApproval("settings", "approved", c.now)})
			}
		}},
		{method: "POST", path: "/admin/approvals/{approval_id}/cancel", handle: func(c *call) {
			if !denied(c) && !fail(c, "not_pending") {
				c.json(200, map[string]any{"approval": sampleApproval("settings", "cancelled", c.now)})
			}
		}},
		{method: "POST", path: "/admin/approvals/{approval_id}/reject", handle: func(c *call) {
			if denied(c) || fail(c, "not_pending") {
				return
			}
			if _, ok := reason(c); ok {
				c.json(200, map[string]any{"approval": sampleApproval("settings", "rejected", c.now)})
			}
		}},
		{method: "GET", path: "/admin/riders/review", handle: func(c *call) {
			if denied(c) {
				return
			}
			v := "pending"
			if c.scenario == "rider_verification_failed" {
				v = "failed"
			}
			page(c, "riders", []map[string]any{sampleRiderReview(v, c.now)})
		}},
		{method: "POST", path: "/admin/riders/{rider_id}/decision", handle: func(c *call) {
			if denied(c) {
				return
			}
			if c.scenario == "rider_not_verified" {
				c.fail(409, "not_verified", "Verification is not complete.", false)
				return
			}
			if fail(c, "not_allowed") {
				return
			}
			body, ok := needsBody(c, "decision", "reason")
			if !ok {
				return
			}
			r := sampleRiderReview("verified", c.now)
			r["status"] = map[any]string{"approve": "active", "reinstate": "active", "suspend": "suspended"}[body["decision"]]
			if r["status"] == "" {
				c.fail(400, "invalid_request", "Unknown decision.", false)
				return
			}
			c.json(200, map[string]any{"rider": r})
		}},
		{method: "GET", path: "/admin/flags", handle: func(c *call) {
			if !denied(c) {
				c.json(200, map[string]any{"flags": []map[string]any{{"key": "eta_offers", "enabled": false, "description": "Offer a longer ETA at no_scooter", "updated_at": stamp(c.now)}}})
			}
		}},
		{method: "PUT", path: "/admin/flags/{flag_key}", handle: func(c *call) {
			if denied(c) || fail(c, "approval_pending") {
				return
			}
			if _, ok := needsBody(c, "enabled", "reason"); ok {
				c.json(202, map[string]any{"approval": sampleApproval("flag", "pending", c.now)})
			}
		}},
		{method: "GET", path: "/admin/zones", handle: func(c *call) {
			if !denied(c) {
				c.json(200, map[string]any{"zones": []map[string]any{{"id": "zone_mock01", "name": "Koramangala", "city": "Bengaluru", "offer_enabled": c.scenario != "kill_switch_off", "is_test": false}}})
			}
		}},
		{method: "GET", path: "/admin/settings", handle: func(c *call) {
			if !denied(c) {
				c.json(200, sampleSettings(nil))
			}
		}},
		{method: "PUT", path: "/admin/settings", handle: settingsChange(denied, fail)},
		{method: "GET", path: "/admin/zones/{zone_id}/settings", handle: func(c *call) {
			if !denied(c) {
				c.json(200, sampleSettings(c.param("zone_id")))
			}
		}},
		{method: "PUT", path: "/admin/zones/{zone_id}/settings", handle: settingsChange(denied, fail)},
		{method: "POST", path: "/admin/zones/{zone_id}/kill-switch", handle: func(c *call) {
			if denied(c) || fail(c, "approval_pending") {
				return
			}
			if _, ok := needsBody(c, "offer_enabled", "reason"); ok {
				c.json(202, map[string]any{"approval": sampleApproval("kill_switch", "pending", c.now)})
			}
		}},
		{method: "GET", path: "/admin/menu/items", handle: func(c *call) {
			if !denied(c) {
				c.json(200, map[string]any{"items": []map[string]any{adminMenuItem("item_burger", "Burger", 14900, "active", false, c.now), adminMenuItem("item_tea", "Lemon tea", 3900, "hidden", true, c.now)}})
			}
		}},
		{method: "POST", path: "/admin/menu/items", handle: func(c *call) {
			if denied(c) || fail(c, "name_taken") {
				return
			}
			body, ok := needsBody(c, "name", "price_paise")
			if ok {
				name, _ := body["name"].(string)
				isVeg, valid := optionalBool(c, body, "is_veg")
				if valid {
					c.json(201, map[string]any{"item": adminMenuItem("item_new", name, 9900, "hidden", isVeg, c.now)})
				}
			}
		}},
		{method: "PATCH", path: "/admin/menu/items/{item_id}", handle: func(c *call) {
			if denied(c) || fail(c, "name_taken") {
				return
			}
			if body, ok := reason(c); ok {
				if isVeg, valid := optionalBool(c, body, "is_veg"); valid {
					c.json(200, map[string]any{"item": adminMenuItem("item_burger", "Burger", 15900, "active", isVeg, c.now)})
				}
			}
		}},
		{method: "GET", path: "/admin/menu/items/{item_id}/prices", handle: func(c *call) {
			if !denied(c) {
				c.json(200, map[string]any{"item_id": c.param("item_id"), "prices": []map[string]any{
					{"price": money(15900), "reason": "New price list", "created_at": stamp(c.now)},
					{"price": money(14900), "reason": "First price", "created_at": stamp(c.now.Add(-30 * 24 * time.Hour))}}})
			}
		}},
		{method: "GET", path: "/admin/gates", handle: func(c *call) {
			if !denied(c) {
				page(c, "gates", []map[string]any{adminGate(2, c.now)})
			}
		}},
		{method: "PATCH", path: "/admin/gates/{gate_id}", handle: func(c *call) {
			if denied(c) || fail(c, "outside_zone") {
				return
			}
			if _, ok := reason(c); ok {
				c.json(200, map[string]any{"gate": adminGate(3, c.now)})
			}
		}},
		{method: "GET", path: "/admin/gates/{gate_id}/history", handle: func(c *call) {
			if denied(c) {
				return
			}
			v := func(n int, label string) map[string]any {
				var by any
				if n > 1 {
					by = "Asha K"
				}
				return map[string]any{"version": n, "label": label, "lat": 12.9352, "lng": 77.6245, "entry_notes": nil, "verified": n > 1, "reason": fmt.Sprintf("version %d", n),
					"changed_by": by, "created_at": stamp(c.now.Add(-time.Duration(3-n) * time.Hour))}
			}
			c.json(200, map[string]any{"gate_id": c.param("gate_id"), "versions": []map[string]any{v(1, "Tower B"), v(2, "Tower B main gate")}})
		}},
		{method: "GET", path: "/admin/hubs", handle: func(c *call) {
			if !denied(c) {
				c.json(200, map[string]any{"hubs": []map[string]any{adminHub("active")}})
			}
		}},
		{method: "POST", path: "/admin/hubs", handle: func(c *call) {
			if denied(c) || fail(c, "name_taken") {
				return
			}
			if _, ok := needsBody(c, "zone_id", "name", "lat", "lng"); ok {
				c.json(201, map[string]any{"hub": adminHub("active")})
			}
		}},
		{method: "PATCH", path: "/admin/hubs/{hub_id}", handle: func(c *call) {
			if denied(c) || fail(c, "name_taken") {
				return
			}
			if _, ok := reason(c); ok {
				c.json(200, map[string]any{"hub": adminHub("paused")})
			}
		}},
		{method: "GET", path: "/admin/hubs/{hub_id}/staff", handle: func(c *call) {
			if !denied(c) {
				c.json(200, map[string]any{"staff": []map[string]any{hubStaffMember("hub_lead", "active")}})
			}
		}},
		{method: "POST", path: "/admin/hubs/{hub_id}/staff", handle: func(c *call) {
			if denied(c) || fail(c, "phone_taken") {
				return
			}
			if _, ok := needsBody(c, "phone", "name", "role"); ok {
				c.json(201, map[string]any{"staff": hubStaffMember("hub_staff", "active")})
			}
		}},
		{method: "PATCH", path: "/admin/hub-staff/{staff_id}", handle: func(c *call) {
			if denied(c) {
				return
			}
			if _, ok := reason(c); ok {
				c.json(200, map[string]any{"staff": hubStaffMember("hub_staff", "suspended")})
			}
		}},
		{method: "GET", path: "/admin/audit", handle: func(c *call) {
			if denied(c) {
				return
			}
			q := c.r.URL.Query()
			for _, k := range []string{"from", "to"} {
				if v := q.Get(k); v != "" {
					if _, err := time.Parse(time.RFC3339, v); err != nil {
						c.fail(400, "invalid_request", k+" must be a date-time.", false)
						return
					}
				}
			}
			events := []map[string]any{{"id": "aud_mock01", "actor_type": "console", "actor_id": consoleUserID, "action": "settings.changed", "entity_type": "zone",
				"entity_id": "zone_mock01", "before": map[string]any{"timer_length_s": 90}, "after": map[string]any{"timer_length_s": 120}, "reason": "Rush hour", "at": stamp(c.now)}}
			if c.scenario == "audit_empty" {
				events = []map[string]any{}
			}
			page(c, "events", events)
		}},
		{method: "GET", path: "/admin/arrival-reviews", handle: func(c *call) {
			if !denied(c) {
				page(c, "reviews", []map[string]any{{"id": "arv_mock01", "order_id": orderID, "status": "open", "lat": 12.9352, "lng": 77.6245,
					"device_time": stamp(c.now.Add(-2 * time.Minute)), "due_at": stamp(c.now.Add(8 * time.Minute)), "decided_at": nil, "decided_by": nil, "decision_reason": nil}})
			}
		}},
		{method: "POST", path: "/admin/arrival-reviews/{review_id}/decision", handle: func(c *call) {
			if denied(c) || fail(c, "review_closed") {
				return
			}
			body, ok := needsBody(c, "decision", "reason")
			if !ok {
				return
			}
			d, _ := body["decision"].(string)
			if d != "arrived_in_time" && d != "late" {
				c.fail(400, "invalid_request", "decision must be arrived_in_time or late.", false)
				return
			}
			c.json(200, map[string]any{"review": map[string]any{"id": c.param("review_id"), "order_id": orderID, "status": d, "lat": 12.9352, "lng": 77.6245,
				"device_time": stamp(c.now.Add(-2 * time.Minute)), "due_at": stamp(c.now.Add(8 * time.Minute)), "decided_at": stamp(c.now),
				"decided_by": "Asha K", "decision_reason": body["reason"]}})
		}},
		{method: "GET", path: "/admin/support-alerts", handle: func(c *call) {
			if !denied(c) {
				page(c, "alerts", []map[string]any{{"id": "alt_mock01", "kind": "handover_wrong_otp", "order_id": orderID, "rider_id": "rdr_mock0001", "created_at": stamp(c.now)}})
			}
		}},
		{method: "GET", path: "/admin/orders", handle: func(c *call) {
			if denied(c) {
				return
			}
			for _, k := range []string{"from", "to"} {
				if v := c.r.URL.Query().Get(k); v != "" {
					if _, err := time.Parse(time.RFC3339, v); err != nil {
						c.fail(400, "invalid_request", k+" must be a date-time.", false)
						return
					}
				}
			}
			orders := []map[string]any{adminOrderSummary("A1002", "ord_mock0002", "TIMER_RUNNING", c.now), adminOrderSummary("A1001", orderID, "COMPLETED", c.now)}
			if c.scenario == "orders_empty" {
				orders = []map[string]any{}
			}
			page(c, "orders", orders)
		}},
		{method: "GET", path: "/admin/orders/{order_id}/timeline", handle: func(c *call) {
			if denied(c) {
				return
			}
			ev := func(ago time.Duration, source, kind, actorType string, actor any, detail map[string]any) map[string]any {
				return map[string]any{"at": stamp(c.now.Add(-ago)), "source": source, "kind": kind, "actor_type": actorType, "actor_name": actor, "detail": detail}
			}
			c.json(200, map[string]any{"order": adminOrder(c.param("order_id"), c.now), "events": []map[string]any{
				ev(20*time.Minute, "order", "order.created", "customer", nil, map[string]any{"state": "CREATED"}),
				ev(19*time.Minute, "payment", "payment.captured", "system", nil, map[string]any{"amount": money(14900)}),
				ev(18*time.Minute, "offer", "offer.accepted", "rider", "Ravi", map[string]any{"offer_id": "ofr_mock0001"}),
				ev(12*time.Minute, "order", "order.timer_started", "system", nil, map[string]any{"perimeter_m": 350}),
				ev(11*time.Minute, "order", "order.handed_over", "rider", "Ravi", map[string]any{"arrival_source": "handover"}),
				ev(8*time.Minute, "ticket", "ticket.opened", "customer", nil, map[string]any{"ticket_id": "tkt_mock01"}),
				ev(5*time.Minute, "refund", "refund.processed", "console", "Asha K", map[string]any{"refund_id": "rfd_mock01", "amount": money(5000)}),
				ev(4*time.Minute, "audit", "refund.requested", "console", "Asha K", map[string]any{"reason": "Cold food"}),
			}, "pings": map[string]any{"count": 212, "first_at": stamp(c.now.Add(-18 * time.Minute)), "last_at": stamp(c.now.Add(-11 * time.Minute))}})
		}},
		{method: "GET", path: "/admin/tickets", handle: func(c *call) {
			if denied(c) {
				return
			}
			t := adminTicket("open", c.now)
			t["messages"] = []map[string]any{}
			page(c, "tickets", []map[string]any{t})
		}},
		{method: "GET", path: "/admin/tickets/{ticket_id}", handle: func(c *call) {
			if !denied(c) {
				c.json(200, map[string]any{"ticket": adminTicket("open", c.now)})
			}
		}},
		{method: "POST", path: "/admin/tickets/{ticket_id}/messages", handle: func(c *call) {
			if denied(c) || fail(c, "ticket_closed") {
				return
			}
			if _, ok := needsBody(c, "text"); ok {
				t := adminTicket("open", c.now)
				t["messages"] = append(t["messages"].([]map[string]any), map[string]any{"id": "msg_mock02", "author_type": "support", "author_name": "Asha K",
					"text": "Sorry about that. We are sending a refund.", "created_at": stamp(c.now)})
				c.json(201, map[string]any{"ticket": t})
			}
		}},
		{method: "POST", path: "/admin/tickets/{ticket_id}/close", handle: func(c *call) {
			if denied(c) || fail(c, "ticket_closed") {
				return
			}
			if body, ok := reason(c); ok {
				t := adminTicket("resolved", c.now)
				t["close_reason"] = body["reason"]
				c.json(200, map[string]any{"ticket": t})
			}
		}},
		{method: "POST", path: "/admin/orders/{order_id}/refund", handle: func(c *call) {
			if denied(c) || fail(c, "refund_too_large", "not_paid", "approval_pending") {
				return
			}
			body, ok := needsBody(c, "amount_paise", "reason")
			if !ok {
				return
			}
			amount, ok := paise(body["amount_paise"])
			if !ok {
				c.fail(400, "invalid_request", "amount_paise must be a whole number of paise, from 1 to 10000000.", false)
				return
			}
			// The setting refund_approval_paise (#147), cumulative per order: refunds already made from
			// the console (the sample order has 5000), plus this one, at or below it is made at once.
			const refundApprovalPaise, alreadyRefunded = 20000, 5000
			if alreadyRefunded+amount > refundApprovalPaise || c.scenario == "refund_needs_approval" {
				a := sampleApproval("refund", "pending", c.now)
				a["change"] = map[string]any{"kind": "refund", "order_id": c.param("order_id"), "amount": money(int(amount))}
				a["summary"] = fmt.Sprintf("Order %s: refund %d paise", c.param("order_id"), int(amount))
				c.json(202, map[string]any{"approval": a})
				return
			}
			c.json(200, map[string]any{"refund": map[string]any{"id": "rfd_mock01", "order_id": c.param("order_id"), "amount": money(int(amount)), "status": "pending",
				"reason": body["reason"], "requested_by": map[string]any{"id": consoleUserID, "name": "Asha K"}, "created_at": stamp(c.now)}})
		}},
		{method: "POST", path: "/admin/orders/{order_id}/replacement", handle: func(c *call) {
			if denied(c) || fail(c, "not_paid", "item_not_on_order", "remedy_exists") {
				return
			}
			body, ok := needsBody(c, "items", "reason")
			if !ok {
				return
			}
			if items, _ := body["items"].([]any); len(items) == 0 {
				c.fail(400, "invalid_request", "Send at least one item.", false)
				return
			}
			r := adminRemedy("replacement", c.now)
			r["items"] = body["items"]
			r["reason"] = body["reason"]
			c.json(201, map[string]any{"remedy": r})
		}},
		{method: "POST", path: "/admin/orders/{order_id}/credit", handle: func(c *call) {
			if denied(c) || fail(c, "not_paid", "remedy_exists") {
				return
			}
			body, ok := needsBody(c, "amount_paise", "reason")
			if !ok {
				return
			}
			amount, ok := paise(body["amount_paise"])
			if !ok {
				c.fail(400, "invalid_request", "amount_paise must be a whole number of paise, from 1 to 10000000.", false)
				return
			}
			r := adminRemedy("credit", c.now)
			r["amount"] = money(int(amount))
			r["reason"] = body["reason"]
			c.json(201, map[string]any{"remedy": r})
		}},
		{method: "GET", path: "/admin/incidents", handle: func(c *call) {
			if denied(c) {
				return
			}
			page(c, "incidents", []map[string]any{adminIncident("support_alert", "open", c.now), adminIncident("ticket", "assigned", c.now), adminIncident("signal_lost", "closed", c.now)})
		}},
		{method: "GET", path: "/admin/incidents/{incident_id}", handle: func(c *call) {
			if !denied(c) {
				c.json(200, incidentReply("ticket", "assigned", c.now))
			}
		}},
		{method: "POST", path: "/admin/incidents/{incident_id}/assign", handle: func(c *call) {
			if denied(c) || fail(c, "incident_closed", "user_cannot_take") {
				return
			}
			body := map[string]any{}
			if err := decodeJSON(c.r, &body); err != nil {
				c.fail(400, "invalid_request", "The body must be JSON.", false)
				return
			}
			user, present := body["user_id"]
			if !present {
				c.fail(400, "invalid_request", "Missing field: user_id (null takes the assignment away).", false)
				return
			}
			if user == nil {
				c.json(200, incidentReply("support_alert", "open", c.now))
				return
			}
			c.json(200, incidentReply("support_alert", "assigned", c.now))
		}},
		{method: "POST", path: "/admin/incidents/{incident_id}/notes", handle: func(c *call) {
			if denied(c) || fail(c, "incident_closed") {
				return
			}
			if body, ok := needsBody(c, "text"); ok {
				reply := incidentReply("support_alert", "assigned", c.now)
				reply["notes"] = append(reply["notes"].([]map[string]any), map[string]any{"id": "inn_mock02", "text": body["text"],
					"author": map[string]any{"id": consoleUserID, "name": "Asha K"}, "created_at": stamp(c.now)})
				reply["incident"].(map[string]any)["notes_count"] = 2
				c.json(201, reply)
			}
		}},
		{method: "POST", path: "/admin/incidents/{incident_id}/close", handle: func(c *call) {
			if denied(c) || fail(c, "incident_closed", "source_open") {
				return
			}
			if _, ok := reason(c); ok {
				c.json(200, incidentReply("support_alert", "closed", c.now))
			}
		}},
		{method: "GET", path: "/admin/hub-ops", handle: func(c *call) {
			if denied(c) {
				return
			}
			hubs := []map[string]any{adminHubOps(adminHub("active"), 1, true)}
			if consoleRole(c) != "hub_manager" {
				other := adminHub("active")
				other["id"], other["name"], other["zone_id"] = "hub_mock02", "Indiranagar hub", "zone_mock02"
				hubs = append(hubs, adminHubOps(other, 0, false))
			}
			// zone_id keeps only the hubs in that zone, as the real call does.
			if zone := c.r.URL.Query().Get("zone_id"); zone != "" {
				kept := hubs[:0]
				for _, h := range hubs {
					if h["hub"].(map[string]any)["zone_id"] == zone {
						kept = append(kept, h)
					}
				}
				hubs = kept
			}
			c.json(200, map[string]any{"hubs": hubs})
		}},
		{method: "GET", path: "/admin/hub-ops/stuck", handle: func(c *call) {
			if !denied(c) {
				page(c, "requests", []map[string]any{adminRestock("packing", 660, nil, c.now)})
			}
		}},
		{method: "POST", path: "/admin/restock/{request_id}/expire", handle: func(c *call) {
			if denied(c) || fail(c, "restock_not_expirable") {
				return
			}
			if _, ok := reason(c); ok {
				c.json(200, map[string]any{"restock": adminRestock("expired", 0, "expired_by_console", c.now)})
			}
		}},
		{method: "GET", path: "/admin/zones/{zone_id}/live", handle: func(c *call) {
			if denied(c) {
				return
			}
			if c.scenario == "poll_too_fast" {
				c.fail(429, "rate_limited", "Poll the live zone no more often than every 5 seconds.", true)
				return
			}
			c.json(200, map[string]any{
				"riders": []map[string]any{
					{"id": "rdr_mock0001", "name": "Ravi", "lat": 12.9355, "lng": 77.6240, "last_ping_at": stamp(c.now.Add(-3 * time.Second)), "state": "moving"},
					{"id": "rdr_mock0003", "name": "Meena", "lat": 12.9371, "lng": 77.6262, "last_ping_at": stamp(c.now.Add(-9 * time.Second)), "state": "idle"}},
				"scooters": []map[string]any{
					{"id": "scr_mock01", "plate": "KA01AB1234", "node_id": nil, "rider_id": "rdr_mock0001", "stock_units": 4},
					{"id": "scr_mock02", "plate": "KA01AB5678", "node_id": "node_mock01", "rider_id": "rdr_mock0003", "stock_units": 6}},
				"orders": []map[string]any{{"id": "ord_mock0002", "order_number": "A1002", "state": "TIMER_RUNNING", "gate": map[string]any{"lat": 12.9352, "lng": 77.6245},
					"rider_id": "rdr_mock0001", "timer_started_at": stamp(c.now.Add(-40 * time.Second))}},
				"incidents_open": 2})
		}},
	}
}

func settingsChange(denied func(*call) bool, fail func(*call, ...string) bool) func(*call) {
	return func(c *call) {
		if denied(c) || fail(c, "approval_pending", "stale_version") {
			return
		}
		if c.scenario == "invalid_setting" {
			c.fail(400, "invalid_setting", "timer_length_s must be from 30 to 600.", false)
			return
		}
		body, ok := needsBody(c, "changes", "base_version", "reason")
		if !ok {
			return
		}
		if changes, _ := body["changes"].(map[string]any); len(changes) == 0 {
			c.fail(400, "invalid_request", "Send at least one change.", false)
			return
		}
		c.json(202, map[string]any{"approval": sampleApproval("settings", "pending", c.now)})
	}
}

// consolePermission is the permission the called operation needs, as the contract names it.
func consolePermission(c *call) string {
	path := c.r.URL.Path
	switch {
	case contains(path, "/admin/orders") && contains(path, "/refund"):
		return "refunds.request"
	case contains(path, "/admin/orders") && (contains(path, "/replacement") || contains(path, "/credit")):
		return "remedies.issue"
	case contains(path, "/admin/orders"):
		return "orders.read"
	case contains(path, "/admin/tickets"):
		return "tickets.manage"
	case contains(path, "/admin/incidents"):
		if c.r.Method == "GET" {
			return "incidents.read"
		}
		return "incidents.manage"
	case contains(path, "/admin/hub-ops"):
		return "hub_ops.read"
	case contains(path, "/admin/restock"):
		return "hub_ops.manage"
	case contains(path, "/live"):
		return "map.read"
	case contains(path, "/admin/users"):
		return "users.manage"
	case contains(path, "/approve") || contains(path, "/reject"):
		return "approvals.decide"
	case contains(path, "/admin/approvals"):
		return "approvals.read"
	case contains(path, "/admin/riders"):
		return "riders.review"
	case contains(path, "/admin/flags"):
		return "flags.manage"
	case contains(path, "/kill-switch"):
		return "zones.kill_switch"
	case contains(path, "/settings"):
		if c.r.Method == "GET" {
			return "settings.read"
		}
		return "settings.request"
	case contains(path, "/admin/zones"):
		return "zones.read"
	case contains(path, "/admin/menu"):
		return "menu.manage"
	case contains(path, "/admin/gates"):
		if c.r.Method == "GET" {
			return "gates.read"
		}
		return "gates.manage"
	case contains(path, "/staff") && c.r.Method != "GET":
		return "hub_staff.manage"
	case contains(path, "/admin/hub-staff"):
		return "hub_staff.manage"
	case contains(path, "/admin/hubs"):
		if c.r.Method == "GET" {
			return "hubs.read"
		}
		return "hubs.manage"
	case contains(path, "/admin/audit"):
		return "audit.read"
	case contains(path, "/decision") && contains(path, "arrival"):
		return "reviews.decide"
	case contains(path, "/admin/arrival-reviews"):
		return "reviews.read"
	case contains(path, "/admin/support-alerts"):
		return "alerts.read"
	}
	return "console"
}

func contains(s, part string) bool {
	for i := 0; i+len(part) <= len(s); i++ {
		if s[i:i+len(part)] == part {
			return true
		}
	}
	return false
}

// paise reads a whole number of paise within the contract's range (1 to 10,000,000, #167), so a
// value past what an int holds is refused instead of wrapping round.
func paise(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok || f < 1 || f > 10000000 || f != math.Trunc(f) {
		return 0, false
	}
	return int(f), true
}

// optionalBool reads a boolean field that may be left out (false then). A value that is not a
// boolean is a 400 and the second result is false.
func optionalBool(c *call, body map[string]any, key string) (value, ok bool) {
	raw, present := body[key]
	if !present {
		return false, true
	}
	value, isBool := raw.(bool)
	if !isBool {
		c.fail(400, "invalid_request", key+" must be true or false.", false)
		return false, false
	}
	return value, true
}
