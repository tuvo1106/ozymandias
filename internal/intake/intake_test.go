package intake

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/naive"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

var now = time.Unix(1790000000, 0)

type env struct {
	h     http.Handler
	store *naive.Store
	meta  *meta.DB
	reg   *selfmetrics.Registry
}

func newEnv(t *testing.T, opts Options) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{reg: selfmetrics.NewRegistry()}
	if opts.Store == nil {
		s, err := naive.Open(filepath.Join(dir, "m.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		e.store, opts.Store = s, s
	}
	if opts.Registry == nil {
		m, err := meta.Open(filepath.Join(dir, "meta.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.Close() })
		e.meta, opts.Registry = m, m
	}
	opts.Clock = testutil.NewFakeClock(now)
	opts.Metrics = e.reg
	opts.Logger = slog.New(slog.DiscardHandler)
	mux := http.NewServeMux()
	New(opts).Register(mux)
	e.h = mux
	return e
}

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	_, _ = io.WriteString(zw, s)
	_ = zw.Close()
	return b.Bytes()
}

func (e *env) post(body []byte, gzipped bool) (*httptest.ResponseRecorder, map[string]any) {
	req := httptest.NewRequest(http.MethodPost, "/v1/series", bytes.NewReader(body))
	if gzipped {
		req.Header.Set("Content-Encoding", "gzip")
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

const good = `{"metric":"http.request.count","type":"count","interval":10,"tags":["route:/a","env:dev"],"points":[[1790000000,4],[1790000010,6]]}`

func payload(items ...string) string { return `{"series":[` + strings.Join(items, ",") + `]}` }

func TestSeries_StoresAcceptedSeries(t *testing.T) {
	e := newEnv(t, Options{})
	rec, out := e.post(gz(t, payload(good)), true)
	if rec.Code != http.StatusAccepted || out["accepted"] != 1.0 || out["rejected"] != 0.0 || out["status"] != "ok" {
		t.Fatalf("%d %v", rec.Code, out)
	}
	if errs, ok := out["errors"].([]any); !ok || len(errs) != 0 {
		t.Fatalf("errors = %#v, want an empty array", out["errors"])
	}
	set, err := e.store.Select(context.Background(), tsdb.Selector{Metric: "http.request.count"}, 0, 1<<62)
	if err != nil || !set.Next() {
		t.Fatalf("not stored: %v", err)
	}
	if got := set.Series().Key(); got != "http.request.count|env:dev,route:/a" {
		t.Fatalf("key = %s", got)
	}
	it := set.Iterator()
	it.Next()
	if it.At() != (tsdb.Sample{T: 1790000000000, V: 4}) {
		t.Fatalf("first sample %+v (want milliseconds)", it.At())
	}
	if m, ok := e.meta.Metric("http.request.count"); !ok || m.Type != wire.KindCount {
		t.Fatalf("type not recorded: %+v", m)
	}
	if e.reg.Counter("ozy.intake.points_accepted").Value() != 2 {
		t.Fatal("points not counted")
	}
}

func TestSeries_AcceptsUncompressedBodies(t *testing.T) {
	e := newEnv(t, Options{})
	if rec, out := e.post([]byte(payload(good)), false); rec.Code != http.StatusAccepted || out["accepted"] != 1.0 {
		t.Fatalf("%d %v", rec.Code, out)
	}
}

func TestSeries_PartialAcceptance(t *testing.T) {
	e := newEnv(t, Options{})
	bad := `{"metric":"1bad","type":"gauge","interval":0,"tags":[],"points":[[1790000000,1]]}`
	rec, out := e.post(gz(t, payload(good, bad)), true)
	if rec.Code != http.StatusAccepted || out["accepted"] != 1.0 || out["rejected"] != 1.0 {
		t.Fatalf("%d %v", rec.Code, out)
	}
	if errs := out["errors"].([]any); !strings.Contains(errs[0].(string), "series[1]") {
		t.Fatalf("errors = %v", errs)
	}
}

func TestSeries_TypeConflictIsRejectedPerSeries(t *testing.T) {
	e := newEnv(t, Options{})
	e.post(gz(t, payload(good)), true)
	asGauge := `{"metric":"http.request.count","type":"gauge","interval":0,"tags":[],"points":[[1790000000,1]]}`
	rec, out := e.post(gz(t, payload(asGauge)), true)
	if rec.Code != http.StatusAccepted || out["accepted"] != 0.0 || out["rejected"] != 1.0 {
		t.Fatalf("%d %v", rec.Code, out)
	}
	if !strings.Contains(fmt.Sprint(out["errors"]), "is a count, not a gauge") {
		t.Fatalf("errors = %v", out["errors"])
	}
}

func TestSeries_ErrorListIsCapped(t *testing.T) {
	e := newEnv(t, Options{})
	var items []string
	for range 25 {
		items = append(items, `{"metric":"-bad"}`)
	}
	_, out := e.post(gz(t, payload(items...)), true)
	if out["rejected"] != 25.0 || len(out["errors"].([]any)) != wire.MaxResponseErrors {
		t.Fatalf("rejected=%v errors=%d", out["rejected"], len(out["errors"].([]any)))
	}
}

func TestSeries_MalformedBodies(t *testing.T) {
	e := newEnv(t, Options{})
	for name, tc := range map[string]struct {
		body    []byte
		gzipped bool
	}{
		"not json":      {[]byte("nope"), false},
		"not gzip":      {[]byte("nope"), true},
		"truncated gz":  {gz(t, payload(good))[:20], true},
		"no series key": {gz(t, `{}`), true},
	} {
		rec, out := e.post(tc.body, tc.gzipped)
		if rec.Code != http.StatusBadRequest || out["status"] != "error" || out["error"] == "" {
			t.Errorf("%s: %d %v", name, rec.Code, out)
		}
	}
}

func TestSeries_SizeLimits(t *testing.T) {
	e := newEnv(t, Options{})
	// Over 4 MiB as sent.
	big := bytes.Repeat([]byte("x"), wire.MaxBodyBytes+1)
	if rec, _ := e.post(big, false); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("big plain body: %d", rec.Code)
	}
	// A zip bomb: small compressed, over 16 MiB once decompressed.
	bomb := gz(t, strings.Repeat(" ", wire.MaxDecompressedBytes+10))
	if len(bomb) > wire.MaxBodyBytes {
		t.Fatalf("bomb is %d bytes compressed; test is wrong", len(bomb))
	}
	if rec, _ := e.post(bomb, true); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("zip bomb: %d", rec.Code)
	}
	// Over 4 MiB of gzip: the limit trips while reading the gzip stream.
	noise := make([]byte, wire.MaxBodyBytes+1024)
	for i := range noise {
		noise[i] = byte(i*7919 + i/13)
	}
	if rec, _ := e.post(append(gz(t, "")[:10], noise...), true); rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusBadRequest {
		t.Errorf("huge gzip: %d", rec.Code)
	}
}

type failingStore struct{ tsdb.MetricStore }

func (failingStore) Append(context.Context, []tsdb.SeriesSamples) (tsdb.AppendResult, error) {
	return tsdb.AppendResult{}, errors.New("disk on fire")
}

type failingRegistry struct{}

func (failingRegistry) Observe(context.Context, string, wire.Kind, int64, time.Time) error {
	return errors.New("sqlite busy")
}

// Server-side failures are 503, which the agent's forwarder retries — the
// payload was fine, so it must not be dropped.
func TestSeries_ServerFailuresAreRetryable(t *testing.T) {
	e := newEnv(t, Options{Store: failingStore{}})
	if rec, _ := e.post(gz(t, payload(good)), true); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("store failure: %d", rec.Code)
	}
	e = newEnv(t, Options{Registry: failingRegistry{}})
	if rec, _ := e.post(gz(t, payload(good)), true); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("registry failure: %d", rec.Code)
	}
}

func TestSeries_StoreRejectionsAreReported(t *testing.T) {
	e := newEnv(t, Options{Store: rejectingStore{}})
	_, out := e.post(gz(t, payload(good)), true)
	if out["accepted"] != 0.0 || out["rejected"] != 1.0 || !strings.Contains(fmt.Sprint(out["errors"]), "no room") {
		t.Fatalf("%v", out)
	}
}

type rejectingStore struct{ tsdb.MetricStore }

func (rejectingStore) Append(_ context.Context, b []tsdb.SeriesSamples) (tsdb.AppendResult, error) {
	var res tsdb.AppendResult
	for _, s := range b {
		res.Rejected = append(res.Rejected, tsdb.Rejected{Series: s.Series, Reason: "no room"})
	}
	return res, nil
}

func TestSeries_OnlyPOST(t *testing.T) {
	e := newEnv(t, Options{})
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/series", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", rec.Code)
	}
}

func TestIngest_ValidatesAndStores(t *testing.T) {
	e := newEnv(t, Options{})
	in := New(Options{Store: e.store, Registry: e.meta, Clock: testutil.NewFakeClock(now), Metrics: selfmetrics.NewRegistry()})
	resp, err := in.Ingest(context.Background(), []wire.Series{
		{Metric: "ozy.x", Type: wire.KindGauge, Tags: []string{"host:h"}, Points: []wire.Point{{Timestamp: now.Unix(), Value: 1}}},
		{Metric: "bad-name", Type: wire.KindGauge, Points: []wire.Point{{Timestamp: now.Unix(), Value: 1}}},
	})
	if err != nil || resp.Accepted != 1 || resp.Rejected != 1 {
		t.Fatalf("%+v %v", resp, err)
	}
	if _, err := New(Options{Store: failingStore{}, Registry: e.meta, Clock: testutil.NewFakeClock(now)}).Ingest(context.Background(),
		[]wire.Series{{Metric: "ozy.x", Type: wire.KindGauge, Points: []wire.Point{{Timestamp: now.Unix(), Value: 1}}}}); err == nil {
		t.Fatal("store failure not reported")
	}
}
