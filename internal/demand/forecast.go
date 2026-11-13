package demand

import (
	"errors"
	"math"
	"time"
)

// History is demand per cell and slot, as one number (Asked, say). A slot missing from it on or
// after Since had no demand; before Since it is unknown.
type History struct {
	Since time.Time // the first IST day with data
	At    map[string]map[Slot]float64
}

// NewHistory builds a History from counts; value chooses what counts as demand.
func NewHistory(counts []Count, since time.Time, value func(Count) float64) History {
	h := History{Since: SlotOf(since).Day, At: map[string]map[Slot]float64{}}
	for _, c := range counts {
		if h.At[c.Cell] == nil {
			h.At[c.Cell] = map[Slot]float64{}
		}
		h.At[c.Cell][c.Slot] += value(c)
	}
	return h
}

// Forecast v1: demand at a cell in a slot is the weighted mean of the same weekday and band over the
// last weeks weeks, week k back weighted decay^(k−1). Weeks before Since are left out; with no
// week known it reports false.
func (h History) Forecast(cell string, s Slot, weeks int, decay float64) (float64, bool) {
	if weeks < 1 || decay <= 0 || decay > 1 {
		return 0, false
	}
	s.Day = SlotOf(s.Day).Day // the map key holds an IST midnight
	sum, wsum := 0.0, 0.0
	for k := 1; k <= weeks; k++ {
		d := s.Day.AddDate(0, 0, -7*k)
		if d.Before(h.Since) {
			break
		}
		w := math.Pow(decay, float64(k-1))
		sum += w * h.At[cell][Slot{Day: d, Band: s.Band}]
		wsum += w
	}
	if wsum == 0 {
		return 0, false
	}
	return sum / wsum, true
}

// Backtest forecasts day for every band at every cell with demand on day or in the weeks before it,
// and gives the mean absolute error against what happened, over the (cell, band) pairs where the
// forecast or the actual is not zero, and how many it scored. Pairs that are zero on both sides are
// left out, so a longer window that brings in quiet cells does not lower the error by itself.
func (h History) Backtest(day time.Time, weeks int, decay float64) (float64, int, error) {
	day = SlotOf(day).Day
	start := day.AddDate(0, 0, -7*weeks)
	cells := map[string]bool{}
	for c, slots := range h.At {
		for s := range slots {
			if s.Day.Equal(day) || (s.Day.Before(day) && !s.Day.Before(start)) {
				cells[c] = true
				break
			}
		}
	}
	errSum, n := 0.0, 0
	for c := range cells {
		for b := range 48 {
			s := Slot{Day: day, Band: b}
			f, ok := h.Forecast(c, s, weeks, decay)
			if !ok {
				continue
			}
			if f == 0 && h.At[c][s] == 0 {
				continue
			}
			errSum += math.Abs(f - h.At[c][s])
			n++
		}
	}
	if n == 0 {
		return 0, 0, errors.New("demand: no history to test against")
	}
	return errSum / float64(n), n, nil
}
