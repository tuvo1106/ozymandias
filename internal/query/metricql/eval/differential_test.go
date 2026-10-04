package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/tuvo1106/ozymandias/internal/query/metricql"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/db"
	"github.com/tuvo1106/ozymandias/internal/tsdb/naive"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// L3: the evaluator must give the same answer over the TSDB as over the naive
// store, for random data and random queries.
//
// M2's TestDB_MatchesTheNaiveStore already argues the two are indistinguishable
// through [tsdb.MetricStore], and the evaluator reads only through that
// interface, so the evaluator's answers *should* agree by transitivity. This
// asks the question directly instead of relying on the argument: a query
// reaches the store as a Selector with a post-filter and a window, and the
// evaluator pushes some of its filtering down (ADR-0018's selection cache keys
// on it) and does the rest itself, which is a seam the store-level test cannot
// see. If the two stores ever differ in what they return for a selector the
// evaluator builds, this is where it shows.
//
// The samples are integer-valued, in order and unique: the stores are known to
// differ on out-of-order and duplicate samples (ADR-0011, TestDB_MatchesTheNaiveStore),
// and summing integers is exact in any order, so no difference here can be
// float addition order or a known store boundary.
func TestEval_SameAnswerOverTheTSDBAndTheNaiveStore(t *testing.T) {
	root := t.TempDir()
	var iteration int
	base := int64(1_790_000_000)
	hosts := []string{"a", "b", "c"}
	routes := []string{"x", "y"}

	var compared, nonEmpty int
	rapid.Check(t, func(t *rapid.T) {
		iteration++
		dir := filepath.Join(root, fmt.Sprintf("run%d", iteration))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		tsdbStore, err := db.Open(db.Options{Dir: dir, Retention: -1, Clock: testutil.NewFakeClock(time.Unix(base, 0))})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tsdbStore.Close() }()
		oracle, err := naive.Open(filepath.Join(dir, "oracle.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = oracle.Close() }()

		// Random series: a subset of host x route, each with random points over
		// the last ten minutes, at ten-second steps with random gaps.
		var data []tsdb.SeriesSamples
		for _, h := range hosts {
			for _, r := range routes {
				if !rapid.Bool().Draw(t, "has "+h+r) {
					continue
				}
				var smp []tsdb.Sample
				for step := int64(0); step < 60; step++ {
					if rapid.IntRange(0, 3).Draw(t, fmt.Sprintf("gap %s%s%d", h, r, step)) == 0 {
						continue
					}
					smp = append(smp, tsdb.Sample{
						T: (base - 600 + step*10) * 1000,
						V: float64(rapid.IntRange(0, 1000).Draw(t, fmt.Sprintf("v %s%s%d", h, r, step))),
					})
				}
				if len(smp) > 0 {
					data = append(data, tsdb.SeriesSamples{
						Series:  tsdb.NewSeriesRef("m", []string{"host:" + h, "route:" + r, "env:dev"}),
						Samples: smp,
					})
				}
			}
		}
		for _, store := range []tsdb.MetricStore{tsdbStore, oracle} {
			for _, s := range data {
				cp := tsdb.SeriesSamples{Series: s.Series, Samples: append([]tsdb.Sample(nil), s.Samples...)}
				if _, err := store.Append(context.Background(), []tsdb.SeriesSamples{cp}); err != nil {
					t.Fatalf("append: %v", err)
				}
			}
		}

		eReal := &Evaluator{Store: tsdbStore, Types: types{"m": wire.KindCount}, Timeout: -1}
		eOracle := &Evaluator{Store: oracle, Types: types{"m": wire.KindCount}, Timeout: -1}

		for q := 0; q < 6; q++ {
			text := drawQuery(t, q)
			expr, err := metricql.Parse(text)
			if err != nil {
				t.Fatalf("generated an unparseable query %q: %v", text, err)
			}
			interval := rapid.SampledFrom([]int64{10, 20, 60}).Draw(t, fmt.Sprintf("interval%d", q))
			from := base - 600 + rapid.Int64Range(-30, 300).Draw(t, fmt.Sprintf("from%d", q))
			to := from + rapid.Int64Range(10, 400).Draw(t, fmt.Sprintf("span%d", q))
			req := Request{Expr: expr, From: from, To: to, Interval: interval}

			got, gotErr := eReal.Eval(context.Background(), req)
			want, wantErr := eOracle.Eval(context.Background(), req)
			if (gotErr == nil) != (wantErr == nil) || (gotErr != nil && gotErr.Error() != wantErr.Error()) {
				t.Fatalf("%s\nerrors differ: tsdb %v, naive %v", text, gotErr, wantErr)
			}
			if gotErr != nil {
				continue
			}
			compared++
			if len(got.Series) > 0 {
				nonEmpty++
			}
			g, w := sortedLines(got), sortedLines(want)
			if strings.Join(g, "\n") != strings.Join(w, "\n") {
				t.Fatalf("%s  (from %d to %d every %ds)\ntsdb\n  %s\nnaive\n  %s",
					text, from, to, interval, strings.Join(g, "\n  "), strings.Join(w, "\n  "))
			}
		}
	})

	// Agreement on nothing is not agreement. Most generated cases must have had a
	// real answer to compare, or this has stopped testing the evaluator.
	if compared == 0 || nonEmpty*2 < compared {
		t.Fatalf("only %d of %d compared answers had any line in them", nonEmpty, compared)
	}
	t.Logf("%d answers compared, %d non-empty", compared, nonEmpty)
}

func sortedLines(r Result) []string {
	out := lines(r)
	sort.Strings(out)
	return out
}

// drawQuery draws a query from a small grammar that reaches the parts of the
// evaluator the stores can influence: every aggregator, filters that push down
// (equality, negation, IN, two terms) and one that cannot match anything,
// groupings including one that does not exist, and each modifier.
func drawQuery(t *rapid.T, i int) string {
	agg := rapid.SampledFrom([]string{"avg", "sum", "min", "max", "count"}).Draw(t, fmt.Sprintf("agg%d", i))
	filter := rapid.SampledFrom([]string{
		"*", "host:a", "!host:a", "route:x", "host IN (a,b)", "host:a,route:y", "host:zzz", "route:x,!host:b", "host:*",
	}).Draw(t, fmt.Sprintf("filter%d", i))
	by := rapid.SampledFrom([]string{
		"", " by {host}", " by {route}", " by {host,route}", " by {nonexistent}",
	}).Draw(t, fmt.Sprintf("by%d", i))
	mod := rapid.SampledFrom([]string{
		"", ".as_rate()", ".as_count()", ".rollup(max)", ".fill(zero)", ".as_rate().fill(last)",
	}).Draw(t, fmt.Sprintf("mod%d", i))
	q := fmt.Sprintf("%s:m{%s}%s%s", agg, filter, by, mod)
	// A ratio of two selections of the same metric, the shape an error-rate
	// widget has, so the join's group matching is compared too.
	if rapid.IntRange(0, 4).Draw(t, fmt.Sprintf("ratio%d", i)) == 0 {
		q = fmt.Sprintf("sum:m{route:x}%s / sum:m{*}%s * 100", by, by)
	}
	return q
}
