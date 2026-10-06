package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent"
	agentconfig "github.com/tuvo1106/ozymandias/internal/agent/config"
	"github.com/tuvo1106/ozymandias/internal/httpserve"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

// The trace path in one process, every layer production code: spans posted to a
// real agent, statistics computed from all of them, a sample forwarded to a real
// ozyd, stored, and read back through the APM API. It pins the argument the whole
// design rests on: the request count a service page shows is the traffic that
// happened, not the fraction the sampler kept.
func TestEndToEnd_TracesStatsBeforeSamplingAndOneTraceAcrossServices(t *testing.T) {
	clk := testutil.NewFakeClock(time.Unix(1790000000, 0))
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
	acfg.Statsd.Enabled = false
	acfg.Collectors.Docker.Enabled = false
	acfg.Collectors.Host.Enabled = false
	acfg.HTTP.ShutdownTimeout = time.Second
	a, err := agent.New(acfg, agent.Options{Logger: quiet, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	ln, _ := httpserve.Listen("127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, ln) }()
	defer func() { cancel(); <-done }()
	testutil.Eventually(t, 2*time.Second, func() bool { return clk.Waiters() >= 1 }, "agent not started")

	startUs := clk.Now().UnixMicro()
	span := func(trace, spanID int, service, resource string, dur time.Duration, extra map[string]any) map[string]any {
		s := map[string]any{
			"trace_id": fmt.Sprintf("%032x", trace), "span_id": fmt.Sprintf("%016x", spanID), "service": service,
			"name": "http.request", "resource": resource, "type": "web", "start": startUs, "duration": dur.Microseconds(), "error": 0,
			"meta": map[string]string{"http.status_code": "200"}, "metrics": map[string]float64{"_top_level": 1, "_sampling_priority": 0},
		}
		for k, v := range extra {
			s[k] = v
		}
		return s
	}
	post := func(chunks ...[]map[string]any) {
		body, _ := json.Marshal(map[string]any{"tracer": map[string]string{"lang": "test"}, "traces": chunks})
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/traces", strings.NewReader(string(body))))
		if rec.Code != 200 {
			t.Fatalf("agent answered %d: %s", rec.Code, rec.Body)
		}
	}

	// 200 requests with the head sampler keeping 10% (priority 1), 20 of them failing.
	const total, failing = 200, 20
	for i := 1; i <= total; i++ {
		extra := map[string]any{}
		if i%10 == 0 {
			extra["metrics"] = map[string]float64{"_top_level": 1, "_sampling_priority": 1}
		}
		if i <= failing {
			extra["error"] = 1
			extra["meta"] = map[string]string{"http.status_code": "503"}
		}
		post([]map[string]any{span(i, i, "shop", "GET /items/:id", time.Duration(10+i%40)*time.Millisecond, extra)})
	}
	// One trace across two services, kept by the head sampler, delivered in two requests.
	post([]map[string]any{span(9001, 1, "gateway", "POST /checkout", 80*time.Millisecond,
		map[string]any{"metrics": map[string]float64{"_top_level": 1, "_sampling_priority": 1}})})
	post([]map[string]any{span(9001, 2, "billing", "charge", 30*time.Millisecond,
		map[string]any{"parent_id": fmt.Sprintf("%016x", 1), "name": "arq.job", "type": "worker",
			"metrics": map[string]float64{"_top_level": 1, "_sampling_priority": 1}})})

	get := func(path string) map[string]any {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		out["_code"] = float64(rec.Code)
		return out
	}
	window := fmt.Sprintf("from=%d&to=%d", clk.Now().Add(-time.Minute).UnixMilli(), clk.Now().Add(5*time.Minute).UnixMilli())

	var shop map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for shop == nil && time.Now().Before(deadline) {
		clk.Advance(time.Second)
		for _, r := range get("/api/v1/services?env=e2e&" + window)["services"].([]any) {
			if row := r.(map[string]any); row["service"] == "shop" && row["requests"] == float64(total) && row["p95_ms"] != nil { // sketches travel on their own hop
				shop = row
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if shop == nil {
		t.Fatalf("the service table never reached the full request count: %v / metrics %v", get("/api/v1/services?env=e2e&"+window), get("/api/v1/metrics?prefix=trace."))
	}

	if shop["errors"] != float64(failing) || shop["error_pct"] != 10.0 {
		t.Errorf("errors = %v (%v%%), want exactly %d: statistics must not be sampled", shop["errors"], shop["error_pct"], failing)
	}
	if shop["p95_ms"] == nil {
		t.Errorf("no p95 in %v", shop)
	}

	// The store holds a sample: far fewer than 200, but every failing request's
	// trace is a candidate for the error sampler and the first of the resource is rare.
	list := get("/api/v1/traces?service=shop&env=e2e&" + window)["traces"].([]any)
	if len(list) == 0 || len(list) >= total/2 {
		t.Errorf("%d shop traces stored of %d sent: want a sample", len(list), total)
	}
	errList := get("/api/v1/traces?service=shop&env=e2e&error=true&" + window)["traces"].([]any)
	if len(errList) == 0 {
		t.Error("no error trace kept although 20 requests failed")
	}

	// The cross-service trace is one trace of two spans, and the map has the edge.
	tr := get("/api/v1/traces/" + fmt.Sprintf("%032x", 9001))
	if tr["_code"] != float64(200) || tr["span_count"] != float64(2) || len(tr["orphans"].([]any)) != 0 {
		t.Fatalf("trace = %v", tr)
	}
	if fmt.Sprint(tr["services"]) != "[billing gateway]" {
		t.Errorf("services = %v", tr["services"])
	}
	sm := get("/api/v1/service-map?env=e2e&" + window)
	edges := sm["edges"].([]any)
	if len(edges) != 1 || edges[0].(map[string]any)["parent"] != "gateway" || edges[0].(map[string]any)["child"] != "billing" {
		t.Errorf("edges = %v", edges)
	}
	if code := get("/api/v1/traces/" + fmt.Sprintf("%032x", 5))["_code"]; code != float64(404) && code != float64(200) {
		t.Errorf("unexpected %v", code)
	}
	if code := get("/api/v1/traces/nothex")["_code"]; code != float64(400) {
		t.Errorf("a malformed id: %v", code)
	}

	// Resource table for one service.
	res := get("/api/v1/services/shop/resources?env=e2e&" + window)["services"].([]any)
	if len(res) != 1 || res[0].(map[string]any)["resource"] != "get /items/:id" || res[0].(map[string]any)["requests"] != float64(total) {
		t.Errorf("resources = %v", res)
	}
}
