package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/naive"
)

// cardinalityAPI serves a store holding series per metric as given, each
// series with a distinct "id" tag plus the listed tags.
func cardinalityAPI(t *testing.T, series map[string]int, tags ...string) http.Handler {
	t.Helper()
	store, err := naive.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for metric, n := range series {
		for i := 0; i < n; i++ {
			ref := tsdb.NewSeriesRef(metric, append([]string{fmt.Sprintf("id:%d", i)}, tags...))
			if _, err := store.Append(context.Background(), []tsdb.SeriesSamples{{Series: ref, Samples: []tsdb.Sample{{T: 1, V: 1}}}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	mux := http.NewServeMux()
	(&Metrics{Store: store}).Register(mux)
	return mux
}

func names(out map[string]any) string {
	var got []string
	for _, row := range out["metrics"].([]any) {
		r := row.(map[string]any)
		got = append(got, fmt.Sprintf("%s=%v", r["name"], r["series"]))
	}
	return strings.Join(got, " ")
}

// Highest first, because the page exists to find the one that is too high;
// names break ties so the order is stable between loads.
func TestMetricsCardinality_HighestFirst(t *testing.T) {
	h := cardinalityAPI(t, map[string]int{"b": 2, "a": 2, "c": 5, "d": 1})
	code, out := get(t, h, "/api/v1/metrics/cardinality")
	if code != 200 || names(out) != "c=5 a=2 b=2 d=1" || out["total"] != 4.0 || out["truncated"] != false {
		t.Fatalf("%d %v", code, out)
	}
}

// "The highest" is a question about every metric, so all are counted and the
// limit only bounds the answer — and says what it left out.
func TestMetricsCardinality_LimitCutsAfterRanking(t *testing.T) {
	h := cardinalityAPI(t, map[string]int{"a": 1, "b": 2, "z": 9})
	code, out := get(t, h, "/api/v1/metrics/cardinality?limit=1")
	if code != 200 || names(out) != "z=9" || out["total"] != 3.0 || out["truncated"] != true {
		t.Fatalf("%d %v", code, out)
	}
	_, out = get(t, h, "/api/v1/metrics/cardinality?prefix=a")
	if names(out) != "a=1" || out["total"] != 1.0 {
		t.Fatalf("prefix: %v", out)
	}
	if code, _ := get(t, h, "/api/v1/metrics/cardinality?limit=0"); code != 400 {
		t.Errorf("limit=0: %d", code)
	}
}

// Most values first: the key with the most values is the likely reason a
// metric is high.
func TestTagsCardinality_MostValuesFirst(t *testing.T) {
	h := cardinalityAPI(t, map[string]int{"m": 3}, "env:prod", "canary")
	code, out := get(t, h, "/api/v1/tags/cardinality?metric=m")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	var got []string
	for _, k := range out["keys"].([]any) {
		r := k.(map[string]any)
		got = append(got, fmt.Sprintf("%s:%v/%v", r["key"], r["series"], r["values"]))
	}
	if strings.Join(got, " ") != "id:3/3 env:3/1 canary:3/0" || out["type"] != nil {
		t.Fatalf("keys %v, type %v", got, out["type"])
	}
	if out["series"] != 3.0 {
		t.Errorf("series: %v", out["series"])
	}
	// Unknown is series 0 — not a metric with no tags, which has series.
	if _, out := get(t, h, "/api/v1/tags/cardinality?metric=nope"); len(out["keys"].([]any)) != 0 || out["series"] != 0.0 {
		t.Errorf("unknown metric: %v", out)
	}
	if code, _ := get(t, h, "/api/v1/tags/cardinality"); code != 400 {
		t.Errorf("no metric: %d", code)
	}
}

type failingCounts struct{ tsdb.MetricStore }

func (failingCounts) SeriesCounts(context.Context, string) ([]tsdb.MetricSeriesCount, error) {
	return nil, errors.New("open /var/lib/ozy/index.dat: permission denied")
}

func (failingCounts) TagCardinality(context.Context, string) (tsdb.MetricTagCardinality, error) {
	return tsdb.MetricTagCardinality{}, errors.New("open /var/lib/ozy/index.dat: permission denied")
}

// The series count is the metric's own, not its prefix's: "m" is not "m.x".
func TestTagsCardinality_SeriesIsTheExactMetrics(t *testing.T) {
	h := cardinalityAPI(t, map[string]int{"m": 2, "m.x": 7})
	if _, out := get(t, h, "/api/v1/tags/cardinality?metric=m"); out["series"] != 2.0 {
		t.Errorf("series %v, want 2", out["series"])
	}
}

// oneRead answers TagCardinality and fails anything else, so the endpoint can
// only succeed by taking the series count from the same read as the keys.
type oneRead struct{ failingCounts }

func (oneRead) TagCardinality(context.Context, string) (tsdb.MetricTagCardinality, error) {
	return tsdb.MetricTagCardinality{Series: 4, Keys: []tsdb.TagKeyCardinality{{Key: "env", Series: 4, Values: 2}}}, nil
}

// The series count and the keys come from one store read: from two, an
// append between them can put a key on 5 series of a metric with 4.
func TestTagsCardinality_SeriesComesFromTheSameRead(t *testing.T) {
	mux := http.NewServeMux()
	(&Metrics{Store: oneRead{}}).Register(mux)
	code, out := get(t, mux, "/api/v1/tags/cardinality?metric=m")
	if code != 200 || out["series"] != 4.0 {
		t.Errorf("%d series=%v, want 200 series=4", code, out["series"])
	}
}

// A store's error names files; the response says only that it failed.
func TestCardinality_StoreFailureIsNotDescribed(t *testing.T) {
	mux := http.NewServeMux()
	(&Metrics{Store: failingCounts{}}).Register(mux)
	for _, url := range []string{"/api/v1/metrics/cardinality", "/api/v1/tags/cardinality?metric=m"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		if rec.Code != 500 || strings.Contains(rec.Body.String(), "index.dat") || !strings.Contains(rec.Body.String(), "series counts could not be read") {
			t.Errorf("%s: %d %s", url, rec.Code, rec.Body.String())
		}
	}
}
