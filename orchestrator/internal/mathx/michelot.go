package mathx

import (
	"cmp"
	"fmt"
	"math"
	"slices"
)

// ProjectSimplex projects x onto the unit probability simplex using the
// Michelot (1986) O(n log n) algorithm. Returns a new slice; x is not modified.
func ProjectSimplex(x []float64) []float64 {
	n := len(x)
	if n == 0 {
		return []float64{}
	}
	for _, v := range x {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			panic(fmt.Sprintf("ProjectSimplex: non-finite input %v", v))
		}
	}

	// Sort descending.
	sorted := make([]float64, n)
	copy(sorted, x)
	slices.SortFunc(sorted, func(a, b float64) int { return cmp.Compare(b, a) })

	// Find rho: largest index where sorted[i] - tau > 0, tau = (cumsum[i]-1)/(i+1).
	cumsum := make([]float64, n)
	cumsum[0] = sorted[0]
	for i := 1; i < n; i++ {
		cumsum[i] = cumsum[i-1] + sorted[i]
	}

	rho := -1
	for i := n - 1; i >= 0; i-- {
		if sorted[i]+(1.0-cumsum[i])/float64(i+1) > 0 {
			rho = i
			break
		}
	}

	if rho < 0 {
		// Numerical edge: put all mass on the largest coordinate.
		out := make([]float64, n)
		maxIdx := 0
		for i := 1; i < n; i++ {
			if x[i] > x[maxIdx] {
				maxIdx = i
			}
		}
		out[maxIdx] = 1.0
		return out
	}

	tau := (cumsum[rho] - 1.0) / float64(rho+1)
	out := make([]float64, n)
	for i, v := range x {
		out[i] = math.Max(v-tau, 0.0)
	}
	return out
}
