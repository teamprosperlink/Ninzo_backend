package orders

import (
	"regexp"
	"testing"
	"time"
)

func TestOrderNumberIsALetterAndFourDigits(t *testing.T) {
	for _, c := range []struct {
		n    int64
		want string
	}{
		{0, "A0000"}, {7, "A0007"}, {1001, "A1001"}, {2931, "A2931"}, {9999, "A9999"},
		{10000, "B0000"}, {12931, "B2931"}, {259999, "Z9999"}, {260000, "A0000"}, {260007, "A0007"},
	} {
		if got := OrderNumber(c.n); got != c.want {
			t.Errorf("OrderNumber(%d) = %s, want %s", c.n, got, c.want)
		}
	}
}

// Issue 158: the same readable number on the customer's order, the offer and the rider's order.
func TestAnOrderHasOneReadableNumberEverywhere(t *testing.T) {
	x := withRiders(newFixture(t))
	start := x.clock
	id := x.placed(x.chai, 1)
	x.pay(id, "paid")
	_, tok := x.signIn(x.scooter)
	x.dispatch(start.Add(6 * time.Second))
	no := x.count("SELECT order_no FROM orders WHERE id = $1", id)
	if no < 1001 {
		t.Fatalf("order_no %d", no)
	}
	want := OrderNumber(int64(no))
	if !regexp.MustCompile(`^[A-Z][0-9]{4}$`).MatchString(want) {
		t.Fatalf("bad form %s", want)
	}
	if _, out := x.send("GET", "/orders/"+id, x.token, ""); orderOf(out)["order_number"] != want {
		t.Errorf("customer order: %v", out)
	}
	o := x.offer(tok)
	if o["order_number"] != want {
		t.Errorf("offer: %v", o)
	}
	code, out := x.call("POST", "/offers/"+o["id"].(string)+"/accept", tok, `{"fencing_token":"`+o["fencing_token"].(string)+`"}`)
	if code != 200 || orderOf(out)["order_number"] != want {
		t.Errorf("rider order: %d %v", code, out)
	}
	// The rider's state carries the shift with the new fields.
	_, state := x.call("GET", "/riders/me/state", tok, "")
	sh, _ := state["shift"].(map[string]any)
	if sh == nil || sh["node_assigned_at"] == nil || sh["scooter"] == nil {
		t.Errorf("shift in state: %v", state["shift"])
	}
	if sc, _ := sh["scooter"].(map[string]any); sc == nil || sc["plate"] == nil {
		t.Errorf("scooter in state: %v", sh)
	}
}
