package mockserver

import "time"

// Every customer response about an order comes from one story, so GET, the stream
// and the actions always agree. GET returns one point in the story, and the stream
// replays all of it.

const (
	orderID      = "ord_mock0001"
	cancelWindow = 5 * time.Second
	maxWait      = 5 * time.Minute
	chooseWindow = 2 * time.Minute
	getStep      = 30 * time.Second
)

type orderStep struct {
	stage string
	apply func(order map[string]any, at time.Time)
}

type orderStory struct {
	steps     []orderStep
	getAt     int           // The step GET returns. -1 means the last.
	age       time.Duration // How long ago payment was confirmed.
	timerLead time.Duration // How long before its first event the timer started.
	refund    string        // The refund reason, if this story ends with one.
}

var (
	stepLooking      = orderStep{stage: "looking_for_rider"}
	stepAfterWindow  = orderStep{stage: "looking_for_rider"}
	stepAccepted     = orderStep{stage: "rider_accepted"}
	stepTimer        = orderStep{stage: "timer_running"}
	stepArrived      = orderStep{stage: "arrived"}
	stepHandedOver   = orderStep{stage: "handed_over"}
	stepPayment      = orderStep{stage: "payment"}
	stepPaymentFails = orderStep{stage: "payment_failed"}
	stepNoScooter    = orderStep{stage: "no_scooter", apply: func(o map[string]any, at time.Time) {
		o["eta_offer"] = map[string]any{"id": "eta_mock01", "eta_s": 600, "expires_at": stamp(at.Add(chooseWindow))}
	}}
)

func stepEnded(reason string) orderStep {
	return orderStep{stage: "ended", apply: func(o map[string]any, _ time.Time) { o["ended_reason"] = reason }}
}

func fullFlow() []orderStep {
	return []orderStep{stepLooking, stepAfterWindow, stepAccepted, stepTimer, stepArrived, stepHandedOver}
}

func storyFor(scenario string) orderStory {
	lookingOnly := []orderStep{stepLooking, stepAfterWindow}
	accepted := []orderStep{stepLooking, stepAfterWindow, stepAccepted}
	switch scenario {
	case "awaiting_payment":
		return orderStory{steps: []orderStep{stepPayment}, getAt: -1}
	case "payment_declined":
		return orderStory{steps: []orderStep{stepPayment, stepPaymentFails}, getAt: -1}
	case "no_scooter", "offer_timeout", "eta_offer_expired":
		return orderStory{steps: append(lookingOnly, stepNoScooter), getAt: -1}
	case "no_scooter_no_offer":
		bare := stepNoScooter
		bare.apply = nil
		return orderStory{steps: append(lookingOnly, bare), getAt: -1}
	case "offer_accepted":
		return orderStory{steps: accepted, getAt: -1}
	case "rider_already_near":
		return orderStory{steps: []orderStep{stepLooking, stepAccepted, stepTimer, stepArrived, stepHandedOver}}
	case "after_cancel_window", "cancel_too_late":
		return orderStory{steps: lookingOnly, getAt: -1}
	case "max_wait_passed":
		return orderStory{steps: []orderStep{stepLooking}, getAt: -1, age: maxWait + time.Minute}
	case "timer_running":
		return orderStory{steps: append(accepted, stepTimer), getAt: -1}
	case "timer_missed":
		missed := orderStep{stage: "timer_running", apply: func(o map[string]any, _ time.Time) {
			o["eta_source"], o["eta_s"] = "unavailable", nil
			o["remedy"] = map[string]any{"type": "credit", "amount": money(5000)}
			o["timer"].(map[string]any)["timer_missed"] = true
		}}
		return orderStory{steps: append(accepted, missed), getAt: -1, age: 3 * time.Minute, timerLead: 2 * time.Minute}
	case "signal_lost":
		lost := orderStep{stage: "timer_running", apply: func(o map[string]any, _ time.Time) {
			o["eta_source"], o["eta_s"] = "unavailable", nil
			o["handover_otp"] = "4821"
			o["remedy"] = map[string]any{"type": "credit", "amount": money(5000)}
			o["timer"].(map[string]any)["timer_missed"] = true
		}}
		return orderStory{steps: append(accepted, lost), getAt: -1, age: 3 * time.Minute, timerLead: 2 * time.Minute}
	case "arrived":
		return orderStory{steps: fullFlow()[:5], getAt: -1}
	case "rider_inside_perimeter":
		return orderStory{steps: append(accepted, stepArrived, stepHandedOver), getAt: 3}
	case "handed_over":
		return orderStory{steps: fullFlow(), getAt: -1}
	case "missing_food":
		return orderStory{steps: append(fullFlow()[:5], orderStep{stage: "handed_over", apply: func(o map[string]any, _ time.Time) {
			o["payment_status"] = "refunded"
		}}), getAt: -1, refund: "missing_food"}
	case "cancelled":
		return orderStory{steps: []orderStep{stepLooking, stepEnded("cancelled")}, getAt: -1, refund: "customer_cancelled"}
	case "cancelled_after_deadline":
		return orderStory{steps: []orderStep{stepLooking, stepEnded("cancelled")}, getAt: -1, age: maxWait + time.Minute, refund: "wait_deadline_passed"}
	case "timed_out":
		return orderStory{steps: append(lookingOnly, stepEnded("timed_out")), getAt: -1, refund: "no_rider_found"}
	case "failed":
		return orderStory{steps: append(accepted, stepEnded("failed")), getAt: -1, refund: "order_failed"}
	case "temperature_failed":
		return orderStory{steps: append(accepted, stepEnded("failed")), getAt: -1, refund: "temperature_failed"}
	}
	return orderStory{steps: fullFlow()}
}

// buildStory makes the orders for each step. paid is when payment was confirmed, and
// step is the time between events. Each stored timer value is set once and never changes.
func buildStory(story orderStory, paid time.Time, step time.Duration) []map[string]any {
	orders := make([]map[string]any, 0, len(story.steps))
	// A replay can be faster than the real window, so the window ends before the second event.
	window := cancelWindow
	if step <= window {
		window = step / 2
	}
	cancelUntil := paid.Add(window)
	if story.age > 0 {
		cancelUntil = paid.Add(cancelWindow)
	}
	deadline := paid.Add(maxWait)
	var timerStart, timerStop any
	var duration any
	riderShown := false
	for i, s := range story.steps {
		at := paid.Add(story.age).Add(time.Duration(i) * step)
		switch s.stage {
		case "timer_running":
			if timerStart == nil {
				timerStart, duration = stamp(at.Add(-story.timerLead)), 90
			}
		case "arrived":
			if timerStart != nil {
				timerStop = stamp(at)
			}
		case "rider_accepted":
			riderShown = true
		}
		o := map[string]any{
			"id": orderID, "seq": i + 1, "customer_stage": s.stage, "ended_reason": nil,
			"gate":  map[string]any{"id": "gate_mock01", "label": "Main gate"},
			"items": []map[string]any{{"item_id": "item_burger", "name": "Burger", "qty": 1, "unit_price": money(14900)}},
			"total": money(14900), "payment_status": "paid", "payment_request": nil,
			"cancellable": cancellableAt(s.stage, at, cancelUntil, deadline), "cancel_until": stamp(cancelUntil), "wait_deadline": nil,
			"eta_source": "placeholder", "eta_s": nil, "eta_offer": nil, "rider": nil,
			"timer":  map[string]any{"timer_started_at": timerStart, "timer_duration_s": duration, "timer_stopped_at": timerStop, "timer_missed": false},
			"remedy": nil, "handover_otp": nil, "created_at": stamp(paid.Add(-time.Minute)), "order_number": "A2931",
		}
		if riderShown {
			o["rider"] = map[string]any{"first_name": "Ravi"}
		}
		switch s.stage {
		case "payment":
			o["payment_status"], o["cancel_until"], o["payment_request"] = "pending", nil, paymentRequest()
		case "payment_failed":
			o["payment_status"], o["cancel_until"] = "failed", nil
		case "looking_for_rider":
			o["wait_deadline"] = stamp(deadline)
		case "no_scooter":
			o["wait_deadline"] = stamp(at.Add(chooseWindow))
		case "rider_accepted":
			o["wait_deadline"] = stamp(deadline)
			o["eta_source"], o["eta_s"] = "measured", 150
		case "timer_running":
			o["eta_source"], o["eta_s"] = "measured", 60
		case "arrived":
			o["eta_source"], o["handover_otp"] = "unavailable", "4821"
		case "handed_over":
			o["eta_source"] = "unavailable"
		case "ended":
			o["eta_source"], o["payment_status"] = "unavailable", "refunded"
		}
		if s.apply != nil {
			s.apply(o, at)
		}
		orders = append(orders, o)
	}
	return orders
}

// cancellableAt follows Cancels and refunds in contracts/README.md.
func cancellableAt(stage string, at, cancelUntil, deadline time.Time) bool {
	switch stage {
	case "payment", "payment_failed", "no_scooter":
		return true
	case "looking_for_rider":
		return at.Before(cancelUntil) || at.After(deadline)
	case "rider_accepted":
		return at.After(deadline)
	}
	return false
}

// currentOrder is the order GET returns for a scenario.
func currentOrder(scenario string, now time.Time) map[string]any {
	story := storyFor(scenario)
	at := story.getAt
	if at < 0 {
		at = len(story.steps) - 1
	}
	paid := now.Add(-story.age).Add(-time.Duration(at) * getStep)
	return buildStory(story, paid, getStep)[at]
}

// cancelledFrom ends an order the customer was allowed to cancel.
func cancelledFrom(o map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range o {
		out[k] = v
	}
	out["seq"] = o["seq"].(int) + 1
	out["customer_stage"], out["ended_reason"], out["cancellable"] = "ended", "cancelled", false
	out["payment_request"], out["wait_deadline"], out["eta_s"], out["eta_offer"] = nil, nil, nil, nil
	out["eta_source"] = "unavailable"
	if o["payment_status"] == "paid" {
		out["payment_status"] = "refunded"
	} else {
		out["payment_status"] = "not_paid"
	}
	return out
}

func paymentRequest() map[string]any {
	return map[string]any{"razorpay_key_id": "rzp_test_mockkey", "razorpay_order_id": "order_mock0001", "amount": money(14900)}
}
