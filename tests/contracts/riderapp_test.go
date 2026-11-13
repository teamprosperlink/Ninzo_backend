package contracts

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// riderAppCalls is every HTTP call the finished rider app makes (lib/core/api/api_endpoints.dart in the
// app, and docs/engineering/rider_apk_compatibility.md), with the operation that serves it. The app cannot
// change, so a call that goes missing from the rider contract breaks the app. The live stream
// (GET /riders/me/stream) is in rider/stream.asyncapi.yaml, not here.
var riderAppCalls = []struct{ method, path, id string }{
	{"post", "/auth/otp/request", "requestOtp"},
	{"post", "/auth/otp/verify", "verifyOtp"},
	{"post", "/auth/refresh", "refreshToken"},
	{"post", "/auth/signout", "signOut"},
	{"get", "/riders/me", "getRider"},
	{"patch", "/riders/me", "updateRider"},
	{"get", "/riders/me/state", "getRiderState"},
	{"post", "/riders/me/devices", "registerRiderDevice"},
	{"delete", "/riders/me/devices/{device_id}", "removeRiderDevice"},
	{"get", "/riders/me/verification", "getVerification"},
	{"post", "/riders/me/verification/start", "startVerification"},
	{"get", "/riders/me/earnings", "getEarnings"},
	{"post", "/riders/me/problems", "reportProblem"},
	{"post", "/riders/me/events", "sendEventBatch"},
	{"post", "/riders/me/location", "locationPing"},
	{"get", "/riders/me/notifications", "listRiderNotifications"},
	{"get", "/riders/me/settings", "getRiderSettings"},
	{"get", "/riders/me/help", "getRiderHelp"},
	{"post", "/riders/me/feedback", "sendRiderFeedback"},
	{"get", "/riders/me/shift-plan", "getShiftPlan"},
	{"post", "/riders/me/shift-plan/reply", "replyToShiftPlan"},
	{"get", "/nodes", "listNodes"},
	{"post", "/riders/me/shifts/start", "startShift"},
	{"post", "/riders/me/shifts/end", "endShift"},
	{"get", "/riders/me/shifts", "listRiderShifts"},
	{"post", "/riders/me/shifts/online", "goOnline"},
	{"post", "/riders/me/shifts/break", "takeBreak"},
	{"post", "/riders/me/shifts/break/end", "endShiftBreak"},
	{"post", "/riders/me/shifts/planned-end", "setPlannedEnd"},
	{"get", "/riders/me/documents", "listRiderDocuments"},
	{"post", "/riders/me/documents", "uploadRiderDocument"},
	{"post", "/riders/me/relocations/{relocation_id}/accept", "acceptRelocation"},
	{"get", "/riders/me/stock", "getStock"},
	{"post", "/riders/me/stock/scan", "scanStock"},
	{"post", "/riders/me/stock/{lot_id}/quarantine", "quarantineLot"},
	{"get", "/riders/me/offers", "listOffers"},
	{"post", "/offers/{offer_id}/accept", "acceptOffer"},
	{"post", "/offers/{offer_id}/decline", "declineOffer"},
	{"post", "/offers/{offer_id}/ready", "markReady"},
	{"get", "/orders/{order_id}", "getRiderOrder"},
	{"get", "/orders/{order_id}/route", "getRoute"},
	{"post", "/orders/{order_id}/call", "startRiderMaskedCall"},
	{"post", "/orders/{order_id}/handover", "handOver"},
	{"post", "/orders/{order_id}/gate-check", "checkGate"},
	{"post", "/restock", "createRestock"},
	{"get", "/restock/options", "getRestockOptions"},
	{"get", "/riders/me/restock-suggestions", "listRestockSuggestions"},
	{"get", "/restock/{request_id}", "getRestock"},
	{"post", "/restock/{request_id}/confirm", "confirmRestock"},
	{"post", "/restock/{request_id}/decline", "declineRestock"},
	{"post", "/restock/{request_id}/shortfall-response", "respondToShortfall"},
}

// riderAppReads lists the fields the app reads that the first rider contract did not have, by schema.
// Each must be a property, so the server can send it. They are optional until part B of Issue 235.
var riderAppReads = map[string][]string{
	"Rider":             {"phone", "email", "area"},
	"Shift":             {"online_at", "planned_end_at", "break", "break_started_at", "break_ends_at", "device_id"},
	"ShiftEntry":        {"online_at", "planned_end_at", "orders", "portions", "avg_delivery_s"},
	"Offer":             {"total", "payment_method"},
	"RiderOrder":        {"total", "payment_method"},
	"RestockOptionHub":  {"manager_phone"},
	"RiderSettings":     {"timer_length_s", "max_break_s", "break_options_s", "checklist", "cancel_pay_cap", "problem_types", "skip_reasons"},
	"ShiftPlanResponse": {"plan"},
}

// riderAppRequests lists the request fields the app sends, by operation.
var riderAppRequests = map[string][]string{
	"updateRider":         {"name", "email", "area", "preferred_area"},
	"startShift":          {"node_id", "device_id", "checklist"},
	"takeBreak":           {"length_s"},
	"setPlannedEnd":       {"planned_end_at"},
	"uploadRiderDocument": {"document", "side", "content_type", "data_base64"},
	"sendRiderFeedback":   {"message"},
	"quarantineLot":       {"reason", "note"},
	"checkGate":           {"method", "value"},
	"replyToShiftPlan":    {"reply"},
}

func riderContract(t *testing.T) (*loader, *doc) {
	t.Helper()
	l := &loader{docs: map[string]*doc{}}
	d, err := l.load(filepath.Join(contractsDir, "rider/openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return l, d
}

func TestRiderContractServesEveryCallTheAppMakes(t *testing.T) {
	_, d := riderContract(t)
	have := map[string]string{}
	for _, op := range operations(d) {
		have[op.method+" "+op.path] = op.id
	}
	for _, call := range riderAppCalls {
		got, ok := have[call.method+" "+call.path]
		switch {
		case !ok:
			t.Errorf("the rider app calls %s %s, and the contract has no such operation", strings.ToUpper(call.method), call.path)
		case got != call.id:
			t.Errorf("%s %s is %s in the contract, and this list says %s", strings.ToUpper(call.method), call.path, got, call.id)
		}
	}
	if len(riderAppCalls) != 51 {
		t.Errorf("the app makes 52 calls, 51 of them here and the stream in the asyncapi file: this list has %d", len(riderAppCalls))
	}
}

func schemaProperties(t *testing.T, d *doc, name string) map[string]any {
	t.Helper()
	components, _ := d.root["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	schema, _ := schemas[name].(map[string]any)
	if schema == nil {
		t.Fatalf("the rider contract has no schema %s", name)
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		// RestockOptionHub extends HubRef with allOf.
		parts, _ := schema["allOf"].([]any)
		for _, p := range parts {
			part, _ := p.(map[string]any)
			if inner, ok := part["properties"].(map[string]any); ok {
				props = inner
			}
		}
	}
	return props
}

func TestRiderContractHasTheFieldsTheAppReads(t *testing.T) {
	_, d := riderContract(t)
	names := make([]string, 0, len(riderAppReads))
	for name := range riderAppReads {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		props := schemaProperties(t, d, name)
		for _, field := range riderAppReads[name] {
			if _, ok := props[field]; !ok {
				t.Errorf("%s has no %s, and the rider app reads it", name, field)
			}
		}
	}
}

func TestRiderContractAcceptsTheFieldsTheAppSends(t *testing.T) {
	l, d := riderContract(t)
	pending := map[string]bool{}
	for id := range riderAppRequests {
		pending[id] = true
	}
	for _, op := range operations(d) {
		want, ok := riderAppRequests[op.id]
		if !ok {
			continue
		}
		body, _ := op.node["requestBody"].(map[string]any)
		content, _ := body["content"].(map[string]any)
		media, _ := content["application/json"].(map[string]any)
		schema, _ := media["schema"].(map[string]any)
		_, resolved, err := l.resolve(d, schema)
		if err != nil {
			t.Fatal(err)
		}
		props, _ := resolved["properties"].(map[string]any)
		for _, field := range want {
			if _, ok := props[field]; !ok {
				t.Errorf("%s does not accept %s, and the rider app sends it", op.id, field)
			}
		}
		delete(pending, op.id)
	}
	for id := range pending {
		t.Errorf("%s is not in the rider contract", id)
	}
}

// TestRiderContractKeepsTheOwnerRules checks that the rules the owner set for the app's calls are in the
// contract text, so the server work in part B cannot drift from them.
func TestRiderContractKeepsTheOwnerRules(t *testing.T) {
	_, d := riderContract(t)
	info, _ := d.root["info"].(map[string]any)
	text, _ := info["description"].(string)
	for _, rule := range []string{"open-ended", "max_break_s", "edited in the console", "arrival is still decided by the rider's", "call_not_available", "each scooter", "never into a", "never against a phone number", "started moving toward the gate"} {
		if !strings.Contains(text, rule) {
			t.Errorf("the rider contract description has lost the rule marked by %q", rule)
		}
	}
	for _, op := range operations(d) {
		if op.id == "reportScooterTelemetry" {
			params, _ := op.node["parameters"].([]any)
			found := false
			for _, p := range params {
				if m, _ := p.(map[string]any); m["name"] == "X-Telemetry-Key" {
					found = true
				}
			}
			if !found {
				t.Error("reportScooterTelemetry must take the scooter's key in X-Telemetry-Key")
			}
		}
		if strings.Contains(op.path, "telephony") {
			t.Errorf("%s: the provider's call bridge must not be in the rider contract", op.path)
		}
	}
}

// TestRiderContractReviewFixes pins the fixes from the independent review of Issue 235 part A.
func TestRiderContractReviewFixes(t *testing.T) {
	_, d := riderContract(t)
	components, _ := d.root["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	settings, _ := schemas["RiderSettings"].(map[string]any)
	required := map[string]bool{}
	for _, r := range settings["required"].([]any) {
		required[r.(string)] = true
	}
	if !required["max_break_s"] {
		t.Error("RiderSettings.max_break_s must be required: it is a new schema")
	}
	payment, _ := schemas["PaymentMethod"].(map[string]any)
	if text, _ := payment["description"].(string); strings.Contains(text, "Open list") || !strings.Contains(text, "closed") {
		t.Errorf("PaymentMethod is a closed enum, and its description must say so: %q", text)
	}
	for _, op := range operations(d) {
		text, _ := op.node["description"].(string)
		responses, _ := op.node["responses"].(map[string]any)
		switch op.id {
		case "requestOtp":
			if !strings.Contains(text, "invalid_phone") {
				t.Error("requestOtp must document invalid_phone")
			}
		case "reportScooterTelemetry":
			if _, ok := responses["429"]; !ok {
				t.Error("reportScooterTelemetry must declare 429")
			}
		}
	}
	if !strings.Contains(fmtProblemTypes(schemas), "breakdown") {
		t.Error("RiderSettings.problem_types must say which codes it may offer")
	}
}

func fmtProblemTypes(schemas map[string]any) string {
	settings, _ := schemas["RiderSettings"].(map[string]any)
	props, _ := settings["properties"].(map[string]any)
	pt, _ := props["problem_types"].(map[string]any)
	text, _ := pt["description"].(string)
	return text
}
