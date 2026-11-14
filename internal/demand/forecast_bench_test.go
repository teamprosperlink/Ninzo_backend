package demand

import (
	"fmt"
	"testing"
	"time"
)

// benchHistory is weeks of demand for cells cells in every half-hour band of every day.
func benchHistory(cells, weeks int) (History, Slot) {
	target := SlotOf(time.Date(2026, 11, 13, 13, 30, 0, 0, time.UTC))
	since := target.Day.AddDate(0, 0, -7*weeks)
	var cs []Count
	for c := range cells {
		for d := 0; d < 7*weeks; d++ {
			for band := 0; band < 48; band += 4 {
				cs = append(cs, Count{Cell: fmt.Sprintf("blr76:%d:%d", c, d), Slot: Slot{Day: since.AddDate(0, 0, d), Band: band}, Asked: 1 + c%3})
			}
		}
	}
	return NewHistory(cs, since, func(c Count) float64 { return float64(c.Asked) }), target
}

// BenchmarkForecast is one cell's forecast; placementplan.Build makes one per cell per weekday and
// band.
func BenchmarkForecast(b *testing.B) {
	h, target := benchHistory(200, 4)
	cell := "blr76:7:3"
	b.ReportAllocs()
	for b.Loop() {
		_, _ = h.Forecast(cell, target, 4, 0.8)
	}
}

func BenchmarkNewHistory(b *testing.B) {
	target := SlotOf(time.Date(2026, 11, 13, 13, 30, 0, 0, time.UTC))
	since := target.Day.AddDate(0, 0, -28)
	cs := make([]Count, 0, 50000)
	for i := range 50000 {
		cs = append(cs, Count{Cell: fmt.Sprintf("blr76:%d:%d", i%500, i%97), Slot: Slot{Day: since.AddDate(0, 0, i%28), Band: i % 48}, Asked: 1})
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = NewHistory(cs, since, func(c Count) float64 { return float64(c.Asked) })
	}
}
