package collector

import (
	"math"
	"testing"
	"time"

	"pgregory.net/rapid"
)

var t0 = time.Unix(1_790_000_000, 0)

func TestRates_Observe(t *testing.T) {
	r := NewRates()
	steps := []struct {
		name   string
		value  float64
		at     time.Duration
		want   float64
		wantOK bool
	}{
		{"the first reading has nothing to subtract from", 100, 0, 0, false},
		{"a normal increase", 250, 15 * time.Second, 10, true},
		{"no change is a rate of zero, not no rate", 250, 30 * time.Second, 0, true},
		{"a reset yields nothing rather than a negative rate", 40, 45 * time.Second, 0, false},
		{"and the reading after it proceeds from the reset", 70, 60 * time.Second, 2, true},
		{"no time passed", 90, 60 * time.Second, 0, false},
		{"a clock step backwards", 95, 50 * time.Second, 0, false},
		{"NaN is not a reading", math.NaN(), 70 * time.Second, 0, false},
		{"and the reading after it starts afresh, not NaN", 100, 80 * time.Second, 0, false},
		{"after which rates resume", 130, 95 * time.Second, 2, true},
		{"+Inf is not a reading either", math.Inf(1), 100 * time.Second, 0, false},
		{"nor is the one after it a rate from +Inf", 140, 110 * time.Second, 0, false},
	}
	for _, s := range steps {
		got, ok := r.Observe("k", s.value, t0.Add(s.at))
		if ok != s.wantOK || got != s.want {
			t.Fatalf("%s: got %v, %v; want %v, %v", s.name, got, ok, s.want, s.wantOK)
		}
	}
}

func TestRates_KeysAreIndependent(t *testing.T) {
	r := NewRates()
	r.Observe("a", 0, t0)
	r.Observe("b", 1000, t0)
	if got, ok := r.Observe("a", 10, t0.Add(10*time.Second)); !ok || got != 1 {
		t.Fatalf("a: %v %v", got, ok)
	}
	if got, ok := r.Observe("b", 900, t0.Add(10*time.Second)); ok {
		t.Fatalf("b reset leaked a rate: %v", got)
	}
}

func TestRates_Prune(t *testing.T) {
	r := NewRates()
	r.Observe("gone", 1, t0)
	r.Observe("here", 1, t0.Add(time.Minute))
	r.Prune(t0.Add(30 * time.Second))
	if r.Len() != 1 {
		t.Fatalf("len = %d, want 1", r.Len())
	}
	// A pruned key starts over: no rate from a reading minutes old.
	if _, ok := r.Observe("gone", 5, t0.Add(2*time.Minute)); ok {
		t.Fatal("a pruned key produced a rate")
	}
}

// Whatever the readings, a rate is never negative and never comes from a
// single reading. For a counter that only goes up, rate × elapsed summed over
// the intervals adds back up to the counter's increase since the first
// reading: nothing is lost or invented between readings.
func TestRates_Properties(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		r := NewRates()
		monotonic := rapid.Bool().Draw(t, "monotonic")
		n := rapid.IntRange(1, 50).Draw(t, "n")
		at, v := t0, 0.0
		var first, integral float64
		for i := range n {
			dt := time.Duration(rapid.IntRange(1, 60).Draw(t, "dt")) * time.Second
			at = at.Add(dt)
			if monotonic {
				v += float64(rapid.IntRange(0, 1_000_000).Draw(t, "inc"))
			} else {
				v = float64(rapid.IntRange(0, 1_000_000).Draw(t, "v"))
			}
			rate, ok := r.Observe("k", v, at)
			switch {
			case i == 0 && ok:
				t.Fatal("a rate from one reading")
			case ok && rate < 0:
				t.Fatalf("negative rate %v", rate)
			case i == 0:
				first = v
			case monotonic && !ok:
				t.Fatalf("reading %d of a monotonic counter gave no rate", i)
			case ok:
				integral += rate * dt.Seconds()
			}
		}
		if monotonic && math.Abs(integral-(v-first)) > 1e-6*math.Max(1, v) {
			t.Fatalf("rates integrate to %v, counter rose by %v", integral, v-first)
		}
	})
}

// Sweep keeps a key at least ForgetAfter, and three gaps when runs are
// further apart than that.
func TestRates_SweepScalesWithTheGap(t *testing.T) {
	r := NewRates()
	at := time.Unix(1790000000, 0)
	r.Observe("a", 1, at)
	r.Sweep(at)
	r.Sweep(at.Add(10 * time.Minute)) // "a" missed a run 10m apart: kept
	if r.Len() != 1 {
		t.Fatal("a key that missed one long-interval run was forgotten")
	}
	r.Sweep(at.Add(20 * time.Minute))
	r.Sweep(at.Add(30 * time.Minute)) // exactly three gaps: still kept
	if r.Len() != 1 {
		t.Fatal("forgotten before three gaps had passed")
	}
	r.Sweep(at.Add(40 * time.Minute)) // four gaps: gone
	if r.Len() != 0 {
		t.Fatal("a key gone for three gaps was kept")
	}

	r = NewRates()
	r.Observe("b", 1, at)
	for i := range 22 { // 15s runs for 5m15s: ForgetAfter governs
		r.Sweep(at.Add(time.Duration(i) * 15 * time.Second))
	}
	if r.Len() != 0 {
		t.Fatal("a key unseen for over ForgetAfter was kept")
	}
}

// Change is the signed difference, with no reset rule: a fall is a fall.
func TestRates_Change(t *testing.T) {
	r := NewRates()
	at := time.Unix(1790000000, 0)
	if _, ok := r.Change("s", 10, at); ok {
		t.Fatal("a first reading has no change")
	}
	if d, ok := r.Change("s", 4, at.Add(time.Second)); !ok || d != -6 {
		t.Fatalf("change = %v, %v; want -6", d, ok)
	}
	if _, ok := r.Change("s", math.NaN(), at.Add(2*time.Second)); ok {
		t.Fatal("a NaN has no change")
	}
	if _, ok := r.Change("s", 1, at.Add(3*time.Second)); ok || !r.Has("s") {
		t.Fatal("after a NaN the key starts over")
	}
	if cutoff := r.Sweep(at.Add(time.Hour)); !cutoff.Equal(at.Add(time.Hour - ForgetAfter)) {
		t.Fatalf("Sweep's cutoff = %v", cutoff)
	}
}
