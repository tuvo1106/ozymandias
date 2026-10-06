package concentrator

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/aggregator"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/pkg/wire"

	"pgregory.net/rapid"
)

type rec struct {
	mu sync.Mutex
	s  []aggregator.Sample
	at []time.Time
}

func (r *rec) Add(s aggregator.Sample, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.s = append(r.s, s)
	r.at = append(r.at, now)
}

func (r *rec) named(suffix string) []aggregator.Sample {
	var out []aggregator.Sample
	for _, s := range r.s {
		if strings.HasSuffix(s.Name, suffix) {
			out = append(out, s)
		}
	}
	return out
}

func sp(service, name, resource string, top bool, over func(*wire.Span)) wire.Span {
	s := wire.Span{TraceID: strings.Repeat("a", 32), SpanID: strings.Repeat("b", 16), Service: service, Name: name, Resource: resource,
		Type: "web", Start: 1_790_000_000_000_000, Duration: 250_000, Meta: map[string]string{"env": "dev", "http.status_code": "200"}, Metrics: map[string]float64{}}
	if top {
		s.Metrics[wire.MetricTopLevel] = 1
	}
	if over != nil {
		over(&s)
	}
	return s
}

func TestObserve_CountsTopLevelAndMeasuredSpansOnly(t *testing.T) {
	r := &rec{}
	c := New(Options{Sink: r})
	c.Observe([]wire.Span{
		sp("api", "http.request", "GET /a", true, nil),
		sp("api", "postgres.query", "select 1", false, nil), // not an entry span: no stats
		sp("api", "postgres.query", "select 2", false, func(s *wire.Span) { s.Metrics[wire.MetricMeasured] = 1 }),
	})
	if n := len(r.named(".hits")); n != 2 {
		t.Fatalf("%d hits, want 2 (the top-level and the measured span)", n)
	}
	if len(r.named(".errors")) != 0 {
		t.Error("errors counted for spans that did not fail")
	}
}

func TestObserve_ErrorDurationTagsAndTime(t *testing.T) {
	r := &rec{}
	c := New(Options{Sink: r, Env: "fallback"})
	c.Observe([]wire.Span{sp("api", "http.request", "GET /a", true, func(s *wire.Span) { s.Error = 1; s.Meta["http.status_code"] = "503"; delete(s.Meta, "env") })})
	if len(r.named(".errors")) != 1 {
		t.Fatal("no error counted")
	}
	d := r.named(".duration")[0]
	if d.Kind != aggregator.Distribution || d.Value != 0.25 {
		t.Errorf("duration sample = %+v, want 0.25 seconds as a distribution", d)
	}
	got := strings.Join(d.Tags, ",")
	for _, want := range []string{"service:api", "resource:GET /a", "env:fallback", "status_class:5xx"} {
		if !strings.Contains(got, want) {
			t.Errorf("tags %q lack %q", got, want)
		}
	}
	if want := time.UnixMicro(1_790_000_000_000_000 + 250_000); !r.at[0].Equal(want) {
		t.Errorf("stamped %v, want the span's end %v", r.at[0], want)
	}
}

func TestStatusClass(t *testing.T) {
	for in, want := range map[string]string{"200": "2xx", "404": "4xx", "599": "5xx", "100": "1xx", "": "", "0": "", "600": "", "abc": "", "20x": ""} {
		if got := StatusClass(in); got != want {
			t.Errorf("StatusClass(%q) = %q, want %q", in, got, want)
		}
	}
}

// A tag is a series: hostile resources must not mint unbounded series.
func TestObserve_CardinalityIsCapped(t *testing.T) {
	r := &rec{}
	c := New(Options{Sink: r, MaxResources: 10, MaxServices: 3, MaxNames: 4})
	var spans []wire.Span
	for i := 0; i < 500; i++ {
		spans = append(spans, sp("api", "http.request", fmt.Sprintf("GET /x/%d", i), true, nil))
	}
	for i := 0; i < 50; i++ {
		spans = append(spans, sp(fmt.Sprintf("svc%d", i), "http.request", "GET /", true, nil))
		spans = append(spans, sp("api", fmt.Sprintf("name%d", i), "GET /", true, nil))
	}
	c.Observe(spans)
	resources, services, names := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, s := range r.s {
		names[s.Name] = true
		for _, tag := range s.Tags {
			k, v := wire.SplitTag(tag)
			switch k {
			case "resource":
				resources[v] = true
			case "service":
				services[v] = true
			}
		}
	}
	if len(resources) > 10+1+1 { // cap, _other_, and "GET /" of another service
		t.Errorf("%d distinct resources", len(resources))
	}
	if len(services) > 3+1 {
		t.Errorf("%d distinct services", len(services))
	}
	if len(names) > (4+1)*3 { // 4 names + "other", times hits/errors/duration
		t.Errorf("%d distinct metric names", len(names))
	}
	if !resources[Other] || !services[Other] {
		t.Error("overflow did not fold into _other_")
	}
}

func TestObserve_TagsAreAlwaysLegal(t *testing.T) {
	r := &rec{}
	c := New(Options{Sink: r})
	c.Observe([]wire.Span{sp("a,b", "http.request", "select a,\nb from t where x = '"+strings.Repeat("é", 400)+"'", true, nil)})
	for _, s := range r.s {
		for _, tag := range s.Tags {
			if !wire.ValidTag(tag) {
				t.Errorf("invalid tag %q", tag)
			}
		}
	}
}

// Σ hits over everything observed == number of top-level spans in, however the
// spans are mixed, folded or batched.
func TestObserve_HitsEqualTopLevelSpans(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		r := &rec{}
		c := New(Options{Sink: r, MaxResources: 3, MaxServices: 2, MaxNames: 2})
		n := rapid.IntRange(0, 60).Draw(t, "n")
		top := 0
		var spans []wire.Span
		for i := 0; i < n; i++ {
			isTop := rapid.Bool().Draw(t, "top")
			if isTop {
				top++
			}
			spans = append(spans, sp(rapid.SampledFrom([]string{"a", "b", "c", "d"}).Draw(t, "svc"), rapid.SampledFrom([]string{"x", "y", "z"}).Draw(t, "name"),
				rapid.StringN(0, 5, 20).Draw(t, "res"), isTop, nil))
		}
		c.Observe(spans)
		var sum float64
		for _, s := range r.named(".hits") {
			sum += s.Value
		}
		if int(sum) != top {
			t.Fatalf("hits %v != %d top-level spans", sum, top)
		}
	})
}

// Through the real aggregator: the series a dashboard will query exist and agree.
func TestObserve_ThroughTheRealAggregator(t *testing.T) {
	start := time.UnixMicro(1_790_000_000_000_000)
	agg := aggregator.New(aggregator.Options{Started: start, Clock: testutil.NewFakeClock(start)})
	c := New(Options{Sink: agg})
	for i := 0; i < 7; i++ {
		c.Observe([]wire.Span{sp("api", "http.request", "GET /a", true, func(s *wire.Span) { s.Error = i % 2 })})
	}
	series, sketches := agg.Flush(start.Add(time.Minute), true)
	hits, errs := 0.0, 0.0
	for _, s := range series {
		for _, p := range s.Points {
			switch s.Metric {
			case "trace.http.request.hits":
				hits += p.Value
			case "trace.http.request.errors":
				errs += p.Value
			}
		}
	}
	if hits != 7 || errs != 3 {
		t.Errorf("hits=%v errors=%v, want 7 and 3", hits, errs)
	}
	if len(sketches) != 1 || sketches[0].Metric != "trace.http.request.duration" {
		t.Errorf("sketches = %+v", sketches)
	}
}
