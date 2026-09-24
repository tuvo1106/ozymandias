package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/config"
	"github.com/tuvo1106/ozymandias/internal/httpserve"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func testConfig(t *testing.T) config.Ozyd {
	cfg := config.DefaultOzyd()
	cfg.DataDir = filepath.Join(t.TempDir(), "nested", "data")
	cfg.HTTP.ShutdownTimeout = 2 * time.Second
	return cfg
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

func TestNew_CreatesDataDir(t *testing.T) {
	cfg := testConfig(t)
	if _, err := New(cfg, Options{Logger: quiet}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(cfg.DataDir); err != nil || !fi.IsDir() {
		t.Fatalf("data dir not created: %v", err)
	}
}

func TestNew_FailsWhenDataDirIsUnusable(t *testing.T) {
	cfg := testConfig(t)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = filepath.Join(blocker, "data")
	if _, err := New(cfg, Options{}); err == nil || !strings.Contains(err.Error(), "data_dir") {
		t.Fatalf("err = %v, want data_dir error", err)
	}
}

func TestHandler_HealthzAndDebugVars(t *testing.T) {
	fc := testutil.NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	reg := selfmetrics.NewRegistry()
	s, err := New(testConfig(t), Options{Logger: quiet, Registry: reg, Clock: fc})
	if err != nil {
		t.Fatal(err)
	}
	fc.Advance(42 * time.Second)

	rec := get(t, s.Handler(), "/healthz")
	var health map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil || rec.Code != 200 {
		t.Fatalf("healthz %d %q: %v", rec.Code, rec.Body, err)
	}
	if health["component"] != "ozyd" || health["uptime_seconds"] != 42.0 {
		t.Fatalf("healthz = %v", health)
	}

	rec = get(t, s.Handler(), "/debug/vars")
	for _, want := range []string{`"ozy.build.info"`, `"ozy.process.uptime_seconds"`, `"route:GET /healthz"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("/debug/vars missing %s:\n%s", want, rec.Body)
		}
	}
}

func TestHandler_ServesUIAtRootWhenProvided(t *testing.T) {
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ui") })
	s, err := New(testConfig(t), Options{Logger: quiet, UI: ui})
	if err != nil {
		t.Fatal(err)
	}
	if rec := get(t, s.Handler(), "/dashboards/abc"); rec.Body.String() != "ui" {
		t.Fatalf("GET /dashboards/abc = %q, want the UI", rec.Body)
	}
	// Specific routes still win over the UI catch-all.
	if rec := get(t, s.Handler(), "/healthz"); !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("healthz shadowed by UI: %q", rec.Body)
	}
}

func TestHandler_NoUIMeansNotFound(t *testing.T) {
	s, err := New(testConfig(t), Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	if rec := get(t, s.Handler(), "/"); rec.Code != http.StatusNotFound {
		t.Fatalf("GET / = %d, want 404", rec.Code)
	}
}

// End to end over a real socket: start, answer, stop cleanly, leak nothing.
func TestRun_ServesUntilCancelledThenStopsCleanly(t *testing.T) {
	testutil.CheckGoroutines(t)
	s, err := New(testConfig(t), Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := httpserve.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, ln) }()

	url := "http://" + ln.Addr().String() + "/healthz"
	if err := httpserve.Probe(context.Background(), url, 2*time.Second); err != nil {
		t.Fatalf("probe: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
}

func TestRun_ListensOnConfiguredAddress(t *testing.T) {
	cfg := testConfig(t)
	cfg.HTTP.Addr = "127.0.0.1:0"
	s, err := New(cfg, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // stop as soon as it starts: this only tests that it can bind
	if err := s.Run(ctx, nil); err != nil {
		t.Fatalf("Run = %v", err)
	}
}

func TestRun_ReportsBindFailure(t *testing.T) {
	taken, err := httpserve.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	cfg := testConfig(t)
	cfg.HTTP.Addr = taken.Addr().String()
	s, err := New(cfg, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Run(context.Background(), nil); err == nil {
		t.Fatal("Run on a taken port: want error")
	}
}

func TestNew_DefaultsDependencies(t *testing.T) {
	s, err := New(testConfig(t), Options{})
	if err != nil || s.log == nil || s.reg == nil || s.clock == nil {
		t.Fatalf("defaults not applied: %+v, %v", s, err)
	}
}

func TestNew_BothStoreEnginesServeTheSameRoundTrip(t *testing.T) {
	// The config switch is only worth having if the two engines are
	// interchangeable from outside. This is that claim at the HTTP layer,
	// where the rest of ozymandias meets them.
	body := `{"series":[{"metric":"http.request.count","type":"count","points":[[1758372000,3]],` +
		`"tags":["env:prod"],"host":"h1","interval":10}]}`

	for _, engine := range []string{"tsdb", "naive"} {
		t.Run(engine, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Storage.MetricStore = engine
			s, err := New(cfg, Options{Logger: quiet})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()

			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/v1/series", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("intake returned %d: %s", rec.Code, rec.Body)
			}

			got := get(t, s.Handler(), "/api/v1/query?metric=http.request.count"+
				"&from=1758371000&to=1758373000&aggregator=sum")
			if got.Code != http.StatusOK {
				t.Fatalf("query returned %d: %s", got.Code, got.Body)
			}
			if !strings.Contains(got.Body.String(), "3") {
				t.Errorf("the point did not come back: %s", got.Body)
			}
			names := get(t, s.Handler(), "/api/v1/metrics")
			if !strings.Contains(names.Body.String(), "http.request.count") {
				t.Errorf("metric names: %s", names.Body)
			}
			tags := get(t, s.Handler(), "/api/v1/tags?metric=http.request.count")
			if !strings.Contains(tags.Body.String(), "env") {
				t.Errorf("tag keys: %s", tags.Body)
			}
		})
	}
}

func TestNew_RejectsAnUnknownStoreEngine(t *testing.T) {
	cfg := testConfig(t)
	cfg.Storage.MetricStore = "postgres"
	if err := cfg.Validate(); err == nil {
		t.Error("an unknown metric_store passed validation")
	}
}

func TestNew_TSDBPublishesItsOwnCounters(t *testing.T) {
	cfg := testConfig(t)
	s, err := New(cfg, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	vars := get(t, s.Handler(), "/debug/vars").Body.String()
	// The rejection counters especially: a drop nobody can see is the failure
	// mode this whole store is written to avoid.
	for _, want := range []string{
		"ozy.tsdb.ooo_rejected", "ozy.tsdb.series_limit_rejected",
		"ozy.tsdb.blocks", "ozy.tsdb.disk_bytes", "store:tsdb",
	} {
		if !strings.Contains(vars, want) {
			t.Errorf("%s is not published", want)
		}
	}
}
