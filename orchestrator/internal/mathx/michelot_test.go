package mathx

import (
	"math"
	"testing"
)

func TestProjectSimplex(t *testing.T) {
	const tol = 1e-12

	assertSimplex := func(t *testing.T, got []float64) {
		t.Helper()
		sum := 0.0
		for _, v := range got {
			if v < -tol {
				t.Fatalf("negative element %v", v)
			}
			sum += v
		}
		if math.Abs(sum-1.0) > tol {
			t.Fatalf("sum = %v, want 1.0 ± %v", sum, tol)
		}
	}

	cases := []struct {
		name string
		in   []float64
		want []float64
	}{
		{
			name: "uniform_4",
			in:   []float64{0.25, 0.25, 0.25, 0.25},
			want: []float64{0.25, 0.25, 0.25, 0.25},
		},
		{
			name: "single_element",
			in:   []float64{42.0},
			want: []float64{1.0},
		},
		{
			name: "already_on_simplex",
			in:   []float64{0.5, 0.3, 0.2},
			want: []float64{0.5, 0.3, 0.2},
		},
		{
			name: "all_negative",
			in:   []float64{-1.0, -2.0, -3.0},
			// largest is -1 at index 0; projection clips all to 0 except index 0
			want: nil, // only check simplex constraints
		},
		{
			name: "13_elements_uniform",
			in: func() []float64 {
				s := make([]float64, 13)
				for i := range s {
					s[i] = 1.0 / 13.0
				}
				return s
			}(),
			want: nil, // check sum==1 and non-negative
		},
		{
			name: "needs_projection",
			in:   []float64{1.5, 0.5, 0.5},
			// tau = (1.5+0.5+0.5 - 1)/3 = 0.5; result = [1.0, 0.0, 0.0]
			want: []float64{1.0, 0.0, 0.0},
		},
		{
			name: "mixed_signs",
			in:   []float64{3.0, 1.0, -1.0},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := make([]float64, len(tc.in))
			copy(original, tc.in)

			got := ProjectSimplex(tc.in)

			// Input must not be mutated.
			for i, v := range original {
				if v != tc.in[i] {
					t.Fatalf("input slice mutated at index %d", i)
				}
			}

			assertSimplex(t, got)

			if tc.want != nil {
				for i, w := range tc.want {
					if math.Abs(got[i]-w) > tol {
						t.Fatalf("index %d: got %v, want %v", i, got[i], w)
					}
				}
			}
		})
	}
}

func TestProjectSimplexPanicOnNaN(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on NaN input")
		}
	}()
	ProjectSimplex([]float64{1.0, math.NaN()})
}

func TestProjectSimplexPanicOnInf(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on Inf input")
		}
	}()
	ProjectSimplex([]float64{math.Inf(1), 0.5})
}

func TestProjectSimplexEmpty(t *testing.T) {
	got := ProjectSimplex([]float64{})
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %v", got)
	}
}
