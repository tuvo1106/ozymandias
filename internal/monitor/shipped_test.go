package monitor

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/monitor/spec"
)

// TestShippedMonitorsLoad: every file in deploy/monitors/ provisions, and
// mentions only notifiers the reference config has. A shipped monitor that
// fails to load is one nobody will know is missing.
func TestShippedMonitorsLoad(t *testing.T) {
	db, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	dir := filepath.Join("..", "..", "deploy", "monitors")
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	res := Provision(context.Background(), provisionDB{db}, []string{dir}, []string{"log", "hook"}, t0, nil)
	if res.Failed != 0 || res.Loaded != len(files) || len(files) == 0 {
		t.Fatalf("%+v of %d files", res, len(files))
	}
	rows, _ := db.Monitors(context.Background())
	for _, r := range rows {
		c, err := CompileRow(r)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range c.Template.Recipients() {
			if m != "log" && m != "hook" {
				t.Errorf("%s mentions @%s, which deploy/ozyd.yaml does not configure", r.UID, m)
			}
		}
	}
}

// shippedErrorRates compiles every shipped monitor whose query selects 5xx
// statuses, with the service its selector names. Found by what the query is,
// not by file name, so no app is named here (docs/plan/extensibility.md).
func shippedErrorRates(t *testing.T) map[string]*spec.Compiled {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join("..", "..", "deploy", "monitors", "*.json"))
	service := regexp.MustCompile(`service:([^,}]+)`)
	out := map[string]*spec.Compiled{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var m spec.Monitor
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(m.Query, "status:5*") {
			continue
		}
		c, err := spec.Compile(m)
		if err != nil {
			t.Fatal(err)
		}
		sm := service.FindStringSubmatch(m.Query)
		if sm == nil {
			t.Fatalf("%s: an error-rate monitor with no service selector", m.UID)
		}
		out[sm[1]] = c
	}
	if len(out) == 0 {
		t.Fatal("no shipped error-rate monitor found")
	}
	return out
}

// TestShippedErrorRatesAreOneRatioOverTheWindow evaluates every shipped 5xx
// monitor on synthetic traffic. A window reduction applies to the expression's
// per-bucket values, so `sum(last_5m)` of a ratio on ten-second buckets adds
// thirty percentages: 1% errors read as 30. Rolling both terms up to the
// window makes one bucket and one ratio. And the direct ratio, unlike its
// complement, reads 100 when every request fails rather than nothing
// (docs/dashboards.md).
func TestShippedErrorRatesAreOneRatioOverTheWindow(t *testing.T) {
	for svc, c := range shippedErrorRates(t) {
		t.Run(svc, func(t *testing.T) { checkErrorRate(t, c, svc) })
	}
}

func checkErrorRate(t *testing.T, c *spec.Compiled, svc string) {
	for _, tc := range []struct {
		name     string
		ok, fail float64 // requests per ten seconds
		want     float64 // NaN: no group at all
	}{
		{"one percent", 99, 1, 1},
		{"total outage", 0, 5, 100},
		// No 5xx series means no value, which a group that never had data
		// does not turn into NO_DATA (state/doc.go): it simply never alerts.
		{"healthy", 100, 0, math.NaN()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			// Ten minutes of traffic, so the window's alignment cannot matter.
			for _, s := range []struct {
				status string
				n      float64
			}{{"200", tc.ok}, {"500", tc.fail}} {
				if s.n > 0 {
					n := s.n
					h.points("http.request.count", []string{"service:" + svc, "status:" + s.status}, 0, 10*time.Minute,
						func(time.Duration) float64 { return n })
				}
			}
			vals, _, err := h.ev.Evaluate(context.Background(), c, t0.Add(10*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if math.IsNaN(tc.want) {
				if len(vals) != 0 {
					t.Fatalf("got %+v, want no group", vals)
				}
				return
			}
			if len(vals) != 1 || !vals[0].Present || math.Abs(vals[0].Value-tc.want) > 1e-9 {
				t.Fatalf("got %+v, want one group at %g", vals, tc.want)
			}
		})
	}
}
