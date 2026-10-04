package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

// The dashboards API's whole bodies, pinned the way TestQuery_Golden pins the
// query API's (L7: dashboard JSON schema goldens).
//
// A dashboard is a stored document that the UI renders, exports and imports, so
// its JSON shape is a contract with every saved row and every exported file: a
// field that stops being emitted, a null that becomes "", a timestamp that
// changes format. The unit tests in dashboards_test.go assert one property each;
// these assert all of it. The requests run in order against one handler, because
// a dashboard has to exist before it can be read, listed or updated.
//
// The clock moves a minute between the create and the update, so the update's
// golden shows `updated_at` advancing while `created_at` stays: with a frozen
// clock the two are equal and a handler that stopped bumping `updated_at`, or
// overwrote `created_at`, would still match. Timestamps are UTC (docs/api.md),
// so the files are the same in every zone the suite runs in.
func TestDashboards_Golden(t *testing.T) {
	db, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clock := testutil.NewFakeClock(now)
	mux := http.NewServeMux()
	(&Dashboards{Store: db, Clock: clock}).Register(mux)
	var h http.Handler = mux
	steps := []struct{ name, method, path, body string }{
		{"dashboards-create", http.MethodPost, "/api/v1/dashboards", definition},
		{"dashboards-get", http.MethodGet, "/api/v1/dashboards/1", ""},
		{"dashboards-list", http.MethodGet, "/api/v1/dashboards", ""},
		{"dashboards-update", http.MethodPut, "/api/v1/dashboards/1",
			`{"title":"Checkout v2","widgets":[{"id":"w1","type":"timeseries","title":"errors",
				"layout":{"x":0,"y":0,"w":12,"h":4},
				"queries":[{"q":"sum:http.request.count{status:5*}.as_rate()","display":"bars"}]}]}`},
		{"dashboards-invalid-query", http.MethodPost, "/api/v1/dashboards",
			`{"title":"Broken","widgets":[{"id":"w1","type":"timeseries","title":"x",
				"layout":{"x":0,"y":0,"w":6,"h":3},"queries":[{"q":"sum:a{b:c by {k}"}]}]}`},
		{"dashboards-missing", http.MethodGet, "/api/v1/dashboards/999", ""},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			if s.name == "dashboards-update" {
				clock.Advance(time.Minute)
			}
			rec, _ := send(t, h, s.method, s.path, s.body)
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
			path := filepath.Join("testdata", "golden", s.name+".json")
			if *updateGolden {
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
