package db

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/naive"
)

// TestDB_MatchesTheNaiveStore is M2's central correctness argument.
//
// The naive store is a thousand lines of SQL and straight-line Go: one row per
// sample, selection by scanning a metric's series and applying
// [tsdb.Matcher]'s reference semantics. The TSDB is Gorilla chunks, a
// write-ahead log, an inverted index, immutable blocks and a merge across all
// of them. They are supposed to be indistinguishable through
// [tsdb.MetricStore], and the only way to believe that is to ask them the same
// random questions and compare every answer.
//
// The operations include the ones the interface is most likely to differ on:
// out-of-order and duplicate samples (ADR-0011), batches mixing good and bad
// series, and queries whose windows fall between samples. It finishes with a
// restart, because what a store returns after a reopen is part of its
// contract too.
//
// Block cuts are deliberately *not* in the operation set. Once a range is
// written to a block the TSDB refuses samples older than it — the data is in
// an immutable file — and the naive store has no blocks and so no such
// boundary. That is a real difference in what the two can promise, not a bug
// in either, and hiding it by teaching the oracle about block ranges would
// make the oracle a model of the TSDB instead of an independent check.
// TestDB_AQueryDoesNotNoticeTheCut covers the cut directly instead.
func TestDB_MatchesTheNaiveStore(t *testing.T) {
	root := t.TempDir()
	var run int
	rapid.Check(t, func(t *rapid.T) {
		run++
		dir := filepath.Join(root, fmt.Sprintf("run%d", run))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		fake := testutil.NewFakeClock(epoch)
		real, err := Open(Options{
			Dir:        dir,
			BlockRange: 10 * time.Second, // small, so cuts actually happen
			Retention:  -1,               // retention is tested on its own
			Clock:      fake,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = real.Close() }()

		oracle, err := naive.Open(filepath.Join(dir, "oracle.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = oracle.Close() }()

		base := epoch.UnixMilli()
		for step := 0; step < rapid.IntRange(1, 12).Draw(t, "steps"); step++ {
			switch rapid.SampledFrom([]string{"append", "query", "metadata"}).
				Draw(t, fmt.Sprintf("op%d", step)) {
			case "append":
				batch := drawBatch(t, base, step)
				gotRes, gotErr := real.Append(ctx, cloneBatch(batch))
				wantRes, wantErr := oracle.Append(ctx, cloneBatch(batch))
				if (gotErr == nil) != (wantErr == nil) {
					t.Fatalf("Append errors differ: tsdb %v, naive %v", gotErr, wantErr)
				}
				compareAppend(t, gotRes, wantRes)

			case "query":
				sel := drawSelector(t, step)
				from := base + rapid.Int64Range(-5_000, 40_000).Draw(t, fmt.Sprintf("from%d", step))
				to := from + rapid.Int64Range(0, 40_000).Draw(t, fmt.Sprintf("span%d", step))
				got := selectAll(t, real, sel, from, to)
				want := selectAll(t, oracle, sel, from, to)
				if got != want {
					t.Fatalf("Select(%+v, %d, %d)\ntsdb  %s\nnaive %s", sel, from, to, got, want)
				}

			case "metadata":
				compareStrings(t, "MetricNames",
					mustStrings(t, func() ([]string, error) { return real.MetricNames(ctx, "", 0) }),
					mustStrings(t, func() ([]string, error) { return oracle.MetricNames(ctx, "", 0) }))
				// Only real metric names: the two stores disagree on what an
				// empty metric means (the TSDB reads it as "no restriction",
				// the naive store as an exact match on ""), and the HTTP layer
				// rejects it before either store sees it. Left unspecified
				// rather than settled under a differential test's thumb; see
				// the M2 notes.
				for _, m := range []string{"a.count", "b.gauge"} {
					compareStrings(t, "TagKeys("+m+")",
						mustStrings(t, func() ([]string, error) { return real.TagKeys(ctx, m) }),
						mustStrings(t, func() ([]string, error) { return oracle.TagKeys(ctx, m) }))
					for _, k := range []string{"env", "host", "nope"} {
						compareStrings(t, fmt.Sprintf("TagValues(%q, %q)", m, k),
							mustStrings(t, func() ([]string, error) { return real.TagValues(ctx, m, k, 0) }),
							mustStrings(t, func() ([]string, error) { return oracle.TagValues(ctx, m, k, 0) }))
					}
				}

			}
		}

		// Finally, a restart: everything acknowledged must come back.
		before := selectAll(t, real, tsdb.Selector{Metric: "a.count"}, 0, 1<<62)
		if err := real.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := Open(Options{Dir: dir, BlockRange: 10 * time.Second, Retention: -1, Clock: fake})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = reopened.Close() }()
		if after := selectAll(t, reopened, tsdb.Selector{Metric: "a.count"}, 0, 1<<62); after != before {
			t.Fatalf("a restart changed the data:\nbefore %s\nafter  %s", before, after)
		}
	})
}

func drawBatch(t *rapid.T, base int64, step int) []tsdb.SeriesSamples {
	n := rapid.IntRange(1, 4).Draw(t, fmt.Sprintf("entries%d", step))
	out := make([]tsdb.SeriesSamples, 0, n)
	for i := 0; i < n; i++ {
		label := fmt.Sprintf("e%d_%d", step, i)
		metric := rapid.SampledFrom([]string{"a.count", "b.gauge"}).Draw(t, "metric"+label)
		tags := rapid.SliceOfNDistinct(
			rapid.SampledFrom([]string{"env:prod", "env:dev", "host:h1", "host:h2"}),
			0, 2, func(s string) string { return s[:3] },
		).Draw(t, "tags"+label)

		entry := tsdb.SeriesSamples{Series: tsdb.NewSeriesRef(metric, tags)}
		// Timestamps are drawn independently and then sorted: sorted is what a
		// well-behaved client sends, and the draws still collide across
		// batches, which is where the interesting disagreements live.
		ts := rapid.SliceOfN(rapid.Int64Range(0, 30), 1, 5).Draw(t, "ts"+label)
		sort.Slice(ts, func(a, b int) bool { return ts[a] < ts[b] })
		for _, sec := range ts {
			entry.Samples = append(entry.Samples, tsdb.Sample{
				T: base + sec*1000,
				V: float64(rapid.IntRange(0, 3).Draw(t, "v"+label)),
			})
		}
		out = append(out, entry)
	}
	return out
}

func drawSelector(t *rapid.T, step int) tsdb.Selector {
	sel := tsdb.Selector{
		Metric: rapid.SampledFrom([]string{"a.count", "b.gauge", "missing"}).
			Draw(t, fmt.Sprintf("qmetric%d", step)),
	}
	for i := 0; i < rapid.IntRange(0, 2).Draw(t, fmt.Sprintf("nmatch%d", step)); i++ {
		label := fmt.Sprintf("m%d_%d", step, i)
		sel.Matchers = append(sel.Matchers, tsdb.Matcher{
			Key:   rapid.SampledFrom([]string{"env", "host", "absent"}).Draw(t, "k"+label),
			Value: rapid.SampledFrom([]string{"prod", "dev", "h1", "h*", "*"}).Draw(t, "v"+label),
			Type: rapid.SampledFrom([]tsdb.MatchType{
				tsdb.Equal, tsdb.NotEqual, tsdb.Wildcard, tsdb.NotWildcard,
			}).Draw(t, "t"+label),
		})
	}
	return sel
}

// cloneBatch gives each store its own copy, so neither can affect the other by
// retaining or mutating the caller's slices.
func cloneBatch(batch []tsdb.SeriesSamples) []tsdb.SeriesSamples {
	out := make([]tsdb.SeriesSamples, len(batch))
	for i, e := range batch {
		out[i] = tsdb.SeriesSamples{
			Series:  e.Series,
			Samples: append([]tsdb.Sample(nil), e.Samples...),
		}
	}
	return out
}

func compareAppend(t *rapid.T, got, want tsdb.AppendResult) {
	t.Helper()
	if got.Samples != want.Samples {
		t.Fatalf("stored %d samples, naive stored %d", got.Samples, want.Samples)
	}
	if got.Series != want.Series {
		t.Fatalf("counted %d series, naive counted %d", got.Series, want.Series)
	}
	// The reason strings are each store's own prose; which series were refused
	// is the contract.
	gotKeys, wantKeys := rejectedKeys(got), rejectedKeys(want)
	if fmt.Sprint(gotKeys) != fmt.Sprint(wantKeys) {
		t.Fatalf("rejected %v, naive rejected %v", gotKeys, wantKeys)
	}
}

func rejectedKeys(res tsdb.AppendResult) []string {
	out := make([]string, 0, len(res.Rejected))
	for _, r := range res.Rejected {
		out = append(out, r.Series.Key())
	}
	sort.Strings(out)
	return out
}

func selectAll(t *rapid.T, store tsdb.MetricStore, sel tsdb.Selector, from, to int64) string {
	t.Helper()
	set, err := store.Select(ctx, sel, from, to)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	var parts []string
	for set.Next() {
		s := set.Series().Key() + "="
		it := set.Iterator()
		for it.Next() {
			smp := it.At()
			s += fmt.Sprintf("%d:%g,", smp.T, smp.V)
		}
		if err := it.Err(); err != nil {
			t.Fatalf("iterating: %v", err)
		}
		parts = append(parts, s)
	}
	if err := set.Err(); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := set.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return fmt.Sprint(parts)
}

func compareStrings(t *rapid.T, what string, got, want []string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s: tsdb %v, naive %v", what, got, want)
	}
}

func mustStrings(t *rapid.T, res func() ([]string, error)) []string {
	t.Helper()
	got, err := res()
	if err != nil {
		t.Fatalf("metadata query: %v", err)
	}
	return got
}
