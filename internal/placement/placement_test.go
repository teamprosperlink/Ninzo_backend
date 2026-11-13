package placement

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/teamprosperlink/Ninzo_backend/internal/routing"
)

// A hand case: candidate 0 covers demand {0,1,2}, 1 covers {2,3}, 2 covers {3,4,5}, 3 covers {0}.
// With q = 0 (maximal covering) and two scooters, the best is {0, 2}: all six points.
func hand() (Coverage, []float64) {
	c := Coverage{
		{true, true, true, false, false, false},
		{false, false, true, true, false, false},
		{false, false, false, true, true, true},
		{true, false, false, false, false, false},
	}
	return c, []float64{1, 1, 1, 1, 1, 1}
}

func TestGreedyFindsTheHandOptimum(t *testing.T) {
	c, w := hand()
	p, err := Greedy(c, w, 2, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(p.Count) != "[1 0 1 0]" || p.Covered != 1 || p.Expected != 1 {
		t.Fatalf("%+v, want candidates 0 and 2 covering everything", p)
	}
}

func TestBusyShareRewardsDoubleCover(t *testing.T) {
	// One heavy demand point covered only by candidate 0, light ones covered by candidate 1.
	c := Coverage{{true, false, false}, {false, true, true}}
	w := []float64{10, 1, 1}
	// q = 0.5: the second scooter at 0 adds 10 × 0.5 × 0.5 = 2.5; the first at 1 adds 2 × 0.5 = 1.
	p, err := Greedy(c, w, 2, 0.5, 2)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(p.Count) != "[2 0]" {
		t.Fatalf("%v, want both scooters at the heavy point", p.Count)
	}
	// With q = 0 a second scooter at the same point adds nothing; it goes to candidate 1.
	if p, _ = Greedy(c, w, 2, 0, 2); fmt.Sprint(p.Count) != "[1 1]" {
		t.Fatalf("q = 0: %v", p.Count)
	}
	// Expected for [2 0] at q = 0.5: 10 × (1 − 0.25) / 12.
	if p, _ = Greedy(c, w, 2, 0.5, 2); math.Abs(p.Expected-7.5/12) > 1e-12 {
		t.Fatalf("expected %v", p.Expected)
	}
}

func TestGainsNeverRise(t *testing.T) {
	rng := rand.New(rand.NewPCG(216, 1))
	for trial := range 50 {
		nc, nd := 8, 30
		c := make(Coverage, nc)
		for i := range c {
			c[i] = make([]bool, nd)
			for d := range c[i] {
				c[i][d] = rng.Float64() < 0.3
			}
		}
		w := make([]float64, nd)
		for d := range w {
			w[d] = rng.Float64()
		}
		p, err := Greedy(c, w, 10, 0.3, 2)
		if err != nil {
			t.Fatal(err)
		}
		for i := 1; i < len(p.Gains); i++ {
			if p.Gains[i] > p.Gains[i-1]+1e-12 {
				t.Fatalf("trial %d: gain rose from %v to %v", trial, p.Gains[i-1], p.Gains[i])
			}
		}
		sum := 0
		for _, n := range p.Count {
			if n > 2 {
				t.Fatalf("trial %d: capacity broken: %v", trial, p.Count)
			}
			sum += n
		}
		if sum != 10 {
			t.Fatalf("trial %d: placed %d of 10", trial, sum)
		}
	}
}

func TestBadInputIsRefused(t *testing.T) {
	c, w := hand()
	for name, f := range map[string]func() error{
		"q = 1":           func() error { _, err := Greedy(c, w, 1, 1, 1); return err },
		"q < 0":           func() error { _, err := Greedy(c, w, 1, -0.1, 1); return err },
		"capacity 0":      func() error { _, err := Greedy(c, w, 1, 0, 0); return err },
		"too many":        func() error { _, err := Greedy(c, w, 5, 0, 1); return err },
		"ragged":          func() error { _, err := Greedy(Coverage{{true}}, w, 1, 0, 1); return err },
		"no candidates":   func() error { _, err := Greedy(nil, w, 1, 0, 1); return err },
		"negative count":  func() error { _, err := Greedy(c, w, -1, 0, 1); return err },
		"negative weight": func() error { _, err := Greedy(c, []float64{1, -1, 1, 1, 1, 1}, 1, 0, 1); return err },
		"NaN weight":      func() error { _, err := Greedy(c, []float64{1, math.NaN(), 1, 1, 1, 1}, 1, 0, 1); return err },
	} {
		if f() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// fixed answers road seconds from a table keyed by (candidate index, demand index).
type fixed struct {
	secs   [][]float64 // -1: no road
	source routing.Source
	demand []routing.Point
}

func (f fixed) Table(_ context.Context, sources []routing.Point, dest routing.Point, _ float64) ([]routing.Leg, error) {
	d := -1
	for i, p := range f.demand {
		if p == dest {
			d = i
		}
	}
	out := make([]routing.Leg, len(sources))
	src := f.source
	if src == "" {
		src = routing.SourceOSRM
	}
	for c := range sources {
		if s := f.secs[c][d]; s >= 0 {
			out[c] = routing.Leg{Seconds: s, Reachable: true, Source: src}
		} else {
			out[c] = routing.Leg{Source: src}
		}
	}
	return out, nil
}

func TestMatrixUsesTheBudgetAndRoads(t *testing.T) {
	cands := []routing.Point{{Lat: 12.93, Lng: 77.61}, {Lat: 12.94, Lng: 77.62}}
	dem := []routing.Point{{Lat: 12.931, Lng: 77.611}, {Lat: 12.941, Lng: 77.621}, {Lat: 12.935, Lng: 77.615}}
	f := fixed{secs: [][]float64{{40, 200, 80}, {150, 30, -1}}, demand: dem}
	cov, err := Matrix(context.Background(), f, cands, dem, 80, 15)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(cov) != "[[true false true] [false true false]]" {
		t.Fatalf("%v: within 80 s by road, unreachable never covers", cov)
	}
	f.source = routing.SourcePlaceholder
	if _, err := Matrix(context.Background(), f, cands, dem, 80, 15); err != ErrNoRoads {
		t.Fatalf("placeholder: %v", err)
	}
}

// flaky answers straight lines on its first n calls, then OSRM.
type flaky struct {
	fixed
	left int
}

func (f *flaky) Table(ctx context.Context, sources []routing.Point, dest routing.Point, speed float64) ([]routing.Leg, error) {
	legs, err := f.fixed.Table(ctx, sources, dest, speed)
	if f.left > 0 {
		f.left--
		for i := range legs {
			legs[i].Source = routing.SourcePlaceholder
		}
	}
	return legs, err
}

func TestMatrixRetriesATransientFallback(t *testing.T) {
	cands := []routing.Point{{Lat: 12.93, Lng: 77.61}}
	dem := []routing.Point{{Lat: 12.931, Lng: 77.611}}
	f := &flaky{fixed: fixed{secs: [][]float64{{40}}, demand: dem}, left: 2}
	cov, err := Matrix(context.Background(), f, cands, dem, 80, 15)
	if err != nil || !cov[0][0] {
		t.Fatalf("two fallbacks then OSRM: %v %v", cov, err)
	}
	f = &flaky{fixed: fixed{secs: [][]float64{{40}}, demand: dem}, left: 3}
	if _, err := Matrix(context.Background(), f, cands, dem, 80, 15); err != ErrNoRoads {
		t.Fatalf("three fallbacks: %v", err)
	}
}

func TestGreedyCapsRespectsEachCapacity(t *testing.T) {
	// Both demand points only reachable from candidate 0, which holds one scooter.
	c := Coverage{{true, true}, {false, false}, {true, false}}
	p, err := GreedyCaps(c, []float64{5, 5}, 3, 0.5, []int{1, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(p.Count) != "[1 1 1]" || fmt.Sprint(p.Picks[:2]) != "[0 2]" {
		t.Fatalf("%v picks %v", p.Count, p.Picks)
	}
	if _, err := GreedyCaps(c, []float64{5, 5}, 3, 0.5, []int{1, 1}); err == nil {
		t.Fatal("one capacity short, accepted")
	}
	if _, err := GreedyCaps(c, []float64{5, 5}, 4, 0.5, []int{1, 1, 1}); err == nil {
		t.Fatal("more scooters than the capacities hold, accepted")
	}
	if _, err := GreedyCaps(c, []float64{5, 5}, 1, 0.5, []int{1, -1, 1}); err == nil {
		t.Fatal("a negative capacity, accepted")
	}
}

func TestFewerScootersArePrefixPlans(t *testing.T) {
	rng := rand.New(rand.NewPCG(224, 1))
	for trial := range 30 {
		c := make(Coverage, 6)
		for i := range c {
			c[i] = make([]bool, 20)
			for d := range c[i] {
				c[i][d] = rng.Float64() < 0.35
			}
		}
		w := make([]float64, 20)
		for d := range w {
			w[d] = rng.Float64()
		}
		caps := []int{1, 2, 3, 1, 2, 1}
		full, err := GreedyCaps(c, w, 10, 0.3, caps)
		if err != nil {
			t.Fatal(err)
		}
		for n := 1; n <= 10; n++ {
			p, _ := GreedyCaps(c, w, n, 0.3, caps)
			e, cv := Score(c, w, 0.3, full.Picks[:n])
			if fmt.Sprint(p.Picks) != fmt.Sprint(full.Picks[:n]) || math.Abs(e-p.Expected) > 1e-12 || math.Abs(cv-p.Covered) > 1e-12 {
				t.Fatalf("trial %d n %d: %v against the prefix %v", trial, n, p.Picks, full.Picks[:n])
			}
		}
	}
}
