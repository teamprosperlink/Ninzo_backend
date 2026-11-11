package availability

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/database"
)

// A rider on a break neither gets offers nor has stock shown or held on the scooter (issue 159,
// agreed with the geo session).
func TestARiderOnABreakHasNoStockShownOrHeld(t *testing.T) {
	f := newFixture(t)
	basket := map[string]int{f.chai: 1}
	pick := func() Pick {
		t.Helper()
		var p Pick
		if err := database.InTx(context.Background(), f.pool, func(tx pgx.Tx) error {
			var err error
			p, err = PickScooter(context.Background(), tx, custLat, custLng, time.Now(), basket)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if res := f.at(custLat, custLng); !res.Offer || qty(res, f.chai) != 8 {
		t.Fatalf("before the break: %+v", res)
	}
	if p := pick(); p.ScooterID != f.scooter || p.Reason != "" {
		t.Fatalf("before the break: %+v", p)
	}

	f.exec("UPDATE shifts SET break_started_at = now(), break_ends_at = now() + interval '30 minutes' WHERE ended_at IS NULL")
	if res := f.at(custLat, custLng); res.Offer || res.Reason != ReasonNoCoverage || len(res.Lines) != 0 {
		t.Errorf("stock shown for a scooter whose rider is on a break: %+v", res)
	}
	if p := pick(); p.ScooterID != "" || p.Reason != ReasonNoCoverage {
		t.Errorf("stock held on a scooter whose rider is on a break: %+v", p)
	}

	f.exec("UPDATE shifts SET break_started_at = NULL, break_ends_at = NULL WHERE ended_at IS NULL")
	if res := f.at(custLat, custLng); !res.Offer || qty(res, f.chai) != 8 {
		t.Errorf("after the break: %+v", res)
	}
	if p := pick(); p.ScooterID != f.scooter {
		t.Errorf("after the break: %+v", p)
	}
}
