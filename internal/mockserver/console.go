package mockserver

import (
	"fmt"
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
	"approval_pending":          "Settings, kill-switch and flag requests fail with approval_pending; the list shows one waiting",
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
		"reviews.decide", "alerts.read"},
	"ops": {"approvals.read", "riders.review", "zones.read", "settings.read", "settings.request", "zones.kill_switch", "menu.manage", "gates.read",
		"gates.manage", "hubs.read", "hubs.manage", "hub_staff.manage", "reviews.read", "reviews.decide", "alerts.read"},
	"support":     {"zones.read", "gates.read", "reviews.read", "reviews.decide", "alerts.read"},
	"hub_manager": {"hubs.read", "hub_staff.manage"},
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

func adminMenuItem(id, name string, price int, status string, now time.Time) map[string]any {
	return map[string]any{"id": id, "name": name, "description": nil, "price": money(price), "status": status, "sort_order": 1, "allergens": []string{},
		"photo_url": nil, "updated_at": stamp(now)}
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

func consoleRoutes() []route {
	denied := func(c *call) bool {
		switch c.scenario {
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
				c.json(200, map[string]any{"items": []map[string]any{adminMenuItem("item_burger", "Burger", 14900, "active", c.now), adminMenuItem("item_tea", "Lemon tea", 3900, "hidden", c.now)}})
			}
		}},
		{method: "POST", path: "/admin/menu/items", handle: func(c *call) {
			if denied(c) || fail(c, "name_taken") {
				return
			}
			body, ok := needsBody(c, "name", "price_paise")
			if ok {
				name, _ := body["name"].(string)
				c.json(201, map[string]any{"item": adminMenuItem("item_new", name, 9900, "hidden", c.now)})
			}
		}},
		{method: "PATCH", path: "/admin/menu/items/{item_id}", handle: func(c *call) {
			if denied(c) || fail(c, "name_taken") {
				return
			}
			if _, ok := reason(c); ok {
				c.json(200, map[string]any{"item": adminMenuItem("item_burger", "Burger", 15900, "active", c.now)})
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
