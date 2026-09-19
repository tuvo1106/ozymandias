package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/config"
	"github.com/tuvo1106/ozymandias/internal/httpserve"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func testConfig() config.Agent {
	cfg := config.Default()
	cfg.HTTP.ShutdownTimeout = 2 * time.Second
	return cfg
}

func TestNew_ExplicitHostnameWins(t *testing.T) {
	cfg := testConfig()
	cfg.Hostname = "configured"
	a, err := New(cfg, Options{Logger: quiet, Hostname: func() (string, error) { return "os", nil }})
	if err != nil || a.Hostname() != "configured" {
		t.Fatalf("hostname = %q, %v", a.Hostname(), err)
	}
}

func TestNew_FallsBackToOSHostname(t *testing.T) {
	a, err := New(testConfig(), Options{Logger: quiet, Hostname: func() (string, error) { return "os-host", nil }})
	if err != nil || a.Hostname() != "os-host" {
		t.Fatalf("hostname = %q, %v", a.Hostname(), err)
	}
}

// Without a host tag every metric would be ambiguous across machines; refuse
// to start rather than send untagged data.
func TestNew_FailsWhenNoHostnameCanBeFound(t *testing.T) {
	_, err := New(testConfig(), Options{Hostname: func() (string, error) { return "", errors.New("no uts") }})
	if err == nil || !strings.Contains(err.Error(), "hostname") {
		t.Fatalf("err = %v", err)
	}
}

func TestHandler_HealthzIncludesHostnameAndIntake(t *testing.T) {
	cfg := testConfig()
	cfg.Hostname = "box"
	a, err := New(cfg, Options{Logger: quiet})
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
	a, err := New(testConfig(), Options{Logger: quiet})
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
	a, err := New(cfg, Options{Logger: quiet})
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
	a, _ = New(cfg, Options{Logger: quiet})
	if err := a.Run(context.Background(), nil); err == nil {
		t.Fatal("taken port: want error")
	}
}

func TestNew_DefaultsDependencies(t *testing.T) {
	a, err := New(testConfig(), Options{})
	if err != nil || a.log == nil || a.reg == nil || a.clock == nil {
		t.Fatalf("defaults not applied: %v", err)
	}
}
