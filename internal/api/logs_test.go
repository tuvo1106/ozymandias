package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/loghub"
	"github.com/tuvo1106/ozymandias/internal/logstore"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

var logsNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

type logsEnv struct {
	h     http.Handler
	store *logstore.Store
	hub   *loghub.Hub
	clk   *testutil.FakeClock
}

func newLogsEnv(t *testing.T) *logsEnv {
	t.Helper()
	clk := testutil.NewFakeClock(logsNow)
	st, err := logstore.Open(logstore.Options{Dir: t.TempDir(), Clock: clk, NoSync: true, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e := &logsEnv{store: st, hub: loghub.New(2), clk: clk}
	mux := http.NewServeMux()
	(&Logs{Store: st, Hub: e.hub, Clock: clk, Logger: slog.New(slog.DiscardHandler), Heartbeat: time.Second}).Register(mux)
	e.h = mux
	return e
}

func (e *logsEnv) seed(t *testing.T, n int) {
	t.Helper()
	var batch []wire.Log
	for i := 0; i < n; i++ {
		status, svc := "info", "web-api"
		if i%4 == 0 {
			status = "error"
		}
		if i%3 == 0 {
			svc = "worker"
		}
		batch = append(batch, wire.Log{
			Ts: logsNow.Add(-time.Duration(n-i) * time.Second).UnixMilli(), Message: fmt.Sprintf("msg %03d", i),
			Status: status, Service: svc, Attrs: map[string]any{"route": fmt.Sprintf("/r%d", i%2), "ms": json.Number(fmt.Sprint(i))},
		})
	}
	if err := e.store.Append(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
}

func (e *logsEnv) get(path string, q url.Values) (*httptest.ResponseRecorder, map[string]any) {
	req := httptest.NewRequest(http.MethodGet, path+"?"+q.Encode(), nil)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestLogsList_PagesThroughEverythingInOrder(t *testing.T) {
	e := newLogsEnv(t)
	e.seed(t, 25)
	var msgs []string
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		q := url.Values{"limit": {"10"}, "cursor": {cursor}}
		rec, out := e.get("/api/v1/logs", q)
		if rec.Code != 200 {
			t.Fatal(rec.Code, rec.Body.String())
		}
		for _, l := range out["logs"].([]any) {
			msgs = append(msgs, l.(map[string]any)["message"].(string))
		}
		cursor, _ = out["cursor"].(string)
		if cursor == "" {
			break
		}
	}
	if len(msgs) != 25 || msgs[0] != "msg 024" || msgs[24] != "msg 000" {
		t.Fatalf("%d messages, first %q last %q", len(msgs), msgs[0], msgs[len(msgs)-1])
	}
	_, out := e.get("/api/v1/logs", url.Values{"order": {"asc"}, "limit": {"2"}})
	first := out["logs"].([]any)[0].(map[string]any)["message"]
	if first != "msg 000" {
		t.Fatalf("asc first = %v", first)
	}
}

func TestLogsList_FiltersAndShapes(t *testing.T) {
	e := newLogsEnv(t)
	e.seed(t, 12)
	_, out := e.get("/api/v1/logs", url.Values{"q": {"status:error service:worker"}})
	for _, l := range out["logs"].([]any) {
		m := l.(map[string]any)
		if m["status"] != "error" || m["service"] != "worker" {
			t.Fatalf("%v", m)
		}
	}
	// Nothing matching is an empty array, never null.
	rec, out := e.get("/api/v1/logs", url.Values{"q": {"nonexistentword"}})
	if rec.Code != 200 || out["logs"] == nil || len(out["logs"].([]any)) != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if _, ok := out["stats"].(map[string]any)["blocks_read"]; !ok {
		t.Fatal("stats missing")
	}
	if out["truncated"] != false {
		t.Fatal("truncated should be present and false")
	}
}

func TestLogsList_BadRequestsAre400(t *testing.T) {
	e := newLogsEnv(t)
	e.seed(t, 3)
	for name, q := range map[string]url.Values{
		"bad query":     {"q": {"service:"}},
		"bad cursor":    {"cursor": {"!!!"}},
		"bad order":     {"order": {"sideways"}},
		"bad limit":     {"limit": {"many"}},
		"bad from":      {"from": {"yesterday"}},
		"from after to": {"from": {"200"}, "to": {"100"}},
	} {
		rec, out := e.get("/api/v1/logs", q)
		if rec.Code != 400 || out["status"] != "error" {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

func TestLogsList_TheWindowDefaultsToTheLast15MinutesInMilliseconds(t *testing.T) {
	e := newLogsEnv(t)
	old := wire.Log{Ts: logsNow.Add(-time.Hour).UnixMilli(), Message: "old", Status: "info", Service: "a"}
	recent := wire.Log{Ts: logsNow.Add(-time.Minute).UnixMilli(), Message: "recent", Status: "info", Service: "a"}
	_ = e.store.Append(context.Background(), []wire.Log{old, recent})
	_, out := e.get("/api/v1/logs", nil)
	if l := out["logs"].([]any); len(l) != 1 || l[0].(map[string]any)["message"] != "recent" {
		t.Fatalf("%v", l)
	}
	_, out = e.get("/api/v1/logs", url.Values{"from": {fmt.Sprint(old.Ts)}, "to": {fmt.Sprint(old.Ts)}})
	if l := out["logs"].([]any); len(l) != 1 || l[0].(map[string]any)["message"] != "old" {
		t.Fatalf("an explicit millisecond window: %v", l)
	}
}

func TestLogsAggregate(t *testing.T) {
	e := newLogsEnv(t)
	e.seed(t, 20)
	rec, out := e.get("/api/v1/logs/aggregate", url.Values{"by": {"status"}, "interval": {"10000"}})
	if rec.Code != 200 || out["interval_ms"].(float64) != 10000 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var total float64
	for _, b := range out["buckets"].([]any) {
		for _, c := range b.(map[string]any)["counts"].(map[string]any) {
			total += c.(float64)
		}
	}
	if total != 20 {
		t.Fatalf("counted %v logs, want 20", total)
	}
	// No interval: one is chosen so the range draws about 60 bars.
	_, out = e.get("/api/v1/logs/aggregate", nil)
	// 15 minutes / 60 bars = 15s; the smallest nice interval with at most 60 bars is 30s.
	if got := out["interval_ms"].(float64); got != 30000 {
		t.Fatalf("auto interval for 15 minutes = %v ms, want 30000", got)
	}
	for name, q := range map[string]url.Values{
		"bad by":       {"by": {"nonsense"}},
		"bad interval": {"interval": {"x"}},
		"negative":     {"interval": {"-5"}},
		"too many":     {"interval": {"1"}, "from": {"0"}, "to": {"100000000"}},
	} {
		if rec, _ := e.get("/api/v1/logs/aggregate", q); rec.Code != 400 {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
	if rec, _ := e.get("/api/v1/logs/aggregate", url.Values{"by": {"@route"}}); rec.Code != 200 {
		t.Errorf("by @attr: %d", rec.Code)
	}
}

func TestAutoInterval(t *testing.T) {
	for _, c := range []struct {
		span time.Duration
		want time.Duration
	}{
		{time.Minute, time.Second}, {time.Hour, time.Minute}, {15 * time.Minute, 30 * time.Second},
		{24 * time.Hour, 30 * time.Minute}, {30 * 24 * time.Hour, 12 * time.Hour}, {365 * 24 * time.Hour, 24 * time.Hour}, {0, time.Second},
		{60 * time.Second, time.Second}, {61 * time.Second, 5 * time.Second},
	} {
		if got := autoInterval(0, c.span.Milliseconds()); got != c.want {
			t.Errorf("autoInterval(%v) = %v, want %v", c.span, got, c.want)
		}
	}
}

func TestLogsFacets(t *testing.T) {
	e := newLogsEnv(t)
	e.seed(t, 12)
	rec, out := e.get("/api/v1/logs/facets", url.Values{"keys": {"status,@route"}, "limit": {"1"}})
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	f := out["facets"].(map[string]any)
	st := f["status"].([]any)
	if len(st) != 1 || st[0].(map[string]any)["value"] != "info" || st[0].(map[string]any)["count"].(float64) != 9 {
		t.Fatalf("%v", st)
	}
	if _, ok := f["@route"]; !ok || out["capped"] == nil {
		t.Fatalf("%v", out)
	}
	for name, q := range map[string]url.Values{
		"no keys":    {},
		"bad key":    {"keys": {"nope"}},
		"too many":   {"keys": {strings.Repeat("status,", 11)}},
		"bad limit":  {"keys": {"status"}, "limit": {"0"}},
		"huge limit": {"keys": {"status"}, "limit": {"1000"}},
	} {
		if rec, _ := e.get("/api/v1/logs/facets", q); rec.Code != 400 {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
}

// openTail starts a real server (a recorder cannot stream) and returns a
// reader of the event stream.
func openTail(t *testing.T, e *logsEnv, q string) (*bufio.Reader, func()) {
	t.Helper()
	srv := httptest.NewServer(e.h)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/logs/tail?q="+url.QueryEscape(q), nil)
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by the returned stop func
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%d %s %s", resp.StatusCode, resp.Header.Get("Content-Type"), b)
	}
	return bufio.NewReader(resp.Body), func() { _ = resp.Body.Close(); srv.Close() }
}

func readEvent(t *testing.T, br *bufio.Reader) (event, data string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case line == "":
				if event != "" || data != "" {
					return
				}
			case strings.HasPrefix(line, "event: "):
				event = line[7:]
			case strings.HasPrefix(line, "data: "):
				data = line[6:]
			case strings.HasPrefix(line, ":"):
				event, data = "comment", line
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an event")
	}
	return event, data
}

func TestLogsTail_StreamsMatchingLogsAndHeartbeats(t *testing.T) {
	e := newLogsEnv(t)
	br, stop := openTail(t, e, "status:error")
	defer stop()
	if ev, _ := readEvent(t, br); ev != "comment" {
		t.Fatalf("first event %q, want the connected comment", ev)
	}
	waitSubs(t, e.hub, 1)
	e.hub.Publish([]wire.Log{
		{Ts: 1, Message: "fine", Status: "info", Service: "a"},
		{Ts: 2, Message: "boom", Status: "error", Service: "a"},
	})
	ev, data := readEvent(t, br)
	var got wire.Log
	if err := json.Unmarshal([]byte(data), &got); err != nil || ev != "log" || got.Message != "boom" {
		t.Fatalf("%q %q %v", ev, data, err)
	}
	// An idle tail heartbeats.
	waitFor(t, func() bool { return e.clk.Waiters() > 0 })
	e.clk.Advance(time.Second)
	if ev, data := readEvent(t, br); ev != "comment" || !strings.Contains(data, "keep-alive") {
		t.Fatalf("%q %q", ev, data)
	}
}

// gateWriter is a ResponseWriter whose writes of log events block until released,
// standing in for a client that has stopped reading.
type gateWriter struct {
	mu    sync.Mutex
	buf   strings.Builder
	hdr   http.Header
	gate  chan struct{}
	armed bool
}

func (g *gateWriter) Header() http.Header { return g.hdr }
func (g *gateWriter) WriteHeader(int)     {}
func (g *gateWriter) Flush()              {}
func (g *gateWriter) Write(p []byte) (int, error) {
	g.mu.Lock()
	armed := g.armed
	g.mu.Unlock()
	if armed && strings.HasPrefix(string(p), "event: log") {
		<-g.gate
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.buf.Write(p)
	return len(p), nil
}
func (g *gateWriter) text() string { g.mu.Lock(); defer g.mu.Unlock(); return g.buf.String() }

func TestLogsTail_ASlowReaderGetsADroppedNotice(t *testing.T) {
	e := newLogsEnv(t)
	gw := &gateWriter{hdr: http.Header{}, gate: make(chan struct{}), armed: true}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&Logs{Hub: e.hub, Clock: e.clk, Logger: slog.Default(), Heartbeat: time.Second}).
			tail(gw, httptest.NewRequest(http.MethodGet, "/api/v1/logs/tail", nil).WithContext(ctx))
	}()
	waitSubs(t, e.hub, 1)
	one := wire.Log{Ts: 1, Message: "x", Status: "info", Service: "a"}
	e.hub.Publish([]wire.Log{one}) // the handler takes it and blocks writing it
	waitFor(t, func() bool { return e.hub.Stats().Delivered == 1 })
	time.Sleep(20 * time.Millisecond)
	batch := make([]wire.Log, loghub.DefaultBuffer+100)
	for i := range batch {
		batch[i] = one
	}
	e.hub.Publish(batch)
	if d := e.hub.Stats().Dropped; d < 100 {
		t.Fatalf("dropped %d, want at least 100", d)
	}
	close(gw.gate)
	// Drain the buffered logs, then the next tick reports the loss.
	waitFor(t, func() bool { return e.clk.Waiters() > 0 })
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(gw.text(), "event: dropped") {
		if time.Now().After(deadline) {
			t.Fatal("no dropped notice:", gw.text()[max(0, len(gw.text())-200):])
		}
		e.clk.Advance(time.Second)
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(gw.text(), `{"dropped":1`) && !strings.Contains(gw.text(), `{"dropped":`) {
		t.Fatal("notice has no count")
	}
	cancel()
	<-done
}

func TestLogsTail_RefusalsAndCleanup(t *testing.T) {
	e := newLogsEnv(t)
	if rec, _ := e.get("/api/v1/logs/tail", url.Values{"q": {"service:"}}); rec.Code != 400 {
		t.Fatalf("bad query: %d", rec.Code)
	}
	// The hub allows two tails: a third is refused.
	_, stop1 := openTail(t, e, "")
	_, stop2 := openTail(t, e, "")
	waitSubs(t, e.hub, 2)
	if rec, _ := e.get("/api/v1/logs/tail", nil); rec.Code != 503 {
		t.Fatalf("over the limit: %d", rec.Code)
	}
	stop1()
	stop2()
	waitSubs(t, e.hub, 0) // a closed connection frees its subscription
	none := httptest.NewRecorder()
	(&Logs{Clock: e.clk, Logger: slog.Default()}).tail(none, httptest.NewRequest(http.MethodGet, "/api/v1/logs/tail", nil))
	if none.Code != 503 {
		t.Fatalf("no hub: %d", none.Code)
	}
}

func waitSubs(t *testing.T, h *loghub.Hub, n int) {
	t.Helper()
	waitFor(t, func() bool { return h.Stats().Subscribers == n })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}
