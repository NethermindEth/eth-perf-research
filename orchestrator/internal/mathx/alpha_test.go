package mathx

import (
	"math"
	"testing"
)

func TestUpdateCoeff(t *testing.T) {
	const tol = 1e-12

	cases := []struct {
		name     string
		fHat     float64
		observed float64
		sigma    float64
		alpha    float64
		check    func(t *testing.T, u AlphaUpdate)
	}{
		{
			name:     "sigma_at_floor",
			fHat:     5.0,
			observed: 5.0,
			sigma:    0.0, // below floor, should use sigmaFloor=1.0
			alpha:    0.1,
			check: func(t *testing.T, u AlphaUpdate) {
				// raw=0, sat=sigma*tanh(0/1)=0
				if math.Abs(u.SaturatedInnovation) > tol {
					t.Fatalf("sat innovation want 0, got %v", u.SaturatedInnovation)
				}
				if math.Abs(u.F-5.0) > tol {
					t.Fatalf("F unchanged when raw=0; got %v", u.F)
				}
			},
		},
		{
			name:     "alpha_bounds_small_innovation",
			fHat:     1000.0,
			observed: 1000.0 + 0.001, // tiny relative move → alpha near A_MIN
			sigma:    1.0,
			alpha:    aMin,
			check: func(t *testing.T, u AlphaUpdate) {
				if u.Alpha < aMin-tol {
					t.Fatalf("alpha below A_MIN: %v", u.Alpha)
				}
				if u.Alpha > aMax+tol {
					t.Fatalf("alpha above A_MAX: %v", u.Alpha)
				}
			},
		},
		{
			name:     "alpha_bounds_large_innovation",
			fHat:     0.0,
			observed: 1e6, // huge → alpha near A_MAX
			sigma:    1.0,
			alpha:    aMax,
			check: func(t *testing.T, u AlphaUpdate) {
				if u.Alpha < aMin-tol {
					t.Fatalf("alpha below A_MIN: %v", u.Alpha)
				}
				if u.Alpha > aMax+tol {
					t.Fatalf("alpha above A_MAX: %v", u.Alpha)
				}
			},
		},
		{
			name:     "large_innovation_saturates",
			fHat:     0.0,
			observed: 1e9,
			sigma:    2.0,
			alpha:    0.1,
			check: func(t *testing.T, u AlphaUpdate) {
				// tanh(1e9/2) ≈ 1; sat ≈ sigma = 2.0
				if math.Abs(u.SaturatedInnovation-2.0) > 1e-6 {
					t.Fatalf("expected sat≈2.0, got %v", u.SaturatedInnovation)
				}
			},
		},
		{
			name:     "zero_observation",
			fHat:     10.0,
			observed: 0.0,
			sigma:    1.0,
			alpha:    0.1,
			check: func(t *testing.T, u AlphaUpdate) {
				if math.IsNaN(u.F) || math.IsInf(u.F, 0) {
					t.Fatalf("F not finite: %v", u.F)
				}
				if math.IsNaN(u.Sigma) || math.IsInf(u.Sigma, 0) {
					t.Fatalf("Sigma not finite: %v", u.Sigma)
				}
				if math.IsNaN(u.Alpha) || math.IsInf(u.Alpha, 0) {
					t.Fatalf("Alpha not finite: %v", u.Alpha)
				}
			},
		},
		{
			name:     "sigma_ewma_update",
			fHat:     0.0,
			observed: 10.0,
			sigma:    1.0,
			alpha:    0.1,
			check: func(t *testing.T, u AlphaUpdate) {
				// sigma_new = 0.9*1.0 + 0.1*|10-0| = 0.9 + 1.0 = 1.9
				want := sigmaEWMADecay*1.0 + (1.0-sigmaEWMADecay)*10.0
				if math.Abs(u.Sigma-want) > tol {
					t.Fatalf("sigma_new: got %v, want %v", u.Sigma, want)
				}
			},
		},
		{
			name:     "alpha_ewma_blends_toward_target",
			fHat:     0.0,
			observed: 0.0,
			sigma:    1.0,
			alpha:    0.15,
			check: func(t *testing.T, u AlphaUpdate) {
				// abs_ratio = 0/1000 = 0; alphaTarget ≈ aMin + (aMax-aMin)/2*(1+tanh(-k*c))
				alphaTarget := aMin + (aMax-aMin)/2.0*(1.0+math.Tanh(k*(0-c)))
				want := alphaEWMADecay*0.15 + (1.0-alphaEWMADecay)*alphaTarget
				if math.Abs(u.Alpha-want) > tol {
					t.Fatalf("alpha_new: got %v, want %v", u.Alpha, want)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := UpdateCoeff(tc.fHat, tc.observed, tc.sigma, tc.alpha)
			tc.check(t, u)
		})
	}
}

func TestUpdateCoeffPanicOnNaN(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on NaN input")
		}
	}()
	UpdateCoeff(math.NaN(), 1.0, 1.0, 0.1)
}

func TestUpdateCoeffPanicOnInf(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on Inf input")
		}
	}()
	UpdateCoeff(1.0, math.Inf(1), 1.0, 0.1)
}
