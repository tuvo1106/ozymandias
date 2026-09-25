package agent

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/config"
	"github.com/tuvo1106/ozymandias/internal/httpserve"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// testConfig binds statsd to a free loopback port and points the forwarder
// at a port where nothing listens, so a test never collides with a running
// agent or sends its data to a real ozyd.
func testConfig() config.Agent {
	cfg := config.Default()
	cfg.HTTP.ShutdownTimeout = 2 * time.Second
	cfg.Statsd.Addr = "127.0.0.1:0"
	cfg.Intake.URL = "http://127.0.0.1:1"
	cfg.Forwarder.ShutdownTimeout = time.Second
	return cfg
}

// newAgent is New plus cleanup of the statsd socket.
func newAgent(t *testing.T, cfg config.Agent, opts Options) (*Agent, error) {
	t.Helper()
	a, err := New(cfg, opts)
	if a != nil {
		t.Cleanup(func() { _ = a.Close() })
	}
	return a, err
}

func TestNew_ExplicitHostnameWins(t *testing.T) {
	cfg := testConfig()
	cfg.Hostname = "configured"
	a, err := newAgent(t, cfg, Options{Logger: quiet, Hostname: func() (string, error) { return "os", nil }})
	if err != nil || a.Hostname() != "configured" {
		t.Fatalf("hostname = %q, %v", a.Hostname(), err)
	}
}

func TestNew_FallsBackToOSHostname(t *testing.T) {
	a, err := newAgent(t, testConfig(), Options{Logger: quiet, Hostname: func() (string, error) { return "os-host", nil }})
	if err != nil || a.Hostname() != "os-host" {
		t.Fatalf("hostname = %q, %v", a.Hostname(), err)
	}
}

// Without a host tag every metric would be ambiguous across machines; refuse
// to start rather than send untagged data.
func TestNew_FailsWhenNoHostnameCanBeFound(t *testing.T) {
	_, err := newAgent(t, testConfig(), Options{Hostname: func() (string, error) { return "", errors.New("no uts") }})
	if err == nil || !strings.Contains(err.Error(), "hostname") {
		t.Fatalf("err = %v", err)
	}
}

func TestHandler_HealthzIncludesHostnameAndIntake(t *testing.T) {
	cfg := testConfig()
	cfg.Hostname = "box"
	a, err := newAgent(t, cfg, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["component"] != "agent" || body["hostname"] != "box" || body["intake_url"] != cfg.Intake.URL {
		t.Fatalf("healthz = %v", body)
	}
	rec = httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/debug/vars", nil))
	if !strings.Contains(rec.Body.String(), `"component:agent"`) {
		t.Fatalf("/debug/vars = %s", rec.Body)
	}
}

func TestRun_ServesUntilCancelledThenStopsCleanly(t *testing.T) {
	testutil.CheckGoroutines(t)
	a, err := newAgent(t, testConfig(), Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := httpserve.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, ln) }()
	if err := httpserve.Probe(context.Background(), "http://"+ln.Addr().String()+"/healthz", 2*time.Second); err != nil {
		t.Fatalf("probe: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
}

func TestRun_BindsConfiguredAddressOrReportsFailure(t *testing.T) {
	cfg := testConfig()
	cfg.HTTP.Addr = "127.0.0.1:0"
	a, err := newAgent(t, cfg, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Run(ctx, nil); err != nil {
		t.Fatalf("Run = %v", err)
	}

	taken, err := httpserve.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	cfg.HTTP.Addr = taken.Addr().String()
	a, _ = newAgent(t, cfg, Options{Logger: quiet})
	if err := a.Run(context.Background(), nil); err == nil {
		t.Fatal("taken port: want error")
	}
}

func TestNew_DefaultsDependencies(t *testing.T) {
	a, err := newAgent(t, testConfig(), Options{})
	if err != nil || a.log == nil || a.reg == nil || a.clock == nil {
		t.Fatalf("defaults not applied: %v", err)
	}
}

func TestNew_StatsdPortConflictFailsStartup(t *testing.T) {
	a, err := newAgent(t, testConfig(), Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.Statsd.Addr = a.StatsdAddr().String()
	if _, err := newAgent(t, cfg, Options{Logger: quiet}); err == nil || !strings.Contains(err.Error(), "statsd listen") {
		t.Fatalf("err = %v", err)
	}
}

func TestNew_StatsdCanBeDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.Statsd.Enabled = false
	a, err := newAgent(t, cfg, Options{Logger: quiet})
	if err != nil || a.StatsdAddr() != nil || a.Close() != nil {
		t.Fatalf("disabled statsd: addr=%v err=%v", a.StatsdAddr(), err)
	}
}

// The agent's whole pipeline in one process: a statsd datagram in, a gzip'd
// /v1/series POST out, carrying the value, the host tag, the agent's tags
// and the agent's own self-metrics.
func TestRun_StatsdToIntake(t *testing.T) {
	testutil.CheckGoroutines(t)
	var mu sync.Mutex
	var got []wire.Series
	intake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var p wire.SeriesPayload
		_ = json.NewDecoder(zr).Decode(&p)
		mu.Lock()
		got = append(got, p.Series...)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer intake.Close()

	clk := testutil.NewFakeClock(time.Unix(1790000001, 0))
	cfg := testConfig()
	cfg.Hostname = "box"
	cfg.Tags = []string{"env:test"}
	cfg.Intake.URL = intake.URL
	a, err := newAgent(t, cfg, Options{Logger: quiet, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	ln, _ := httpserve.Listen("127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, ln) }()

	conn, err := net.Dial("udp", a.StatsdAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("page.views:3|c|#route:/x\nqueue.depth:7|g\nusers:u1|s\nusers:u2|s\nlat:5|ms"))
	received := a.reg.Counter("ozy.agent.statsd.messages_received")
	testutil.Eventually(t, 2*time.Second, func() bool { return received.Value() == 5 }, "statsd got %d", received.Value())
	testutil.Eventually(t, 2*time.Second, func() bool { return clk.Waiters() >= 1 }, "aggregator not running")
	clk.Advance(10 * time.Second)

	find := func(metric string) *wire.Series {
		mu.Lock()
		defer mu.Unlock()
		for i := range got {
			if got[i].Metric == metric {
				return &got[i]
			}
		}
		return nil
	}
	testutil.Eventually(t, 3*time.Second, func() bool { return find("page.views") != nil }, "nothing forwarded")
	pv := find("page.views")
	if pv.Type != wire.KindCount || pv.Points[0] != (wire.Point{Timestamp: 1790000000, Value: 3}) ||
		strings.Join(pv.Tags, ",") != "env:test,host:box,route:/x" {
		t.Fatalf("page.views = %+v", pv)
	}
	for metric, want := range map[string]float64{"queue.depth": 7, "users": 2, "lat.max": 5, "lat.count": 1} {
		if s := find(metric); s == nil || s.Points[0].Value != want {
			t.Errorf("%s = %+v, want %v", metric, s, want)
		}
	}
	if s := find("ozy.agent.statsd.messages_received"); s == nil || !slices.Contains(s.Tags, "host:box") {
		t.Errorf("self-metrics not forwarded with the host tag: %+v", s)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
