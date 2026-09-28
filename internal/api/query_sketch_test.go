package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/sketch"
	"github.com/tuvo1106/ozymandias/internal/sketchstore"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/naive"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// sketchAPI is the fixture the heatmap endpoint needs: a distribution metric
// with real sketches behind it.
//
// The real sketch store rather than a fake, because the identity rule it
// enforces — a sketch is filed under the bare metric while selection runs
// against `<metric>.count` (ADR-0015) — is precisely what this endpoint depends
// on, and a fake would agree with whatever the handler did.
func sketchAPI(t *testing.T) http.Handler {
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
	for _, m := range []struct {
		name string
		kind wire.Kind
	}{{"lat", wire.KindDistribution}, {"lat.count", wire.KindCount}} {
		if err := md.Observe(ctx, m.name, m.kind, 10, now); err != nil {
			t.Fatal(err)
		}
	}

	// Two hosts, ten seconds apart, with distributions that do not overlap —
	// so a merge is visible in the answer and a mix-up would be too.
	for _, s := range []struct {
		host   string
		at     int64
		values []float64
	}{
		{"a", now.Unix() - 20, []float64{1, 2, 3, 4}},
		{"b", now.Unix() - 20, []float64{100, 200}},
		{"a", now.Unix() - 10, []float64{5}},
	} {
		sk8 := sketch.NewDefault()
		for _, v := range s.values {
			if err := sk8.Add(v); err != nil {
				t.Fatal(err)
			}
		}
		tags := []string{"host:" + s.host}
		if _, err := store.Append(ctx, []tsdb.SeriesSamples{{
			Series:  tsdb.NewSeriesRef("lat"+wire.SuffixCount, tags),
			Samples: []tsdb.Sample{{T: s.at * 1000, V: sk8.Count()}},
		}}); err != nil {
			t.Fatal(err)
		}
		if _, err := sk.Append(ctx, []sketchstore.Entry{{
			Series: tsdb.NewSeriesRef("lat", tags),
			Points: []sketchstore.Point{{TimeMs: s.at * 1000, Sketch: sk8}},
		}}); err != nil {
			t.Fatal(err)
		}
	}

	mux := http.NewServeMux()
	(&Metrics{Store: store, Sketches: sk, Types: md, Clock: testutil.NewFakeClock(now)}).Register(mux)
	return mux
}

// sketchResponse is the documented shape, decoded strictly: a client that
// implements this struct must be able to read what the endpoint sends.
type sketchResponse struct {
	Status   string  `json:"status"`
	Query    string  `json:"query"`
	From     int64   `json:"from"`
	To       int64   `json:"to"`
	Interval int64   `json:"interval"`
	Gamma    float64 `json:"gamma"`
	Bins     int     `json:"bins"`
	Series   []struct {
		Metric  string            `json:"metric"`
		Tags    map[string]string `json:"tags"`
		Scope   string            `json:"scope"`
		Buckets []struct {
			T     int64       `json:"t"`
			Count float64     `json:"count"`
			Sum   float64     `json:"sum"`
			Min   float64     `json:"min"`
			Max   float64     `json:"max"`
			Bins  [][]float64 `json:"bins"`
		} `json:"buckets"`
	} `json:"series"`
	Warnings []string `json:"warnings"`
}

func getSketch(t *testing.T, h http.Handler, url string) (int, sketchResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	var out sketchResponse
	dec := json.NewDecoder(strings.NewReader(rec.Body.String()))
	dec.DisallowUnknownFields()
	if rec.Code == 200 {
		if err := dec.Decode(&out); err != nil {
			t.Fatalf("%s: the documented shape does not decode the response: %v\n%s", url, err, rec.Body.String())
		}
	}
	return rec.Code, out
}

func TestQuerySketch_AnswersADistribution(t *testing.T) {
	h := sketchAPI(t)
	code, out := getSketch(t, h, "/api/v1/query/sketch?q="+urlEncode("dist:lat{*}")+window)
	if code != 200 {
		t.Fatalf("%d", code)
	}
	if out.Status != "ok" || out.Query != "dist:lat{*}" {
		t.Errorf("%+v", out)
	}
	if len(out.Series) != 1 {
		t.Fatalf("%d series, want one merged group", len(out.Series))
	}
	s := out.Series[0]
	if s.Metric != "lat" || s.Scope != "*" {
		t.Errorf("%+v", s)
	}
	if len(s.Buckets) == 0 {
		t.Fatal("no buckets")
	}
	// Everything merged into one bucket: both hosts, and both timestamps,
	// because the fixture's two instants are ten seconds apart inside a
	// twenty-second bucket. Exact aggregates prove the merge without depending
	// on the bin layout.
	first := s.Buckets[0]
	if first.Count != 7 || first.Min != 1 || first.Max != 200 {
		t.Errorf("the merge lost observations: %+v", first)
	}
	if first.Sum != 1+2+3+4+5+100+200 {
		t.Errorf("sum = %v", first.Sum)
	}
	// The bins are the heatmap's rows: three numbers each, ascending, and they
	// account for every observation.
	total := 0.0
	for i, b := range first.Bins {
		if len(b) != 3 {
			t.Fatalf("bin %d is %d numbers, want [lower, upper, count]", i, len(b))
		}
		if b[0] > b[1] {
			t.Errorf("bin %d is inverted: %v", i, b)
		}
		if i > 0 && b[0] < first.Bins[i-1][0] {
			t.Errorf("bin %d is out of order: %v", i, b)
		}
		total += b[2]
	}
	if total != first.Count {
		t.Errorf("bins hold %g of %g observations", total, first.Count)
	}
	if out.Gamma <= 1 || out.Bins == 0 {
		t.Errorf("gamma = %v, bins = %d", out.Gamma, out.Bins)
	}
	if out.Warnings == nil {
		t.Error("warnings is absent rather than empty")
	}
}

// The grid splits the distribution in time, which is the axis a heatmap draws
// along: the same data at a ten-second interval is two columns, not one.
func TestQuerySketch_TheGridIsTheHeatmapsTimeAxis(t *testing.T) {
	h := sketchAPI(t)
	code, out := getSketch(t, h,
		"/api/v1/query/sketch?q="+urlEncode("dist:lat{*}")+"&from=1789999980&to=1790000000&interval=10")
	if code != 200 {
		t.Fatalf("%d", code)
	}
	buckets := out.Series[0].Buckets
	if len(buckets) != 2 {
		t.Fatalf("%d buckets at a 10s interval, want 2: %+v", len(buckets), buckets)
	}
	if buckets[1].T-buckets[0].T != 10_000 {
		t.Errorf("buckets are %dms apart, want 10000", buckets[1].T-buckets[0].T)
	}
	// The earlier bucket holds both hosts; the later one holds the single
	// later observation.
	if buckets[0].Count != 6 || buckets[1].Count != 1 {
		t.Errorf("counts %g and %g, want 6 and 1", buckets[0].Count, buckets[1].Count)
	}
	if buckets[1].Min != 5 || buckets[1].Max != 5 {
		t.Errorf("the later bucket is %+v", buckets[1])
	}
}

// Grouping splits the distribution, exactly as it splits a line chart — one
// heatmap per host.
func TestQuerySketch_GroupsLikeEveryOtherQuery(t *testing.T) {
	h := sketchAPI(t)
	code, out := getSketch(t, h, "/api/v1/query/sketch?q="+urlEncode("dist:lat{*} by {host}")+window)
	if code != 200 {
		t.Fatalf("%d", code)
	}
	if len(out.Series) != 2 {
		t.Fatalf("%d series, want one per host", len(out.Series))
	}
	scopes := map[string]bool{}
	for _, s := range out.Series {
		scopes[s.Scope] = true
		if s.Tags["host"] == "" {
			t.Errorf("%+v has no host tag", s)
		}
	}
	if !scopes["host:a"] || !scopes["host:b"] {
		t.Errorf("scopes %v", scopes)
	}
}

// The two endpoints answer different questions about the same metric, and each
// says where the other one is.
func TestQuerySketch_SendsTheWrongQueryToTheRightPlace(t *testing.T) {
	h := sketchAPI(t)

	// A number asked of the distribution endpoint.
	code, _ := getSketch(t, h, "/api/v1/query/sketch?q="+urlEncode("p95:lat{*}")+window)
	if code != 400 {
		t.Errorf("p95 on /query/sketch gave %d, want 400", code)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/query/sketch?q="+urlEncode("p95:lat{*}")+window, nil))
	if !strings.Contains(rec.Body.String(), "/api/v1/query") {
		t.Errorf("the refusal does not say where it belongs: %s", rec.Body.String())
	}

	// And a distribution asked of the numeric endpoint.
	code2, out := get(t, h, "/api/v1/query?q="+urlEncode("dist:lat{*}")+window)
	if code2 != 400 {
		t.Errorf("dist: on /query gave %d, want 400", code2)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "/api/v1/query/sketch") {
		t.Errorf("the refusal does not say where it belongs: %q", msg)
	}
}

// POST takes the same query, because a dashboard's outgrows a URL — and the
// window rules are the single-query endpoint's, not a second set.
func TestQuerySketch_POSTAndTheSameWindowRules(t *testing.T) {
	h := sketchAPI(t)
	rec := httptest.NewRecorder()
	body := `{"q":"dist:lat{*}","from":1789999980,"to":1790000000,"interval":20}`
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/query/sketch", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var out sketchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.From != 1789999980 || out.To != 1790000000 || out.Interval != 20 {
		t.Errorf("window %d/%d/%d", out.From, out.To, out.Interval)
	}
	if len(out.Series) == 0 {
		t.Error("POST found nothing that GET found")
	}
}

// A server that stores no sketches cannot answer this endpoint at all, where on
// /api/v1/query only a percentile needs one. 503, not 400: the query is fine.
func TestQuerySketch_NoSketchStoreIs503(t *testing.T) {
	h := sketchlessAPI(t)
	code, _ := getSketch(t, h, "/api/v1/query/sketch?q="+urlEncode("dist:lat{*}")+window)
	if code != 503 {
		t.Errorf("%d, want 503", code)
	}
}

// A metric that is not a distribution has no distribution to draw.
func TestQuerySketch_ANonDistributionIs400(t *testing.T) {
	h, _ := metricsAPI(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/query/sketch?q="+urlEncode("dist:http.request.count{*}")+window, nil))
	if rec.Code != 400 {
		t.Fatalf("%d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not a distribution") {
		t.Errorf("%s", rec.Body.String())
	}
}
