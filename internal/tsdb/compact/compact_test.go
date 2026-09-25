package compact

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/block"
)

var epoch = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

func ref(metric string, tags ...string) tsdb.SeriesRef {
	return tsdb.NewSeriesRef(metric, tags)
}

// writeBlock puts one block covering [t0, t0+span) under parent, with one
// sample per second for each named series.
func writeBlock(t *testing.T, parent string, t0, span int64, level int, series ...tsdb.SeriesRef) *block.Block {
	t.Helper()
	return writeBlockRes(t, parent, t0, span, level, 0, series...)
}

// writeBlockRes is writeBlock with a rollup resolution, for the interleaving
// that only exists once rollups land.
func writeBlockRes(t *testing.T, parent string, t0, span int64, level, resolutionS int, series ...tsdb.SeriesRef) *block.Block {
	t.Helper()
	batch := make([]tsdb.SeriesSamples, 0, len(series))
	for _, r := range series {
		e := tsdb.SeriesSamples{Series: r}
		for ts := t0; ts < t0+span; ts += 1000 {
			e.Samples = append(e.Samples, tsdb.Sample{T: ts, V: float64(ts / 1000)})
		}
		batch = append(batch, e)
	}
	m, err := block.Write(parent, batch, block.WriterOptions{Now: epoch, Level: level, ResolutionS: resolutionS})
	if err != nil {
		t.Fatal(err)
	}
	b, err := block.Open(filepath.Join(parent, m.ULID.String()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func TestNext_WaitsForThreeAdjacentBlocksOfOneLevel(t *testing.T) {
	parent := t.TempDir()
	r := ref("m", "env:prod")

	var blocks []*block.Block
	for i := int64(0); i < 2; i++ {
		blocks = append(blocks, writeBlock(t, parent, i*60_000, 60_000, 0, r))
	}
	if p := Next(blocks, Options{}); p != nil {
		t.Errorf("planned a compaction of %d blocks; three is the threshold", len(p.Sources))
	}
	blocks = append(blocks, writeBlock(t, parent, 120_000, 60_000, 0, r))
	p := Next(blocks, Options{})
	if p == nil {
		t.Fatal("three adjacent level-0 blocks were not planned")
	}
	if len(p.Sources) != 3 || p.Level != 1 {
		t.Errorf("plan merges %d blocks into level %d", len(p.Sources), p.Level)
	}
	minT, maxT := p.Span()
	if minT != 0 || maxT != 179_000 {
		t.Errorf("plan spans [%d, %d]", minT, maxT)
	}
}

func TestNext_DoesNotMixLevelsOrResolutions(t *testing.T) {
	parent := t.TempDir()
	r := ref("m", "env:prod")
	// Two level-0 and one level-1: no level has three, so nothing to do.
	blocks := []*block.Block{
		writeBlock(t, parent, 0, 60_000, 0, r),
		writeBlock(t, parent, 60_000, 60_000, 0, r),
		writeBlock(t, parent, 120_000, 60_000, 1, r),
	}
	if p := Next(blocks, Options{}); p != nil {
		t.Errorf("planned across levels: %d sources at level %d", len(p.Sources), p.Level)
	}

	// A rollup sits at the same time range as its source but must never be
	// merged with it: the samples mean different things.
	rollup, err := block.Write(parent, []tsdb.SeriesSamples{{
		Series: r, Samples: []tsdb.Sample{{T: 0, V: 1}},
	}}, block.WriterOptions{Now: epoch, ResolutionS: 60})
	if err != nil {
		t.Fatal(err)
	}
	rb, err := block.Open(filepath.Join(parent, rollup.ULID.String()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rb.Close() }()
	blocks = append(blocks[:2], rb)
	if p := Next(blocks, Options{}); p != nil {
		t.Errorf("planned a merge of raw and rollup blocks: %+v", p)
	}
}

func TestNext_RefusesToExceedTheMaxBlockRange(t *testing.T) {
	parent := t.TempDir()
	r := ref("m", "env:prod")
	var blocks []*block.Block
	for i := int64(0); i < 3; i++ {
		blocks = append(blocks, writeBlock(t, parent, i*60_000, 60_000, 0, r))
	}
	// Three blocks spanning 180s, capped at 100s: the run cannot be formed.
	if p := Next(blocks, Options{MaxBlockRange: 100 * time.Second}); p != nil {
		t.Errorf("planned a %d-block merge past the range cap", len(p.Sources))
	}
	if p := Next(blocks, Options{MaxBlockRange: 300 * time.Second}); p == nil {
		t.Error("a cap wide enough should allow the merge")
	}
}

func TestRun_MergeIsInvisibleToAReader(t *testing.T) {
	// The property compaction lives or dies by: it rewrites every byte of its
	// inputs, and a reader must not be able to tell.
	parent := t.TempDir()
	a := ref("m", "env:prod")
	b := ref("m", "env:dev")
	c := ref("other", "env:prod") // only in the middle block

	blocks := []*block.Block{
		writeBlock(t, parent, 0, 60_000, 0, a, b),
		writeBlock(t, parent, 60_000, 60_000, 0, a, b, c),
		writeBlock(t, parent, 120_000, 60_000, 0, a),
	}
	want := map[string][]tsdb.Sample{}
	for _, blk := range blocks {
		all, err := blk.All()
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range all {
			want[s.Series.Key()] = append(want[s.Series.Key()], s.Samples...)
		}
	}

	p := Next(blocks, Options{})
	if p == nil {
		t.Fatal("nothing planned")
	}
	sourceDirs := make([]string, len(p.Sources))
	for i, s := range p.Sources {
		sourceDirs[i] = s.Dir()
	}
	meta, err := Run(parent, p, epoch.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	merged, err := block.Open(filepath.Join(parent, meta.ULID.String()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = merged.Close() }()

	got := map[string][]tsdb.Sample{}
	all, err := merged.All()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range all {
		got[s.Series.Key()] = s.Samples
	}
	if !reflect.DeepEqual(got, want) {
		for k := range want {
			if !reflect.DeepEqual(got[k], want[k]) {
				t.Errorf("series %s: merged has %d samples, sources had %d",
					k, len(got[k]), len(want[k]))
			}
		}
		t.Fatalf("the merge changed the data (%d series out, %d in)", len(got), len(want))
	}

	// Provenance, so a human can trace the block back.
	if meta.Compaction.Level != 1 || len(meta.Compaction.Sources) != 3 {
		t.Errorf("compaction meta = %+v", meta.Compaction)
	}
	if meta.MinTime != 0 || meta.MaxTime != 179_000 {
		t.Errorf("merged block spans [%d, %d]", meta.MinTime, meta.MaxTime)
	}
	// The sources outlive Run: they are the only readable copy until the
	// caller has the merged block open and serving, which is the whole reason
	// deleting them is a separate call.
	for _, dir := range sourceDirs {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("Run deleted source %s: %v", filepath.Base(dir), err)
		}
	}
	if err := DeleteSources(p); err != nil {
		t.Fatal(err)
	}
	for _, dir := range sourceDirs {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("source %s survived DeleteSources: %v", filepath.Base(dir), err)
		}
	}
}

func TestRun_PacksChunksThatTheSourcesLeftShort(t *testing.T) {
	// Head cuts leave chunks short wherever a block boundary fell. Compaction
	// re-encodes, so the merged block holds the same samples in fewer chunks —
	// which is most of why compacting is worth the rewrite.
	parent := t.TempDir()
	r := ref("m", "env:prod")
	var blocks []*block.Block
	var sourceChunks int
	for i := int64(0); i < 3; i++ {
		b := writeBlock(t, parent, i*60_000, 60_000, 0, r) // 60 samples each
		sourceChunks += b.Meta().Stats.Chunks
		blocks = append(blocks, b)
	}
	if sourceChunks != 3 {
		t.Fatalf("expected one short chunk per source, got %d", sourceChunks)
	}
	p := Next(blocks, Options{})
	meta, err := Run(parent, p, epoch.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// 180 samples repack into two chunks (120 + 60) instead of three.
	if meta.Stats.Chunks != 2 {
		t.Errorf("merged into %d chunks, want 2", meta.Stats.Chunks)
	}
	if meta.Stats.Samples != 180 {
		t.Errorf("merged block holds %d samples", meta.Stats.Samples)
	}
}

func TestRun_ToleratesSourcesThatOverlap(t *testing.T) {
	// A crash between writing a merged block and deleting its sources leaves
	// two blocks covering the same instant. The next compaction must not
	// encode that timestamp twice — chunkenc would reject it and the whole
	// compaction would fail.
	parent := t.TempDir()
	r := ref("m", "env:prod")
	blocks := []*block.Block{
		writeBlock(t, parent, 0, 60_000, 0, r),
		writeBlock(t, parent, 30_000, 60_000, 0, r), // overlaps the first
		writeBlock(t, parent, 90_000, 60_000, 0, r),
	}
	p := Next(blocks, Options{})
	if p == nil {
		t.Fatal("nothing planned")
	}
	meta, err := Run(parent, p, epoch.Add(time.Hour))
	if err != nil {
		t.Fatalf("a compaction of overlapping sources failed: %v", err)
	}
	merged, err := block.Open(filepath.Join(parent, meta.ULID.String()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = merged.Close() }()
	all, err := merged.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("%d series in the merged block", len(all))
	}
	var last int64 = math.MinInt64
	for _, s := range all[0].Samples {
		if s.T <= last {
			t.Fatalf("timestamp %d repeats or goes backwards", s.T)
		}
		last = s.T
	}
	// 0..149s once each, not 150 plus the 30 duplicated seconds.
	if n := len(all[0].Samples); n != 150 {
		t.Errorf("merged block holds %d samples, want 150 with the overlap collapsed", n)
	}
}

func TestRun_RejectsAnEmptyPlan(t *testing.T) {
	if _, err := Run(t.TempDir(), nil, epoch); err == nil {
		t.Error("Run accepted a nil plan")
	}
	parent := t.TempDir()
	one := []*block.Block{writeBlock(t, parent, 0, 1000, 0, ref("m"))}
	if _, err := Run(parent, &Plan{Sources: one, Level: 1}, epoch); err == nil {
		t.Error("Run accepted a one-block plan")
	}
}

func TestNext_NothingToDo(t *testing.T) {
	if p := Next(nil, Options{}); p != nil {
		t.Errorf("planned something from no blocks: %+v", p)
	}
}

func TestRun_ReportsAnUnreadableSource(t *testing.T) {
	parent := t.TempDir()
	r := ref("m", "env:prod")
	var blocks []*block.Block
	for i := int64(0); i < 3; i++ {
		blocks = append(blocks, writeBlock(t, parent, i*60_000, 60_000, 0, r))
	}
	p := Next(blocks, Options{})
	// Corrupt a chunk in the middle source, checksum and all.
	victim := filepath.Join(p.Sources[1].Dir(), block.ChunksFilename)
	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(victim, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(parent, p, epoch.Add(time.Hour)); err == nil {
		t.Fatal("a compaction over a corrupt source succeeded")
	}
	// The sources must all still be there: a failed compaction deletes nothing.
	for i, s := range p.Sources {
		if _, err := os.Stat(s.Dir()); err != nil {
			t.Errorf("source %d was deleted by a failed compaction: %v", i, err)
		}
	}
	// And no half-built output was left behind.
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("%d directories after the failure: %v", len(entries), names)
	}
}

func TestMergedSeries_IsAUnionInKeyOrder(t *testing.T) {
	parent := t.TempDir()
	blocks := []*block.Block{
		writeBlock(t, parent, 0, 1000, 0, ref("m", "h:1"), ref("m", "h:3")),
		writeBlock(t, parent, 1000, 1000, 0, ref("m", "h:2"), ref("m", "h:3")),
	}
	var got []string
	for _, r := range mergedSeries(blocks) {
		got = append(got, r.Key())
	}
	want := []string{"m|h:1", "m|h:2", "m|h:3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergedSeries = %v, want %v", got, want)
	}
}

func TestNext_PicksTheOldestEligibleRun(t *testing.T) {
	parent := t.TempDir()
	r := ref("m", "env:prod")
	var blocks []*block.Block
	for i := int64(0); i < 6; i++ {
		blocks = append(blocks, writeBlock(t, parent, i*60_000, 60_000, 0, r))
	}
	// Shuffled input: the planner sorts, so the answer must not depend on it.
	blocks[0], blocks[5] = blocks[5], blocks[0]
	p := Next(blocks, Options{})
	if p == nil {
		t.Fatal("nothing planned")
	}
	minT, _ := p.Span()
	if minT != 0 {
		t.Errorf("planned a run starting at %d; the oldest starts at 0", minT)
	}
	for i := 1; i < len(p.Sources); i++ {
		if p.Sources[i-1].Meta().MinTime > p.Sources[i].Meta().MinTime {
			t.Errorf("plan sources are not in time order: %v", p.Sources)
		}
	}
}

// TestNext_FindsARunThatRollupBlocksAreInterleavedWith.
//
// Next scans a list ordered by MinTime alone, and a rollup block sits beside
// its source at the same time range. So once rollups exist the order is
// raw(t0), roll(t0), raw(t1), roll(t1), ... and a scan that *stopped* at the
// first block of another resolution ended every run after one block. No run
// ever reached MinBlocks, and compaction stopped permanently — for both
// resolutions at once — while the file count kept growing.
//
// It is latent today only because levels happen to be segregated in time.
// This is the arrangement M2 part two produces, asserted now so that landing
// rollups does not quietly turn compaction off.
func TestNext_FindsARunThatRollupBlocksAreInterleavedWith(t *testing.T) {
	parent := t.TempDir()
	r := ref("m", "env:prod")
	var blocks []*block.Block
	for i := int64(0); i < 3; i++ {
		t0 := i * 60_000
		blocks = append(blocks,
			writeBlockRes(t, parent, t0, 60_000, 0, 0, r),  // raw
			writeBlockRes(t, parent, t0, 60_000, 0, 60, r)) // its rollup
	}

	p := Next(blocks, Options{})
	if p == nil {
		t.Fatal("no plan: three same-level blocks of one resolution are a run, " +
			"whatever is interleaved with them")
	}
	if len(p.Sources) != 3 {
		t.Fatalf("planned %d sources, want 3", len(p.Sources))
	}
	// And it must still be one resolution — the samples mean different things.
	want := p.Sources[0].Meta().ResolutionS
	for _, b := range p.Sources {
		if got := b.Meta().ResolutionS; got != want {
			t.Errorf("the plan mixes resolutions: %d and %d", want, got)
		}
	}
}
