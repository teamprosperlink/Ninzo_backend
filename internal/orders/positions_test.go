package orders

import (
	"context"
	"testing"
	"time"
)

// Every ping keeps its grid cell and moves the rider's position, through the live ping and the
// offline batch alike; an older ping arriving late never moves the rider back (#207).
func TestPingsKeepACellAndMoveTheRidersPosition(t *testing.T) {
	tr := begin(t, nil)
	ctx := context.Background()
	var rider string
	if err := tr.pool.QueryRow(ctx, "SELECT rider_id::text FROM offers WHERE id = $1", tr.offer).Scan(&rider); err != nil {
		t.Fatal(err)
	}
	position := func() (lat float64, device time.Time, cell *string) {
		t.Helper()
		if err := tr.pool.QueryRow(ctx, "SELECT lat, device_time, cell FROM rider_positions WHERE rider_id = $1", rider).Scan(&lat, &device, &cell); err != nil {
			t.Fatal(err)
		}
		return
	}
	// A live ping.
	tr.at(atGate, 5*time.Second)
	lat, device, cell := position()
	if lat != atGate[0] || !device.Equal(tr.clock.Truncate(time.Microsecond)) || cell == nil {
		t.Fatalf("after a live ping: %v %v %v", lat, device, cell)
	}
	var pingCell *string
	if err := tr.pool.QueryRow(ctx, "SELECT cell FROM location_pings WHERE rider_id = $1 ORDER BY id DESC LIMIT 1", rider).Scan(&pingCell); err != nil {
		t.Fatal(err)
	}
	if pingCell == nil || *pingCell != *cell {
		t.Fatalf("the ping's cell %v, the position's %v", pingCell, *cell)
	}
	newest := tr.clock.Truncate(time.Microsecond) // the database keeps microseconds
	// A queued ping from before, sent in the offline batch: kept, but the rider stays where they are.
	code, out := tr.batch(locationEvent("evt-loc-0207", atGateToo, newest.Add(-20*time.Second)))
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if lat, device, _ = position(); lat != atGate[0] || !device.Equal(newest) {
		t.Fatalf("a late batch ping moved the rider back: %v %v", lat, device)
	}
	// A newer one in the batch moves the rider.
	tr.clock = tr.clock.Add(10 * time.Second)
	code, out = tr.batch(locationEvent("evt-loc-0208", atGateToo, tr.clock))
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if lat, device, _ = position(); lat != atGateToo[0] || !device.Equal(tr.clock.Truncate(time.Microsecond)) {
		t.Fatalf("a newer batch ping did not move the rider: %v %v", lat, device)
	}
	var pings, withCell int
	if err := tr.pool.QueryRow(ctx, "SELECT count(*), count(cell) FROM location_pings WHERE rider_id = $1", rider).Scan(&pings, &withCell); err != nil {
		t.Fatal(err)
	}
	if pings < 3 || withCell != pings {
		t.Fatalf("%d pings, %d with a cell", pings, withCell)
	}
}
