package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent"
	agentconfig "github.com/tuvo1106/ozymandias/internal/agent/config"
	"github.com/tuvo1106/ozymandias/internal/httpserve"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/internal/sketch"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
)

// L8: M2's last acceptance criterion, end to end in one process.
//
// statsd `d` datagrams go into a real agent over UDP; a fake-clock advance
// makes it flush; the forwarder POSTs to /v1/sketches on a real ozyd, which
// writes the sketches to Pebble and their four exact aggregates to the TSDB;
// the query API answers `p95:… by {route}` from them. The answer has to be
// within 1% of the p95 computed from the raw values the test sent — which it
// still has, and the rest of the pipeline never did.
func TestEndToEnd_DistributionToPercentile(t *testing.T) {
	clk := testutil.NewFakeClock(time.Unix(1790000001, 0))

	srv, err := New(testConfig(t), Options{Logger: quiet, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer func() { _ = srv.Close() }()

	acfg := agentconfig.Default()
	acfg.Hostname = "box"
	acfg.Tags = []string{"service:checkout"}
	acfg.Intake.URL = ts.URL
	acfg.Statsd.Addr = "127.0.0.1:0"
	acfg.HTTP.ShutdownTimeout = time.Second
	areg := selfmetrics.NewRegistry()
	a, err := agent.New(acfg, agent.Options{Logger: quiet, Clock: clk, Registry: areg})
	if err != nil {
		t.Fatal(err)
	}
	ln, _ := httpserve.Listen("127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, ln) }()
	defer func() { cancel(); <-done }()

	conn, err := net.Dial("udp", a.StatsdAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Two routes with different shapes, so a p95 that ignored the grouping
	// or averaged the routes together would be visibly wrong. Values are
	// spread over three orders of magnitude, which is the case a fixed-width
	// histogram cannot serve and this one must.
	//
	// Sent in two flushes, and queried at an interval that spans both, so
	// the query has to *merge* two agent buckets rather than answer from one.
	// The first version of this test sent everything in one bucket and passed
	// with merging disabled entirely — green because it was too small to
	// reach the case, which is M2's own lesson.
	sent := map[string][]float64{}
	send := func(batch int) {
		t.Helper()
		for i := 1; i <= 150; i++ {
			fast := float64((batch*150+i)%100) + 1
			slow := float64((batch*150+i)%100)*10 + 500
			sent["/items"] = append(sent["/items"], fast)
			sent["/checkout"] = append(sent["/checkout"], slow)
			if _, err := fmt.Fprintf(conn, "http.request.duration:%g|d|#route:/items", fast); err != nil {
				t.Fatal(err)
			}
			if _, err := fmt.Fprintf(conn, "http.request.duration:%g|d|#route:/checkout", slow); err != nil {
				t.Fatal(err)
			}
		}
	}

	received := areg.Counter("ozy.agent.statsd.messages_received")
	testutil.Eventually(t, 2*time.Second, func() bool { return clk.Waiters() >= 1 }, "aggregator loop not started")

	send(0)
	testutil.Eventually(t, 5*time.Second, func() bool { return received.Value() == 300 },
		"agent received %d of 300", received.Value())
	clk.Advance(10 * time.Second) // bucket [1790000000, +10) closes

	send(1)
	testutil.Eventually(t, 5*time.Second, func() bool { return received.Value() == 600 },
		"agent received %d of 600", received.Value())
	clk.Advance(10 * time.Second) // bucket [1790000010, +10) closes

	query := func(url string) map[string]float64 {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
		var r struct {
			Series []struct {
				Tags   map[string]string `json:"tags"`
				Points [][2]*float64     `json:"points"`
			} `json:"series"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
			return nil
		}
		out := map[string]float64{}
		for _, s := range r.Series {
			for _, p := range s.Points {
				if p[1] != nil {
					out[s.Tags["route"]] = *p[1]
				}
			}
		}
		return out
	}

	// One output bucket spanning both agent buckets: the answer can only be
	// right if the two sketches were merged.
	const url = "/api/v1/query?metric=http.request.duration&agg=p95&by=route" +
		"&filter=service:checkout&from=1790000000&to=1790000019&interval=20"
	const countsURL = "/api/v1/query?metric=http.request.duration.count&agg=sum&by=route" +
		"&from=1790000000&to=1790000019&interval=20"

	// Wait on the sketches themselves, and on nothing else.
	//
	// Two weaker signals both look right and are not. A p95 that arrived
	// from only the first flush is a plausible number, so waiting for the
	// percentile to be non-null proves nothing. And `.count` — which this
	// test used to wait on — becomes visible *before* the sketch it was
	// derived from: the intake writes the scalars first on purpose, so that
	// the append-only store rules on which buckets exist (ADR-0015), and a
	// sketch is written only for the buckets it accepted. Between those two
	// writes a count is queryable and its sketch is not.
	//
	// That window is nanoseconds on a fast machine and wide enough to lose
	// on a loaded CI runner, where this failed with a p95 of 1408.37 — which
	// is exactly the right answer for the first flush alone. The store is
	// the only thing that can answer "is every observation I am about to
	// assert on actually here".
	sketchCount := func(route string) float64 {
		ref := tsdb.NewSeriesRef("http.request.duration",
			[]string{"host:box", "route:" + route, "service:checkout"})
		points, err := srv.sketches.Read(context.Background(), ref, 1790000000_000, 1790000019_999)
		if err != nil {
			t.Fatalf("reading sketches for %s: %v", route, err)
		}
		var total float64
		for _, p := range points {
			total += p.Sketch.Count()
		}
		return total
	}
	testutil.Eventually(t, 5*time.Second, func() bool {
		return sketchCount("/items") == 300 && sketchCount("/checkout") == 300
	}, "sketches never completed: /items %v, /checkout %v of 300 each",
		sketchCount("/items"), sketchCount("/checkout"))

	got := query(url)
	for route, values := range sent {
		want := exactQuantile(values, 0.95)
		if math.Abs(got[route]-want) > sketch.DefaultAlpha*want {
			t.Errorf("p95 of %s = %v, want %v within %v relative error",
				route, got[route], want, sketch.DefaultAlpha)
		}
	}

	// The exact aggregates took the ordinary path beside the sketches, so
	// they are queryable as series — and they are exact, not estimates.
	counts := query(countsURL)
	for route := range sent {
		if counts[route] != 300 {
			t.Errorf("%s count = %v, want 300 exactly", route, counts[route])
		}
	}
	maxes := query("/api/v1/query?metric=http.request.duration.max&agg=max&by=route" +
		"&from=1790000000&to=1790000019&interval=20")
	for route, values := range sent {
		want := slicesMax(values)
		if maxes[route] != want {
			t.Errorf("%s max = %v, want %v exactly", route, maxes[route], want)
		}
	}

	// A distribution does not answer avg, and says what to ask instead.
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET",
		"/api/v1/query?metric=http.request.duration&agg=avg&from=1790000000&to=1790000019", nil))
	if rec.Code != 400 {
		t.Errorf("avg of a distribution: %d, want 400", rec.Code)
	}
}

// exactQuantile is the oracle: the rank convention q*(n-1) over the sorted
// raw values, which is the one the sketch uses.
func exactQuantile(values []float64, q float64) float64 {
	s := append([]float64(nil), values...)
	sort.Float64s(s)
	return s[int(q*float64(len(s)-1))]
}

func slicesMax(values []float64) float64 {
	out := values[0]
	for _, v := range values {
		out = math.Max(out, v)
	}
	return out
}
