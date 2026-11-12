package geo

import (
	"errors"
	"testing"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/dbtest"
)

// The tier settings (#199): their starting values, their ranges, and the rules between them.
func TestTheTierSettings(t *testing.T) {
	pool := dbtest.Fresh(t)
	e := load(t, pool, "")
	for name, want := range map[string]int{"timer_zone_margin_s": 15, "outer_ring_budget_s": 300} {
		if got, ok := e.Get(name); !ok || got != want {
			t.Errorf("%s: %d, %v; want %d", name, got, ok, want)
		}
	}
	for _, c := range []struct {
		name string
		ch   []Change
	}{
		{"margin as long as the timer", []Change{{"timer_length_s", ptr(60)}, {"timer_zone_margin_s", ptr(60)}}},
		{"outer ring inside the sweet spot", []Change{{"outer_ring_budget_s", ptr(60)}}},
		{"sweet spot past the outer ring", []Change{{"sweet_spot_budget_s", ptr(400)}}},
	} {
		if _, err := change(t, pool, "", c.ch...); !errors.Is(err, ErrInvalidSetting) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// Ranges are checked where changes are read (ParseChanges), as for every setting.
	for _, arg := range []string{"timer_zone_margin_s=121", "timer_zone_margin_s=-1", "outer_ring_budget_s=29", "outer_ring_budget_s=3601"} {
		if _, err := ParseChanges([]string{arg}); !errors.Is(err, ErrInvalidSetting) {
			t.Errorf("%s: %v", arg, err)
		}
	}
	// Both together are fine: a wider sweet spot with a wider outer ring.
	if _, err := change(t, pool, "", Change{"sweet_spot_budget_s", ptr(400)}, Change{"outer_ring_budget_s", ptr(600)}); err != nil {
		t.Fatal(err)
	}
	// A zone cannot break the rule either.
	zone := newZone(t, pool, "Tier zone (test)")
	if _, err := change(t, pool, zone, Change{"timer_zone_margin_s", ptr(90)}); !errors.Is(err, ErrInvalidSetting) {
		t.Errorf("zone margin as long as the timer: %v", err)
	}
}
