package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/config"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

type stoppedStore struct{}

func (stoppedStore) Broken() error { return errors.New("wal: log is no longer writable: injected") }

func readyz(h http.Handler) (int, map[string]any) {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

func checkOf(body map[string]any, name string) string {
	c, _ := body["checks"].(map[string]any)
	s, _ := c[name].(string)
	return s
}

func TestReadyz_ReadyWhenEveryStoreIsOpenAndWritable(t *testing.T) {
	cfg := testConfig(t)
	cfg.Pipeline.Mode = config.PipelineDirect
	s, err := New(cfg, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	code, body := readyz(s.Handler())
	if code != 200 || body["status"] != "ready" {
		t.Fatalf("%d %v", code, body)
	}
	for _, name := range []string{"meta", "tsdb", "logstore", "draining"} {
		if checkOf(body, name) != "ok" {
			t.Errorf("check %q = %q, want ok (all: %v)", name, checkOf(body, name), body["checks"])
		}
	}
}

// The case /readyz exists for: a store that has stopped itself is not ready, and
// the process is still alive, so /healthz must stay green (a restart is the fix,
// and a liveness failure would be what asks for it, but that is the
// orchestrator's call on /readyz's evidence, not ours).
func TestReadyz_AStoppedStoreIsNotReadyButIsStillHealthy(t *testing.T) {
	cfg := testConfig(t)
	cfg.Pipeline.Mode = config.PipelineDirect
	s, err := New(cfg, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for i := range s.ready.stores {
		name := s.ready.stores[i].name
		orig := s.ready.stores[i].b
		s.ready.stores[i].b = stoppedStore{}
		code, body := readyz(s.Handler())
		if code != 503 || body["status"] != "not_ready" || !strings.Contains(checkOf(body, name), "no longer writable") {
			t.Errorf("%s stopped: %d %v", name, code, body)
		}
		for _, other := range s.ready.stores {
			if other.name != name && checkOf(body, other.name) != "ok" {
				t.Errorf("%s stopped but %s reads %q", name, other.name, checkOf(body, other.name))
			}
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
		if w.Code != 200 {
			t.Errorf("/healthz = %d with %s stopped; liveness must not fail for what a restart is the answer to", w.Code, name)
		}
		s.ready.stores[i].b = orig
	}
}

func TestReadyz_TheBrokenGaugesExistFromStartup(t *testing.T) {
	s, err := New(testConfig(t), Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	seen := map[string]float64{}
	for _, p := range s.reg.Snapshot() {
		if p.Name == "ozy.store.broken" {
			for _, tg := range p.Tags {
				seen[tg] = p.Value
			}
		}
	}
	for _, want := range []string{"store:tsdb", "store:logstore"} {
		if v, ok := seen[want]; !ok || v != 0 {
			t.Errorf("ozy.store.broken{%s} = %v (present %v), want 0 from startup", want, v, ok)
		}
	}
}

// In queued mode a server whose consumers are not running accepts requests into
// a queue nobody reads, so it is not ready until they are.
func TestReadyz_QueuedModeWaitsForItsConsumers(t *testing.T) {
	cfg := testConfig(t)
	cfg.Pipeline.Mode = config.PipelineQueued
	cfg.Pipeline.Queue.SyncInterval = -1
	s, err := New(cfg, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	if code, body := readyz(s.Handler()); code != 503 || checkOf(body, "consumers") == "ok" {
		t.Fatalf("before Run: %d %v", code, body)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, ln) }()
	testutil.Eventually(t, 5*time.Second, func() bool { code, _ := readyz(s.Handler()); return code == 200 }, "never became ready once running")
	cancel()
	<-done
}

// At SIGTERM /readyz goes 503 while the listener is still up, for DrainDelay, so
// a balancer that polls it takes the instance out before connections are refused.
func TestReadyz_GoesUnreadyBeforeTheListenerCloses(t *testing.T) {
	cfg := testConfig(t)
	cfg.Pipeline.Mode = config.PipelineDirect
	cfg.HTTP.DrainDelay = 600 * time.Millisecond
	s, err := New(cfg, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	url := "http://" + ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, ln) }()
	testutil.Eventually(t, 5*time.Second, func() bool {
		r, err := http.Get(url + "/readyz")
		if err != nil {
			return false
		}
		_ = r.Body.Close()
		return r.StatusCode == 200
	}, "never ready")
	cancel()
	testutil.Eventually(t, 2*time.Second, func() bool {
		r, err := http.Get(url + "/readyz")
		if err != nil {
			return false
		}
		_ = r.Body.Close()
		return r.StatusCode == 503
	}, "/readyz did not go 503 while the listener was still up")
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("never stopped")
	}
}
