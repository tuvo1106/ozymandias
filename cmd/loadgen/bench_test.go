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
	if p50 != 51 || p95 != 96 || p99 != 100 || max != 100 {
		t.Fatalf("got p50=%v p95=%v p99=%v max=%v, want 51 96 100 100", p50, p95, p99, max)
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
				Points [][]float64 `json:"points"`
			} `json:"series"`
		}
		if err := json.Unmarshal(b, &body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		for _, s := range body.Series {
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
		// The reported rate is what the server took in, per second of the run: 150ms here.
		var reqs, rate float64
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "requests/s,") {
				if _, err := fmt.Sscanf(strings.TrimSpace(l), "%f requests/s, %f", &reqs, &rate); err != nil {
					t.Fatalf("%s: unreadable report line %q: %v", signal, l, err)
				}
			}
		}
		if want := float64(f.items.Load()) / 0.15; rate < want-1 || rate > want+1 {
			t.Errorf("%s: reported %.0f items/s, the server took %.0f", signal, rate, want)
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
}

func TestIntakeLatency_FailsAtOnceWhenAPointIsNotAcknowledged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	if err := intakeLatency(io.Discard, srv.URL, 3); err == nil || !strings.Contains(err.Error(), "not acknowledged") {
		t.Fatalf("a refused point must be an error, not a latency: %v", err)
	}
}
