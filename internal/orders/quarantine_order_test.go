package orders

import (
	"testing"
	"time"

	"github.com/teamprosperlink/Ninzo_backend/internal/availability"
)

// Issue 235, part B3, after review: a rider quarantines a lot an order holds part of, through a real order
// hold made by the customer API and a real quarantine made by the rider API.

func (x *riders) chaiOffered() int {
	x.t.Helper()
	res, err := availability.At(x.t.Context(), x.pool, gateLat, gateLng, time.Now())
	if err != nil {
		x.t.Fatal(err)
	}
	for _, l := range res.Lines {
		if l.Item.ID == x.chai {
			return l.QtyLeft
		}
	}
	return 0
}

func TestQuarantiningALotAnOrderHoldsPartOfKeepsTheOrdersUnitsAndTakesTheRestOutOfSale(t *testing.T) {
	x := withRiders(newFixture(t))
	id := x.placed(x.chai, 1) // One unit, from L-CHAI-2: it expires first, and holds 2 units of its own.
	_, tok := x.signIn(x.scooter)
	lot := x.id("SELECT id::text FROM lots WHERE code = 'L-CHAI-2'")
	if n := x.count("SELECT reserved FROM scooter_stock WHERE lot_id = $1", lot); n != 1 {
		t.Fatalf("the order holds %d of the lot, want 1", n)
	}
	if n := x.chaiOffered(); n != 4 {
		t.Fatalf("a customer is offered %d chai before (3 + 2 on hand, 1 held), want 4", n)
	}

	code, out := x.call("POST", "/riders/me/stock/"+lot+"/quarantine", tok, `{"reason":"temperature","note":"warm to the touch"}`)
	if code != 409 || out["code"] != "lot_reserved" {
		t.Fatalf("a lot an order holds part of: %d %v", code, out)
	}
	// The lot is quarantined, the free unit is off the scooter, and the held unit stays with its order.
	if n := x.count("SELECT count(*) FROM lots WHERE id = $1 AND status = 'quarantined'", lot); n != 1 {
		t.Error("the lot is not quarantined")
	}
	if n := x.count("SELECT on_hand FROM scooter_stock WHERE lot_id = $1", lot); n != 1 {
		t.Errorf("on hand %d, want the 1 held unit", n)
	}
	if n := x.count("SELECT reserved FROM scooter_stock WHERE lot_id = $1", lot); n != 1 {
		t.Errorf("held %d, want 1", n)
	}
	if n := x.count("SELECT count(*) FROM reservations WHERE order_id = $1 AND lot_id = $2 AND status = 'held' AND qty = 1", id, lot); n != 1 {
		t.Error("the order lost its held unit")
	}
	if n := x.count("SELECT delta_on_hand FROM stock_ledger WHERE lot_id = $1 AND reason = 'quarantine'", lot); n != -1 {
		t.Errorf("the quarantine ledger entry is %d, want -1", n)
	}
	if n := x.chaiOffered(); n != 3 {
		t.Errorf("a customer is offered %d chai after, want only L-CHAI-1's 3", n)
	}
	// A second customer cannot be promised the quarantined lot's free unit any more: two chai still fit L-CHAI-1.
	if code, out := x.order(x.chai, 4); code == 201 {
		t.Errorf("an order for 4 chai was taken with only 3 on sale: %v", out)
	}
	// Asking again with the lot still held changes nothing.
	code, out = x.call("POST", "/riders/me/stock/"+lot+"/quarantine", tok, `{"reason":"temperature"}`)
	if code != 409 || out["code"] != "lot_reserved" {
		t.Errorf("a repeat: %d %v", code, out)
	}
	if n := x.count("SELECT count(*) FROM stock_ledger WHERE lot_id = $1 AND reason = 'quarantine'", lot); n != 1 {
		t.Errorf("%d quarantine ledger entries, want 1", n)
	}
	// The customer cancels: the order gives its unit back, and asking again takes it out and answers 200.
	if code, out := x.send("POST", "/orders/"+id+"/cancel", x.token, `{}`); code != 200 {
		t.Fatalf("cancel: %d %v", code, out)
	}
	if n := x.count("SELECT reserved FROM scooter_stock WHERE lot_id = $1", lot); n != 0 {
		t.Fatalf("the cancelled order still holds %d", n)
	}
	code, out = x.call("POST", "/riders/me/stock/"+lot+"/quarantine", tok, `{"reason":"temperature"}`)
	if code != 200 {
		t.Fatalf("after the order gave its unit back: %d %v", code, out)
	}
	if n := x.count("SELECT on_hand FROM scooter_stock WHERE lot_id = $1", lot); n != 0 {
		t.Errorf("on hand %d, want 0", n)
	}
	if n := x.count("SELECT coalesce(sum(delta_on_hand), 0) FROM stock_ledger WHERE lot_id = $1 AND reason = 'quarantine'", lot); n != -2 {
		t.Errorf("the quarantine entries add up to %d, want -2 (the lot held 2; the fixture wrote no scan entry)", n)
	}
	if n := x.chaiOffered(); n != 3 {
		t.Errorf("a customer is offered %d chai at the end, want 3", n)
	}
}
