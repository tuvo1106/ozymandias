package head

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"pgregory.net/rapid"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/wal"
)

// TestHead_WhatWentInComesOut: for any batch the head accepts, a query over
// the full time range returns exactly the accepted samples, in order, once
// each — across chunk boundaries, block boundaries and the stripe hash.
func TestHead_WhatWentInComesOut(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		blockRange := rapid.SampledFrom([]int64{1, 1000, 60_000, 1 << 40}).Draw(t, "blockRange")
		h := New(Options{BlockRange: blockRange})

		want := map[string][]tsdb.Sample{}
		for step := 0; step < rapid.IntRange(1, 8).Draw(t, "batches"); step++ {
			refs, samples := drawBatch(t, step)
			stored, _ := h.Append(refs, samples)
			for i, ok := range stored {
				if ok {
					key := refs[i].Key()
					want[key] = append(want[key], tsdb.Sample{T: samples[i].T, V: samples[i].V})
				}
			}
		}

		got := map[string][]tsdb.Sample{}
		for _, metric := range []string{"a", "b"} {
			for _, s := range h.Select(tsdb.Selector{Metric: metric}, math.MinInt64, math.MaxInt64) {
				got[s.Series.Key()] = s.Samples
			}
		}
		if len(got) != len(want) {
			t.Fatalf("selected %d series, accepted samples for %d", len(got), len(want))
		}
		for key, expect := range want {
			// The head stores in append order, which for accepted samples is
			// ascending by construction — a sample out of order is refused.
			sort.Slice(expect, func(i, j int) bool { return expect[i].T < expect[j].T })
			g := got[key]
			if len(g) != len(expect) {
				t.Fatalf("series %s: selected %d samples, accepted %d", key, len(g), len(expect))
			}
			for i := range expect {
				if g[i].T != expect[i].T || math.Float64bits(g[i].V) != math.Float64bits(expect[i].V) {
					t.Fatalf("series %s sample %d: got (%d, %v), accepted (%d, %v)",
						key, i, g[i].T, g[i].V, expect[i].T, expect[i].V)
				}
			}
		}
	})
}

// TestHead_ReplayReproducesTheHead: for any sequence of batches, a head
// rebuilt from the log alone is indistinguishable from the one that wrote it.
//
// This is the durability claim in its general form. The unit tests check it on
// a handful of shapes; this checks it on batches full of the awkward cases —
// duplicates, out-of-order samples, series that appear and disappear between
// batches — which is where an accounting mistake in replay would hide.
func TestHead_ReplayReproducesTheHead(t *testing.T) {
	root := t.TempDir()
	var run int
	rapid.Check(t, func(t *rapid.T) {
		run++
		dir := filepath.Join(root, fmt.Sprint(run))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		w, err := wal.Open(wal.Options{Dir: dir})
		if err != nil {
			t.Fatal(err)
		}
		blockRange := rapid.SampledFrom([]int64{1000, 60_000, 1 << 40}).Draw(t, "blockRange")
		h := New(Options{WAL: w, BlockRange: blockRange, SyncOnAppend: true})

		for step := 0; step < rapid.IntRange(1, 8).Draw(t, "batches"); step++ {
			refs, samples := drawBatch(t, step)
			h.Append(refs, samples)
		}
		want := snapshot(h)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		restored := New(Options{BlockRange: blockRange})
		if _, err := Replay(restored, dir); err != nil {
			t.Fatalf("replay: %v", err)
		}
		if got := snapshot(restored); got != want {
			t.Fatalf("replay differs:\nwrote   %s\nreplayed %s", want, got)
		}
	})
}

// snapshot renders everything observable about a head, so two can be compared
// in one assertion: series identities, ids, and every sample.
func snapshot(h *Head) string {
	var out []string
	for _, metric := range []string{"a", "b"} {
		for _, id := range h.Postings().Select(tsdb.Selector{Metric: metric}) {
			ref, ok := h.Series(id)
			if !ok {
				continue
			}
			line := fmt.Sprintf("%d=%s:", id, ref.Key())
			for _, s := range h.Select(tsdb.Selector{Metric: metric}, math.MinInt64, math.MaxInt64) {
				if s.Series.Key() != ref.Key() {
					continue
				}
				for _, smp := range s.Samples {
					line += fmt.Sprintf("(%d,%x)", smp.T, math.Float64bits(smp.V))
				}
			}
			out = append(out, line)
		}
	}
	sort.Strings(out)
	return fmt.Sprint(out)
}

// drawBatch makes a batch out of a small pool of series and a small span of
// timestamps, so that collisions — duplicates and out-of-order samples — are
// common rather than a one-in-a-million draw.
func drawBatch(t *rapid.T, step int) ([]tsdb.SeriesRef, []Sample) {
	n := rapid.IntRange(1, 10).Draw(t, fmt.Sprintf("n%d", step))
	refs := make([]tsdb.SeriesRef, 0, n)
	samples := make([]Sample, 0, n)
	for i := 0; i < n; i++ {
		label := fmt.Sprintf("s%d_%d", step, i)
		metric := rapid.SampledFrom([]string{"a", "b"}).Draw(t, "metric"+label)
		tags := rapid.SliceOfNDistinct(
			rapid.SampledFrom([]string{"env:prod", "env:dev", "host:h1"}), 0, 2,
			func(s string) string { return s[:3] },
		).Draw(t, "tags"+label)
		refs = append(refs, tsdb.NewSeriesRef(metric, tags))
		samples = append(samples, Sample{
			T: rapid.Int64Range(0, 20).Draw(t, "t"+label) * 1000,
			V: float64(rapid.IntRange(0, 2).Draw(t, "v"+label)),
		})
	}
	return refs, samples
}
