package sim

import (
	"math"
	"testing"
)

func TestSignTestMatchesTheBinomialTable(t *testing.T) {
	for _, c := range []struct {
		better, worse int
		want          float64
	}{
		{0, 0, 1},
		{1, 0, 1},              // 2 × 0.5
		{5, 5, 1},              // capped at 1
		{20, 0, 2 / 1048576.0}, // 2 × 0.5^20
		{3, 8, 0.2265625},      // 2 × (1+11+55+165)/2048
		{1, 17, 0.000144958},   // 2 × (1+18)/262144
		{18, 1, 7.62939e-05},   // 2 × (1+19)/524288
		{6, 12, 0.237885},      // 2 × Σ_{i≤6} C(18,i)/2^18
	} {
		if got := SignTest(c.better, c.worse); math.Abs(got-c.want) > 1e-4*c.want {
			t.Errorf("SignTest(%d, %d) = %g, want %g", c.better, c.worse, got, c.want)
		}
		if SignTest(c.better, c.worse) != SignTest(c.worse, c.better) {
			t.Errorf("not symmetric for %d, %d", c.better, c.worse)
		}
	}
}
