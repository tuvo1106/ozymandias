package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/sketch"
	"github.com/tuvo1106/ozymandias/internal/sketchstore"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// newSketchEnv is newEnv with a real sketch store behind it. Real rather than
// a fake, because the thing worth testing here is that two stores agree about
// what was accepted, and a fake would agree by construction.
func newSketchEnv(t *testing.T, opts Options) (*env, *sketchstore.Store) {
	t.Helper()
	sk, err := sketchstore.Open(sketchstore.Options{Dir: filepath.Join(t.TempDir(), "sketches")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sk.Close() })
	opts.Sketches = sk
	return newEnv(t, opts), sk
}

func (e *env) postSketches(body string) (*httptest.ResponseRecorder, map[string]any) {
	req := httptest.NewRequest(http.MethodPost, "/v1/sketches", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// sketchPayload builds a body carrying one series with one point, built from
// the given values so the aggregates are real rather than asserted.
func sketchPayload(t *testing.T, metric string, tags []string, ts int64, values ...float64) string {
	t.Helper()
	s := sketch.NewDefault()
	for _, v := range values {
		if err := s.Add(v); err != nil {
			t.Fatal(err)
		}
	}
	body, err := json.Marshal(wire.SketchesPayload{Sketches: []wire.SketchSeries{{
		Metric:   metric,
		Tags:     tags,
		Interval: 10,
		Points:   []wire.SketchPoint{{Timestamp: ts, Sketch: s.ToWire()}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// The whole contract in one test: the sketch lands in the sketch store, and
// the four exact aggregates land in the TSDB as ordinary series.
func TestSketches_StoresTheSketchAndItsScalars(t *testing.T) {
	e, sk := newSketchEnv(t, Options{})
	body := sketchPayload(t, "http.request.duration", []string{"route:/a", "env:dev"}, 1790000000, 1, 2, 3, 4, 100)

	rec, out := e.postSketches(body)
	if rec.Code != http.StatusAccepted || out["accepted"] != 1.0 || out["rejected"] != 0.0 {
		t.Fatalf("%d %v", rec.Code, out)
	}

	ref := tsdb.NewSeriesRef("http.request.duration", []string{"env:dev", "route:/a"})
	points, err := sk.Read(context.Background(), ref, 0, 1<<42)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("got %d sketches, want 1", len(points))
	}
	if got := points[0].Sketch.Count(); got != 5 {
		t.Errorf("count = %v, want 5", got)
	}

	for metric, want := range map[string]float64{
		"http.request.duration.count": 5,
		"http.request.duration.sum":   110,
		"http.request.duration.min":   1,
		"http.request.duration.max":   100,
	} {
		if got := firstSample(t, e, metric); got != want {
			t.Errorf("%s = %v, want %v", metric, got, want)
		}
	}

	// And the metric's type is recorded, which is what tells the query layer
	// it answers percentiles.
	if m, ok := e.meta.Metric("http.request.duration"); !ok || m.Type != wire.KindDistribution {
		t.Errorf("metadata: %+v %v, want a distribution", m, ok)
	}
	if m, ok := e.meta.Metric("http.request.duration.count"); !ok || m.Type != wire.KindCount || m.Interval != 10 {
		t.Errorf(".count metadata: %+v %v, want a count at interval 10", m, ok)
	}
	if m, ok := e.meta.Metric("http.request.duration.min"); !ok || m.Type != wire.KindGauge || m.Interval != 0 {
		t.Errorf(".min metadata: %+v %v, want a gauge", m, ok)
	}
}

func firstSample(t *testing.T, e *env, metric string) float64 {
	t.Helper()
	set, err := e.store.Select(context.Background(), tsdb.Selector{Metric: metric}, 0, 1<<62)
	if err != nil {
		t.Fatalf("Select(%s): %v", metric, err)
	}
	defer set.Close()
	if !set.Next() {
		t.Fatalf("%s: no series", metric)
	}
	it := set.Iterator()
	if !it.Next() {
		t.Fatalf("%s: no samples", metric)
	}
	return it.At().V
}

// A metric that arrived as a series cannot become a distribution, for exactly
// the reason a count cannot become a gauge: every past query would be
// ambiguous.
func TestSketches_TypeConflict(t *testing.T) {
	e, _ := newSketchEnv(t, Options{})
	if rec, _ := e.post(gz(t, payload(good)), true); rec.Code != http.StatusAccepted {
		t.Fatalf("setup: %d", rec.Code)
	}
	body := sketchPayload(t, "http.request.count", []string{"route:/a"}, 1790000000, 1, 2)
	rec, out := e.postSketches(body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%d %v", rec.Code, out)
	}
	if out["accepted"] != 0.0 || out["rejected"] != 1.0 {
		t.Fatalf("%v", out)
	}
	if errs, _ := out["errors"].([]any); len(errs) != 1 || !strings.Contains(errs[0].(string), "type conflict") {
		t.Errorf("errors = %v, want a type conflict", out["errors"])
	}
}

// A name at the length limit cannot carry a suffix, and saying so beats
// writing three of the four scalar series and dropping the fourth.
func TestSketches_RejectsANameTooLongForItsSuffixes(t *testing.T) {
	e, _ := newSketchEnv(t, Options{})
	long := "a" + strings.Repeat("b", wire.MaxMetricNameLen-1)
	if !wire.ValidMetricName(long) {
		t.Fatalf("test fixture is not a valid metric name")
	}
	rec, out := e.postSketches(sketchPayload(t, long, nil, 1790000000, 1))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%d %v", rec.Code, out)
	}
	if out["rejected"] != 1.0 {
		t.Fatalf("%v", out)
	}
	if errs, _ := out["errors"].([]any); len(errs) != 1 || !strings.Contains(errs[0].(string), "too long") {
		t.Errorf("errors = %v, want one about the name being too long", out["errors"])
	}
}

// The two stores must hold the same set of buckets. The TSDB is append-only
// and the sketch store is last-write-wins, so the TSDB rules: a bucket it
// refuses as out of order gets no sketch either, because `.count` is what a
// percentile query selects on and a sketch it cannot find is not data, it is
// disk.
func TestSketches_ABucketTheTSDBRefusesGetsNoSketch(t *testing.T) {
	e, sk := newSketchEnv(t, Options{})
	tags := []string{"route:/a"}

	if rec, out := e.postSketches(sketchPayload(t, "d", tags, 1790000010, 5, 6)); rec.Code != http.StatusAccepted || out["accepted"] != 1.0 {
		t.Fatalf("setup: %d %v", rec.Code, out)
	}
	// Older than what is already stored: append-only refuses it.
	rec, out := e.postSketches(sketchPayload(t, "d", tags, 1790000000, 1, 2))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%d %v", rec.Code, out)
	}
	if out["accepted"] != 0.0 {
		t.Fatalf("the older bucket was accepted: %v", out)
	}

	points, err := sk.Read(context.Background(), tsdb.NewSeriesRef("d", tags), 0, 1<<42)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(points) != 1 || points[0].TimeMs != 1790000010*1000 {
		t.Fatalf("sketch store holds %+v, want only the bucket the TSDB accepted", points)
	}
}

// Without a sketch store the endpoint exists and says why it cannot serve —
// 503, not 404: the route is real, this deployment is not configured for it.
func TestSketches_NoSketchStore(t *testing.T) {
	e := newEnv(t, Options{})
	rec, out := e.postSketches(sketchPayload(t, "d", nil, 1790000000, 1))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d %v", rec.Code, out)
	}
}

func TestSketches_BadBodies(t *testing.T) {
	e, _ := newSketchEnv(t, Options{})
	for _, tc := range []struct {
		name string
		body string
		code int
		want string
	}{
		{"not json", `{`, http.StatusBadRequest, "not a sketch payload"},
		{"no sketches key", `{"series":[]}`, http.StatusBadRequest, `no "sketches"`},
		{"a bad series is a rejection", `{"sketches":[{"metric":"1bad","interval":10,"points":[]}]}`, http.StatusAccepted, "invalid metric name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, out := e.postSketches(tc.body)
			if rec.Code != tc.code {
				t.Fatalf("%d %v", rec.Code, out)
			}
			if !strings.Contains(fmt.Sprint(out), tc.want) {
				t.Errorf("%v does not mention %q", out, tc.want)
			}
		})
	}
}

// An empty bucket contributes its count and nothing else: the minimum of no
// observations is not zero, and a chart should not show one.
func TestSketches_AnEmptyBucketWritesNoMinOrMax(t *testing.T) {
	e, _ := newSketchEnv(t, Options{})
	body := sketchPayload(t, "d", nil, 1790000000)
	if rec, out := e.postSketches(body); rec.Code != http.StatusAccepted || out["accepted"] != 1.0 {
		t.Fatalf("%d %v", rec.Code, out)
	}
	if got := firstSample(t, e, "d.count"); got != 0 {
		t.Errorf("d.count = %v, want 0", got)
	}
	for _, metric := range []string{"d.min", "d.max"} {
		set, err := e.store.Select(context.Background(), tsdb.Selector{Metric: metric}, 0, 1<<62)
		if err != nil {
			t.Fatal(err)
		}
		if set.Next() {
			t.Errorf("%s exists for a bucket that observed nothing", metric)
		}
		_ = set.Close()
	}
}
