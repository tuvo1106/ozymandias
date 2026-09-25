package sketch_test

import (
	"fmt"
	"math"

	"github.com/tuvo1106/ozymandias/internal/sketch"
)

// The everyday use: record observations, ask for a percentile.
func ExampleSketch_Quantile() {
	s := sketch.NewDefault()
	for i := 1; i <= 1000; i++ {
		_ = s.Add(float64(i))
	}

	p95, _ := s.Quantile(0.95)
	fmt.Printf("p95 is within 1%% of 951: %t\n", math.Abs(p95-951) <= 0.01*951)
	fmt.Printf("count=%.0f min=%.0f max=%.0f\n", s.Count(), s.Min(), s.Max())
	// Output:
	// p95 is within 1% of 951: true
	// count=1000 min=1 max=1000
}

// Why sketches exist: percentiles do not average, but sketches merge. Nine
// hundred fast requests on one host and a hundred slow ones on another make a
// fleet whose p95 is the slow host's latency — an answer neither host holds and
// that the mean of their two p95s misses by half.
func ExampleSketch_Merge() {
	fast, slow := sketch.NewDefault(), sketch.NewDefault()
	for i := 0; i < 900; i++ {
		_ = fast.Add(10)
	}
	for i := 0; i < 100; i++ {
		_ = slow.Add(1000)
	}

	fleet := fast.Clone()
	_ = fleet.Merge(slow)

	a, _ := fast.Quantile(0.95)
	b, _ := slow.Quantile(0.95)
	merged, _ := fleet.Quantile(0.95)

	fmt.Printf("per-host p95s:        %.0fms and %.0fms\n", a, b)
	fmt.Printf("mean of the two p95s: %.0fms (wrong)\n", (a+b)/2)
	fmt.Printf("merged p95:           %.0fms\n", merged)
	fmt.Printf("observations:         %.0f\n", fleet.Count())
	// Output:
	// per-host p95s:        10ms and 1000ms
	// mean of the two p95s: 505ms (wrong)
	// merged p95:           1000ms
	// observations:         1000
}
