package httpserve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// The contract that matters for an ingest pipeline: a request already in
// flight when shutdown starts still completes, while new connections are
// refused immediately.
func TestServe_GracefulShutdownFinishesInFlightAndRefusesNew(t *testing.T) {
	testutil.CheckGoroutines(t)
	ln := listen(t)
	addr := "http://" + ln.Addr().String()
	started, release := make(chan struct{}), make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "finished")
	})}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, srv, ln, 5*time.Second) }()

	client := &http.Client{}
	defer client.CloseIdleConnections()
	type result struct {
		body string
		err  error
	}
	inflight := make(chan result, 1)
	go func() {
		resp, err := client.Get(addr + "/slow")
		if err != nil {
			inflight <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		inflight <- result{string(b), err}
	}()
	<-started
	cancel()

	// New connections are refused while the old request is still running.
	testutil.Eventually(t, 2*time.Second, func() bool {
		c, err := net.DialTimeout("tcp", ln.Addr().String(), 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
		}
		return err != nil
	}, "listener still accepting after shutdown began")

	close(release)
	if r := <-inflight; r.err != nil || r.body != "finished" {
		t.Fatalf("in-flight request = %q, %v; want it to complete", r.body, r.err)
	}
	if err := <-served; err != nil {
		t.Fatalf("Serve = %v, want nil", err)
	}
}

func TestServe_GraceExpiryIsReported(t *testing.T) {
	testutil.CheckGoroutines(t)
	ln := listen(t)
	started := make(chan struct{})
	stuck := make(chan struct{})
	defer close(stuck)
	srv := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-stuck
	})}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, srv, ln, 50*time.Millisecond) }()

	client := &http.Client{}
	defer client.CloseIdleConnections()
	go func() {
		if resp, err := client.Get("http://" + ln.Addr().String()); err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	cancel()
	err := <-served
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("Serve = %v, want grace-expiry error", err)
	}
}

func TestServe_ReturnsListenerFailure(t *testing.T) {
	ln := listen(t)
	_ = ln.Close() // Serve will fail immediately on a closed listener
	err := Serve(context.Background(), &http.Server{}, ln, time.Second)
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve = %v, want the listener error", err)
	}
}

func TestListen_WrapsBindFailure(t *testing.T) {
	ln := listen(t)
	defer ln.Close()
	_, err := Listen(ln.Addr().String())
	if !errors.Is(err, ErrNotListening) {
		t.Fatalf("err = %v, want ErrNotListening", err)
	}
}

func TestInstrument_CountsByRoutePatternAndStatusClass(t *testing.T) {
	reg := selfmetrics.NewRegistry()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		w.WriteHeader(http.StatusOK) // superfluous: the first status wins
	})
	mux.HandleFunc("GET /ok", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "x") })
	h := Instrument(reg, "test", mux)

	for _, path := range []string{"/items/1", "/items/2", "/ok", "/nowhere"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}
	want := map[string]int64{
		"route:GET /items/{id}|status_class:4xx": 2, // two ids, one series
		"route:GET /ok|status_class:2xx":         1,
		"route:unmatched|status_class:4xx":       1,
	}
	for k, n := range want {
		route, class, _ := strings.Cut(k, "|")
		if got := reg.Counter("ozy.http.requests", "component:test", route, class).Value(); got != n {
			t.Errorf("%s = %d, want %d", k, got, n)
		}
	}
}

func TestStatusWriter_UnwrapReachesUnderlyingWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec}
	if sw.Unwrap() != rec {
		t.Fatal("Unwrap did not return the wrapped writer")
	}
}

func TestLoopbackURL(t *testing.T) {
	cases := map[string]string{
		":9400":          "http://127.0.0.1:9400/healthz",
		"0.0.0.0:9400":   "http://127.0.0.1:9400/healthz",
		"[::]:9400":      "http://127.0.0.1:9400/healthz",
		"10.0.0.5:8126":  "http://10.0.0.5:8126/healthz",
		"[::1]:8126":     "http://[::1]:8126/healthz",
		"localhost:1234": "http://localhost:1234/healthz",
	}
	for addr, want := range cases {
		if got, err := LoopbackURL(addr, "/healthz"); err != nil || got != want {
			t.Errorf("LoopbackURL(%q) = %q, %v; want %q", addr, got, err, want)
		}
	}
	if _, err := LoopbackURL("no-port", "/"); err == nil {
		t.Error("want error for address without port")
	}
}

func TestProbe(t *testing.T) {
	testutil.CheckGoroutines(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			http.Error(w, "database is on fire", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	ctx := context.Background()

	if err := Probe(ctx, srv.URL+"/good", time.Second); err != nil {
		t.Errorf("healthy probe: %v", err)
	}
	if err := Probe(ctx, srv.URL+"/bad", time.Second); err == nil || !strings.Contains(err.Error(), "on fire") {
		t.Errorf("unhealthy probe = %v, want error carrying the body", err)
	}
	if err := Probe(ctx, "http://127.0.0.1:1/", time.Second); err == nil {
		t.Error("probe of closed port: want error")
	}
	if err := Probe(ctx, "::not a url", time.Second); err == nil {
		t.Error("probe of invalid URL: want error")
	}
}

func TestHealth_ReportsComponentVersionUptimeAndExtras(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { return start.Add(90*time.Second + 400*time.Millisecond) }
	h := Health("thing", start, now, func() map[string]any { return map[string]any{"hostname": "box"} })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || body["status"] != "ok" || body["component"] != "thing" ||
		body["version"] != "dev" || body["uptime_seconds"] != 90.0 || body["hostname"] != "box" {
		t.Fatalf("code=%d body=%v", rec.Code, body)
	}
	rec = httptest.NewRecorder()
	Health("bare", start, now, nil).ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
}
