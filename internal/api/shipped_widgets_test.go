package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/dashboard"
	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/query/metricql"
	"github.com/tuvo1106/ozymandias/internal/sketch"
	"github.com/tuvo1106/ozymandias/internal/sketchstore"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/naive"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// L8: every shipped dashboard, answered the way the UI answers it.
//
// shipped_test.go in internal/dashboard only parses these files, and a widget
// can parse and still be blank, or an error, forever: a query the planner
// refuses (the bucket limit, a percentile of a gauge, `avg` of a distribution),
// a filter nothing can satisfy, a ratio whose halves never join. None of that
// fails startup, smoke or a parse, so each of those was found by looking at a
// chart. This posts each dashboard's queries to /api/v1/query/batch, as one
// request like the UI does, over data that matches what the queries ask for, and
// requires an answer with at least one line for every widget.
//
// What it cannot find, and so what a person still has to check: a widget that
// asks for a tag the real emitter does not set. The data here is derived from
// the queries, so it always has the tag. (The sandbox-exits widget filtered on
// `compose_project`, which sandboxes do not have; this test would have passed.)
// That check needs the emitted tags, which docs/metrics-catalog.md holds in
// prose and nothing holds in code.
func TestShippedDashboardsDrawLines(t *testing.T) {
	dir := filepath.Join("..", "..", "deploy", "dashboards")
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no shipped dashboards in %s (%v): this test has stopped checking anything", dir, err)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			d, err := dashboard.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			vars := shippedVars(d)
			var queries, owner []string
			for _, w := range d.Widgets {
				for _, q := range w.Queries {
					queries = append(queries, q.Q)
					owner = append(owner, w.ID)
				}
			}
			if len(queries) == 0 {
				t.Skip("no queries")
			}
			h := shippedAPI(t, synthesize(t, queries, vars))
			// A heatmap's `dist:` query is not a number, so it goes to
			// /api/v1/query/sketch (ADR-0019), as the UI sends it; everything else
			// is one batch, as the UI sends that.
			var numeric, numericOwner []string
			for i, q := range queries {
				if strings.HasPrefix(strings.TrimSpace(q), "dist:") {
					if n := sketchLines(t, h, q, vars); n == 0 {
						t.Errorf("widget %q drew no lines, though data matching its query exists\n  query: %s", owner[i], q)
					}
					continue
				}
				numeric = append(numeric, q)
				numericOwner = append(numericOwner, owner[i])
			}
			if len(numeric) == 0 {
				return
			}
			for i, r := range batch(t, h, numeric, vars) {
				if r["status"] != "ok" {
					t.Errorf("widget %q: %s\n  query: %s", numericOwner[i], r["error"], numeric[i])
					continue
				}
				if lines, _ := r["series"].([]any); len(lines) == 0 {
					t.Errorf("widget %q drew no lines, though data matching its query exists\n  query: %s", numericOwner[i], numeric[i])
				}
			}
		})
	}
}

// The case that bit us: an error-rate chart must show a line when every request
// is an error, and, by documented design, none when there are no errors
// (docs/dashboards.md). A "fix" that drew 0% for a healthy service blanked it
// during a total outage; that is why this is pinned on the widget that ships
// rather than on the evaluator alone.
func TestShippedErrorRateWidget(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "dashboards", "service.json"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := dashboard.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	var q string
	for _, w := range d.Widgets {
		if w.ID == "error-rate" {
			q = w.Queries[0].Q
		}
	}
	if q == "" {
		t.Fatal("service.json has no error-rate widget")
	}
	vars := map[string][]string{"service": {"service:checkout"}, "env": {"env:dev"}}

	at := func(statuses ...string) []tsdb.SeriesSamples {
		var out []tsdb.SeriesSamples
		for i, s := range statuses {
			out = append(out, tsdb.SeriesSamples{
				Series:  tsdb.NewSeriesRef("http.request.count", []string{"service:checkout", "env:dev", "status:" + s}),
				Samples: counterSamples(i + 1),
			})
		}
		return out
	}
	lineCount := func(data []tsdb.SeriesSamples) (int, float64) {
		r := batch(t, shippedAPI(t, dataset{kinds: map[string]wire.Kind{"http.request.count": wire.KindCount}, series: data}), []string{q}, vars)[0]
		if r["status"] != "ok" {
			t.Fatalf("%v", r)
		}
		lines, _ := r["series"].([]any)
		if len(lines) == 0 {
			return 0, 0
		}
		var peak float64
		for _, p := range lines[0].(map[string]any)["points"].([]any) {
			if v, ok := p.([]any)[1].(float64); ok && v > peak {
				peak = v
			}
		}
		return len(lines), peak
	}

	if n, _ := lineCount(at("200", "404")); n != 0 {
		t.Errorf("healthy: drew %d lines; the widget's design is no line when there are no 5xx", n)
	}
	if n, peak := lineCount(at("200", "500")); n != 1 || peak <= 0 || peak >= 100 {
		t.Errorf("mixed: %d lines, peak %g; want one line between 0 and 100", n, peak)
	}
	if n, peak := lineCount(at("500", "503")); n != 1 || peak != 100 {
		t.Errorf("total outage: %d lines, peak %g; want one line at 100", n, peak)
	}
}

// --- fixtures ---

type dataset struct {
	kinds  map[string]wire.Kind
	series []tsdb.SeriesSamples
	// sketches maps a distribution's series key to its sketches' series.
	sketches []sketchstore.Entry
}

// counterSamples are eleven ten-second points ending at now, so a 300 s window
// has data in its last buckets whatever interval the planner picks.
func counterSamples(base int) []tsdb.Sample {
	var out []tsdb.Sample
	for i := 0; i <= 10; i++ {
		out = append(out, tsdb.Sample{T: (now.Unix() - int64(10*(10-i))) * 1000, V: float64(base + i)})
	}
	return out
}

// shippedVars resolves a dashboard's template variables the way a viewer's
// defaults would: a concrete default becomes a constraint, "*" (all) none.
func shippedVars(d dashboard.Dashboard) map[string][]string {
	vars := map[string][]string{}
	for _, tv := range d.TemplateVars {
		if tv.Default == "" || tv.Default == "*" {
			vars[tv.Name] = []string{}
			continue
		}
		vars[tv.Name] = []string{tv.Tag + ":" + tv.Default}
	}
	return vars
}

// synthesize builds the series each query selects: its positive matchers set
// tags (a trailing `*` becomes `00`, so `status:5*` is `status:500`), a negated
// matcher sets none (absence satisfies it), and each `by` key the filter does
// not pin gets two values so grouping has something to group. A metric's type is
// what its uses imply: a percentile means a distribution, a rate or count
// modifier a count, anything else a gauge.
func synthesize(t *testing.T, queries []string, vars map[string][]string) dataset {
	t.Helper()
	ds := dataset{kinds: map[string]wire.Kind{}}
	seen := map[string]bool{}
	dist := map[string]bool{}
	var wanted []struct {
		q    *metricql.Query
		tags map[string]string
	}
	for _, text := range queries {
		expr, err := metricql.Parse(text)
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		metricql.Walk(expr, func(n metricql.Node) {
			q, ok := n.(*metricql.Query)
			if !ok {
				return
			}
			tags := map[string]string{}
			for _, m := range q.Filter {
				switch {
				case m.Var != "":
					for _, kv := range vars[m.Var] {
						k, v, _ := strings.Cut(kv, ":")
						tags[k] = v
					}
				case m.Neg:
				case len(m.Values) > 0:
					tags[m.Key] = strings.ReplaceAll(m.Values[0], "*", "00")
				}
			}
			wanted = append(wanted, struct {
				q    *metricql.Query
				tags map[string]string
			}{q, tags})
			if _, pct := q.Agg.Quantile(); pct || q.Agg == metricql.Dist {
				dist[q.Metric] = true
				return
			}
			for _, m := range q.Modifiers {
				if m.Kind == metricql.ModAsRate || m.Kind == metricql.ModAsCount {
					ds.kinds[q.Metric] = wire.KindCount
				}
			}
		})
	}
	for _, w := range wanted {
		if dist[w.q.Metric] {
			ds.kinds[w.q.Metric] = wire.KindDistribution
		} else if _, ok := ds.kinds[w.q.Metric]; !ok {
			ds.kinds[w.q.Metric] = wire.KindGauge
		}
		// The cross product of the unpinned by-keys, two values each.
		combos := []map[string]string{{}}
		for _, k := range w.q.By {
			if _, pinned := w.tags[k]; pinned {
				continue
			}
			var next []map[string]string
			for _, c := range combos {
				for _, v := range []string{k + "1", k + "2"} {
					n := map[string]string{k: v}
					for ck, cv := range c {
						n[ck] = cv
					}
					next = append(next, n)
				}
			}
			combos = next
		}
		for _, c := range combos {
			all := map[string]string{}
			for k, v := range w.tags {
				all[k] = v
			}
			for k, v := range c {
				all[k] = v
			}
			var tags []string
			for k, v := range all {
				tags = append(tags, k+":"+v)
			}
			sort.Strings(tags)
			key := w.q.Metric + "{" + strings.Join(tags, ",") + "}"
			if seen[key] {
				continue
			}
			seen[key] = true
			if dist[w.q.Metric] {
				sk := sketch.NewDefault()
				for v := 1; v <= 50; v++ {
					_ = sk.Add(float64(v))
				}
				ds.series = append(ds.series, tsdb.SeriesSamples{
					Series:  tsdb.NewSeriesRef(w.q.Metric+wire.SuffixCount, tags),
					Samples: counterSamples(50),
				})
				var pts []sketchstore.Point
				for _, s := range counterSamples(1) {
					pts = append(pts, sketchstore.Point{TimeMs: s.T, Sketch: sk})
				}
				ds.sketches = append(ds.sketches, sketchstore.Entry{Series: tsdb.NewSeriesRef(w.q.Metric, tags), Points: pts})
				continue
			}
			ds.series = append(ds.series, tsdb.SeriesSamples{Series: tsdb.NewSeriesRef(w.q.Metric, tags), Samples: counterSamples(5)})
		}
	}
	return ds
}

// shippedAPI serves a dataset through the real handler over the real stores.
func shippedAPI(t *testing.T, ds dataset) http.Handler {
	t.Helper()
	dir := t.TempDir()
	store, err := naive.Open(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	md, err := meta.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = md.Close() })
	sk, err := sketchstore.Open(sketchstore.Options{Dir: filepath.Join(dir, "sketches"), Retention: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sk.Close() })

	ctx := context.Background()
	for name, kind := range ds.kinds {
		if err := md.Observe(ctx, name, kind, 10, now); err != nil {
			t.Fatal(err)
		}
		if kind == wire.KindDistribution {
			if err := md.Observe(ctx, name+wire.SuffixCount, wire.KindCount, 10, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, s := range ds.series {
		if _, err := store.Append(ctx, []tsdb.SeriesSamples{s}); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range ds.sketches {
		if _, err := sk.Append(ctx, []sketchstore.Entry{e}); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	(&Metrics{Store: store, Sketches: sk, Types: md, Clock: testutil.NewFakeClock(now)}).Register(mux)
	return mux
}

// batch posts queries as one request, like a dashboard does, and returns the
// per-query results in order.
func batch(t *testing.T, h http.Handler, queries []string, vars map[string][]string) []map[string]any {
	t.Helper()
	type spec struct {
		Q string `json:"q"`
	}
	body := struct {
		Queries []spec              `json:"queries"`
		From    int64               `json:"from"`
		To      int64               `json:"to"`
		Vars    map[string][]string `json:"vars"`
	}{From: now.Unix() - 300, To: now.Unix(), Vars: vars}
	for _, q := range queries {
		body.Queries = append(body.Queries, spec{Q: q})
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	code, out := post(t, h, "/api/v1/query/batch", string(raw))
	if code != 200 {
		t.Fatalf("batch: %d %v", code, out)
	}
	results := resultsOf(t, out)
	if len(results) != len(queries) {
		t.Fatalf("got %d results for %d queries", len(results), len(queries))
	}
	return results
}

// sketchLines asks /api/v1/query/sketch the way the heatmap widget does and
// returns how many lines came back.
func sketchLines(t *testing.T, h http.Handler, q string, vars map[string][]string) int {
	t.Helper()
	url := fmt.Sprintf("/api/v1/query/sketch?q=%s&from=%d&to=%d", urlEncode(q), now.Unix()-300, now.Unix())
	for name, values := range vars {
		if len(values) == 0 {
			// "All": a variable bound to nothing, which a query string spells as an
			// empty value (see queryRequest's var.* handling).
			url += "&var." + name + "="
		}
		for _, v := range values {
			url += "&var." + name + "=" + urlEncode(v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != 200 {
		t.Fatalf("%s: %d %s", q, rec.Code, rec.Body.String())
	}
	_, out := getSketch(t, h, url)
	return len(out.Series)
}
