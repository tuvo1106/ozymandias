package monitor

import (
	"context"
	"math"
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

// shippedErrorRate is one shipped 5xx monitor and the service it selects.
type shippedErrorRate struct {
	c       *spec.Compiled
	service string
}

// shippedErrorRates provisions deploy/monitors/ the way ozyd does and returns
// every monitor whose query selects 5xx statuses, by uid. Found by what the
// query is, not by file name, so no app is named here
// (docs/plan/extensibility.md).
func shippedErrorRates(t *testing.T) map[string]shippedErrorRate {
	t.Helper()
	db, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	dir := filepath.Join("..", "..", "deploy", "monitors")
	if res := Provision(context.Background(), provisionDB{db}, []string{dir}, []string{"log", "hook"}, t0, nil); res.Failed != 0 {
		t.Fatalf("provisioning: %+v", res)
	}
	rows, err := db.Monitors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	service := regexp.MustCompile(`service:([^,}]+)`)
	out := map[string]shippedErrorRate{}
	for _, r := range rows {
		if !strings.Contains(r.Query, "status:5*") {
			continue
		}
		c, err := CompileRow(r)
		if err != nil {
			t.Fatal(err)
		}
		sm := service.FindStringSubmatch(r.Query)
		if sm == nil {
			t.Fatalf("%s: an error-rate monitor with no service selector", r.UID)
		}
		out[r.UID] = shippedErrorRate{c, sm[1]}
	}
	if len(out) == 0 {
		t.Fatal("no shipped error-rate monitor found")
	}
	return out
}

// TestShippedErrorRatesReadWholeRatios evaluates every shipped 5xx monitor on
// synthetic traffic. A window reduction applies to the expression's
// per-bucket values, so `sum(last_5m)` of a ratio on ten-second buckets adds
// thirty percentages: 1% errors read as 30. The shipped monitors roll both
// terms up to two-minute buckets, so each bucket is one ratio, and take the
// max over the last three, so a burst of three minutes or more fills a whole
// bucket wherever the bucket boundaries fall. The direct ratio, unlike its
// complement, reads 100 when every request fails rather than nothing
// (docs/dashboards.md).
func TestShippedErrorRatesReadWholeRatios(t *testing.T) {
	for uid, m := range shippedErrorRates(t) {
		t.Run(uid, func(t *testing.T) { checkErrorRate(t, m.c, m.service) })
	}
}

func checkErrorRate(t *testing.T, c *spec.Compiled, svc string) {
	const end = 10 * time.Minute // evaluated at t0+10m
	steady := func(n float64) func(time.Duration) float64 { return func(time.Duration) float64 { return n } }
	for _, tc := range []struct {
		name     string
		ok, fail func(time.Duration) float64 // requests per ten seconds
		want     float64                     // NaN: no group at all
	}{
		{"one percent", steady(99), steady(1), 1},
		{"total outage", steady(0), steady(5), 100},
		// No 5xx series means no value, which a group that never had data
		// does not turn into NO_DATA (state/doc.go): it simply never alerts.
		{"healthy", steady(100), steady(0), math.NaN()},
		// 5 failed per 100 good for three minutes, straddling 12:05:00, the boundary five-minute
		// buckets would split it on (each half then reads 1.5%).
		{"burst across a boundary", steady(100), func(off time.Duration) float64 {
			if off >= 3*time.Minute+30*time.Second && off < 6*time.Minute+30*time.Second {
				return 5
			}
			return 0
		}, 100 * 5.0 / 105},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			for _, s := range []struct {
				status string
				n      func(time.Duration) float64
			}{{"200", tc.ok}, {"500", tc.fail}} {
				n := s.n
				h.points("http.request.count", []string{"service:" + svc, "status:" + s.status}, 0, end, func(off time.Duration) float64 {
					if v := n(off); v > 0 {
						return v
					}
					return math.NaN() // no requests of this status: no point, as the agent sends
				})
			}
			vals, _, err := h.ev.Evaluate(context.Background(), c, t0.Add(9*time.Minute))
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
