// Package riderapp serves the rider app's simple calls that need no other module's data (issue 235, part B2):
// the settings and choices the app works by, help articles, feedback, and the day's shift plan.
package riderapp

import (
	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
)

// Choice is one option the app offers: a code the app sends back, and the words it shows. Hint and
// Check are only sent when set. Check is for checklist items only.
type Choice struct {
	Code  string `json:"code"`
	Label string `json:"label"`
	Hint  string `json:"hint,omitempty"`
	Check string `json:"check,omitempty"`
}

// The constants below are the only values the settings call does not take from the console. Each is
// a shape of the app or of the code, not a number operations tune, and each is written here once.
const (
	// HandoverCodeLength is the digits in the customer's handover code. The code is made by
	// orders (four digits), so this cannot be a setting without changing how codes are made.
	HandoverCodeLength = 4
	// DefaultMaxBreakS is the longest break when a settings row has no max_break_s.
	DefaultMaxBreakS = 1800
)

// breakOptionsS and addTimeOptionsS are the lengths the app offers. A break option longer than the
// console's max_break_s is left out, so the app never offers what takeBreak would cut short.
var (
	breakOptionsS   = []int{300, 600, 900, 1800}
	addTimeOptionsS = []int{900, 1800, 3600}
)

// SkipReasons are the reasons the app offers for declining an offer. declineOffer takes free text, so
// any code is valid there.
var SkipReasons = []Choice{
	{Code: "too_far", Label: "Too far"},
	{Code: "not_ready", Label: "Not ready"},
	{Code: "need_break", Label: "Need a break"},
}

// ProblemTypes are the problems the app offers. Every code is a value of reportProblem's type, because
// the app sends the code straight into it. A test checks this against the contract.
var ProblemTypes = []Choice{
	{Code: "breakdown", Label: "Scooter broke down", Hint: "Move to a safe place first."},
	{Code: "accident", Label: "I had an accident", Hint: "Get to safety. Support will call you."},
	{Code: "gate_closed", Label: "Gate is closed", Hint: "Tell the customer you are waiting."},
	{Code: "customer_absent", Label: "Customer is not at the gate"},
	{Code: "unsafe_route", Label: "The route is not safe", Hint: "This never counts against you."},
}

// Checklist is the Before you start list. startShift keeps only codes listed here. manual is the rider's
// own tick; the others are checked by the app.
var Checklist = []Choice{
	{Code: "helmet", Label: "Helmet on", Check: "manual"},
	{Code: "phone_battery", Label: "Phone charged", Check: "phone_battery"},
	{Code: "scooter_battery", Label: "Scooter charged", Check: "scooter_battery"},
	{Code: "location", Label: "Location is on", Check: "location"},
}

// ChecklistCodes is the set of codes startShift may keep.
func ChecklistCodes() map[string]bool {
	out := map[string]bool{}
	for _, c := range Checklist {
		out[c.Code] = true
	}
	return out
}

// fallbacks are used only for a setting no stored version holds (a row written without it). The
// migration gave every stored version these numbers, and the console's value always wins.
var fallbacks = map[string]int{
	"away_from_spot_m": 400, "scooter_capacity_qty": 24, "min_phone_battery_pct": 20, "min_scooter_battery_pct": 30,
	"low_scooter_battery_pct": 15, "cancel_pay_cap_paise": 4000, "break_extension_s": 900, "not_at_gate_after_s": 30,
	"shift_ending_soon_before_s": 900, "stock_request_ask_before_s": 3600, "stock_request_reminder_before_s": 1800,
	"verification_within_s": 86400, "max_break_s": DefaultMaxBreakS,
}

// Settings is the RiderSettings of the contract, built from what the zone runs with.
func Settings(e geo.Effective) map[string]any {
	// own reads a console setting, or its fallback.
	own := func(name string) int {
		if v, ok := e.Get(name); ok {
			return v
		}
		return fallbacks[name]
	}
	// shared reads a setting the order flow also uses, and is null if the zone has none.
	shared := func(name string) any {
		if v, ok := e.Get(name); ok {
			return v
		}
		return nil
	}
	maxBreak := own("max_break_s")
	breaks := []int{}
	for _, s := range breakOptionsS {
		if s <= maxBreak {
			breaks = append(breaks, s)
		}
	}
	return map[string]any{
		"timer_length_s": shared("timer_length_s"), "max_break_s": maxBreak,
		"arrive_m": shared("arrival_perimeter_m"), "almost_there_m": shared("zone_radius_m"), "away_from_spot_m": own("away_from_spot_m"),
		"low_stock_qty": shared("restock_low_qty"), "scooter_capacity_qty": own("scooter_capacity_qty"),
		"min_phone_battery_pct": own("min_phone_battery_pct"), "min_scooter_battery_pct": own("min_scooter_battery_pct"),
		"low_scooter_battery_pct": own("low_scooter_battery_pct"), "ride_speed_kmh": shared("straight_line_speed_kmh"),
		"break_options_s": breaks, "add_time_options_s": addTimeOptionsS,
		"skip_reasons": SkipReasons, "problem_types": ProblemTypes, "checklist": Checklist,
		"stock_request_ask_before_s": own("stock_request_ask_before_s"), "stock_request_reminder_before_s": own("stock_request_reminder_before_s"),
		"shift_ending_soon_before_s": own("shift_ending_soon_before_s"), "break_extension_s": own("break_extension_s"),
		"not_at_gate_after_s": own("not_at_gate_after_s"), "handover_code_length": HandoverCodeLength,
		"cancel_pay_cap":        map[string]any{"amount_minor": own("cancel_pay_cap_paise"), "currency": "INR"},
		"verification_within_s": own("verification_within_s"),
	}
}
