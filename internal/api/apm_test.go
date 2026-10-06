package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/internal/tracestore"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

type fakeTraces struct {
	spans    []wire.Span
	edges    []tracestore.Edge
	services [][2]string
	filter   tracestore.Filter
	from, to int64
	err      error
}

func (f *fakeTraces) Trace(context.Context, string) ([]wire.Span, error) { return f.spans, f.err }
func (f *fakeTraces) Search(_ context.Context, fl tracestore.Filter, from, to int64, _ int, cursor string) (*tracestore.SearchResult, error) {
	f.filter, f.from, f.to = fl, from, to
	if cursor == "bad" {
		return nil, errors.New("tracestore: bad cursor")
	}
	return &tracestore.SearchResult{Traces: []tracestore.TraceSummary{{TraceID: "t", SpanID: "s", Service: "api", StartUs: 5,
		Summary: tracestore.Summary{Name: "http.request", Duration: 9, Error: 1, StatusCode: 500}}}}, f.err
}
func (f *fakeTraces) ServiceEdges(context.Context, string, int64, int64) ([]tracestore.Edge, error) {
	return f.edges, f.err
}
func (f *fakeTraces) ServicesIn(context.Context, string, int64, int64) ([][2]string, error) {
	return f.services, f.err
}

func apmServer(f *fakeTraces) *http.ServeMux {
	mux := http.NewServeMux()
	(&APM{Traces: f, Clock: testutil.NewFakeClock(time.UnixMilli(1_790_000_000_000))}).Register(mux)
	return mux
}

func call(mux http.Handler, path string) (int, map[string]any) {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

const tid32 = "0123456789abcdef0123456789abcdef"

func TestTraceView_OrphansServicesDurationAndErrors(t *testing.T) {
	spans := []wire.Span{
		{TraceID: tid32, SpanID: "0000000000000001", Service: "api", Start: 100, Duration: 50},
		{TraceID: tid32, SpanID: "0000000000000002", ParentID: "0000000000000001", Service: "db", Start: 110, Duration: 100, Error: 1},
		{TraceID: tid32, SpanID: "0000000000000003", ParentID: "00000000000000ff", Service: "worker", Start: 120, Duration: 5},
	}
	v := buildTraceView(tid32, spans)
	if v.Duration != 110 || v.Start != 100 || v.Errors != 1 || v.SpanCount != 3 {
		t.Errorf("%+v", v)
	}
	if strings.Join(v.Orphans, ",") != "0000000000000003" {
		t.Errorf("orphans = %v: only the span whose parent is missing", v.Orphans)
	}
	if strings.Join(v.Services, ",") != "api,db,worker" {
		t.Errorf("services = %v", v.Services)
	}
}

func TestTraceEndpoint_StatusCodes(t *testing.T) {
	f := &fakeTraces{}
	mux := apmServer(f)
	if code, _ := call(mux, "/api/v1/traces/nothex"); code != 400 {
		t.Errorf("malformed id: %d", code)
	}
	if code, _ := call(mux, "/api/v1/traces/"+tid32); code != 404 {
		t.Errorf("no spans: %d", code)
	}
	f.spans = []wire.Span{{TraceID: tid32, SpanID: "0000000000000001", Service: "api", Start: 1, Duration: 1}}
	if code, body := call(mux, "/api/v1/traces/"+tid32); code != 200 || body["span_count"] != float64(1) || len(body["orphans"].([]any)) != 0 {
		t.Errorf("%d %v", code, body)
	}
	f.err = errors.New("disk")
	if code, _ := call(mux, "/api/v1/traces/"+tid32); code != 500 {
		t.Errorf("store failure: %d", code)
	}
}

func TestSearchEndpoint_ParametersReachTheStoreWithTheRightUnits(t *testing.T) {
	f := &fakeTraces{}
	mux := apmServer(f)
	code, body := call(mux, "/api/v1/traces?service=api&env=dev&resource=GET+/a&name=http.request&error=true&min_duration_ms=5&max_duration_ms=50&status_code=500&from=1000&to=2000&limit=3")
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	want := tracestore.Filter{Env: "dev", Service: "api", Resource: "GET /a", Name: "http.request", ErrorsOnly: true, MinDurationUs: 5000, MaxDurationUs: 50000, StatusCode: 500}
	if f.filter != want {
		t.Errorf("filter = %+v", f.filter)
	}
	if f.from != 1_000_000 || f.to != 2_000_999 {
		t.Errorf("window %d..%d: milliseconds in, microseconds to the store, inclusive of the last millisecond", f.from, f.to)
	}
	row := body["traces"].([]any)[0].(map[string]any)
	if row["error"] != true || row["duration"] != float64(9) || row["status_code"] != float64(500) {
		t.Errorf("%v", row)
	}
}

func TestSearchEndpoint_BadParameters(t *testing.T) {
	mux := apmServer(&fakeTraces{})
	for _, q := range []string{"error=maybe", "from=x", "min_duration_ms=-", "from=5&to=1", "cursor=bad", "limit=abc"} {
		if code, _ := call(mux, "/api/v1/traces?"+q); code != 400 {
			t.Errorf("%s: %d", q, code)
		}
	}
}

func TestServiceMap_NodesEdgesAndEnvMerge(t *testing.T) {
	f := &fakeTraces{
		services: [][2]string{{"dev", "api"}, {"dev", "lonely"}, {"prod", "other"}},
		edges: []tracestore.Edge{
			{Env: "dev", Parent: "api", Child: "worker", Calls: 2, Errors: 1, DurationSumUs: 100},
			{Env: "prod", Parent: "api", Child: "worker", Calls: 2, DurationSumUs: 300},
		},
	}
	code, body := call(apmServer(f), "/api/v1/service-map")
	if code != 200 {
		t.Fatal(code)
	}
	edges := body["edges"].([]any)
	if len(edges) != 1 {
		t.Fatalf("%v", edges)
	}
	e := edges[0].(map[string]any)
	if e["calls"] != float64(4) || e["errors"] != float64(1) || e["avg_duration"] != float64(100) {
		t.Errorf("edge = %v: calls and errors sum across env, avg is the sum over calls", e)
	}
	var nodes []string
	for _, n := range body["nodes"].([]any) {
		nodes = append(nodes, n.(map[string]any)["service"].(string))
	}
	if strings.Join(nodes, ",") != "api,lonely,other,worker" {
		t.Errorf("nodes = %v: a service with no edges is still a node", nodes)
	}
}

func TestMatcher_RefusesWhatTheGrammarCannotExpress(t *testing.T) {
	if m, err := matcher("service", "my app"); err != nil || m != "service:my app" {
		t.Errorf("%q %v", m, err)
	}
	for _, v := range []string{"a,b", "a}b", "a{b", "a\nb"} {
		if _, err := matcher("service", v); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
}

func TestRedTable_BadServiceNameIs400(t *testing.T) {
	mux := apmServer(&fakeTraces{})
	// No metric store is needed: the value is refused before any query runs.
	if code, _ := call(mux, "/api/v1/services/a%2Cb/resources"); code != 400 {
		t.Errorf("%d", code)
	}
	if code, _ := call(mux, "/api/v1/services?env=a%7Db"); code != 400 {
		t.Errorf("%d", code)
	}
}
