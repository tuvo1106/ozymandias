package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/query/metricql/eval"
)

// evalMaxQueriesPerBatch is the evaluator's limit, named here so a test cannot
// quietly disagree with the constant it is checking.
const evalMaxQueriesPerBatch = eval.MaxQueriesPerBatch

// postRaw is post without the JSON decoding, for a test that wants the bytes.
func postRaw(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return rec
}

// batchWindow is the fixture's data as a JSON batch would name it.
const batchWindow = `"from":1789999980,"to":1790000000,"interval":20`

// resultsOf decodes the per-query answers, checking the shape a client relies
// on: an array, in request order, with the index each entry claims.
func resultsOf(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	raw, ok := out["results"].([]any)
	if !ok {
		t.Fatalf("no results in %v", out)
	}
	list := make([]map[string]any, 0, len(raw))
	for i, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("result %d is %T", i, r)
		}
		if m["index"] != float64(i) {
			t.Errorf("result %d claims index %v", i, m["index"])
		}
		list = append(list, m)
	}
	return list
}

// A dashboard's worth of questions, answered in one request and in order.
func TestQueryBatch_AnswersEachQuery(t *testing.T) {
	h, _ := metricsAPI(t)
	code, out := post(t, h, "/api/v1/query/batch", `{"queries":[
		{"q":"sum:http.request.count{*} by {route}"},
		{"q":"avg:http.request.count{*}"},
		{"metric":"queue.depth","agg":"max"}
	],`+batchWindow+`}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	results := resultsOf(t, out)
	if len(results) != 3 {
		t.Fatalf("got %d results for 3 queries", len(results))
	}
	for i, r := range results {
		if r["status"] != "ok" {
			t.Errorf("result %d: %v", i, r)
		}
		if r["query"] == "" {
			t.Errorf("result %d does not say what was evaluated", i)
		}
		if _, ok := r["series"].([]any); !ok {
			t.Errorf("result %d has no series: %v", i, r)
		}
	}
	// The structured entry is translated, like it is on /api/v1/query.
	if q, _ := results[2]["query"].(string); !strings.Contains(q, "queue.depth") {
		t.Errorf("the M1 parameters were not translated: %q", q)
	}
	// The window belongs to the batch, so it is reported once — and the
	// interval does not, because a rollup can put one query on its own grid.
	if out["from"] != 1789999980.0 || out["to"] != 1790000000.0 {
		t.Errorf("window %v/%v", out["from"], out["to"])
	}
	if _, ok := out["interval"]; ok {
		t.Errorf("the batch reports one interval for every result: %v", out["interval"])
	}
	for i, r := range results {
		if r["interval"] != 20.0 {
			t.Errorf("result %d: interval %v, want 20", i, r["interval"])
		}
	}
}

// The reason the endpoint reports per query: one broken widget must not blank
// the dashboard around it.
func TestQueryBatch_OneBadQueryDoesNotSinkTheBatch(t *testing.T) {
	h, _ := metricsAPI(t)
	code, out := post(t, h, "/api/v1/query/batch", `{"queries":[
		{"q":"sum:http.request.count{*} by {route}"},
		{"q":"sum:http.request.count{"},
		{"q":"avg:http.request.count{*}"}
	],`+batchWindow+`}`)
	if code != 200 {
		t.Fatalf("%d %v — one unparseable query took the batch down", code, out)
	}
	results := resultsOf(t, out)
	if results[0]["status"] != "ok" || results[2]["status"] != "ok" {
		t.Errorf("a neighbour failed: %v / %v", results[0], results[2])
	}
	bad := results[1]
	if bad["status"] != "error" {
		t.Fatalf("the broken query was accepted: %v", bad)
	}
	if bad["code"] != 400.0 {
		t.Errorf("code %v, want 400 — it is the query's fault", bad["code"])
	}
	if msg, _ := bad["error"].(string); msg == "" {
		t.Error("no error message, so nothing can be shown in the widget")
	}
	// A failed query carries no half-built answer — and says so with an empty
	// list, because every result has the same fields.
	series, ok := bad["series"].([]any)
	if !ok {
		t.Fatalf("the failed result has no series field: %v", bad)
	}
	if len(series) != 0 {
		t.Errorf("the failed query carries %d series", len(series))
	}
}

// Every query failing is still a well-formed batch: twelve messages are more
// use than one.
func TestQueryBatch_AllQueriesBadIsStill200(t *testing.T) {
	h, _ := metricsAPI(t)
	code, out := post(t, h, "/api/v1/query/batch",
		`{"queries":[{"q":"sum:{"},{"q":"!!"}],`+batchWindow+`}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	for i, r := range resultsOf(t, out) {
		if r["status"] != "error" || r["code"] != 400.0 {
			t.Errorf("result %d: %v", i, r)
		}
	}
	// No grid was planned for either, and inventing one would be a lie.
	for i, r := range resultsOf(t, out) {
		if r["interval"] != 0.0 {
			t.Errorf("result %d: interval %v, want 0 — it was never evaluated", i, r["interval"])
		}
	}
}

// The batch itself can be malformed, and then there is no per-query answer to
// put an error in.
func TestQueryBatch_RequestLevelFailuresAre400(t *testing.T) {
	h, _ := metricsAPI(t)
	for _, tc := range []struct{ name, body, want string }{
		{"no queries", `{"queries":[],` + batchWindow + `}`, "queries is required"},
		{"queries missing", `{` + batchWindow + `}`, "queries is required"},
		{"not JSON", `{"queries":`, "body"},
		{"an unknown field", `{"queries":[{"q":"sum:temp{*}"}],"interval":20,"windo":1}`, "unknown field"},
		{"an unknown field in a query", `{"queries":[{"q":"sum:temp{*}","from":1}],"interval":20}`, "unknown field"},
		{"trailing content", `{"queries":[{"q":"sum:temp{*}"}]}{"queries":[]}`, "trailing content"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := post(t, h, "/api/v1/query/batch", tc.body)
			if code != 400 {
				t.Fatalf("%d %v, want 400", code, out)
			}
			if msg, _ := out["error"].(string); !strings.Contains(msg, tc.want) {
				t.Errorf("error %q, want it to mention %q", msg, tc.want)
			}
		})
	}
}

// The batch is bounded, and the message says what the bound is — the caller is
// a dashboard, and the fix is to ask for less.
func TestQueryBatch_TooManyQueriesIs400(t *testing.T) {
	h, _ := metricsAPI(t)
	var qs []string
	for range evalMaxQueriesPerBatch + 1 {
		qs = append(qs, `{"q":"avg:http.request.count{*}"}`)
	}
	code, out := post(t, h, "/api/v1/query/batch",
		`{"queries":[`+strings.Join(qs, ",")+`],`+batchWindow+`}`)
	if code != 400 {
		t.Fatalf("%d %v, want 400", code, out)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, fmt.Sprint(evalMaxQueriesPerBatch)) {
		t.Errorf("error %q does not say what the limit is", msg)
	}

	// The boundary itself is answered.
	code, out = post(t, h, "/api/v1/query/batch",
		`{"queries":[`+strings.Join(qs[:evalMaxQueriesPerBatch], ",")+`],`+batchWindow+`}`)
	if code != 200 {
		t.Fatalf("a batch of exactly %d: %d %v", evalMaxQueriesPerBatch, code, out)
	}
}

// A batch's body is larger than one query's, and the error says which limit was
// hit rather than reporting a truncated body as malformed JSON.
func TestQueryBatch_AnOversizedBodyIs400(t *testing.T) {
	h, _ := metricsAPI(t)
	big := `{"queries":[{"q":"avg:http.request.count{*}"}],"vars":{"env":["` +
		strings.Repeat("x", maxBatchBodyBytes) + `"]}}`
	code, out := post(t, h, "/api/v1/query/batch", big)
	if code != 400 {
		t.Fatalf("%d, want 400", code)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, fmt.Sprint(maxBatchBodyBytes)) {
		t.Errorf("error %q does not name the batch's limit", msg)
	}
	// And the single-query limit is still its own, smaller one.
	if maxBatchBodyBytes <= maxBodyBytes {
		t.Errorf("the batch limit (%d) is not larger than one query's (%d)", maxBatchBodyBytes, maxBodyBytes)
	}
}

// Template variables belong to the batch: a dashboard has one variable bar, and
// the names fold the way the lexer folds them.
func TestQueryBatch_VariablesApplyToEveryQuery(t *testing.T) {
	h, _ := metricsAPI(t)
	code, out := post(t, h, "/api/v1/query/batch", `{"queries":[
		{"q":"sum:http.request.count{$Env} by {route}"},
		{"q":"avg:http.request.count{$env}"}
	],`+batchWindow+`,"vars":{"ENV":["env:dev"]}}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	for i, r := range resultsOf(t, out) {
		if r["status"] != "ok" {
			t.Fatalf("result %d: %v", i, r)
		}
		if len(r["series"].([]any)) == 0 {
			t.Errorf("result %d matched nothing, so the binding did not reach it", i)
		}
	}
}

// Warnings are per query and always present on a success: a client asking
// "what should I know about this widget?" gets an answer, not a missing field.
func TestQueryBatch_WarningsArePerQueryAndAlwaysPresent(t *testing.T) {
	h, _ := metricsAPI(t)
	code, out := post(t, h, "/api/v1/query/batch", `{"queries":[
		{"q":"sum:http.request.count{*} by {route}"},
		{"metric":"http.request.count","filter":"route","agg":"sum"}
	],`+batchWindow+`}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	results := resultsOf(t, out)
	for i, r := range results {
		if _, ok := r["warnings"].([]any); !ok {
			t.Errorf("result %d has no warnings array: %v", i, r)
		}
	}
	// The bare `route` term is widened to `route:*`, and the warning that says
	// so belongs to the query that caused it and to no other.
	if n := len(results[0]["warnings"].([]any)); n != 0 {
		t.Errorf("the first query collected %d warnings that are not its own", n)
	}
	if n := len(results[1]["warnings"].([]any)); n == 0 {
		t.Error("the widened filter did not warn")
	}
}

// The endpoint is POST only: a batch is a body, and a GET of one would be a
// query string nobody can read.
func TestQueryBatch_GetIsNotAllowed(t *testing.T) {
	h, _ := metricsAPI(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/query/batch?q=sum:temp%7B*%7D", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET gave %d, want 405", rec.Code)
	}
}

// The response has to survive being read by a client, not just by a test that
// knows what it put in. This asserts the documented shape field by field.
func TestQueryBatch_ResponseShape(t *testing.T) {
	h, _ := metricsAPI(t)
	rec := postRaw(t, h, "/api/v1/query/batch",
		`{"queries":[{"q":"sum:http.request.count{*} by {route}"},{"q":"sum:{"}],`+batchWindow+`}`)
	var body struct {
		Status  string `json:"status"`
		From    int64  `json:"from"`
		To      int64  `json:"to"`
		Results []struct {
			Index    int      `json:"index"`
			Status   string   `json:"status"`
			Query    string   `json:"query"`
			Interval int64    `json:"interval"`
			Warnings []string `json:"warnings"`
			Code     int      `json:"code"`
			Error    string   `json:"error"`
			Series   []struct {
				Metric string              `json:"metric"`
				Tags   map[string]string   `json:"tags"`
				Scope  string              `json:"scope"`
				Points [][]json.RawMessage `json:"points"`
			} `json:"series"`
		} `json:"results"`
	}
	dec := json.NewDecoder(strings.NewReader(rec.Body.String()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("the documented shape does not decode the response: %v\n%s", err, rec.Body.String())
	}
	if body.Status != "ok" || len(body.Results) != 2 {
		t.Fatalf("%+v", body)
	}
	ok, bad := body.Results[0], body.Results[1]
	if ok.Status != "ok" || len(ok.Series) == 0 || ok.Code != 0 || ok.Error != "" {
		t.Errorf("the successful result: %+v", ok)
	}
	if len(ok.Series[0].Points) == 0 || ok.Series[0].Scope == "" {
		t.Errorf("the series is not renderable: %+v", ok.Series[0])
	}
	// Each point is the [t, v] pair the single-query endpoint already sends;
	// a batch must not invent a second spelling of a point.
	for i, p := range ok.Series[0].Points {
		if len(p) != 2 {
			t.Errorf("point %d is %d values, want [t, v]", i, len(p))
		}
	}
	if bad.Status != "error" || bad.Code != 400 || bad.Error == "" || len(bad.Series) != 0 {
		t.Errorf("the failed result: %+v", bad)
	}
}

// How a failure is described is the part of this endpoint a client cannot
// recover from being wrong about: the whole response is a 200, so the code and
// the message are the only things that say what went wrong, and a store's
// error must not travel in one.
//
// Tested on batchOutcome directly because two of the four cases — the shared
// deadline, and a store failing mid-batch — cannot be provoked through HTTP
// without a store that fails on command, and a fixture built to make an
// assertion pass is weaker evidence than the classification itself.
func TestBatchOutcome_ClassifiesFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantCode int
		wantMsg  string
		leaks    string // a substring that must NOT reach the client
		logged   bool
	}{
		{
			name:     "the query's own fault",
			err:      fmt.Errorf("%w: temp is a gauge", eval.ErrBadQuery),
			wantCode: 400,
			wantMsg:  "temp is a gauge",
		},
		{
			name:     "the batch ran out of time",
			err:      context.DeadlineExceeded,
			wantCode: 503,
			wantMsg:  "the batch ran out of time",
		},
		{
			name:     "this server cannot answer it",
			err:      fmt.Errorf("%w: lat", eval.ErrNoSketchStore),
			wantCode: 503,
			wantMsg:  "lat",
			logged:   true,
		},
		{
			name:     "ours",
			err:      errors.New("open /var/lib/ozy/blocks/01J.../index: permission denied"),
			wantCode: 500,
			wantMsg:  "the query could not be answered",
			leaks:    "/var/lib/ozy",
			logged:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &logSink{}
			m := &Metrics{Logger: slog.New(sink)}
			got := m.batchOutcome(
				batchResult{Index: 2, Query: "p95:lat{*}"},
				eval.Outcome{Err: tc.err},
			)
			if got.Status != "error" {
				t.Errorf("status %q", got.Status)
			}
			if got.Code != tc.wantCode {
				t.Errorf("code %d, want %d", got.Code, tc.wantCode)
			}
			if !strings.Contains(got.Error, tc.wantMsg) {
				t.Errorf("error %q, want it to mention %q", got.Error, tc.wantMsg)
			}
			if tc.leaks != "" && strings.Contains(got.Error, tc.leaks) {
				t.Errorf("error %q leaks %q to the caller", got.Error, tc.leaks)
			}
			// The index and the query survive: a widget has to know which of
			// its twelve neighbours this belongs to.
			if got.Index != 2 || got.Query != "p95:lat{*}" {
				t.Errorf("%+v lost its identity", got)
			}
			if len(got.Series) != 0 || got.Series == nil {
				t.Errorf("series = %v, want an empty list", got.Series)
			}
			if gotLogged := len(sink.records) > 0; gotLogged != tc.logged {
				t.Errorf("logged = %v, want %v (records: %d)", gotLogged, tc.logged, len(sink.records))
			}
			// Whatever was hidden from the caller is in the log, or nobody
			// can act on it.
			if tc.leaks != "" && tc.logged {
				found := false
				for i := range sink.records {
					if strings.Contains(fmt.Sprint(sink.attr(i, "err")), tc.leaks) {
						found = true
					}
				}
				if !found {
					t.Errorf("%q reached neither the caller nor the log", tc.leaks)
				}
			}
		})
	}
}

// End to end, on the one server-side failure a fixture can actually produce: a
// percentile with no sketch store. The batch keeps going and the widget that
// asked gets the reason.
func TestQueryBatch_AQueryThisServerCannotAnswerIs503InItsOwnResult(t *testing.T) {
	h := sketchlessAPI(t)
	code, out := post(t, h, "/api/v1/query/batch", `{"queries":[
		{"q":"sum:lat.count{*}"},
		{"q":"p95:lat{*}"}
	],`+batchWindow+`}`)
	if code != 200 {
		t.Fatalf("%d %v — one unanswerable query took the batch down", code, out)
	}
	results := resultsOf(t, out)
	if results[0]["status"] != "ok" {
		t.Errorf("the answerable query failed: %v", results[0])
	}
	bad := results[1]
	if bad["code"] != 503.0 {
		t.Errorf("code %v, want 503 — not here, not now, rather than never", bad["code"])
	}
	if msg, _ := bad["error"].(string); !strings.Contains(msg, "sketch store") {
		t.Errorf("error %q does not name what is missing", msg)
	}
}

// A `.rollup(method, seconds)` sets the grid of the query it is written on
// (ADR-0016), so two queries in one batch can be evaluated on different bucket
// widths. The endpoint reported a single interval for the whole batch, which
// meant the response stated a width some of its own results did not have and
// gave no way to find the real one — a review caught it, and this is the case.
//
// Such a query shares no work with its neighbours, which is a reason not to
// write one, not a reason for the answer to be wrong.
func TestQueryBatch_AQueryWithItsOwnRollupReportsItsOwnInterval(t *testing.T) {
	h, _ := metricsAPI(t)
	code, out := post(t, h, "/api/v1/query/batch", `{"queries":[
		{"q":"sum:http.request.count{*}"},
		{"q":"sum:http.request.count{*}.rollup(sum, 600)"}
	],"from":1789999980,"to":1790000000}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	results := resultsOf(t, out)
	for i, r := range results {
		if r["status"] != "ok" {
			t.Fatalf("result %d: %v", i, r)
		}
	}
	plain, rolled := results[0]["interval"], results[1]["interval"]
	if rolled != 600.0 {
		t.Errorf("the rolled-up query reports interval %v, want 600", rolled)
	}
	if plain == rolled {
		t.Errorf("both results report %v; the rollup did not set its own grid", plain)
	}
	// And each result's points are on the grid it claims.
	for i, r := range results {
		series := r["series"].([]any)
		if len(series) == 0 {
			t.Fatalf("result %d has no series", i)
		}
		points := series[0].(map[string]any)["points"].([]any)
		if len(points) < 2 {
			continue
		}
		first := points[0].([]any)[0].(float64)
		second := points[1].([]any)[0].(float64)
		if gap := (second - first) / 1000; gap != r["interval"] {
			t.Errorf("result %d: points are %gs apart but it claims %v", i, gap, r["interval"])
		}
	}
}

// A window belongs to the request, so a bad one is answered once. Before, every
// query failed with the same sentence inside a 200 — fifty copies of it for a
// dashboard, which is how a client learns to stop reading them.
func TestQueryBatch_ABadWindowIsOneRequestLevelError(t *testing.T) {
	h, _ := metricsAPI(t)
	for _, tc := range []struct{ name, window, want string }{
		{"backwards", `"from":1790000000,"to":1789999980`, "must be after"},
		{"a negative interval", `"from":1789999980,"to":1790000000,"interval":-5`, "must be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := post(t, h, "/api/v1/query/batch",
				`{"queries":[{"q":"sum:http.request.count{*}"},{"q":"avg:http.request.count{*}"}],`+tc.window+`}`)
			if code != 400 {
				t.Fatalf("%d %v, want 400 — the window is the request's, not each query's", code, out)
			}
			if msg, _ := out["error"].(string); !strings.Contains(msg, tc.want) {
				t.Errorf("error %q, want it to mention %q", msg, tc.want)
			}
			if _, ok := out["results"]; ok {
				t.Error("a request-level refusal carries per-query results")
			}
		})
	}
}

// A client that hangs up gets 499 — but only when a query actually died of it.
//
// The two halves are tested apart from HTTP because the case that distinguishes
// them — the client disconnecting after the last query answered — is a race that
// cannot be provoked through the handler. The first version of this check tested
// the request context alone and would have thrown away a complete batch; the
// first version of *this test* accepted either answer, which pinned nothing.
func TestAbandoned(t *testing.T) {
	ok := eval.Outcome{}
	cancelled := eval.Outcome{Err: fmt.Errorf("selecting: %w", context.Canceled)}
	failed := eval.Outcome{Err: errors.New("disk is on fire")}

	for _, tc := range []struct {
		name       string
		requestErr error
		out        []eval.Outcome
		want       bool
	}{
		{"the client left and a query died of it", context.Canceled, []eval.Outcome{ok, cancelled}, true},
		{"the client left after every query answered", context.Canceled, []eval.Outcome{ok, ok}, false},
		{"a store cancelled its own work", nil, []eval.Outcome{cancelled}, false},
		{"nothing is wrong", nil, []eval.Outcome{ok}, false},
		{"the client left and a query failed for its own reasons", context.Canceled, []eval.Outcome{failed}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := abandoned(tc.requestErr, tc.out); got != tc.want {
				t.Errorf("abandoned = %v, want %v", got, tc.want)
			}
		})
	}
}

// End to end on the case that is reachable: the client is already gone when the
// batch starts, so the query is cancelled and 499 is the answer.
func TestQueryBatch_AnAbandonedBatchIs499(t *testing.T) {
	h, _ := metricsAPI(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/query/batch",
		strings.NewReader(`{"queries":[{"q":"sum:http.request.count{*}"}],`+batchWindow+`}`))
	h.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != statusClientClosedRequest {
		t.Errorf("%d, want %d — a departed client is not a server error",
			rec.Code, statusClientClosedRequest)
	}
}
