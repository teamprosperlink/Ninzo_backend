package placement

import (
	"fmt"
	"math/rand/v2"
	"testing"
)

// benchProblem is a random coverage problem: each candidate covers each demand point with chance
// density, and demand weights are uniform in [0, 1). Seeded, so every run solves the same problem.
func benchProblem(cands, points int, density float64) (Coverage, []float64) {
	rng := rand.New(rand.NewPCG(1, uint64(cands*points)))
	cov := make(Coverage, cands)
	for i := range cov {
		cov[i] = make([]bool, points)
		for d := range cov[i] {
			cov[i][d] = rng.Float64() < density
		}
	}
	w := make([]float64, points)
	for d := range w {
		w[d] = rng.Float64()
	}
	return cov, w
}

// Sizes: the pilot (20 staging nodes, 500 demand cells, 8 scooters), a busy zone, and a city-scale
// zone. placementplan.Build solves one of these per weekday and half-hour band (up to 336 a build).
var benchSizes = []struct {
	name          string
	cands, points int
	n             int
}{
	{"pilot_20x500_n8", 20, 500, 8},
	{"zone_60x2000_n30", 60, 2000, 30},
	{"city_300x10000_n100", 300, 10000, 100},
}

func BenchmarkGreedyCaps(b *testing.B) {
	for _, s := range benchSizes {
		b.Run(s.name, func(b *testing.B) {
			cov, w := benchProblem(s.cands, s.points, 0.05)
			caps := make([]int, s.cands)
			for i := range caps {
				caps[i] = 2
			}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := GreedyCaps(cov, w, s.n, 0.3, caps); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkScore(b *testing.B) {
	for _, s := range benchSizes {
		b.Run(s.name, func(b *testing.B) {
			cov, w := benchProblem(s.cands, s.points, 0.05)
			picks := make([]int, s.n)
			for i := range picks {
				picks[i] = i % s.cands
			}
			b.ReportAllocs()
			for b.Loop() {
				_, _ = Score(cov, w, 0.3, picks)
			}
		})
	}
}

func ExampleGreedyCaps() {
	cov, w := benchProblem(20, 500, 0.05)
	caps := make([]int, 20)
	for i := range caps {
		caps[i] = 2
	}
	p, _ := GreedyCaps(cov, w, 8, 0.3, caps)
	fmt.Println(len(p.Picks))
	// Output: 8
}
