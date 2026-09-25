package forwarder

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// scripted is an intake that answers from a script, one response per
// request, repeating the last one when the script runs out.
type scripted struct {
	mu       sync.Mutex
	script   []func(w http.ResponseWriter)
	requests []*http.Request
	bodies   []wire.SeriesPayload
}

func (s *scripted) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var p wire.SeriesPayload
	if zr, err := gzip.NewReader(r.Body); err == nil {
		_ = json.NewDecoder(zr).Decode(&p)
	}
	s.mu.Lock()
	s.requests = append(s.requests, r)
	s.bodies = append(s.bodies, p)
	step := s.script[min(len(s.requests)-1, len(s.script)-1)]
	s.mu.Unlock()
	step(w)
}

func (s *scripted) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func status(code int, headers ...string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.WriteHeader(code)
		if code == http.StatusAccepted {
			_, _ = io.WriteString(w, `{"status":"ok","accepted":1,"rejected":0,"errors":[]}`)
		}
	}
}

var ok = status(http.StatusAccepted)

func series(n int) []wire.Series {
	out := make([]wire.Series, n)
	for i := range out {
		out[i] = wire.Series{Metric: fmt.Sprintf("m.%d", i), Type: wire.KindGauge, Tags: []string{"host:h"},
			Points: []wire.Point{{Timestamp: 1790000000, Value: float64(i)}}}
	}
	return out
}

type harness struct {
	f     *Forwarder
	srv   *scripted
	clock *testutil.FakeClock
	reg   *selfmetrics.Registry
}

func start(t *testing.T, opts Options, script ...func(w http.ResponseWriter)) *harness {
	t.Helper()
	h := &harness{srv: &scripted{script: script}, clock: testutil.NewFakeClock(time.Unix(1790000000, 0)), reg: selfmetrics.NewRegistry()}
	ts := httptest.NewServer(h.srv)
	t.Cleanup(ts.Close)
	if opts.URL == "" {
		opts.URL = ts.URL
	}
	opts.Clock, opts.Registry = h.clock, h.reg
	opts.Logger = slog.New(slog.DiscardHandler)
	opts.Rand = rand.New(rand.NewPCG(1, 2))
	opts.Hostname, opts.Version = "host-a", "v9"
	h.f = New(opts)
	h.f.Start()
	t.Cleanup(func() { _ = h.f.Shutdown(context.Background()) })
	return h
}

func (h *harness) counter(name string) int64 { return h.reg.Counter(name).Value() }

func TestForwarder_SendsGzipWithIdentityHeaders(t *testing.T) {
	h := start(t, Options{}, ok)
	h.f.Submit(series(3))
	testutil.Eventually(t, 2*time.Second, func() bool { return h.counter("ozy.agent.forwarder.payloads_sent") == 1 }, "not sent")
	r := h.srv.requests[0]
	if r.Method != http.MethodPost || r.URL.Path != "/v1/series" || r.Header.Get("Content-Encoding") != "gzip" ||
		r.Header.Get(wire.HeaderHost) != "host-a" || r.Header.Get(wire.HeaderAgentVersion) != "v9" ||
		r.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("request %s %s headers %v", r.Method, r.URL.Path, r.Header)
	}
	if got := len(h.srv.bodies[0].Series); got != 3 {
		t.Fatalf("body has %d series, want 3", got)
	}
	if n, b := h.f.Queued(); n != 0 || b != 0 {
		t.Fatalf("queue not empty: %d payloads, %d bytes", n, b)
	}
	if h.counter("ozy.agent.forwarder.series_sent") != 1 {
		t.Fatal("accepted count from the response not recorded")
	}
}

// TestForwarder_SkipsUnencodableSeries pins the blast radius of one bad
// value: a non-finite point makes wire.Point refuse to marshal, and failing
// the whole batch on it would discard every healthy series in the flush —
// including the agent's own self-metrics — on every interval for as long as
// whatever produced it keeps producing it.
func TestForwarder_SkipsUnencodableSeries(t *testing.T) {
	h := start(t, Options{}, ok)
	batch := series(3)
	batch[1].Points[0].Value = math.Inf(1)
	h.f.Submit(batch)
	testutil.Eventually(t, 2*time.Second, func() bool { return h.counter("ozy.agent.forwarder.payloads_sent") == 1 }, "not sent")
	got := make([]string, 0, 2)
	for _, sr := range h.srv.bodies[0].Series {
		got = append(got, sr.Metric)
	}
	if strings.Join(got, ",") != "m.0,m.2" {
		t.Fatalf("delivered %v, want the two finite series", got)
	}
	if h.counter("ozy.agent.forwarder.dropped") != 1 {
		t.Fatalf("dropped = %d, want just the one bad series", h.counter("ozy.agent.forwarder.dropped"))
	}
}

func TestForwarder_SplitsBySeriesCountAndBytes(t *testing.T) {
	f := New(Options{MaxSeriesPerPayload: 4, Registry: selfmetrics.NewRegistry()})
	ps, err := encodeSeries(f, series(10))
	if err != nil || len(ps) != 3 || ps[0].series != 4 || ps[2].series != 2 {
		t.Fatalf("by count: %d payloads, err %v", len(ps), err)
	}
	f = New(Options{MaxPayloadBytes: 300, Registry: selfmetrics.NewRegistry()})
	ps, err = encodeSeries(f, series(10))
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, p := range ps {
		zr, _ := gzip.NewReader(strings.NewReader(string(p.body)))
		raw, _ := io.ReadAll(zr)
		if len(raw) > 300 {
			t.Errorf("payload of %d bytes exceeds 300", len(raw))
		}
		var decoded wire.SeriesPayload
		if err := json.Unmarshal(raw, &decoded); err != nil || len(decoded.Series) != p.series {
			t.Fatalf("payload does not decode: %v", err)
		}
		total += p.series
	}
	if total != 10 || len(ps) < 3 {
		t.Fatalf("by bytes: %d payloads carrying %d series", len(ps), total)
	}
	// A single series larger than the limit still goes, alone.
	f = New(Options{MaxPayloadBytes: 10, Registry: selfmetrics.NewRegistry()})
	if ps, _ := encodeSeries(f, series(2)); len(ps) != 2 {
		t.Fatalf("oversized series: %d payloads", len(ps))
	}
}

func TestForwarder_RetriesTransientFailures(t *testing.T) {
	for name, fail := range map[string]func(http.ResponseWriter){
		"500": status(500), "503": status(503), "429": status(429), "408": status(408),
	} {
		t.Run(name, func(t *testing.T) {
			h := start(t, Options{BackoffMin: time.Second, BackoffMax: 4 * time.Second}, fail, fail, ok)
			h.f.Submit(series(1))
			for want := 1; want <= 2; want++ {
				testutil.Eventually(t, 2*time.Second, func() bool { return h.srv.count() == want && h.clock.Waiters() == 1 },
					"attempt %d not made/rescheduled", want)
				h.clock.Advance(4 * time.Second) // past any jittered backoff
			}
			testutil.Eventually(t, 2*time.Second, func() bool { return h.counter("ozy.agent.forwarder.payloads_sent") == 1 }, "never delivered")
			if h.counter("ozy.agent.forwarder.retries") != 2 {
				t.Fatalf("retries = %d", h.counter("ozy.agent.forwarder.retries"))
			}
		})
	}
}

func TestForwarder_RetriesNetworkErrors(t *testing.T) {
	h := start(t, Options{URL: "http://127.0.0.1:1", Timeout: time.Second}) // nothing listens on port 1
	h.f.Submit(series(1))
	testutil.Eventually(t, 3*time.Second, func() bool { return h.counter("ozy.agent.forwarder.retries") >= 1 }, "no retry scheduled")
	if n, _ := h.f.Queued(); n != 1 {
		t.Fatalf("payload not kept for retry (%d queued)", n)
	}
}

func TestForwarder_GivesUpOnOtherClientErrors(t *testing.T) {
	for _, code := range []int{400, 401, 404, 413} {
		h := start(t, Options{}, status(code))
		h.f.Submit(series(2))
		testutil.Eventually(t, 2*time.Second, func() bool { return h.counter("ozy.agent.forwarder.payloads_rejected") == 1 }, "%d: not rejected", code)
		if h.counter("ozy.agent.forwarder.retries") != 0 || h.counter("ozy.agent.forwarder.dropped") != 2 {
			t.Fatalf("%d: retries=%d dropped=%d", code, h.counter("ozy.agent.forwarder.retries"), h.counter("ozy.agent.forwarder.dropped"))
		}
		if n, _ := h.f.Queued(); n != 0 {
			t.Fatalf("%d: payload still queued", code)
		}
	}
}

// With Retry-After: 30 and a backoff that would allow 1s, nothing is sent
// before 30s have passed.
func TestForwarder_HonorsRetryAfter(t *testing.T) {
	h := start(t, Options{BackoffMin: time.Second, BackoffMax: time.Second}, status(429, "Retry-After", "30"), ok)
	h.f.Submit(series(1))
	testutil.Eventually(t, 2*time.Second, func() bool { return h.srv.count() == 1 && h.clock.Waiters() == 1 }, "first attempt")
	h.clock.Advance(29 * time.Second)
	time.Sleep(20 * time.Millisecond)
	if h.srv.count() != 1 {
		t.Fatal("retried before Retry-After elapsed")
	}
	h.clock.Advance(time.Second)
	testutil.Eventually(t, 2*time.Second, func() bool { return h.srv.count() == 2 }, "not retried after Retry-After")
}

func TestForwarder_DropsOldestAtTheMemoryCap(t *testing.T) {
	f := New(Options{Registry: selfmetrics.NewRegistry(), MaxSeriesPerPayload: 1})
	one, _ := encodeSeries(f, series(1))
	size := len(one[0].body)
	reg := selfmetrics.NewRegistry()
	f = New(Options{Registry: reg, MaxSeriesPerPayload: 1, MaxQueueBytes: 3*size + size/2})
	// Not started: everything stays queued.
	f.Submit(series(5))
	n, b := f.Queued()
	if n != 3 || b > 3*size+size/2 {
		t.Fatalf("queued %d payloads / %d bytes, want 3 under the cap", n, b)
	}
	if got := reg.Counter("ozy.agent.forwarder.dropped").Value(); got != 2 {
		t.Fatalf("dropped = %d, want 2", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for _, p := range f.queue {
		zr, _ := gzip.NewReader(strings.NewReader(string(p.body)))
		var d wire.SeriesPayload
		_ = json.NewDecoder(zr).Decode(&d)
		names = append(names, d.Series[0].Metric)
	}
	if strings.Join(names, ",") != "m.2,m.3,m.4" {
		t.Fatalf("kept %v, want the newest three", names)
	}
}

func TestForwarder_PayloadLargerThanTheCapIsDropped(t *testing.T) {
	reg := selfmetrics.NewRegistry()
	f := New(Options{Registry: reg, MaxQueueBytes: 10})
	f.Submit(series(3))
	if n, _ := f.Queued(); n != 0 || reg.Counter("ozy.agent.forwarder.dropped").Value() != 3 {
		t.Fatal("oversized payload was queued")
	}
}

func TestForwarder_ShutdownSendsWhatIsQueued(t *testing.T) {
	h := start(t, Options{BackoffMin: time.Hour, BackoffMax: time.Hour}, status(503), ok)
	h.f.Submit(series(1))
	testutil.Eventually(t, 2*time.Second, func() bool { return h.srv.count() == 1 && h.clock.Waiters() == 1 }, "first attempt")
	h.f.Submit(series(2)) // behind the one backing off for an hour
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.f.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if h.counter("ozy.agent.forwarder.payloads_sent") < 2 || h.counter("ozy.agent.forwarder.dropped") != 0 {
		t.Fatalf("sent=%d dropped=%d", h.counter("ozy.agent.forwarder.payloads_sent"), h.counter("ozy.agent.forwarder.dropped"))
	}
}

func TestForwarder_ShutdownGivesUpWhenIntakeIsDown(t *testing.T) {
	h := start(t, Options{BackoffMin: time.Hour, BackoffMax: time.Hour}, status(503))
	h.f.Submit(series(4))
	testutil.Eventually(t, 2*time.Second, func() bool { return h.srv.count() == 1 }, "first attempt")
	err := h.f.Shutdown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "4 series not delivered") {
		t.Fatalf("err = %v", err)
	}
	if h.counter("ozy.agent.forwarder.dropped") != 4 {
		t.Fatalf("dropped = %d", h.counter("ozy.agent.forwarder.dropped"))
	}
}

// A hung intake must not hold shutdown past its budget: the in-flight
// request is cancelled, and the final attempt is bounded by ctx.
func TestForwarder_ShutdownIsBoundedByItsBudget(t *testing.T) {
	release := make(chan struct{})
	hang := func(w http.ResponseWriter) { <-release }
	h := start(t, Options{Timeout: time.Minute}, hang)
	defer close(release)
	h.f.Submit(series(1))
	testutil.Eventually(t, 2*time.Second, func() bool { return h.srv.count() == 1 }, "request not in flight")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	began := time.Now()
	if err := h.f.Shutdown(ctx); err == nil {
		t.Fatal("no error for an undelivered payload")
	}
	if d := time.Since(began); d > 2*time.Second {
		t.Fatalf("shutdown took %v", d)
	}
}

func TestForwarder_RecordsSeriesRejectedByTheIntake(t *testing.T) {
	partial := func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"status":"ok","accepted":1,"rejected":2,"errors":["x","y"]}`)
	}
	h := start(t, Options{}, partial)
	h.f.Submit(series(3))
	testutil.Eventually(t, 2*time.Second, func() bool { return h.counter("ozy.agent.forwarder.series_rejected") == 2 }, "not recorded")
}

func TestForwarder_EncodingFailureDropsTheBatch(t *testing.T) {
	reg := selfmetrics.NewRegistry()
	f := New(Options{Registry: reg, Logger: slog.New(slog.DiscardHandler)})
	bad := []wire.Series{{Metric: "x", Type: wire.KindGauge, Points: []wire.Point{{Timestamp: 1, Value: inf()}}}}
	f.Submit(bad)
	if reg.Counter("ozy.agent.forwarder.dropped").Value() != 1 {
		t.Fatal("not counted")
	}
}

func inf() float64 { var z float64; return 1 / z }

func TestBackoff_FullJitterWithinBounds(t *testing.T) {
	f := New(Options{BackoffMin: time.Second, BackoffMax: 60 * time.Second, Registry: selfmetrics.NewRegistry()})
	for attempt := 1; attempt <= 40; attempt++ {
		ceiling := min(time.Second<<min(attempt-1, 30), 60*time.Second)
		for range 50 {
			if d := f.backoff(attempt); d < 0 || d > ceiling {
				t.Fatalf("attempt %d: %v outside [0, %v]", attempt, d, ceiling)
			}
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	for in, want := range map[string]time.Duration{"30": 30 * time.Second, "0": 0, "": 0, "-1": 0, "Wed, 21 Oct 2015 07:28:00 GMT": 0} {
		if got := parseRetryAfter(in); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

// encodeSeries is the series half of the generic encoder, for the tests that
// were written before there was a second endpoint.
func encodeSeries(f *Forwarder, series []wire.Series) ([]*payload, error) {
	return encode(f, f.series, series, func(s *wire.Series) string { return s.Metric })
}
