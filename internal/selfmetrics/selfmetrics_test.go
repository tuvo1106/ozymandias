package selfmetrics

import (
	"encoding/json"
	"math"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
)

func TestCounter_SameIdentityRegardlessOfTagOrder(t *testing.T) {
	r := NewRegistry()
	a := r.Counter("ozy.test.hits", "b:2", "a:1")
	b := r.Counter("ozy.test.hits", "a:1", "b:2", "a:1")
	if a != b {
		t.Fatal("same name and tag set returned different counters")
	}
	if c := r.Counter("ozy.test.hits", "a:1"); c == a {
		t.Fatal("different tag set returned the same counter")
	}
}

// A counter that decreases reads as a process restart downstream.
func TestCounter_IgnoresNegativeAdds(t *testing.T) {
	c := NewRegistry().Counter("ozy.test.hits")
	c.Add(5)
	c.Add(-3)
	c.Add(0)
	c.Inc()
	if got := c.Value(); got != 6 {
		t.Fatalf("Value = %d, want 6", got)
	}
}

func TestGauge_SetAndValue(t *testing.T) {
	g := NewRegistry().Gauge("ozy.test.depth")
	g.Set(2.5)
	g.Set(-1.25)
	if got := g.Value(); got != -1.25 {
		t.Fatalf("Value = %v, want -1.25", got)
	}
}

func TestSnapshot_SortedAndTyped(t *testing.T) {
	r := NewRegistry()
	r.Counter("ozy.b", "x:2").Add(3)
	r.Counter("ozy.b", "x:1").Add(1)
	r.Gauge("ozy.a").Set(7)
	r.GaugeFunc("ozy.c", func() float64 { return 9 })

	want := []Point{
		{"ozy.a", TypeGauge, []string{}, 7},
		{"ozy.b", TypeCounter, []string{"x:1"}, 1},
		{"ozy.b", TypeCounter, []string{"x:2"}, 3},
		{"ozy.c", TypeGauge, []string{}, 9},
	}
	if got := r.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Snapshot =\n%v\nwant\n%v", got, want)
	}
}

func TestGaugeFunc_ReRegisteringReplacesFunction(t *testing.T) {
	r := NewRegistry()
	r.GaugeFunc("ozy.up", func() float64 { return 1 })
	r.GaugeFunc("ozy.up", func() float64 { return 2 })
	snap := r.Snapshot()
	if len(snap) != 1 || snap[0].Value != 2 {
		t.Fatalf("Snapshot = %v, want one point with value 2", snap)
	}
}

// fn runs without the registry lock, so it may use the registry itself
// without deadlocking.
func TestGaugeFunc_MayUseRegistryWithoutDeadlock(t *testing.T) {
	r := NewRegistry()
	r.GaugeFunc("ozy.meta.instruments", func() float64 {
		return float64(r.Counter("ozy.other").Value())
	})
	_ = r.Snapshot()
}

func TestHandler_ServesJSONWithNullForNonFinite(t *testing.T) {
	r := NewRegistry()
	r.Gauge("ozy.nan").Set(math.NaN())
	r.Gauge("ozy.inf").Set(math.Inf(1))
	r.Counter("ozy.n", "k:v").Add(2)

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/debug/vars", nil))
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var body struct {
		Metrics []struct {
			Name  string   `json:"name"`
			Type  string   `json:"type"`
			Tags  []string `json:"tags"`
			Value *float64 `json:"value"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON %q: %v", rec.Body.String(), err)
	}
	if len(body.Metrics) != 3 {
		t.Fatalf("metrics = %+v, want 3", body.Metrics)
	}
	for _, m := range body.Metrics {
		switch m.Name {
		case "ozy.nan", "ozy.inf":
			if m.Value != nil {
				t.Errorf("%s value = %v, want null", m.Name, *m.Value)
			}
		case "ozy.n":
			if m.Value == nil || *m.Value != 2 || m.Type != "counter" || m.Tags[0] != "k:v" {
				t.Errorf("counter = %+v", m)
			}
		}
	}
}

func TestRegistry_ConcurrentUseIsSafe(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				r.Counter("ozy.hits", "shared:yes").Inc()
				r.Gauge("ozy.g").Set(float64(j))
				_ = r.Snapshot()
			}
		}()
	}
	wg.Wait()
	if got := r.Counter("ozy.hits", "shared:yes").Value(); got != 8000 {
		t.Fatalf("hits = %d, want 8000 (lost increments)", got)
	}
}
