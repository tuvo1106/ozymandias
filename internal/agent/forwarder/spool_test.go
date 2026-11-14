package forwarder

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// intake is a server that can be switched down (503) and up, and records every
// payload it accepted, in order, as "path name".
type intake struct {
	up   atomic.Bool
	mu   sync.Mutex
	seen []string
}

func (in *intake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	zr, err := gzip.NewReader(r.Body)
	var raw []byte
	if err == nil {
		raw, _ = io.ReadAll(zr)
	}
	if !in.up.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	in.mu.Lock()
	in.seen = append(in.seen, r.URL.Path+" "+firstName(string(raw)))
	in.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
	_, _ = io.WriteString(w, `{"status":"ok","accepted":1,"rejected":0,"errors":[]}`)
}

// firstName pulls the first "metric" or "name" value out of a body.
func firstName(s string) string {
	for _, k := range []string{`"metric":"`, `"name":"`} {
		if i := strings.Index(s, k); i >= 0 {
			rest := s[i+len(k):]
			return rest[:strings.Index(rest, `"`)]
		}
	}
	return "?"
}

func (in *intake) got() []string {
	in.mu.Lock()
	defer in.mu.Unlock()
	return append([]string(nil), in.seen...)
}

type spoolEnv struct {
	in  *intake
	url string
	clk *testutil.FakeClock
	reg *selfmetrics.Registry
}

func newSpoolEnv(t *testing.T) *spoolEnv {
	in := &intake{}
	ts := httptest.NewServer(in)
	t.Cleanup(ts.Close)
	return &spoolEnv{in: in, url: ts.URL, clk: testutil.NewFakeClock(time.Unix(1790000000, 0)), reg: selfmetrics.NewRegistry()}
}

func (e *spoolEnv) forwarder(t *testing.T, dir string, memBytes int) *Forwarder {
	f := New(Options{
		URL: e.url, Clock: e.clk, Registry: selfmetrics.NewRegistry(), Logger: slog.New(slog.DiscardHandler),
		Rand: rand.New(rand.NewPCG(1, 2)), BufferDir: dir, MaxQueueBytes: memBytes,
		MaxDiskBytes: 8 << 20, BackoffMin: time.Second, BackoffMax: 4 * time.Second,
	})
	e.reg = f.opts.Registry
	f.Start()
	return f
}

func one(name string) []wire.Series {
	return []wire.Series{{Metric: name, Type: wire.KindGauge, Tags: []string{"host:h"}, Points: []wire.Point{{Timestamp: 1790000000, Value: 1}}}}
}

// drain advances the fake clock past every backoff until want payloads have been accepted.
func (e *spoolEnv) drain(t *testing.T, want int) {
	t.Helper()
	testutil.Eventually(t, 10*time.Second, func() bool {
		e.clk.Advance(5 * time.Second)
		return len(e.in.got()) >= want
	}, "only %d of %d payloads delivered: %v", len(e.in.got()), want, e.in.got())
}

func (e *spoolEnv) counter(name string) int64 { return e.reg.Counter(name).Value() }

// gauge reads a gauge by name from a snapshot, so function gauges count too.
func (e *spoolEnv) gauge(name string) float64 {
	for _, p := range e.reg.Snapshot() {
		if p.Name == name {
			return p.Value
		}
	}
	return -1
}

// Memory first, then disk: an outage longer than the memory queue loses nothing
// while the disk has room, and what arrives is oldest first.
func TestForwarder_OverflowSpillsToDiskAndDeliversInOrder(t *testing.T) {
	e := newSpoolEnv(t)
	f := e.forwarder(t, t.TempDir(), 400)
	defer func() { _ = f.Shutdown(context.Background()) }()
	const n = 12
	for i := range n {
		f.Submit(one(fmt.Sprintf("s%02d", i)))
	}
	if d := e.counter("ozy.agent.forwarder.dropped"); d != 0 {
		t.Fatalf("dropped %d with a disk buffer that has room", d)
	}
	if sp := e.counter("ozy.agent.forwarder.spooled"); sp == 0 {
		t.Fatal("nothing spilled: the memory cap was never reached, so this tests nothing")
	}
	e.in.up.Store(true)
	e.drain(t, n)
	for i, got := range e.in.got() {
		if want := fmt.Sprintf("/v1/series s%02d", i); got != want {
			t.Fatalf("delivery %d = %q, want %q (oldest first)\nall: %v", i, got, want, e.in.got())
		}
	}
}

// What is queued when the agent stops is kept, not dropped, and sent by the next one.
func TestForwarder_ASpilledQueueSurvivesARestart(t *testing.T) {
	e := newSpoolEnv(t)
	dir := t.TempDir()
	f := e.forwarder(t, dir, 1<<20)
	for i := range 5 {
		f.Submit(one(fmt.Sprintf("r%d", i)))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := f.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown = %v: payloads kept on disk are not lost", err)
	}
	if d := e.counter("ozy.agent.forwarder.dropped"); d != 0 {
		t.Fatalf("dropped %d at shutdown with a disk buffer", d)
	}

	e.in.up.Store(true)
	f2 := e.forwarder(t, dir, 1<<20)
	defer func() { _ = f2.Shutdown(context.Background()) }()
	e.drain(t, 5)
	if n, _ := f2.Queued(); n != 0 {
		testutil.Eventually(t, 5*time.Second, func() bool { e.clk.Advance(5 * time.Second); n, _ := f2.Queued(); return n == 0 }, "still queued")
	}
	if got := e.in.got(); len(got) != 5 {
		t.Fatalf("%d payloads after the restart, want 5 exactly once: %v", len(got), got)
	}
}

// After an outage the cheapest, most valuable signal drains first.
func TestForwarder_DrainsSeriesThenSketchesThenSpans(t *testing.T) {
	e := newSpoolEnv(t)
	f := e.forwarder(t, t.TempDir(), 1) // every payload exceeds the memory cap and goes straight to disk
	defer func() { _ = f.Shutdown(context.Background()) }()
	for i := range 2 {
		f.SubmitSpans([]wire.Span{{TraceID: "t", SpanID: "s", Name: fmt.Sprintf("span%d", i), Service: "x"}})
		f.SubmitSketches([]wire.SketchSeries{{Metric: fmt.Sprintf("sk%d", i), Interval: 10}})
		f.Submit(one(fmt.Sprintf("se%d", i)))
	}
	e.in.up.Store(true)
	e.drain(t, 6)
	want := []string{"/v1/series se0", "/v1/series se1", "/v1/sketches sk0", "/v1/sketches sk1", "/v1/traces span0", "/v1/traces span1"}
	if fmt.Sprint(e.in.got()) != fmt.Sprint(want) {
		t.Fatalf("order\n got %v\nwant %v", e.in.got(), want)
	}
}

// A spool past its cap drops its oldest segment and says how much it lost.
func TestForwarder_ADiskBufferAtItsCapDropsAndCounts(t *testing.T) {
	e := newSpoolEnv(t)
	f := New(Options{
		URL: e.url, Clock: e.clk, Registry: e.reg, Logger: slog.New(slog.DiscardHandler), Rand: rand.New(rand.NewPCG(1, 2)),
		BufferDir: t.TempDir(), MaxQueueBytes: 1, MaxDiskBytes: 16 << 20, // the smallest cap the config allows
	})
	f.Start()
	defer func() { _ = f.Shutdown(context.Background()) }()
	// Random tags do not compress, so a few payloads are a real size.
	big := make([]wire.Series, 0, 1500)
	for i := range 1500 {
		tag := fmt.Sprintf("k:%016x%016x%016x%016x", rand.Uint64(), rand.Uint64(), rand.Uint64(), rand.Uint64())
		big = append(big, wire.Series{Metric: fmt.Sprintf("m.%06d", i), Type: wire.KindGauge,
			Tags: []string{tag, fmt.Sprintf("j:%016x%016x", rand.Uint64(), rand.Uint64())}, Points: []wire.Point{{Timestamp: 1790000000, Value: rand.Float64()}}})
	}
	for range 260 {
		f.Submit(big)
	}
	st := f.spools["series"].Stats()
	if st.Bytes > 16<<20+(2<<20) {
		t.Fatalf("spool holds %d bytes against a 16 MiB cap", st.Bytes)
	}
	if e.counter("ozy.agent.forwarder.dropped") == 0 {
		t.Fatal("the cap dropped data and nothing was counted")
	}
}

// A buffer that cannot be opened must not stop the agent, and must not look fine.
func TestForwarder_AnUnusableBufferDirIsMemoryOnlyAndSaysSo(t *testing.T) {
	e := newSpoolEnv(t)
	f := e.forwarder(t, "/dev/null/not-a-dir", 1<<20)
	defer func() { _ = f.Shutdown(context.Background()) }()
	e.in.up.Store(true)
	f.Submit(one("m"))
	e.drain(t, 1)
	if v := e.gauge("ozy.agent.forwarder.spool_enabled"); v != 0 {
		t.Fatalf("spool_enabled = %v with no buffer", v)
	}
	if e.counter("ozy.agent.forwarder.spool_errors") == 0 {
		t.Fatal("an unopenable buffer was not counted")
	}
}

func TestForwarder_SpoolMetricsExistWhenEnabled(t *testing.T) {
	e := newSpoolEnv(t)
	f := e.forwarder(t, t.TempDir(), 1<<20)
	defer func() { _ = f.Shutdown(context.Background()) }()
	if v := e.gauge("ozy.agent.forwarder.spool_enabled"); v != 1 {
		t.Fatalf("spool_enabled = %v, want 1", v)
	}
}

// A payload that is being sent when the memory cap needs room must not be moved
// to disk: it would be delivered and still be spooled, and go out again.
func TestForwarder_AnInFlightPayloadIsNotSpilled(t *testing.T) {
	e := newSpoolEnv(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		e.in.ServeHTTP(w, r)
	}))
	defer slow.Close()
	e.in.up.Store(true)
	f := New(Options{
		URL: slow.URL, Clock: e.clk, Registry: selfmetrics.NewRegistry(), Logger: slog.New(slog.DiscardHandler),
		Rand: rand.New(rand.NewPCG(1, 2)), BufferDir: t.TempDir(), MaxQueueBytes: 400, MaxDiskBytes: 8 << 20,
	})
	f.Start()
	defer func() { _ = f.Shutdown(context.Background()) }()

	f.Submit(one("first"))
	<-entered // its request is now in flight, holding no lock
	for i := range 8 {
		f.Submit(one(fmt.Sprintf("later%d", i))) // overflow: the in-flight one is the oldest
	}
	close(release)
	testutil.Eventually(t, 10*time.Second, func() bool {
		e.clk.Advance(5 * time.Second)
		return len(e.in.got()) >= 9
	}, "delivered %v", e.in.got())
	time.Sleep(50 * time.Millisecond) // a duplicate, if any, would arrive by now
	n := 0
	for _, g := range e.in.got() {
		if g == "/v1/series first" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the in-flight payload was delivered %d times: %v", n, e.in.got())
	}
}

// A spool that a failed fsync has stopped is as good as none, and says so.
func TestForwarder_AStoppedSpoolReadsAsDisabled(t *testing.T) {
	e := newSpoolEnv(t)
	f := e.forwarder(t, t.TempDir(), 1<<20)
	defer func() { _ = f.Shutdown(context.Background()) }()
	if e.gauge("ozy.agent.forwarder.spool_enabled") != 1 {
		t.Fatal("not enabled to begin with")
	}
	sp := f.spools["series"]
	// Stop it the way a failed fsync does: through the spool's own latch.
	if _, err := sp.Put(1, make([]byte, 16<<20)); err == nil {
		t.Fatal("an oversize Put succeeded")
	}
	sp.Break(errors.New("injected"))
	if e.gauge("ozy.agent.forwarder.spool_enabled") != 0 {
		t.Fatal("spool_enabled still 1 with a stopped spool")
	}
	f.Submit(one("after")) // must not panic, and must not hang
}

// At shutdown, with older payloads already on disk, a newer one in memory goes
// behind them on disk: sending it would put newer data on the wire first.
func TestForwarder_ShutdownKeepsNewerPayloadsBehindOlderSpooledOnes(t *testing.T) {
	e := newSpoolEnv(t)
	dir := t.TempDir()
	f := e.forwarder(t, dir, 1) // everything goes straight to disk
	f.Submit(one("old"))
	e.in.up.Store(true) // ozyd is back, but only for the final attempt
	f.mu.Lock()
	f.queue = append(f.queue, &payload{ep: "series", url: f.series.url, body: mustGzip(t, `{"series":[{"metric":"new"}]}`), series: 1})
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := f.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	for _, g := range e.in.got() {
		if strings.HasSuffix(g, " new") {
			t.Fatalf("a newer payload was sent from memory while an older one waited on disk: %v", e.in.got())
		}
	}
}

func mustGzip(t *testing.T, s string) []byte {
	t.Helper()
	b, err := gzipBytes([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
