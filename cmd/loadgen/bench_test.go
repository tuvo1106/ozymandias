package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPercentiles_PicksTheRankedValueAndSurvivesEmptyInput(t *testing.T) {
	var d []time.Duration
	for i := 1; i <= 100; i++ {
		d = append(d, time.Duration(i)*time.Millisecond)
	}
	// Shuffle-proof: the function sorts, so feed it reversed.
	for i, j := 0, len(d)-1; i < j; i, j = i+1, j-1 {
		d[i], d[j] = d[j], d[i]
	}
	p50, p95, p99, max := percentiles(d)
	// Nearest rank: of 100 samples 1..100ms, half are at or below 50.
	if p50 != 50 || p95 != 95 || p99 != 99 || max != 100 {
		t.Fatalf("got p50=%v p95=%v p99=%v max=%v, want 50 95 99 100", p50, p95, p99, max)
	}
	if p50, _, _, max := percentiles([]time.Duration{7 * time.Millisecond}); p50 != 7 || max != 7 {
		t.Fatalf("one sample is every percentile: %v %v", p50, max)
	}
	if a, b, c, e := percentiles(nil); a+b+c+e != 0 {
		t.Fatalf("empty input: %v %v %v %v", a, b, c, e)
	}
}

// A fake ozyd: accepts what intake takes, remembers each series point, and answers a query with
// the point asked for, after `delay` (the queue).
type fakeIntake struct {
	mu     sync.Mutex
	points map[int64]float64
	delay  time.Duration
	seen   map[int64]time.Time
	posts  atomic.Int64
	items  atomic.Int64
	paths  sync.Map
	runs   sync.Map // the run: tag of each series seen
	hold   time.Duration
}

func (f *fakeIntake) handler() http.Handler {
	f.points, f.seen = map[int64]float64{}, map[int64]time.Time{}
	mux := http.NewServeMux()
	ack := func(w http.ResponseWriter, r *http.Request, items int) {
		f.posts.Add(1)
		f.items.Add(int64(items))
		f.paths.Store(r.URL.Path, true)
		fmt.Fprintf(w, `{"status":"ok","accepted":%d,"rejected":0}`, items)
	}
	mux.HandleFunc("/v1/series", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Series []struct {
				Tags   []string    `json:"tags"`
				Points [][]float64 `json:"points"`
			} `json:"series"`
		}
		if err := json.Unmarshal(b, &body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		time.Sleep(f.hold)
		f.mu.Lock()
		for _, s := range body.Series {
			for _, tag := range s.Tags {
				if strings.HasPrefix(tag, "run:") {
					f.runs.Store(tag, true)
				}
			}
			for _, p := range s.Points {
				f.points[int64(p[0])], f.seen[int64(p[0])] = p[1], time.Now()
			}
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		ack(w, r, len(body.Series))
	})
	mux.HandleFunc("/v1/logs", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Logs []json.RawMessage `json:"logs"`
		}
		if err := json.Unmarshal(b, &body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		ack(w, r, len(body.Logs))
	})
	mux.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		from, err := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		v, ok := f.points[from]
		at := f.seen[from]
		f.mu.Unlock()
		if !ok || time.Since(at) < f.delay {
			fmt.Fprint(w, `{"series":[]}`)
			return
		}
		fmt.Fprintf(w, `{"series":[{"points":[[%d,%v]]}]}`, from, v)
	})
	return mux
}

func TestIntakeLatency_MeasuresAckAndTheLaterMomentThePointIsQueryable(t *testing.T) {
	f := &fakeIntake{delay: 30 * time.Millisecond}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	var buf strings.Builder
	if err := intakeLatency(&buf, srv.URL, 5); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "acknowledged (202)") || !strings.Contains(out, "queryable") {
		t.Fatalf("report lacks a row:\n%s", out)
	}
	p50 := func(label string) float64 {
		for _, line := range strings.Split(out, "\n") {
			if !strings.Contains(line, label) {
				continue
			}
			var v, p95, p99, mx float64
			if _, err := fmt.Sscanf(strings.SplitN(line, "p50", 2)[1], "%f p95 %f p99 %f max %f", &v, &p95, &p99, &mx); err != nil {
				t.Fatalf("unreadable %q row %q: %v", label, line, err)
			}
			return v
		}
		t.Fatalf("no %q row:\n%s", label, out)
		return 0
	}
	ackP50, visP50 := p50("acknowledged (202)"), p50("queryable")
	if ackP50 <= 0 || ackP50 >= 30 {
		t.Fatalf("the ack is timed from the request to its 202, before the point is queryable: ack p50 %v\n%s", ackP50, out)
	}
	if visP50 < 30 || visP50 <= ackP50 {
		t.Fatalf("a point that becomes queryable 30ms after its ack must show it: ack p50 %v, queryable p50 %v\n%s", ackP50, visP50, out)
	}
}

func TestIntakeThroughput_CountsOnlyAcknowledgedItemsAndHonoursTheSignal(t *testing.T) {
	for _, signal := range []string{"series", "logs"} {
		f := &fakeIntake{}
		srv := httptest.NewServer(f.handler())
		var buf strings.Builder
		if err := intakeThroughput(&buf, srv.URL, signal, 3, 10, 150*time.Millisecond); err != nil {
			t.Fatalf("%s: %v", signal, err)
		}
		out := buf.String()
		srv.Close()
		if f.posts.Load() == 0 {
			t.Fatalf("%s: nothing was sent", signal)
		}
		if f.items.Load() != f.posts.Load()*10 {
			t.Errorf("%s: %d requests carried %d items, want 10 each", signal, f.posts.Load(), f.items.Load())
		}
		want := map[string]string{"series": "/v1/series", "logs": "/v1/logs"}[signal]
		if _, ok := f.paths.Load(want); !ok {
			t.Errorf("%s: never posted to %s", signal, want)
		}
		if !strings.Contains(out, "0 requests refused or failed") {
			t.Errorf("%s: report:\n%s", signal, out)
		}
		// The reported rate is what the server took over the seconds the run reports it took.
		var secs, reqs, rate float64
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "throughput,") {
				i := strings.LastIndex(l, ", ")
				if _, err := fmt.Sscanf(l[i+2:], "%fs", &secs); err != nil {
					t.Fatalf("%s: unreadable header %q: %v", signal, l, err)
				}
			}
			if strings.Contains(l, "requests/s,") {
				if _, err := fmt.Sscanf(strings.TrimSpace(l), "%f requests/s, %f", &reqs, &rate); err != nil {
					t.Fatalf("%s: unreadable report line %q: %v", signal, l, err)
				}
			}
		}
		if secs < 0.15 {
			t.Errorf("%s: the run reports %.1fs, shorter than the 0.15s it was asked for", signal, secs)
		}
		if want := float64(f.items.Load()) / secs; rate < want*0.9-1 || rate > want*1.1+1 {
			t.Errorf("%s: reported %.0f items/s, the server took %.0f over %.1fs", signal, rate, want, secs)
		}
	}
	if err := intakeThroughput(io.Discard, "http://127.0.0.1:1", "spans", 1, 1, time.Millisecond); err == nil {
		t.Fatal("an unknown signal was accepted")
	}
}

func TestIntakeThroughput_CountsRefusalsInsteadOfCallingThemThroughput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	var buf strings.Builder
	if err := intakeThroughput(&buf, srv.URL, "series", 2, 5, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "0 requests/s, 0 points/s") || strings.Contains(out, "(0 requests refused") {
		t.Fatalf("429s must be refusals, not acknowledged items:\n%s", out)
	}
	if !strings.Contains(out, "first failure: 429") {
		t.Fatalf("the report must say why requests failed:\n%s", out)
	}
}

func TestIntakeLatency_FailsAtOnceWhenAPointIsNotAcknowledged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	if err := intakeLatency(io.Discard, srv.URL, 3); err == nil || !strings.Contains(err.Error(), "not acknowledged") || !strings.Contains(err.Error(), "429") {
		t.Fatalf("a refused point must be an error, not a latency: %v", err)
	}
}

func TestCheckIntakeFlags_RefusesWhatWouldReportNothingAsAResult(t *testing.T) {
	for _, ok := range [][4]int{{8, 100, 200, int(time.Second)}, {1, 1, 1, 1}} {
		if err := checkIntakeFlags(ok[0], ok[1], ok[2], time.Duration(ok[3])); err != nil {
			t.Fatalf("%v is valid and was refused: %v", ok, err)
		}
	}
	for name, args := range map[string][4]int{"workers": {0, 1, 1, 1}, "batch": {1, 0, 1, 1}, "n": {1, 1, 0, 1}, "duration": {1, 1, 1, 0}} {
		if err := checkIntakeFlags(args[0], args[1], args[2], time.Duration(args[3])); err == nil {
			t.Errorf("a zero %s was accepted", name)
		}
	}
}

// A second invocation against the same ozyd must not send points older than the first's newest:
// each run is a series of its own.
func TestIntake_EachInvocationSendsASeriesOfItsOwn(t *testing.T) {
	f := &fakeIntake{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	for i := 0; i < 2; i++ {
		if err := intakeThroughput(io.Discard, srv.URL, "series", 1, 2, 30*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // the tag is the clock's nanoseconds
	}
	n := 0
	f.runs.Range(func(any, any) bool { n++; return true })
	if n != 2 {
		t.Fatalf("two throughput invocations used %d run tags, want 2", n)
	}
	for i := 0; i < 2; i++ {
		if err := intakeLatency(io.Discard, srv.URL, 2); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	n = 0
	f.runs.Range(func(any, any) bool { n++; return true })
	if n != 4 {
		t.Fatalf("two latency invocations did not each tag their series with a run of their own: %d tags in all, want 4", n)
	}
}

func TestIntakeThroughput_BacksOffFromAServerThatRefuses(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	if err := intakeThroughput(io.Discard, srv.URL, "series", 2, 5, 200*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	// Two workers at one request per 10ms for 200ms is about 40; unthrottled it is thousands.
	if n := hits.Load(); n > 100 {
		t.Fatalf("%d requests in 200ms to a server that refuses every one", n)
	}
}

func TestIntakeThroughput_RatesAreOverTheMeasuredTimeNotTheRequestedOne(t *testing.T) {
	f := &fakeIntake{hold: 120 * time.Millisecond}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	var buf strings.Builder
	if err := intakeThroughput(&buf, srv.URL, "series", 1, 3, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	// One worker, 120ms per request: the first request starts at 0, before the 100ms deadline, and
	// ends after it, so the run took 120ms. Dividing by the requested 100ms would report 1.2x the
	// rate the server gave.
	var secs float64
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(l, "throughput,") {
			if _, err := fmt.Sscanf(l[strings.LastIndex(l, ", ")+2:], "%fs", &secs); err != nil {
				t.Fatal(err)
			}
		}
	}
	if secs < 0.115 {
		t.Fatalf("the run reports %.2fs, but it took at least 0.12s (the in-flight request finishes after the 0.10s deadline)\n%s", secs, buf.String())
	}
}

func TestPostWhy_SaysWhyAndAgreesWithPost(t *testing.T) {
	reply := func(status int, body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			fmt.Fprint(w, body)
		}))
	}
	cases := []struct {
		name   string
		status int
		body   string
		want   string // "" = acknowledged
	}{
		{"acknowledged", 202, `{"accepted":3,"rejected":0}`, ""},
		{"shed", 429, `{"error":"queue full"}`, "queue full"},
		{"rejected items", 202, `{"accepted":2,"rejected":1,"errors":["bad"]}`, "rejected=1"},
		{"nothing accepted", 202, `{"accepted":0,"rejected":0}`, "accepted=0"},
		{"unreadable", 202, `not json`, "unreadable"},
	}
	for _, c := range cases {
		srv := reply(c.status, c.body)
		err := postWhy(srv.Client(), srv.URL, "{}")
		ok := post(srv.Client(), srv.URL, "{}")
		srv.Close()
		if c.want == "" && (err != nil || !ok) {
			t.Errorf("%s: err=%v ok=%v", c.name, err, ok)
		}
		if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want) || ok) {
			t.Errorf("%s: err=%v ok=%v, want an error containing %q", c.name, err, ok, c.want)
		}
	}
}
