package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent"
	agentconfig "github.com/tuvo1106/ozymandias/internal/agent/config"
	"github.com/tuvo1106/ozymandias/internal/httpserve"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

// L8: the tracer bullet in one process. A statsd datagram goes into a real
// agent over UDP; a fake-clock advance makes the agent flush; the forwarder
// POSTs to a real ozyd; the query API returns the value. Every layer is
// the production code — only the clock and the ports are the test's.
func TestEndToEnd_StatsdToQuery(t *testing.T) {
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
	acfg.Tags = []string{"env:e2e"}
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
	for i := range 5 {
		_, _ = fmt.Fprintf(conn, "e2e.hits:1|c|#route:/r%d", i%2)
	}
	_, _ = conn.Write([]byte("e2e.depth:4|g"))
	received := areg.Counter("ozy.agent.statsd.messages_received")
	testutil.Eventually(t, 3*time.Second, func() bool { return received.Value() == 6 }, "agent received %d", received.Value())
	testutil.Eventually(t, 2*time.Second, func() bool { return clk.Waiters() >= 1 }, "aggregator loop not started")

	clk.Advance(10 * time.Second) // bucket [1790000000, +10) closes → flush → forward

	type queryResp struct {
		Series []struct {
			Tags   map[string]string `json:"tags"`
			Points [][2]*float64     `json:"points"`
		} `json:"series"`
	}
	query := func(url string) queryResp {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
		var r queryResp
		_ = json.Unmarshal(rec.Body.Bytes(), &r)
		return r
	}
	sumAt := func(r queryResp, ts int64) float64 {
		var total float64
		for _, s := range r.Series {
			for _, p := range s.Points {
				if p[0] != nil && int64(*p[0]) == ts*1000 && p[1] != nil {
					total += *p[1]
				}
			}
		}
		return total
	}

	hits := "/api/v1/query?metric=e2e.hits&agg=sum&from=1790000000&to=1790000009&interval=10"
	testutil.Eventually(t, 3*time.Second, func() bool { return sumAt(query(hits), 1790000000) == 5 },
		"query never returned 5: %+v", query(hits))

	byRoute := query(hits + "&by=route&filter=env:e2e,host:box")
	if len(byRoute.Series) != 2 || sumAt(byRoute, 1790000000) != 5 {
		t.Fatalf("by route: %+v", byRoute)
	}
	if r := query("/api/v1/query?metric=e2e.depth&from=1790000000&to=1790000009&interval=10"); sumAt(r, 1790000000) != 4 {
		t.Fatalf("gauge: %+v", r)
	}
	// The agent's own metrics took the same path.
	self := "/api/v1/query?metric=ozy.agent.statsd.messages_received&agg=sum&from=1790000000&to=1790000019&interval=20&filter=host:box"
	if r := query(self); sumAt(r, 1790000000) != 6 {
		t.Fatalf("agent self-metric: %+v", r)
	}
}
