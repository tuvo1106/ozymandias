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
	for _, r := range shippedRows(t) {
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

// shippedRows provisions deploy/monitors/ the way ozyd does, with the
// notifiers deploy/ozyd.yaml configures, and returns the stored rows. Every
// file must load: a shipped monitor that fails is one nobody will know is
// missing.
func shippedRows(t *testing.T) []meta.MonitorRow {
	t.Helper()
	db, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	dir := filepath.Join("..", "..", "deploy", "monitors")
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	res := Provision(context.Background(), provisionDB{db}, []string{dir}, []string{"log", "hook"}, t0, nil)
	if res.Failed != 0 || res.Loaded != len(files) || len(files) == 0 {
		t.Fatalf("%+v of %d files", res, len(files))
	}
	rows, err := db.Monitors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// shippedErrorRate is one shipped 5xx monitor and the service it selects.
type shippedErrorRate struct {
	c       *spec.Compiled
	service string
}

// shippedErrorRates returns every shipped monitor whose query selects 5xx
// statuses, by uid. Found by what the
// query is, not by file name, so no app is named here
// (docs/plan/extensibility.md).
func shippedErrorRates(t *testing.T) map[string]shippedErrorRate {
	t.Helper()
	rows := shippedRows(t)
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
// max over the last three, so a burst of four minutes or more fills a whole
// bucket wherever the bucket boundaries fall (swept below). The direct ratio, unlike its
// complement, reads 100 when every request fails rather than nothing
// (docs/dashboards.md).
func TestShippedErrorRatesReadWholeRatios(t *testing.T) {
	for uid, m := range shippedErrorRates(t) {
		t.Run(uid, func(t *testing.T) { checkErrorRate(t, m.c, m.service) })
	}
}

func checkErrorRate(t *testing.T, c *spec.Compiled, svc string) {
	// Rolled-up buckets are epoch-aligned, so where a burst falls decides how
	// it is split. Swept over every ten-second offset across a whole bucket: a
	// burst of twice the bucket width (four minutes) always fills one bucket
	// and reads its full rate; a burst of one to two widths always puts at
	// least half a bucket's worth into one, so reads at least half its rate.
	full := 100 * 5.0 / 105
	for _, burst := range []struct {
		length  time.Duration
		atLeast float64
	}{{4 * time.Minute, full}, {3 * time.Minute, full / 2}, {2 * time.Minute, full / 2}} {
		for off := time.Duration(0); off < 2*time.Minute; off += 10 * time.Second {
			start := 4*time.Minute + off
			h := newHarness(t)
			h.points("http.request.count", []string{"env:dev", "service:" + svc, "status:200"}, 0, 20*time.Minute,
				func(time.Duration) float64 { return 100 })
			h.points("http.request.count", []string{"env:dev", "service:" + svc, "status:500"}, start, start+burst.length,
				func(time.Duration) float64 { return 5 })
			// Late enough that the window, which ends 30 s and up to one bucket
			// before now, has closed over the whole burst; early enough that it
			// still starts before it.
			now := start + burst.length + 2*time.Minute + 30*time.Second
			vals, _, err := h.ev.Evaluate(context.Background(), c, t0.Add(now))
			if err != nil {
				t.Fatal(err)
			}
			if len(vals) != 1 || vals[0].Value < burst.atLeast-1e-9 {
				t.Fatalf("a %v burst at +%v: got %+v, want at least %.2f", burst.length, start, vals, burst.atLeast)
			}
			if burst.length == 4*time.Minute && math.Abs(vals[0].Value-full) > 1e-9 {
				t.Fatalf("a four-minute burst at +%v reads %g, want its full rate %g", start, vals[0].Value, full)
			}
		}
	}

	// One environment's errors are not diluted by another's traffic: a prod
	// outage under a busy dev stack still reads as prod's own ratio.
	t.Run("environments apart", func(t *testing.T) {
		h := newHarness(t)
		for _, s := range []struct {
			tags []string
			n    float64
		}{
			{[]string{"env:dev", "service:" + svc, "status:200"}, 1000},
			{[]string{"env:prod", "service:" + svc, "status:200"}, 95},
			{[]string{"env:prod", "service:" + svc, "status:500"}, 5},
		} {
			n := s.n
			h.points("http.request.count", s.tags, 0, 10*time.Minute, func(time.Duration) float64 { return n })
		}
		vals, _, err := h.ev.Evaluate(context.Background(), c, t0.Add(9*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if len(vals) != 1 || vals[0].Group.String() != "env:prod" || math.Abs(vals[0].Value-5) > 1e-9 {
			t.Fatalf("got %+v, want env:prod alone at 5", vals)
		}
	})

	const end = 10 * time.Minute // traffic runs to t0+10m; evaluated at t0+9m
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			for _, s := range []struct {
				status string
				n      func(time.Duration) float64
			}{{"200", tc.ok}, {"500", tc.fail}} {
				n := s.n
				h.points("http.request.count", []string{"env:dev", "service:" + svc, "status:" + s.status}, 0, end, func(off time.Duration) float64 {
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

// TestShippedMessagesNameTheirGroupAndRecover renders every shipped monitor's
// message for each transition it can make, with a group built from its own
// `by` clause. A message must name the environment it is about (two stacks
// reporting to one ozyd are otherwise indistinguishable), print group values
// rather than raw keys, carry no template artifacts, and not send the alert's
// words as the recovery.
func TestShippedMessagesNameTheirGroupAndRecover(t *testing.T) {
	byClause := regexp.MustCompile(`by \{([^}]*)\}|\.by\("([^"]+)"\)`)
	for _, r := range shippedRows(t) {
		c, err := CompileRow(r)
		if err != nil {
			t.Fatal(err)
		}
		group := spec.Group{}
		for _, m := range byClause.FindAllStringSubmatch(r.Query, -1) {
			for _, k := range strings.Split(m[1]+m[2], ",") {
				if k = strings.TrimSpace(k); k != "" {
					group[k] = "val-" + strings.TrimPrefix(k, "@")
				}
			}
		}
		render := func(d spec.Data) string {
			t.Helper()
			d.Group, d.Monitor = group, spec.Ref{Name: r.Name}
			out, err := c.Template.Render(d)
			if err != nil {
				t.Fatalf("%s: %v", r.UID, err)
			}
			return out
		}
		cases := map[string]spec.Data{
			"alert":    {Status: "ALERT", PrevStatus: "OK", Value: 42, Threshold: 20, IsAlert: true},
			"recovery": {Status: "OK", PrevStatus: "ALERT", Value: 1, Threshold: 20, IsRecovery: true},
		}
		if c.Options.NotifyNoData {
			cases["no data"] = spec.Data{Status: "NO_DATA", PrevStatus: "OK", Value: math.NaN(), IsNoData: true}
		}
		for name, d := range cases {
			out := render(d)
			for _, bad := range []string{"<no value>", "%!", "NaN"} {
				if strings.Contains(out, bad) {
					t.Errorf("%s %s: %q contains %q", r.UID, name, out, bad)
				}
			}
			for k, v := range group {
				if !strings.Contains(out, v) {
					t.Errorf("%s %s: %q does not name its %s", r.UID, name, out, k)
				}
				if strings.Contains(out, k+":"+v) {
					t.Errorf("%s %s: %q prints the raw key %s", r.UID, name, out, k)
				}
			}
		}
		if render(cases["alert"]) == strings.Replace(render(cases["recovery"]), "1", "42", 1) {
			t.Errorf("%s: the recovery is the alert's message", r.UID)
		}
	}
}
