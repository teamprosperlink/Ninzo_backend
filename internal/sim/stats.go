package sim

import "math"

// SignTest is the exact two-sided sign test for paired comparisons: better and worse are the pairs
// (seeds) where one rule beat the other, ties left out. It returns the chance of a split at least
// this uneven if neither rule were better (p = 1 with no untied pairs).
func SignTest(better, worse int) float64 {
	n := better + worse
	if n == 0 {
		return 1
	}
	k := min(better, worse)
	// P(X <= k) for X ~ Binomial(n, 1/2), doubled for two sides, summed in logs to stay exact for
	// large n.
	tail := 0.0
	for i := 0; i <= k; i++ {
		lg, _ := math.Lgamma(float64(n + 1))
		a, _ := math.Lgamma(float64(i + 1))
		b, _ := math.Lgamma(float64(n - i + 1))
		tail += math.Exp(lg - a - b - float64(n)*math.Ln2)
	}
	return math.Min(1, 2*tail)
}
