package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/naive"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

var now = time.Unix(1790000000, 0)

func metricsAPI(t *testing.T) (http.Handler, *naive.Store) {
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
	ctx := context.Background()
	if err := md.Observe(ctx, "http.request.count", wire.KindCount, 10, now); err != nil {
		t.Fatal(err)
	}
	for _, s := range []tsdb.SeriesSamples{
		{Series: tsdb.NewSeriesRef("http.request.count", []string{"route:/a", "env:dev"}), Samples: []tsdb.Sample{{T: (now.Unix() - 20) * 1000, V: 3}, {T: (now.Unix() - 10) * 1000, V: 4}}},
		{Series: tsdb.NewSeriesRef("http.request.count", []string{"route:/b", "env:dev"}), Samples: []tsdb.Sample{{T: (now.Unix() - 10) * 1000, V: 1}}},
		{Series: tsdb.NewSeriesRef("queue.depth", []string{"queue:q"}), Samples: []tsdb.Sample{{T: (now.Unix() - 10) * 1000, V: 8}}},
	} {
		if _, err := store.Append(ctx, []tsdb.SeriesSamples{s}); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	(&Metrics{Store: store, Types: md, Clock: testutil.NewFakeClock(now)}).Register(mux)
	return mux, store
}

func get(t *testing.T, h http.Handler, url string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: body %q is not JSON", url, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("%s: Content-Type %q", url, ct)
	}
	return rec.Code, out
}

func TestQuery_EndToEndOverTheStore(t *testing.T) {
	h, _ := metricsAPI(t)
	code, out := get(t, h, "/api/v1/query?metric=http.request.count&by=route&agg=sum&from=1789999970&to=1789999999&interval=10")
	if code != 200 || out["status"] != "ok" || out["interval"] != 10.0 || out["from"] != 1789999970.0 {
		t.Fatalf("%d %v", code, out)
	}
	series := out["series"].([]any)
	if len(series) != 2 {
		t.Fatalf("series = %v", series)
	}
	first := series[0].(map[string]any)
	if first["tags"].(map[string]any)["route"] != "/a" {
		t.Fatalf("first = %v", first)
	}
	b, _ := json.Marshal(first["points"])
	if string(b) != "[[1789999970000,null],[1789999980000,3],[1789999990000,4]]" {
		t.Fatalf("points %s", b)
	}
}

func TestQuery_DefaultsToTheLastHourAndAvg(t *testing.T) {
	h, _ := metricsAPI(t)
	code, out := get(t, h, "/api/v1/query?metric=queue.depth")
	if code != 200 || out["to"] != float64(now.Unix()) || out["from"] != float64(now.Unix()-3600) || out["interval"] != 20.0 {
		t.Fatalf("%d %v", code, out)
	}
	if len(out["series"].([]any)) != 1 {
		t.Fatalf("series %v", out["series"])
	}
}

func TestQuery_Filters(t *testing.T) {
	h, _ := metricsAPI(t)
	_, out := get(t, h, "/api/v1/query?metric=http.request.count&filter=route:/b&agg=sum&from=1789999980&to=1789999999&interval=20")
	b, _ := json.Marshal(out["series"].([]any)[0].(map[string]any)["points"])
	if string(b) != "[[1789999980000,1]]" {
		t.Fatalf("points %s", b)
	}
}

func TestQuery_BadRequests(t *testing.T) {
	h, _ := metricsAPI(t)
	for url, want := range map[string]string{
		"/api/v1/query":                               "valid metric name",
		"/api/v1/query?metric=m&from=x":               "from must be an integer",
		"/api/v1/query?metric=m&agg=nope":             "agg",
		"/api/v1/query?metric=m&filter=:x":            "no tag key",
		"/api/v1/query?metric=m&from=10&to=5":         "must be after",
		"/api/v1/query?metric=m&interval=1&from=0":    "buckets",
		"/api/v1/tags":                                "metric is required",
		"/api/v1/tags/values?metric=m":                "metric and key",
		"/api/v1/metrics?limit=0":                     "limit",
		"/api/v1/metrics?limit=abc":                   "limit",
		"/api/v1/tags/values?metric=m&key=k&limit=-1": "limit",
	} {
		code, out := get(t, h, url)
		if code != 400 || out["status"] != "error" || !strings.Contains(out["error"].(string), want) {
			t.Errorf("%s: %d %v, want 400 about %q", url, code, out, want)
		}
	}
}

func TestMetadataEndpoints(t *testing.T) {
	h, _ := metricsAPI(t)
	if _, out := get(t, h, "/api/v1/metrics?prefix=http"); strings.Join(toStrings(out["metrics"]), ",") != "http.request.count" {
		t.Errorf("metrics = %v", out)
	}
	if _, out := get(t, h, "/api/v1/metrics"); len(toStrings(out["metrics"])) != 2 {
		t.Errorf("all metrics = %v", out)
	}
	if _, out := get(t, h, "/api/v1/tags?metric=http.request.count"); strings.Join(toStrings(out["keys"]), ",") != "env,route" {
		t.Errorf("tags = %v", out)
	}
	if _, out := get(t, h, "/api/v1/tags/values?metric=http.request.count&key=route&limit=1"); strings.Join(toStrings(out["values"]), ",") != "/a" {
		t.Errorf("values = %v", out)
	}
	if _, out := get(t, h, "/api/v1/tags?metric=absent"); out["keys"] == nil {
		t.Errorf("absent metric keys should be [], got %v", out)
	}
}

func TestEndpoints_StoreFailuresAre500(t *testing.T) {
	_, store := metricsAPI(t)
	_ = store.Close()
	mux := http.NewServeMux()
	(&Metrics{Store: store, Types: noTypes{}}).Register(mux)
	for _, url := range []string{
		"/api/v1/query?metric=m",
		"/api/v1/metrics",
		"/api/v1/tags?metric=m",
		"/api/v1/tags/values?metric=m&key=k",
	} {
		if code, out := get(t, mux, url); code != 500 || out["status"] != "error" {
			t.Errorf("%s: %d %v", url, code, out)
		}
	}
}

type noTypes struct{}

func (noTypes) Metric(string) (meta.Metric, bool) { return meta.Metric{}, false }

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}
