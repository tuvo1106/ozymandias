package api

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite the committed golden files")

// goldenCases are requests whose exact JSON is committed under testdata/golden.
//
// The unit tests above each assert one thing about a response, which is what
// makes them readable and what makes them miss the rest: a field that quietly
// stops being emitted, a number that changes shape, a null that becomes a
// zero. The whole body is the contract the UI and every SDK read, so the whole
// body is what is pinned. A diff here is not automatically a failure — it is
// the question "did you mean to change the API?", asked at the only moment
// somebody can still answer it.
var goldenCases = []struct{ name, method, path, body string }{
	{"expression", "GET", "/api/v1/query?q=sum%3Ahttp.request.count%7B*%7D+by+%7Broute%7D" + window, ""},
	{"ungrouped", "GET", "/api/v1/query?q=sum%3Ahttp.request.count%7B*%7D" + window, ""},
	{"rate", "GET", "/api/v1/query?q=sum%3Ahttp.request.count%7B*%7D.as_rate%28%29" + window, ""},
	{"arithmetic-drops-a-group", "GET",
		"/api/v1/query?q=" + urlEncode("sum:http.request.count{route:/a} by {route} / sum:http.request.count{*} by {route}") + window, ""},
	{"structured-parameters", "GET",
		"/api/v1/query?metric=http.request.count&filter=env:dev&by=route&agg=sum" + window, ""},
	{"structured-bare-tag-is-widened", "GET",
		"/api/v1/query?metric=http.request.count&filter=env&agg=sum" + window, ""},
	{"post-with-a-variable", "POST", "/api/v1/query", `{
		"q": "sum:http.request.count{$env} by {route}",
		"from": 1789999980, "to": 1790000000, "interval": 20,
		"vars": {"env": ["env:dev"]}
	}`},
	{"empty-result", "GET", "/api/v1/query?q=" + urlEncode("sum:http.request.count{route:/nothing}") + window, ""},
	{"parse-error", "GET", "/api/v1/query?q=" + urlEncode("sum:http.request.count{a:b by {k}") + window, ""},
	{"refused-percentile", "GET", "/api/v1/query?q=" + urlEncode("p95:http.request.count{*}") + window, ""},
	{"validate-ok", "POST", "/api/v1/query/validate", `{"q":"SUM:x{ a : b } BY {K}"}`},
	{"validate-error", "POST", "/api/v1/query/validate", `{"q":"sum:x{a:b by {k}"}`},
}

func TestQuery_Golden(t *testing.T) {
	h, _ := metricsAPI(t)
	for _, tc := range goldenCases {
		t.Run(tc.name, func(t *testing.T) {
			var body *strings.Reader
			if tc.body == "" {
				body = strings.NewReader("")
			} else {
				body = strings.NewReader(tc.body)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, body))

			// Indented, so a diff points at the field that moved rather than
			// at one very long line.
			var pretty bytes.Buffer
			pretty.WriteString(http.StatusText(rec.Code) + "\n")
			var v any
			if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
				t.Fatalf("body %q is not JSON", rec.Body.String())
			}
			enc := json.NewEncoder(&pretty)
			enc.SetIndent("", "  ")
			if err := enc.Encode(v); err != nil {
				t.Fatal(err)
			}

			path := filepath.Join("testdata", "golden", tc.name+".json")
			if *updateGolden {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Logf("wrote %s", path)
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v — regenerate with: go test ./internal/api -run Golden -update-golden", err)
			}
			if !bytes.Equal(want, pretty.Bytes()) {
				t.Errorf("the response no longer matches %s.\n--- committed ---\n%s\n--- now ---\n%s\n"+
					"If the change is intended, regenerate with -update-golden and say in the PR what moved.",
					path, want, pretty.String())
			}
		})
	}
}
