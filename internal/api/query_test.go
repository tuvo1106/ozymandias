package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/query/metricql"
	"github.com/tuvo1106/ozymandias/internal/query/metricql/eval"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/naive"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// post sends a JSON body and returns the status and the decoded object.
func post(t *testing.T, h http.Handler, path, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: body %q is not JSON", path, rec.Body.String())
	}
	return rec.Code, out
}

// window is the fixture's data, which sits in the twenty seconds before `now`.
const window = "&from=1789999980&to=1790000000&interval=20"

func seriesOf(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	raw, ok := out["series"].([]any)
	if !ok {
		t.Fatalf("no series in %v", out)
	}
	list := make([]map[string]any, 0, len(raw))
	for _, s := range raw {
		list = append(list, s.(map[string]any))
	}
	return list
}

// The query language is now what the endpoint speaks.
func TestQuery_Expression(t *testing.T) {
	h, _ := metricsAPI(t)
	code, out := get(t, h, "/api/v1/query?q=sum:http.request.count%7B*%7D+by+%7Broute%7D"+window)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	series := seriesOf(t, out)
	if len(series) != 2 {
		t.Fatalf("got %d lines, want one per route: %v", len(series), out)
	}
	if got := series[0]["scope"]; got != "route:/a" {
		t.Errorf("scope %v, want route:/a — a legend should not have to join the tags itself", got)
	}
	// The response echoes what was evaluated, canonically spelled.
	if got := out["query"]; got != "sum:http.request.count{*} by {route}" {
		t.Errorf("query %q, want the canonical spelling", got)
	}
	if _, ok := out["warnings"].([]any); !ok {
		t.Errorf("warnings missing from %v; an empty list is not the same as absent", out)
	}
}

// Arithmetic between two queries is the thing the M1 parameters could not
// express at all, and is why the endpoint moved.
func TestQuery_Arithmetic(t *testing.T) {
	h, _ := metricsAPI(t)
	q := "sum:http.request.count{route:/a} by {route} / sum:http.request.count{*} by {route} * 100"
	code, out := get(t, h, "/api/v1/query?q="+urlEncode(q)+window)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	series := seriesOf(t, out)
	if len(series) != 1 {
		t.Fatalf("got %d lines, want the one group that matched: %v", len(series), out)
	}
	points := series[0]["points"].([]any)
	if v := points[0].([]any)[1]; v != 100.0 {
		t.Errorf("value %v, want 100", v)
	}
	// The unmatched group is reported rather than silently dropped.
	warnings := out["warnings"].([]any)
	if len(warnings) != 1 || !strings.Contains(warnings[0].(string), "route:/b") {
		t.Errorf("warnings %v, want one naming the dropped group", warnings)
	}
}

// POST exists because a generated dashboard query outgrows a URL.
func TestQuery_Post(t *testing.T) {
	h, _ := metricsAPI(t)
	code, out := post(t, h, "/api/v1/query", `{
		"q": "sum:http.request.count{$env} by {route}",
		"from": 1789999980, "to": 1790000000, "interval": 20,
		"vars": {"env": ["env:dev"]}
	}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if len(seriesOf(t, out)) != 2 {
		t.Errorf("got %v, want both routes", out)
	}
}

// A dashboard's multi-select arrives as a repeated parameter, and means
// either value rather than both at once.
func TestQuery_TemplateVariablesFromTheQueryString(t *testing.T) {
	h, _ := metricsAPI(t)
	code, out := get(t, h,
		"/api/v1/query?q="+urlEncode("sum:http.request.count{$r} by {route}")+
			"&var.r=route:/a&var.r=route:/b"+window)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if n := len(seriesOf(t, out)); n != 2 {
		t.Errorf("got %d lines, want both routes — a multi-select is a choice", n)
	}
}

// The editor calls this on every keystroke and wants a column to underline,
// so a query that does not parse is still a well-formed request.
func TestQueryValidate(t *testing.T) {
	h, _ := metricsAPI(t)

	code, out := post(t, h, "/api/v1/query/validate", `{"q":"sum:x{*} by {k}"}`)
	if code != 200 || out["ok"] != true {
		t.Fatalf("%d %v", code, out)
	}
	if out["query"] != "sum:x{*} by {k}" {
		t.Errorf("query %v, want the canonical spelling", out["query"])
	}

	code, out = post(t, h, "/api/v1/query/validate", `{"q":"sum:x{a:b by {k}"}`)
	if code != 200 {
		t.Fatalf("a query that does not parse is still a valid request: %d", code)
	}
	if out["ok"] != false {
		t.Fatalf("ok %v, want false: %v", out["ok"], out)
	}
	e := out["error"].(map[string]any)
	if e["col"].(float64) != 14 {
		t.Errorf("col %v, want 14 (the '{' that ended the value)", e["col"])
	}
	if !strings.Contains(e["msg"].(string), "expected") {
		t.Errorf("msg %q", e["msg"])
	}
	// The column is what the editor underlines, so it must not carry the
	// "col N:" prefix that Error() adds for a terminal.
	if strings.Contains(e["msg"].(string), "col ") {
		t.Errorf("msg %q repeats the column", e["msg"])
	}
}

// The M1 parameters still work: they are translated into the query language
// and evaluated by the one engine.
func TestQuery_StructuredParametersStillWork(t *testing.T) {
	h, _ := metricsAPI(t)
	code, out := get(t, h, "/api/v1/query?metric=http.request.count&filter=env:dev&by=route&agg=sum"+window)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if len(seriesOf(t, out)) != 2 {
		t.Fatalf("got %v", out)
	}
	// And the response says what they became, which is how somebody migrating
	// finds out what to write.
	if got := out["query"]; got != "sum:http.request.count{env:dev} by {route}" {
		t.Errorf("query %q, want the translation", got)
	}
}

// The adapter builds a program out of strings the caller sent. A value that
// closed the filter early would let a caller write a query of their own
// choosing — a different metric, a different aggregation — under parameters
// that say otherwise.
func TestQuery_StructuredParametersCannotInjectAQuery(t *testing.T) {
	h, _ := metricsAPI(t)
	for _, tc := range []struct{ name, url, want string }{
		{
			"a brace in a filter value",
			"/api/v1/query?metric=http.request.count&filter=" + urlEncode("route:/a} + sum:queue.depth{*"),
			"cannot contain",
		},
		{
			"a brace in a negated value",
			"/api/v1/query?metric=http.request.count&filter=" + urlEncode("!route:{"),
			"cannot contain",
		},
		{
			"an aggregator that is not one",
			"/api/v1/query?metric=http.request.count&agg=" + urlEncode("sum:queue.depth{*}} + sum"),
			"want one of",
		},
		{
			"a metric name that is not one",
			"/api/v1/query?metric=" + urlEncode("x{*} + sum:queue.depth{*"),
			"valid metric name",
		},
		{
			"a group-by key that is not one",
			"/api/v1/query?metric=http.request.count&by=" + urlEncode("k} by {j"),
			"is not a tag key",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := get(t, h, tc.url+window)
			if code != 400 {
				t.Fatalf("%d %v, want 400", code, out)
			}
			if !strings.Contains(out["error"].(string), tc.want) {
				t.Errorf("error %q, want it to mention %q", out["error"], tc.want)
			}
		})
	}
}

// Sending both spellings is a request that says two things; the caller is the
// one who should decide which.
func TestQuery_BothSpellingsIsAnError(t *testing.T) {
	h, _ := metricsAPI(t)
	code, out := get(t, h, "/api/v1/query?q=sum:x%7B*%7D&metric=y"+window)
	if code != 400 || !strings.Contains(out["error"].(string), "not both") {
		t.Errorf("%d %v, want a 400 about sending both", code, out)
	}
}

// M1 read a bare `k` as "has the bare tag k", which the query language cannot
// spell. Widening it is better than failing, but only if it is said out loud.
func TestQuery_ABareTagTermIsWidenedWithAWarning(t *testing.T) {
	h, _ := metricsAPI(t)
	code, out := get(t, h, "/api/v1/query?metric=http.request.count&filter=env&agg=sum"+window)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if got := out["query"]; got != "sum:http.request.count{env:*}" {
		t.Errorf("query %q, want the widened filter", got)
	}
	warnings := out["warnings"].([]any)
	if len(warnings) != 1 || !strings.Contains(warnings[0].(string), "widened") {
		t.Errorf("warnings %v, want one saying the term was widened", warnings)
	}
}

// A parse error is the caller's, and carries the column. A store failure is
// ours, and the body says nothing about our tables.
func TestQuery_ErrorsAreClassified(t *testing.T) {
	h, store := metricsAPI(t)
	code, out := get(t, h, "/api/v1/query?q="+urlEncode("sum:x{a:b")+window)
	if code != 400 || !strings.Contains(out["error"].(string), "col ") {
		t.Errorf("a parse error gave %d %v, want 400 with a column", code, out)
	}

	code, out = get(t, h, "/api/v1/query?q="+urlEncode("sum:http.request.count{*}")+
		"&from=1789990000&to=1790000000&interval=1")
	if code != 400 || !strings.Contains(out["error"].(string), "buckets") {
		t.Errorf("too many buckets gave %d %v, want 400", code, out)
	}

	_ = store.Close()
	code, out = get(t, h, "/api/v1/query?q="+urlEncode("sum:http.request.count{*}")+window)
	if code != 500 {
		t.Fatalf("a closed store gave %d %v, want 500", code, out)
	}
	if msg := out["error"].(string); strings.Contains(msg, "sql") || strings.Contains(msg, "database") {
		t.Errorf("the 500 body leaks the store's internals: %q", msg)
	}
}

func TestQuery_BadBodies(t *testing.T) {
	h, _ := metricsAPI(t)
	for _, body := range []string{``, `{`, `{"q":1}`, `{"nope":1}`, `[]`} {
		if code, _ := post(t, h, "/api/v1/query", body); code != 400 {
			t.Errorf("POST %q: %d, want 400", body, code)
		}
		if code, _ := post(t, h, "/api/v1/query/validate", body); code != 400 {
			t.Errorf("POST validate %q: %d, want 400", body, code)
		}
	}
}

func urlEncode(s string) string {
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '_', c == '-', c == '/', c == ':', c == '*':
			b.WriteByte(c)
		default:
			b.WriteString("%" + strings.ToUpper(hex(c)))
		}
	}
	return b.String()
}

func hex(c byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[c>>4], digits[c&0xf]})
}

// The structured parameters are translated, not reimplemented, so the shapes
// of a filter term each need to come out as the right query text.
func TestFilterTerms(t *testing.T) {
	for _, tc := range []struct {
		filter  string
		want    string
		warning bool
		err     string
	}{
		{filter: "", want: "*"},
		{filter: "env:dev", want: "env:dev"},
		{filter: " env:dev , route:/a ", want: "env:dev,route:/a"},
		{filter: "!status:5*", want: "!status:5*"},
		{filter: "env", want: "env:*", warning: true},
		{filter: "!env", want: "!env:*", warning: true},
		{filter: ":dev", err: "has no tag key"},
		{filter: "!:dev", err: "has no tag key"},
		{filter: "bad key:x", err: "is not a tag key"},
		{filter: "url:http://h:8080/p", want: "url:http://h:8080/p"},
	} {
		t.Run(tc.filter, func(t *testing.T) {
			terms, warnings, err := filterTerms(tc.filter)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err %v, want it to mention %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Join(terms, ",")
			if got == "" {
				got = "*"
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
			if (len(warnings) > 0) != tc.warning {
				t.Errorf("warnings %v, want any = %v", warnings, tc.warning)
			}
		})
	}
}

// A store that is merely slow is not a bad query, and telling the two apart
// is the difference between a caller retrying and a caller rewriting.
func TestStatusFor(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"a deadline", fmt.Errorf("reading: %w", context.DeadlineExceeded), 503},
		{"a refused query", fmt.Errorf("x: %w", eval.ErrBadQuery), 400},
		{"a parse error", &metricql.Error{Col: 1, Msg: "nope"}, 400},
		{"a store failure", errors.New("sql: database is closed"), 500},
	} {
		if got := statusFor(tc.err); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
}

// A dashboard whose template variable is cleared sends the parameter with no
// value. Binding "" would refuse the request as "not a key:value tag" — the
// one thing an "all" selection must not do.
func TestQuery_AClearedVariableMeansAll(t *testing.T) {
	h, _ := metricsAPI(t)
	for _, spelling := range []string{"&var.env=", "&var.env", "&var.env=&var.env="} {
		t.Run(spelling, func(t *testing.T) {
			code, out := get(t, h,
				"/api/v1/query?q="+urlEncode("sum:http.request.count{$env} by {route}")+window+spelling)
			if code != 200 {
				t.Fatalf("%d %v", code, out)
			}
			if n := len(seriesOf(t, out)); n != 2 {
				t.Errorf("got %d lines, want every route — an unconstrained $env is not a filter", n)
			}
			warnings := out["warnings"].([]any)
			if len(warnings) != 1 || !strings.Contains(warnings[0].(string), "env") {
				t.Errorf("warnings %v, want one saying $env constrained nothing", warnings)
			}
		})
	}
}

// The explorer re-queries every ten seconds and passes an AbortSignal, so an
// abandoned request is ordinary traffic, not a failure. A 500 and an ERROR line
// per keystroke would bury the failures that matter.
func TestQuery_AnAbandonedRequestIsNotAServerError(t *testing.T) {
	h, _ := metricsAPI(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/query?q="+urlEncode("sum:http.request.count{*}")+window, nil)
	h.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != 499 {
		t.Errorf("status %d, want 499 (the client closed the request), not a 5xx", rec.Code)
	}
}

// A cancellation that did not come from the caller is a failure, and must not
// be filed as somebody closing a tab.
func TestStatusFor_DoesNotReadCancellationAsTheClientLeaving(t *testing.T) {
	if got := statusFor(fmt.Errorf("reading: %w", context.Canceled)); got != 500 {
		t.Errorf("%d, want 500: only the handler can see whose context was cancelled", got)
	}
}

// Both spellings of the window must mean the same thing, including the epoch:
// on POST, `"from": 0` used to be indistinguishable from an absent field and
// silently became `to - 3600`.
func TestQuery_TheEpochIsAWindowNotAnAbsentField(t *testing.T) {
	h, _ := metricsAPI(t)
	code, out := post(t, h, "/api/v1/query",
		`{"q":"sum:http.request.count{*}","from":0,"to":86400,"interval":86400}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if out["from"] != 0.0 {
		t.Errorf("from %v, want 0 — an explicit epoch is a value, not a missing field", out["from"])
	}

	// And a `to` on its own still takes the default range back from it.
	code, out = post(t, h, "/api/v1/query", `{"q":"sum:http.request.count{*}","to":1790000000}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if out["from"] != float64(1790000000-3600) {
		t.Errorf("from %v, want an hour before to", out["from"])
	}
}

// A body one byte over the limit is not malformed JSON, and saying it is sends
// the caller looking for a brace that was never missing.
func TestQuery_AnOversizedBodySaysSo(t *testing.T) {
	h, _ := metricsAPI(t)
	big := `{"q":"sum:x{k:` + strings.Repeat("v", 70<<10) + `}"}`
	code, out := post(t, h, "/api/v1/query", big)
	if code != 400 {
		t.Fatalf("%d %v", code, out)
	}
	if msg := out["error"].(string); !strings.Contains(msg, "larger than") {
		t.Errorf("error %q, want it to say the body was too large", msg)
	}

	// Two objects in one body is not a request with the first one in it.
	if code, out := post(t, h, "/api/v1/query", `{"q":"sum:x{*}"} {"q":"sum:y{*}"}`); code != 400 {
		t.Errorf("%d %v, want 400 for trailing content", code, out)
	}
}

// The lexer lower-cases a tag key as it reads one, so the structured path must
// too — otherwise it refuses a request that `q=` accepts, while its comment
// claims to apply the parser's own rules.
func TestQuery_StructuredKeysAreLowerCasedLikeTheLexerDoes(t *testing.T) {
	h, _ := metricsAPI(t)
	code, out := get(t, h, "/api/v1/query?metric=http.request.count&filter=Env:dev&by=Route&agg=sum"+window)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if got := out["query"]; got != "sum:http.request.count{env:dev} by {route}" {
		t.Errorf("query %q, want the keys lower-cased", got)
	}
	if n := len(seriesOf(t, out)); n != 2 {
		t.Errorf("got %d lines, want the same answer q= would give", n)
	}
}

// sketchlessAPI is the fixture for the two conditions that are the server's
// rather than the query's: a distribution metric exists and is queryable, but
// this server has nothing to answer percentiles from.
func sketchlessAPI(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()
	store, err := naive.Open(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	md, err := meta.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = md.Close() })
	ctx := context.Background()
	if err := md.Observe(ctx, "lat", wire.KindDistribution, 10, now); err != nil {
		t.Fatal(err)
	}
	if err := md.Observe(ctx, "lat.count", wire.KindCount, 10, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(ctx, []tsdb.SeriesSamples{{
		Series:  tsdb.NewSeriesRef("lat.count", []string{"host:a"}),
		Samples: []tsdb.Sample{{T: (now.Unix() - 10) * 1000, V: 5}},
	}}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&Metrics{Store: store, Types: md, Clock: testutil.NewFakeClock(now)}).Register(mux)
	return mux
}

// A server with no sketch store cannot answer a percentile, and that is its
// own shortcoming rather than a bad request: the same query on a server that
// has one is correct. 503 says "not here, not now"; 400 would say "never".
func TestQuery_APercentileWithNoSketchStoreIs503(t *testing.T) {
	h := sketchlessAPI(t)
	code, out := get(t, h, "/api/v1/query?q="+urlEncode("p95:lat{*}")+window)
	if code != 503 {
		t.Fatalf("%d %v, want 503", code, out)
	}
	if msg := out["error"].(string); !strings.Contains(msg, "sketch store") {
		t.Errorf("error %q, want it to name what is missing", msg)
	}
}

// The lexer folds a `$Name`, so the request's binding has to fold too or the
// two never meet. Both verbs, because they read the variables from different
// places.
func TestQuery_VariableNamesAreCaseInsensitive(t *testing.T) {
	h, _ := metricsAPI(t)
	q := urlEncode("sum:http.request.count{$Env} by {route}")

	// GET: the parameter is capitalised, the query is not, and vice versa.
	for _, param := range []string{"&var.Env=env:dev", "&var.env=env:dev", "&var.ENV=env:dev"} {
		code, out := get(t, h, "/api/v1/query?q="+q+window+param)
		if code != 200 {
			t.Fatalf("%s: %d %v", param, code, out)
		}
		if n := len(seriesOf(t, out)); n != 2 {
			t.Errorf("%s: got %d lines, want 2 — the binding did not reach the query", param, n)
		}
	}

	// POST: the JSON object's keys fold the same way.
	code, out := post(t, h, "/api/v1/query", `{
		"q": "sum:http.request.count{$env} by {route}",
		"from": 1789999980, "to": 1790000000, "interval": 20,
		"vars": {"ENV": ["env:dev"]}
	}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if n := len(seriesOf(t, out)); n != 2 {
		t.Errorf("POST: got %d lines, want 2", n)
	}
}
