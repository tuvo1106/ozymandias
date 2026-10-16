package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/config"
	"github.com/tuvo1106/ozymandias/internal/httpserve"
	"github.com/tuvo1106/ozymandias/internal/monitor/notify"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// hookReceiver collects webhook notifications.
type hookReceiver struct {
	mu    sync.Mutex
	notes []notify.Notification
}

func (h *hookReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var n notify.Notification
	if err := json.Unmarshal(b, &n); err == nil {
		h.mu.Lock()
		h.notes = append(h.notes, n)
		h.mu.Unlock()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *hookReceiver) statuses() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, n := range h.notes {
		out = append(out, n.Status)
	}
	return out
}

// TestEndToEnd_MonitorFiresAndRecovers is the in-process twin of smoke v6: a
// monitor provisioned from a file, a gauge pushed through intake, a fake clock
// driving the real scheduler and dispatcher, a webhook receiving ALERT and then
// exactly one recovery.
func TestEndToEnd_MonitorFiresAndRecovers(t *testing.T) {
	recv := &hookReceiver{}
	hook := httptest.NewServer(recv)
	defer hook.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "smoke.json"), []byte(`{
		"uid": "smoke-gauge", "name": "smoke gauge high", "type": "metric",
		"query": "max(last_1m):avg:smoke.gauge{*} > 5",
		"options": {"evaluation_interval_s": 10, "evaluation_delay_s": 0},
		"message": "smoke.gauge is {{.Value}} @hook"
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.Provisioning.MonitorPaths = []string{dir}
	cfg.Notifiers = map[string]config.Notifier{"hook": {Type: "webhook", URL: hook.URL}}
	clk := testutil.NewFakeClock(time.Unix(1790000000, 0))
	srv, err := New(cfg, Options{Logger: quiet, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	ln, _ := httpserve.Listen("127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, ln) }()
	defer func() { cancel(); <-done }()

	push := func(v float64) {
		t.Helper()
		resp, err := srv.intake.Ingest(context.Background(), []wire.Series{{Metric: "smoke.gauge", Type: wire.KindGauge,
			Points: []wire.Point{{Timestamp: clk.Now().Unix() - 5, Value: v}}}})
		if err != nil || resp.Rejected > 0 {
			t.Fatalf("ingest: %v %+v", err, resp)
		}
	}
	// Advance in ten-second steps, pushing v each step, until the receiver
	// has n notifications. Each step waits briefly for the evaluation and the
	// delivery it may have caused, both of which run on their own goroutines.
	drive := func(v float64, n int, what string) {
		t.Helper()
		for range 30 {
			push(v)
			clk.Advance(10 * time.Second)
			if poll(300*time.Millisecond, func() bool { return len(recv.statuses()) >= n }) {
				return
			}
		}
		t.Fatalf("%s: got %v", what, recv.statuses())
	}
	drive(10, 1, "ALERT delivered")
	drive(0, 2, "recovery delivered")
	// A few more evaluations at 0: no further messages.
	for range 6 {
		push(0)
		clk.Advance(10 * time.Second)
	}
	poll(200*time.Millisecond, func() bool { return len(recv.statuses()) > 2 })
	got := recv.statuses()
	if len(got) != 2 || got[0] != "ALERT" || got[1] != "OK" {
		t.Fatalf("notifications %v, want [ALERT OK]", got)
	}
	recv.mu.Lock()
	first := recv.notes[0]
	recv.mu.Unlock()
	if first.MonitorUID != "smoke-gauge" || first.Value == nil || *first.Value != 10 || first.Message != "smoke.gauge is 10 @hook" {
		t.Fatalf("payload %+v", first)
	}
	// The API agrees: the monitor is OK, and its event log has both
	// transitions and both deliveries.
	rec := get(t, srv.Handler(), "/api/v1/monitors")
	var list struct {
		Monitors []struct {
			ID     int64   `json:"id"`
			Status *string `json:"status"`
		} `json:"monitors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Monitors) != 1 || *list.Monitors[0].Status != "OK" {
		t.Fatalf("list %s", rec.Body)
	}
	testutil.Eventually(t, 2*time.Second, func() bool {
		var evs struct {
			Events []struct{ Kind string } `json:"events"`
		}
		_ = json.Unmarshal(get(t, srv.Handler(), "/api/v1/events?monitor_id=1").Body.Bytes(), &evs)
		kinds := map[string]int{}
		for _, e := range evs.Events {
			kinds[e.Kind]++
		}
		return kinds["transition"] == 2 && kinds["notify_ok"] == 2
	}, "events")
}

// poll reports whether cond became true within d.
func poll(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}
