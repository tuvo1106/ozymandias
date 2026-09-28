package dashboard

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/query/metricql"
)

// The dashboards ozymandias ships in deploy/dashboards/.
//
// These need a test more than a hand-written one does, because provisioning is
// deliberately forgiving: a definition that does not validate is logged and
// *skipped*, so a broken shipped dashboard does not fail startup, does not fail
// smoke, and simply is not there. Nothing else would catch it.
func TestShippedDashboards(t *testing.T) {
	dir := filepath.Join("..", "..", "deploy", "dashboards")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var found int
	for _, e := range entries {
		// The same predicate provisioning uses (see jsonFiles), not a
		// case-sensitive one: a foo.JSON would otherwise be provisioned and
		// never validated, which is precisely the hole this test exists to
		// close.
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".json") {
			continue
		}
		found++
		t.Run(e.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			d, err := Parse(data)
			if err != nil {
				t.Fatalf("does not validate, so provisioning would skip it silently: %v", err)
			}
			// Provisioning refuses a definition with no uid, again by logging
			// and skipping. Same reasoning.
			if d.UID == "" {
				t.Error(`no "uid", so provisioning would skip it`)
			}
			for _, w := range d.Widgets {
				for _, q := range w.Queries {
					assertMetricsExist(t, w.ID, q.Q)
				}
			}
			if !d.Template {
				return
			}
			// A shipped template is served through
			// /api/v1/dashboards/service/<name>, and an instance that does not
			// validate is one the UI cannot offer to save a copy of. Checked
			// here rather than only in template_test.go, because this is the
			// definition that actually ships.
			inst, err := d.Instantiate("checkout")
			if err != nil {
				t.Fatalf("it is a template and will not instantiate: %v", err)
			}
			if err := inst.Validate(); err != nil {
				t.Errorf("its instance does not validate: %v", err)
			}
			if _, err := d.Metrics(); err != nil {
				t.Errorf("its metrics cannot be read, so it discovers no services: %v", err)
			}
		})
	}
	if found == 0 {
		t.Fatal("no dashboards found; this test has stopped checking anything")
	}
}

// selfMetric matches the ozy.* names in the emitting code, which is the same
// pattern scripts/check-docs.sh uses against the catalogue.
var selfMetric = regexp.MustCompile(`(?:Counter|Gauge|GaugeFunc)\("(ozy\.[a-z0-9_.]+)"`)

// assertMetricsExist checks that a shipped dashboard asks for metrics this
// build actually emits.
//
// A dashboard naming a metric nobody writes is *valid* — that is on purpose,
// since a dashboard is often written before the service ships (see the package
// comment). But one that ozymandias ships to describe itself has no such
// excuse: every metric in it is one of ozyd's or the agent's own, so a name
// that has drifted is a widget that is permanently blank. The self-metrics were
// renamed once already (ozymandias.* -> ozy.*), which is exactly when this
// breaks.
func assertMetricsExist(t *testing.T, widget, query string) {
	t.Helper()
	emitted := emittedSelfMetrics(t)
	expr, err := metricql.Parse(query)
	if err != nil {
		t.Fatalf("%s: %v", widget, err)
	}
	metricql.Walk(expr, func(n metricql.Node) {
		q, ok := n.(*metricql.Query)
		if !ok || !strings.HasPrefix(q.Metric, "ozy.") {
			return
		}
		// A percentile selects on <metric>.count, and the four derived series
		// of a distribution are not registered names either.
		name := strings.TrimSuffix(q.Metric, ".count")
		for _, suffix := range []string{".sum", ".max", ".min", ".avg"} {
			name = strings.TrimSuffix(name, suffix)
		}
		if !emitted[name] && !emitted[q.Metric] {
			t.Errorf("%s asks for %q, which nothing in this build emits — the widget would be blank forever",
				widget, q.Metric)
		}
	})
}

var selfMetricsCache map[string]bool

func emittedSelfMetrics(t *testing.T) map[string]bool {
	t.Helper()
	if selfMetricsCache != nil {
		return selfMetricsCache
	}
	out := map[string]bool{}
	for _, root := range []string{filepath.Join("..", ".."), filepath.Join("..", "..", "cmd")} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			// Skip the web tree's vendored Go package (see AGENTS.md §7).
			if strings.Contains(path, "node_modules") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			for _, m := range selfMetric.FindAllStringSubmatch(string(data), -1) {
				out[m[1]] = true
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(out) == 0 {
		t.Fatal("no self-metrics matched: this test's pattern has drifted from the code")
	}
	selfMetricsCache = out
	return out
}
