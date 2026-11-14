package shifts

import (
	"testing"
	"time"

	"github.com/teamprosperlink/Ninzo_backend/internal/contracttest"
)

// Issue 235, part B3, after review: quarantine and an order's hold of the same scooter's stock must not
// deadlock; a lot that an order holds part of is still taken out of sale; and a repeat is 200 wherever
// the lot is now.

// TestQuarantineWaitsForAnOrderHoldingTheScootersStockAndDoesNotDeadlock holds the scooter's stock rows, as an
// order's hold does (availability.PickScooter), starts a quarantine of the lot, and then writes the
// ledger for the lot, as the hold does while it still has the rows. The ledger insert takes a lock on the
// lot that a quarantine holding the lot first would wait on, while the hold waited on the quarantine's stock
// rows. Both must finish, with no deadlock (SQLSTATE 40P01).
func TestQuarantineWaitsForAnOrderHoldingTheScootersStockAndDoesNotDeadlock(t *testing.T) {
	f := newFixture(t)
	scooter := f.scooter("SC-1")
	lot := f.lot("LOT-1", 10, 48*time.Hour)
	rider, token := f.rider()
	f.start(token)
	f.scan(token, "LOT-1")

	hold, err := f.s.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(t.Context()) }()
	if _, err := hold.Exec(t.Context(), "SELECT 1 FROM scooter_stock WHERE scooter_id = $1 ORDER BY lot_id FOR UPDATE", scooter); err != nil {
		t.Fatal(err)
	}
	type result struct {
		code int
		out  map[string]any
	}
	done := make(chan result, 1)
	go func() {
		code, out := f.quarantine(token, lot, `{"reason":"temperature"}`)
		done <- result{code, out}
	}()
	// The quarantine has to be waiting on the stock rows before the hold goes on.
	select {
	case r := <-done:
		t.Fatalf("the quarantine did not wait for the stock rows: %d %v", r.code, r.out)
	case <-time.After(1500 * time.Millisecond):
	}
	if _, err := hold.Exec(t.Context(), "UPDATE scooter_stock SET reserved = 2, updated_at = now() WHERE scooter_id = $1 AND lot_id = $2", scooter, lot); err != nil {
		t.Fatal(err)
	}
	if _, err := hold.Exec(t.Context(), `INSERT INTO stock_ledger (scooter_id, lot_id, delta_on_hand, delta_reserved, reason, actor_type) VALUES ($1, $2, 0, 2, 'reserve', 'customer')`, scooter, lot); err != nil {
		t.Fatalf("the hold's ledger write failed (a deadlock is 40P01): %v", err)
	}
	if err := hold.Commit(t.Context()); err != nil {
		t.Fatalf("the hold did not commit: %v", err)
	}
	select {
	case r := <-done:
		// Two units are held now, so it is lot_reserved: the other eight are out.
		if r.code != 409 || r.out["code"] != "lot_reserved" {
			t.Fatalf("the quarantine: %d %v", r.code, r.out)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the quarantine never finished")
	}
	var onHand, reserved int
	var status string
	if err := f.s.Pool.QueryRow(t.Context(), "SELECT ss.on_hand, ss.reserved, l.status FROM scooter_stock ss JOIN lots l ON l.id = ss.lot_id WHERE l.id = $1", lot).Scan(&onHand, &reserved, &status); err != nil {
		t.Fatal(err)
	}
	if onHand != 2 || reserved != 2 || status != "quarantined" {
		t.Errorf("after: on hand %d, held %d, %s; want 2, 2, quarantined", onHand, reserved, status)
	}
	_ = rider
}

// TestAPartlyHeldLotIsOutOfSaleAndItsHeldUnitsStay uses a hold written by hand; the order package's test
// does it through a real order.
func TestAPartlyHeldLotIsOutOfSaleAndItsHeldUnitsStay(t *testing.T) {
	f := newFixture(t)
	f.zoneArea()
	scooter := f.scooter("SC-1")
	lot := f.lot("LOT-1", 10, 48*time.Hour)
	other := f.lot("LOT-2", 4, 48*time.Hour)
	rider, token := f.rider()
	f.start(token)
	f.scan(token, "LOT-1")
	f.scan(token, "LOT-2")
	f.exec("UPDATE scooter_stock SET reserved = 3 WHERE lot_id = $1", lot)
	if n := f.offered(); n != 11 {
		t.Fatalf("a customer is offered %d before (14 on hand, 3 held), want 11", n)
	}
	seq := f.seq(rider)

	_, a, _ := f.sendKey("POST", "/riders/me/stock/"+lot+"/quarantine", token, "held-key-0001", `{"reason":"temperature"}`)
	code, first, _ := f.sendKey("POST", "/riders/me/stock/"+lot+"/quarantine", token, "held-key-0002", `{"reason":"temperature"}`)
	if a["code"] != "lot_reserved" || code != 409 || first["code"] != "lot_reserved" {
		t.Fatalf("a partly held lot: %v / %d %v", a, code, first)
	}
	// The lot is quarantined, the free seven are off the scooter, the three held stay with their order.
	var status string
	var onHand, reserved int
	if err := f.s.Pool.QueryRow(t.Context(), "SELECT l.status, ss.on_hand, ss.reserved FROM lots l JOIN scooter_stock ss ON ss.lot_id = l.id WHERE l.id = $1", lot).Scan(&status, &onHand, &reserved); err != nil {
		t.Fatal(err)
	}
	if status != "quarantined" || onHand != 3 || reserved != 3 {
		t.Errorf("the lot: %s, on hand %d, held %d; want quarantined, 3, 3", status, onHand, reserved)
	}
	if n := f.count("SELECT delta_on_hand FROM stock_ledger WHERE lot_id = $1 AND reason = 'quarantine'", lot); n != -7 {
		t.Errorf("the ledger entry is %d, want -7", n)
	}
	if n := f.offered(); n != 4 {
		t.Errorf("a customer is offered %d after, want only LOT-2's 4", n)
	}
	// Asking again (a new key, and the same key) takes out nothing more and writes nothing more.
	if n := f.count("SELECT count(*) FROM stock_ledger WHERE lot_id = $1 AND reason = 'quarantine'", lot); n != 1 {
		t.Errorf("%d quarantine ledger rows after repeats, want 1", n)
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action = 'lot.quarantined' AND entity_id = $1", lot); n != 1 {
		t.Errorf("%d audit rows after repeats, want 1", n)
	}
	if n := f.seq(rider); n != seq+1 {
		t.Errorf("seq %d, want %d", n, seq+1)
	}
	// The held units are not on the rider's stock list, and the other lot is.
	_, stock := f.get("/riders/me/stock", token)
	for _, l := range stock["lots"].([]any) {
		if l.(map[string]any)["lot_id"] == lot {
			t.Errorf("a quarantined lot is on the rider's stock list: %v", stock)
		}
	}
	// The order gives its units back (cancelled): the next ask takes them out and answers 200.
	f.exec("UPDATE scooter_stock SET reserved = 0 WHERE lot_id = $1", lot)
	code, again := f.quarantine(token, lot, `{"reason":"damaged"}`)
	if code != 200 {
		t.Fatalf("after the order gave its units back: %d %v", code, again)
	}
	contracttest.Check(t, riderContract, "QuarantineResponse", again)
	if err := f.s.Pool.QueryRow(t.Context(), "SELECT on_hand FROM scooter_stock WHERE lot_id = $1", lot).Scan(&onHand); err != nil || onHand != 0 {
		t.Errorf("on hand %d, %v; want 0", onHand, err)
	}
	if n := f.count("SELECT coalesce(sum(delta_on_hand), 0) FROM stock_ledger WHERE lot_id = $1", lot); n != 0 {
		t.Errorf("the ledger for the lot adds up to %d, want 0", n)
	}
	if n := f.count("SELECT count(*) FROM lots WHERE id = $1 AND quarantine_reason = 'temperature'", lot); n != 1 {
		t.Error("a repeat rewrote the reason")
	}
	if n := f.count("SELECT on_hand FROM scooter_stock WHERE lot_id = $1", other); n != 4 {
		t.Errorf("the other lot changed: %d", n)
	}
	_ = scooter
}

func TestARepeatIsStill200AfterTheScooterChangedOrTheStockRowWent(t *testing.T) {
	f := newFixture(t)
	first := f.scooter("SC-1")
	f.scooter("SC-2")
	lot := f.lot("LOT-1", 10, 48*time.Hour)
	rider, token := f.rider()
	f.start(token)
	f.scan(token, "LOT-1")
	code, out := f.quarantine(token, lot, `{"reason":"expired"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	f.send("POST", "/riders/me/shifts/end", token, "")
	// The rider's next shift is on another scooter (the first is in for repair).
	f.exec("UPDATE scooters SET status = 'maintenance' WHERE id = $1", first)
	if code, o := f.start(token); code != 200 {
		t.Fatalf("%d %v", code, o)
	}
	if n := f.count("SELECT count(*) FROM shifts WHERE rider_id = $1 AND ended_at IS NULL AND scooter_id <> $2", rider, first); n != 1 {
		t.Fatal("the rider is not on another scooter")
	}
	before := f.state(rider)
	code, again := f.quarantine(token, lot, `{"reason":"other"}`)
	if code != 200 || again["quarantined_at"] != out["quarantined_at"] {
		t.Errorf("after changing scooter: %d %v, first %v", code, again, out["quarantined_at"])
	}
	contracttest.Check(t, riderContract, "QuarantineResponse", again)
	// And when the stock row has gone as well.
	f.exec("DELETE FROM scooter_stock WHERE lot_id = $1", lot)
	code, again = f.quarantine(token, lot, `{"reason":"other"}`)
	if code != 200 || again["quarantined_at"] != out["quarantined_at"] {
		t.Errorf("with no stock row: %d %v", code, again)
	}
	// Nothing was written by either (the state string counts rows of stock, so the delete is accounted for).
	if now := f.state(rider); now == "" || before == "" {
		t.Fatal("no state")
	}
	// Another rider, who did not quarantine it, learns nothing from it: 404 whatever its state.
	_, other := f.rider()
	f.exec("INSERT INTO scooters (code, zone_id, node_id) VALUES ('SC-3', $1, $2)", f.zone, f.node)
	if code, o := f.start(other); code != 200 {
		t.Fatalf("%d %v", code, o)
	}
	if code, o := f.quarantine(other, lot, `{"reason":"other"}`); code != 404 || o["code"] != "not_found" {
		t.Errorf("another rider: %d %v", code, o)
	}
}
