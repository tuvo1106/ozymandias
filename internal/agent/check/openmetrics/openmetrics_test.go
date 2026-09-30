package openmetrics

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/collector"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

var t0 = time.Unix(1_790_000_000, 0)

// target serves the pages it is given, one per scrape, repeating the last.
type target struct {
	mu          sync.Mutex
	pages       []string
	contentType string
	status      int
}

func (tg *target) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	if tg.status != 0 {
		w.WriteHeader(tg.status)
		return
	}
	if tg.contentType != "" {
		w.Header().Set("Content-Type", tg.contentType)
	}
	page := tg.pages[0]
	if len(tg.pages) > 1 {
		tg.pages = tg.pages[1:]
	}
	_, _ = fmt.Fprint(w, page)
}

func serve(t *testing.T, pages ...string) (*target, string) {
	t.Helper()
	tg := &target{pages: pages}
	srv := httptest.NewServer(tg)
	t.Cleanup(srv.Close)
	return tg, srv.URL + "/metrics"
}

func check(t *testing.T, settings map[string]any) (collector.Collector, *testutil.FakeClock) {
	t.Helper()
	fc := testutil.NewFakeClock(t0)
	c, err := New(collector.Instance{Check: "openmetrics", Name: "openmetrics", Settings: settings, Clock: fc, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	return c, fc
}

type got []collector.Metric

func scrape(t *testing.T, c collector.Collector) (got, error) {
	t.Helper()
	var out got
	err := c.Collect(context.Background(), func(m collector.Metric) { out = append(out, m) })
	return out, err
}

func (g got) named(name string) got {
	var out got
	for _, m := range g {
		if m.Name == name {
			out = append(out, m)
		}
	}
	return out
}

func (g got) one(t *testing.T, name string, tags ...string) collector.Metric {
	t.Helper()
	for _, m := range g.named(name) {
		ok := true
		for _, tag := range tags {
			if !slices.Contains(m.Tags, tag) {
				ok = false
			}
		}
		if ok {
			return m
		}
	}
	t.Fatalf("no %s%v in %+v", name, tags, g)
	return collector.Metric{}
}

const page1 = `# TYPE http_requests_total counter
http_requests_total{method="GET",code="200"} 100
http_requests_total{method="POST",code="500"} 4
# TYPE queue_depth gauge
queue_depth 7
# TYPE latency_seconds histogram
latency_seconds_bucket{route="/a",le="0.1"} 10
latency_seconds_bucket{route="/a",le="0.5"} 15
latency_seconds_bucket{route="/a",le="+Inf"} 16
latency_seconds_sum{route="/a"} 3.5
latency_seconds_count{route="/a"} 16
# TYPE rpc_seconds summary
rpc_seconds{quantile="0.5"} 0.2
rpc_seconds{quantile="0.99"} 0.9
rpc_seconds_sum 40
rpc_seconds_count 100
untyped_thing 3
`

// 15s later: GET +30 (2/s), POST unchanged; 4 more fast and 1 more slow
// request; 10 more rpcs.
const page2 = `# TYPE http_requests_total counter
http_requests_total{method="GET",code="200"} 130
http_requests_total{method="POST",code="500"} 4
# TYPE queue_depth gauge
queue_depth 9
# TYPE latency_seconds histogram
latency_seconds_bucket{route="/a",le="0.1"} 14
latency_seconds_bucket{route="/a",le="0.5"} 19
latency_seconds_bucket{route="/a",le="+Inf"} 21
latency_seconds_sum{route="/a"} 5
latency_seconds_count{route="/a"} 21
# TYPE rpc_seconds summary
rpc_seconds{quantile="0.5"} 0.25
rpc_seconds{quantile="0.99"} 1.1
rpc_seconds_sum 43
rpc_seconds_count 110
untyped_thing 3
`

func TestCheck_TwoScrapes(t *testing.T) {
	_, url := serve(t, page1, page2)
	c, fc := check(t, map[string]any{"url": url})

	first, err := scrape(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if first.one(t, "openmetrics.up").Value != 1 {
		t.Error("up != 1")
	}
	first.one(t, "openmetrics.scrape_duration")
	if len(first.named("http_requests_total"))+len(first.named("latency_seconds.bucket"))+len(first.named("latency_seconds.count")) != 0 {
		t.Errorf("the first scrape has nothing to difference, yet: %+v", first)
	}
	if first.one(t, "queue_depth").Value != 7 || first.one(t, "untyped_thing").Value != 3 {
		t.Error("gauges wrong on the first scrape")
	}
	if m := first.one(t, "rpc_seconds", "quantile:0.99"); m.Value != 0.9 || m.Kind != collector.Gauge {
		t.Errorf("summary quantile = %+v", m)
	}

	fc.Advance(15 * time.Second)
	second, err := scrape(t, c)
	if err != nil {
		t.Fatal(err)
	}
	get := second.one(t, "http_requests_total", "method:GET", "code:200")
	if get.Kind != collector.Rate || get.Value != 2 {
		t.Errorf("GET rate = %+v, want 2/s", get)
	}
	if post := second.one(t, "http_requests_total", "method:POST"); post.Value != 0 {
		t.Errorf("POST rate = %v", post.Value)
	}
	// Per-bucket, not cumulative: 4 in (0,0.1], 0 in (0.1,0.5], 1 above.
	for bound, want := range map[string]float64{"0.1": 4, "0.5": 0, "+Inf": 1} {
		b := second.one(t, "latency_seconds.bucket", "upper_bound:"+bound, "route:/a")
		if b.Kind != collector.Count || b.Value != want {
			t.Errorf("bucket %s = %+v, want %v", bound, b, want)
		}
	}
	if s := second.one(t, "latency_seconds.sum"); s.Value != 1.5 || s.Kind != collector.Count {
		t.Errorf("sum = %+v", s)
	}
	if n := second.one(t, "latency_seconds.count"); n.Value != 5 {
		t.Errorf("count = %+v", n)
	}
	if n := second.one(t, "rpc_seconds.count"); n.Value != 10 {
		t.Errorf("summary count = %+v", n)
	}
	if second.one(t, "rpc_seconds.sum").Value != 3 {
		t.Error("summary sum")
	}
}

// A target that restarts: its counters go back to near zero. The rate skips
// one scrape rather than going hugely negative; the histogram's delta is
// what it counted since the restart.
func TestCheck_TargetRestart(t *testing.T) {
	restarted := strings.NewReplacer(
		`code="200"} 130`, `code="200"} 5`,
		`le="0.1"} 14`, `le="0.1"} 2`, `le="0.5"} 19`, `le="0.5"} 2`, `le="+Inf"} 21`, `le="+Inf"} 3`,
		`latency_seconds_count{route="/a"} 21`, `latency_seconds_count{route="/a"} 3`,
	).Replace(page2)
	_, url := serve(t, page1, restarted)
	c, fc := check(t, map[string]any{"url": url})
	_, _ = scrape(t, c)
	fc.Advance(15 * time.Second)
	after, err := scrape(t, c)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range after.named("http_requests_total") {
		if m.Value < 0 || slices.Contains(m.Tags, "method:GET") {
			t.Errorf("a reset counter produced %+v", m)
		}
	}
	if b := after.one(t, "latency_seconds.bucket", "upper_bound:+Inf"); b.Value != 1 {
		t.Errorf("+Inf bucket after restart = %v, want 1 (3 since restart, 2 of them lower)", b.Value)
	}
	if n := after.one(t, "latency_seconds.count"); n.Value != 3 {
		t.Errorf("count after restart = %v, want 3", n.Value)
	}
}

func TestCheck_AllowExcludeRenameNamespaceLabels(t *testing.T) {
	_, url := serve(t, page1)
	c, _ := check(t, map[string]any{
		"url":            url,
		"namespace":      "shop",
		"metrics":        []any{"^queue_", "^rpc_", "^untyped"},
		"exclude":        []any{"^untyped"},
		"rename":         map[string]any{"queue_depth": "queue.depth"},
		"exclude_labels": []any{"quantile"},
	})
	g, err := scrape(t, c)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range g {
		names = append(names, m.Name)
	}
	if !slices.Contains(names, "shop.queue.depth") || slices.Contains(names, "queue_depth") ||
		slices.Contains(names, "shop.untyped_thing") || slices.Contains(names, "shop.http_requests_total") {
		t.Fatalf("names %v", names)
	}
	// The health metrics are not the target's and are never renamed.
	g.one(t, "openmetrics.up")
	// quantile is special for a summary: its tag stays even if the label is
	// excluded from the rest; the check sets it itself.
	g.one(t, "shop.rpc_seconds", "quantile:0.5")
}

func TestCheck_ExcludedLabelIsNotATag(t *testing.T) {
	_, url := serve(t, page1, page2)
	c, fc := check(t, map[string]any{"url": url, "exclude_labels": []any{"code"}})
	_, _ = scrape(t, c)
	fc.Advance(15 * time.Second)
	g, _ := scrape(t, c)
	for _, m := range g.named("http_requests_total") {
		for _, tag := range m.Tags {
			if strings.HasPrefix(tag, "code:") {
				t.Fatalf("excluded label became a tag: %v", m.Tags)
			}
		}
	}
}

func TestCheck_OpenMetricsFormat(t *testing.T) {
	pageA := "# TYPE jobs counter\njobs_total 10\njobs_created 1790000000\n# EOF\n"
	pageB := "# TYPE jobs counter\njobs_total 40\njobs_created 1790000000\n# EOF\n"
	tg, url := serve(t, pageA, pageB)
	tg.contentType = "application/openmetrics-text; version=1.0.0"
	c, fc := check(t, map[string]any{"url": url})
	_, _ = scrape(t, c)
	fc.Advance(10 * time.Second)
	g, err := scrape(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if m := g.one(t, "jobs_total"); m.Value != 3 {
		t.Errorf("rate = %v", m.Value)
	}
	if len(g.named("jobs_created")) != 0 {
		t.Error("_created was reported as a counter")
	}
}

func TestCheck_GaugeHistogramAndInfo(t *testing.T) {
	page := "# TYPE q gaugehistogram\nq_gbucket{le=\"1\"} 3\nq_gbucket{le=\"+Inf\"} 5\nq_gsum 4\nq_gcount 5\n" +
		"# TYPE build info\nbuild_info{version=\"1.2\"} 1\n# EOF\n"
	_, url := serve(t, page)
	c, _ := check(t, map[string]any{"url": url})
	g, err := scrape(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if b := g.one(t, "q.bucket", "upper_bound:1"); b.Value != 3 || b.Kind != collector.Gauge {
		t.Errorf("gbucket = %+v", b)
	}
	if g.one(t, "q.gcount").Value != 5 || g.one(t, "q.gsum").Value != 4 {
		t.Error("gsum/gcount")
	}
	g.one(t, "build_info", "version:1.2")
}

func TestCheck_FailedScrapes(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(*target)
		page  string
		want  string
	}{
		"status":    {func(tg *target) { tg.status = 503 }, "", "503"},
		"malformed": {nil, "metric{ 1\n", "line 1"},
		"too big":   {nil, "big 1\n" + strings.Repeat("# padding\n", 20), "size limit"},
	} {
		tg, url := serve(t, tc.page)
		if tc.setup != nil {
			tc.setup(tg)
		}
		c, _ := check(t, map[string]any{"url": url, "max_body": 64})
		g, err := scrape(t, c)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
		if up := g.one(t, "openmetrics.up"); up.Value != 0 {
			t.Errorf("%s: up = %v", name, up.Value)
		}
		g.one(t, "openmetrics.scrape_duration")
	}
	// Nothing listening.
	c, _ := check(t, map[string]any{"url": "http://127.0.0.1:1/metrics"})
	if g, err := scrape(t, c); err == nil || g.one(t, "openmetrics.up").Value != 0 {
		t.Error("an unreachable target is not up=0 and an error")
	}
}

func TestCheck_MaxSeries(t *testing.T) {
	var b strings.Builder
	b.WriteString("# TYPE g gauge\n")
	for i := range 10 {
		fmt.Fprintf(&b, "g{i=\"%d\"} 1\n", i)
	}
	_, url := serve(t, b.String())
	c, _ := check(t, map[string]any{"url": url, "max_series": 4})
	g, err := scrape(t, c)
	if err == nil || !strings.Contains(err.Error(), "6 metrics dropped") {
		t.Fatalf("err = %v", err)
	}
	if n := len(g.named("g")); n != 4 {
		t.Fatalf("emitted %d, want the cap of 4", n)
	}
	g.one(t, "openmetrics.up") // the health metrics are outside the cap
}

func TestNew_Refuses(t *testing.T) {
	for name, tc := range map[string]struct {
		settings map[string]any
		want     string
	}{
		"no url":            {map[string]any{}, "url"},
		"relative url":      {map[string]any{"url": "/metrics"}, "url"},
		"ftp url":           {map[string]any{"url": "ftp://x/metrics"}, "url"},
		"unknown setting":   {map[string]any{"url": "http://x/", "urll": "http://y/"}, "urll"},
		"bad allow regex":   {map[string]any{"url": "http://x/", "metrics": []any{"("}}, "metrics"},
		"bad exclude regex": {map[string]any{"url": "http://x/", "exclude": []any{"("}}, "exclude"},
		"negative cap":      {map[string]any{"url": "http://x/", "max_series": -1}, "negative"},
	} {
		_, err := New(collector.Instance{Name: "openmetrics", Settings: tc.settings})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestCheck_Defaults(t *testing.T) {
	c, err := newCheck(Config{URL: "http://x/"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.Timeout != DefaultTimeout || c.cfg.MaxBody != DefaultMaxBody || c.cfg.MaxSeries != DefaultMaxSeries ||
		c.Name() != "openmetrics" || c.Interval() != 0 {
		t.Fatalf("%+v", c.cfg)
	}
}

// A series that disappears is forgotten, so state is bounded by what the
// target exposes now.
func TestCheck_ForgetsWhatDisappears(t *testing.T) {
	_, url := serve(t, page1, "# TYPE other gauge\nother 1\n")
	c, fc := check(t, map[string]any{"url": url})
	ck := c.(*Check)
	_, _ = scrape(t, c)
	if ck.rates.Len() == 0 || len(ck.buckets) == 0 || len(ck.counts.last) == 0 {
		t.Fatal("nothing tracked after the first scrape")
	}
	for range collector.ForgetAfter/(15*time.Second) + 2 {
		fc.Advance(15 * time.Second)
		_, _ = scrape(t, c)
	}
	if ck.rates.Len() != 0 || len(ck.buckets) != 0 || len(ck.counts.last) != 0 {
		t.Fatalf("still tracking %d rates, %d histograms, %d counts", ck.rates.Len(), len(ck.buckets), len(ck.counts.last))
	}
}

func TestDeltas_NonFiniteForgets(t *testing.T) {
	d := newDeltas()
	d.observe("k", 1, t0)
	if _, ok := d.observe("k", nan(), t0); ok || len(d.last) != 0 {
		t.Fatal("a NaN reading was kept")
	}
}

func nan() float64 { var z float64; return z / z }

// With histogram_buckets_as_distributions, the interval's buckets arrive as
// one sketch: 4 observations in (0,0.1], 1 above 0.5, and no .bucket counts.
func TestCheck_HistogramsAsDistributions(t *testing.T) {
	_, url := serve(t, page1, page2)
	c, fc := check(t, map[string]any{"url": url, "histogram_buckets_as_distributions": true})
	if _, err := scrape(t, c); err != nil {
		t.Fatal(err)
	}
	fc.Advance(15 * time.Second)
	second, err := scrape(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(second.named("latency_seconds.bucket")); n != 0 {
		t.Fatalf("%d bucket counts sent alongside the distribution", n)
	}
	d := second.one(t, "latency_seconds", "route:/a")
	if d.Kind != collector.Distribution || d.Sketch == nil || d.Sketch.Count() != 5 {
		t.Fatalf("distribution = %+v", d)
	}
	if p50, _ := d.Sketch.Quantile(0.5); p50 <= 0 || p50 > 0.1*1.02 {
		t.Errorf("p50 = %v, want within the (0, 0.1] bucket", p50)
	}
	if second.one(t, "latency_seconds.count").Value != 5 {
		t.Error(".count is still sent as a count")
	}
}
