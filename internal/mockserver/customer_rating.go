package mockserver

import (
	"math"
	"time"
)

// Customer ratings (Issue 265, part A). The mock keeps nothing: it answers from the order story, so an
// order is rated or not by its scenario, never by an earlier call.

// ratingReasons is the fixed list. There is no free text.
var ratingReasons = map[string]bool{"late": true, "cold_food": true, "rude": true, "wrong_items": true, "other": true}

// sampleOrderRating is the customer's rating that the already_rated scenario shows.
func sampleOrderRating(at time.Time) map[string]any {
	return map[string]any{"stars": 4, "reason": "late", "rated_at": stamp(at.Add(time.Minute))}
}

// rateOrder follows rateOrder in the customer contract. Order of checks: another customer's order (404),
// the body (400), then the order's own state (409 not_delivered, already_rated, rating_window_closed).
func rateOrder(c *call) {
	if c.param("order_id") == "ord_missing" {
		c.fail(404, "not_found", "No such order.", false)
		return
	}
	body, ok := needsBody(c, "stars")
	if !ok {
		return
	}
	for key := range body {
		if key != "stars" && key != "reason" {
			c.fail(400, "invalid_request", "Send stars and, if you like, reason.", false)
			return
		}
	}
	stars, isNumber := body["stars"].(float64)
	if !isNumber || stars != math.Trunc(stars) || stars < 1 || stars > 5 {
		c.fail(400, "invalid_request", "stars must be a whole number from 1 to 5.", false)
		return
	}
	var reason any
	if raw, present := body["reason"]; present {
		text, isText := raw.(string)
		if !isText || !ratingReasons[text] {
			c.fail(400, "invalid_request", "reason must be late, cold_food, rude, wrong_items or other.", false)
			return
		}
		reason = text
	}
	order := currentOrder(c.scenario, c.now)
	switch {
	case order["customer_stage"] != "handed_over":
		c.fail(409, "not_delivered", "This order was not handed over to you.", false)
	case order["rating"] != nil:
		c.fail(409, "already_rated", "You have already rated this order.", false)
	case order["can_rate"] != true:
		c.fail(409, "rating_window_closed", "The time to rate this order has passed.", false)
	default:
		order["seq"] = order["seq"].(int) + 1
		order["rating"] = map[string]any{"stars": int(stars), "reason": reason, "rated_at": stamp(c.now)}
		order["can_rate"] = false
		c.json(200, map[string]any{"order": order})
	}
}
