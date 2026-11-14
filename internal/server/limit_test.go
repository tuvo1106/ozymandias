package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/testutil"
)

// One open live tail must not use up the query slots: a tail holds its
// connection for as long as the page is open, so counting it would let a few
// tabs lock every other query out.
func TestQueryLimit_ALiveTailDoesNotTakeASlot(t *testing.T) {
	cfg := testConfig(t)
	cfg.Query.MaxConcurrent = 1
	cfg.Query.MaxQueue = 1
	cfg.Query.QueueWait = 200 * time.Millisecond
	s, err := New(cfg, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer func() { ts.Close(); _ = s.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/v1/logs/tail?q=service:x", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	testutil.Eventually(t, 2*time.Second, func() bool { return s.logHub.Stats().Subscribers == 1 }, "tail never subscribed")

	for range 2 { // a second query would 503 if the tail held the only slot
		r, err := http.Get(ts.URL + "/api/v1/metrics")
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("query beside an open tail: %d", r.StatusCode)
		}
	}
}

func TestIsQuery_SelectsReadsByRouteNotMethod(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{"GET", "/api/v1/metrics", true},
		{"GET", "/api/v1/logs", true},
		{"POST", "/api/v1/query", true},
		{"POST", "/api/v1/query/batch", true},
		{"POST", "/api/v1/query/sketch", true},
		{"POST", "/api/v1/monitors/backtest", true},
		{"GET", "/api/v1/logs/tail", false},              // the stream
		{"GET", "/api/v1/dashboards/service/tail", true}, // merely ends in /tail
		{"POST", "/api/v1/dashboards", false},            // a write
		{"POST", "/api/v1/monitors", false},              // a write
		{"POST", "/v1/series", false},                    // intake has its own backpressure
		{"GET", "/healthz", false},
	} {
		r := httptest.NewRequest(c.method, c.path, nil)
		if got := isQuery(r); got != c.want {
			t.Errorf("%s %s: limited=%v, want %v", c.method, c.path, got, c.want)
		}
	}
}
