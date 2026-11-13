package demand

import (
	"math"
	"testing"
	"time"
)

func ist(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, india)
}

func TestSlotsAreIndianTime(t *testing.T) {
	for _, c := range []struct {
		at   time.Time
		day  string
		band int
	}{
		{ist(2026, 11, 13, 0, 0), "2026-11-13", 0},
		{ist(2026, 11, 13, 0, 29), "2026-11-13", 0},
		{ist(2026, 11, 13, 0, 30), "2026-11-13", 1},
		{ist(2026, 11, 13, 23, 59), "2026-11-13", 47},
		// 18:45 UTC is 00:15 IST the next day.
		{time.Date(2026, 11, 12, 18, 45, 0, 0, time.UTC), "2026-11-13", 0},
		{time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC), "2026-11-13", 25},
	} {
		s := SlotOf(c.at)
		if s.Day.Format("2006-01-02") != c.day || s.Band != c.band {
			t.Errorf("%v: %v band %d, want %s band %d", c.at, s.Day.Format("2006-01-02"), s.Band, c.day, c.band)
		}
	}
}

func TestWhoChangesEachDay(t *testing.T) {
	d := ist(2026, 11, 13, 0, 0)
	a, b := who("c1", d), who("c1", d.AddDate(0, 0, 1))
	if string(a) == string(b) || len(a) != 32 || string(a) == string(who("c2", d)) {
		t.Fatal("the hash must differ by day and customer, and be 32 bytes")
	}
	if string(a) != string(who("c1", d)) {
		t.Fatal("the hash must be the same for the same customer and day")
	}
}

func history(since time.Time, vals map[time.Time]float64) History {
	h := History{Since: SlotOf(since).Day, At: map[string]map[Slot]float64{"c": {}}}
	for at, v := range vals {
		h.At["c"][SlotOf(at)] = v
	}
	return h
}

func TestForecastWeightsRecentWeeks(t *testing.T) {
	fri := ist(2026, 11, 13, 19, 0) // band 38
	h := history(ist(2026, 10, 1, 0, 0), map[time.Time]float64{
		fri.AddDate(0, 0, -7):      10,
		fri.AddDate(0, 0, -14):     4,
		fri.AddDate(0, 0, -21):     1,
		fri.AddDate(0, 0, -1):      99, // Thursday: another weekday, never used
		fri.Add(-30 * time.Minute): 99, // another band
	})
	f, ok := h.Forecast("c", SlotOf(fri), 3, 0.5)
	// (10 + 0.5·4 + 0.25·1) / 1.75
	if !ok || math.Abs(f-12.25/1.75) > 1e-12 {
		t.Fatalf("%v %v", f, ok)
	}
	// A day given at UTC midnight, or mid-afternoon, is the same IST day.
	for _, d := range []time.Time{time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC), ist(2026, 11, 13, 15, 0)} {
		if g, _ := h.Forecast("c", Slot{Day: d, Band: 38}, 3, 0.5); math.Abs(g-f) > 1e-12 {
			t.Fatalf("day %v: %v, want %v", d, g, f)
		}
	}
	// decay 1: a plain mean of the three weeks.
	if f, _ = h.Forecast("c", SlotOf(fri), 3, 1); math.Abs(f-5) > 1e-12 {
		t.Fatalf("plain mean %v", f)
	}
	// A week with no row after Since counts as zero demand.
	if f, _ = h.Forecast("c", SlotOf(fri), 4, 1); math.Abs(f-15.0/4) > 1e-12 {
		t.Fatalf("empty week %v", f)
	}
}

func TestForecastSkipsWeeksBeforeData(t *testing.T) {
	fri := ist(2026, 11, 13, 19, 0)
	h := history(fri.AddDate(0, 0, -10), map[time.Time]float64{fri.AddDate(0, 0, -7): 6})
	if f, ok := h.Forecast("c", SlotOf(fri), 4, 1); !ok || f != 6 {
		t.Fatalf("only one week is known: %v %v", f, ok)
	}
	h = history(fri.AddDate(0, 0, -3), nil)
	if _, ok := h.Forecast("c", SlotOf(fri), 4, 1); ok {
		t.Fatal("no week known, yet a forecast")
	}
	for _, bad := range []struct {
		weeks int
		decay float64
	}{{0, 1}, {2, 0}, {2, 1.5}} {
		if _, ok := h.Forecast("c", SlotOf(fri), bad.weeks, bad.decay); ok {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestBacktestMeasuresTheError(t *testing.T) {
	fri := ist(2026, 11, 13, 19, 0)
	h := history(ist(2026, 10, 1, 0, 0), map[time.Time]float64{
		fri.AddDate(0, 0, -7): 4,
		fri:                   6, // forecast 4, so error 2 in this band
	})
	// Bands that are zero on both sides are not scored: one band, error 2.
	mae, n, err := h.Backtest(fri, 1, 1)
	if err != nil || n != 1 || math.Abs(mae-2) > 1e-12 {
		t.Fatalf("mae %v over %d: %v", mae, n, err)
	}
	if _, _, err := (History{Since: fri, At: map[string]map[Slot]float64{}}).Backtest(fri, 1, 1); err == nil {
		t.Fatal("no history, yet a score")
	}
}

func TestNewHistorySumsByValue(t *testing.T) {
	s := SlotOf(ist(2026, 11, 13, 19, 0))
	h := NewHistory([]Count{{Cell: "c", Slot: s, Asked: 3, Paid: 1}, {Cell: "d", Slot: s, Asked: 2}}, s.Day, func(c Count) float64 { return float64(c.Asked) })
	if h.At["c"][s] != 3 || h.At["d"][s] != 2 {
		t.Fatalf("%+v", h.At)
	}
}
