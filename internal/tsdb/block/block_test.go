package block

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/index"
)

var epoch = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

func ref(metric string, tags ...string) tsdb.SeriesRef {
	return tsdb.NewSeriesRef(metric, tags)
}

// sample data: three metrics, overlapping tag values so the symbol table and
// the postings have something to do.
func fixture(n int) []tsdb.SeriesSamples {
	var out []tsdb.SeriesSamples
	for _, metric := range []string{"http.request.count", "http.request.duration", "db.query.count"} {
		for _, env := range []string{"prod", "staging"} {
			for _, route := range []string{"/api/comics", "/api/users"} {
				s := tsdb.SeriesSamples{Series: ref(metric, "env:"+env, "route:"+route)}
				for i := 0; i < n; i++ {
					s.Samples = append(s.Samples, tsdb.Sample{T: int64(i) * 10_000, V: float64(i) * 1.5})
				}
				out = append(out, s)
			}
		}
	}
	return out
}

func writeFixture(t *testing.T, samplesPerSeries int) (parent string, b *Block, want []tsdb.SeriesSamples) {
	t.Helper()
	parent = t.TempDir()
	want = fixture(samplesPerSeries)
	if _, err := Write(parent, want, WriterOptions{Now: epoch}); err != nil {
		t.Fatal(err)
	}
	blocks, err := OpenAll(parent)
	if err != nil || len(blocks) != 1 {
		t.Fatalf("OpenAll gave %d blocks, err %v", len(blocks), err)
	}
	t.Cleanup(func() { _ = blocks[0].Close() })
	return parent, blocks[0], want
}

func TestBlock_RoundTrip(t *testing.T) {
	_, b, want := writeFixture(t, 250)

	m := b.Meta()
	if m.Version != MetaVersion || m.Stats.Series != len(want) {
		t.Errorf("meta = %+v", m)
	}
	if m.MinTime != 0 || m.MaxTime != 249*10_000 {
		t.Errorf("meta time range is [%d, %d]", m.MinTime, m.MaxTime)
	}
	// 250 samples per series is three chunks: 120, 120, 10.
	if want := len(want) * 3; m.Stats.Chunks != want {
		t.Errorf("Chunks = %d, want %d", m.Stats.Chunks, want)
	}
	if m.Stats.Samples != len(want)*250 {
		t.Errorf("Samples = %d", m.Stats.Samples)
	}

	got, err := b.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("read %d series, wrote %d", len(got), len(want))
	}
	// All() is in key order; sort the input the same way to compare.
	byKey := map[string][]tsdb.Sample{}
	for _, s := range want {
		byKey[s.Series.Key()] = s.Samples
	}
	for _, s := range got {
		if !reflect.DeepEqual(s.Samples, byKey[s.Series.Key()]) {
			t.Errorf("series %s did not survive the round trip", s.Series.Key())
		}
	}
}

func TestBlock_SelectMatchesTheReferenceSemantics(t *testing.T) {
	// The block index is an optimization of tsdb.Selector.Matches, exactly as
	// the head's index is. Where they disagree, the block is wrong.
	_, b, want := writeFixture(t, 5)

	cases := []tsdb.Selector{
		{Metric: "http.request.count"},
		{Metric: "http.request.count", Matchers: []tsdb.Matcher{{Key: "env", Value: "prod", Type: tsdb.Equal}}},
		{Metric: "http.request.count", Matchers: []tsdb.Matcher{{Key: "env", Value: "prod", Type: tsdb.NotEqual}}},
		{Metric: "db.query.count", Matchers: []tsdb.Matcher{{Key: "route", Value: "/api/*", Type: tsdb.Wildcard}}},
		{Metric: "db.query.count", Matchers: []tsdb.Matcher{{Key: "route", Value: "/api/u*", Type: tsdb.NotWildcard}}},
		{Metric: "http.request.count", Matchers: []tsdb.Matcher{
			{Key: "env", Value: "prod", Type: tsdb.Equal},
			{Key: "route", Value: "/api/users", Type: tsdb.Equal},
		}},
		{Metric: "nope"},
		{Metric: "http.request.count", Matchers: []tsdb.Matcher{{Key: "zone", Value: "a", Type: tsdb.NotEqual}}},
	}
	for _, sel := range cases {
		t.Run(fmt.Sprint(sel), func(t *testing.T) {
			var expect []string
			for _, s := range want {
				if sel.Matches(s.Series) {
					expect = append(expect, s.Series.Key())
				}
			}
			got, err := b.Select(sel, math.MinInt64, math.MaxInt64)
			if err != nil {
				t.Fatal(err)
			}
			var keys []string
			for _, s := range got {
				keys = append(keys, s.Series.Key())
			}
			if !reflect.DeepEqual(keys, expect) {
				t.Errorf("Select gave %v, reference says %v", keys, expect)
			}
		})
	}
}

func TestBlock_SelectReadsOnlyTheOverlappingChunks(t *testing.T) {
	_, b, _ := writeFixture(t, 250)
	sel := tsdb.Selector{Metric: "db.query.count", Matchers: []tsdb.Matcher{
		{Key: "env", Value: "prod", Type: tsdb.Equal},
		{Key: "route", Value: "/api/comics", Type: tsdb.Equal},
	}}

	// A window inside the third chunk only.
	got, err := b.Select(sel, 2_400_000, 2_450_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("selected %d series", len(got))
	}
	for _, s := range got[0].Samples {
		if s.T < 2_400_000 || s.T > 2_450_000 {
			t.Fatalf("sample at %d is outside the window", s.T)
		}
	}
	if n := len(got[0].Samples); n != 6 {
		t.Errorf("window holds %d samples, want 6", n)
	}
	// Outside the block entirely: no work, no error.
	if got, err := b.Select(sel, 9_000_000, 9_100_000); err != nil || got != nil {
		t.Errorf("a window past the block gave %v, %v", got, err)
	}
	if b.Overlaps(9_000_000, 9_100_000) {
		t.Error("Overlaps is true for a window past the block")
	}
}

func TestBlock_MetadataMatchesTheHeadIndex(t *testing.T) {
	// The same three queries the UI's autocomplete makes, asked of a block and
	// of an in-memory index built from the same series. They must agree, or an
	// aging series would disappear from autocomplete.
	_, b, want := writeFixture(t, 3)
	mem := index.NewMemPostings()
	for i, s := range want {
		mem.Add(uint64(i), s.Series)
	}
	lookups := []struct {
		name       string
		block, ref []string
	}{
		{"metrics", index.Metrics(b.Lookup(), ""), mem.Metrics("")},
		{"metrics with prefix", index.Metrics(b.Lookup(), "http."), mem.Metrics("http.")},
		{"keys", index.Keys(b.Lookup(), ""), mem.Keys("")},
		{"keys of a metric", index.Keys(b.Lookup(), "db.query.count"), mem.Keys("db.query.count")},
		{"values", index.Values(b.Lookup(), "", "env"), mem.Values("", "env")},
		{"values of a metric", index.Values(b.Lookup(), "db.query.count", "route"),
			mem.Values("db.query.count", "route")},
		{"values of an absent key", index.Values(b.Lookup(), "", "nope"), mem.Values("", "nope")},
	}
	for _, l := range lookups {
		if !reflect.DeepEqual(l.block, l.ref) {
			t.Errorf("%s: block says %v, the in-memory index says %v", l.name, l.block, l.ref)
		}
	}
}

func TestBlock_SymbolTableEarnsItsKeep(t *testing.T) {
	// The claim the format rests on: tag strings repeat across series, so
	// storing each distinct string once and referring to it by ordinal makes
	// the index a fraction of what the strings alone would cost. Measured,
	// not asserted in prose.
	//
	// The shape is the realistic one: a long metric name and a long
	// environment value shared by every series, plus one value that varies.
	parent := t.TempDir()
	var series []tsdb.SeriesSamples
	for i := 0; i < 200; i++ {
		r := ref("http.request.duration.by.route",
			"env:production-us-east-1", fmt.Sprintf("shard:%03d", i))
		series = append(series, tsdb.SeriesSamples{
			Series: r, Samples: []tsdb.Sample{{T: 1000, V: 1}, {T: 2000, V: 2}},
		})
	}
	m, err := Write(parent, series, WriterOptions{Now: epoch})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, m.ULID.String())

	// The counterfactual is storing each series' strings inside its own
	// record, which is what the symbol table replaces.
	var inline int
	for _, s := range series {
		inline += len(s.Series.Metric)
		for _, tag := range s.Series.Tags {
			inline += len(tag.Key) + len(tag.Value)
		}
	}
	ix, err := openIndexReader(dir)
	if err != nil {
		t.Fatal(err)
	}
	var stored int
	for _, s := range ix.symbols {
		stored += len(s)
	}
	if stored*4 > inline {
		t.Errorf("symbols hold %d bytes against %d inline; they are barely deduplicating",
			stored, inline)
	}
	t.Logf("200 series: %d distinct symbols = %d B, against %d B stored inline; "+
		"index file %d B, chunks %d B", len(ix.symbols), stored, inline,
		fileSize(t, filepath.Join(dir, IndexFilename)),
		fileSize(t, filepath.Join(dir, ChunksFilename)))
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// --- durability ------------------------------------------------------------

func TestWriter_AHalfWrittenBlockIsNeverVisible(t *testing.T) {
	parent := t.TempDir()
	w, err := NewWriter(parent, WriterOptions{Now: epoch})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddSeries(ref("m", "env:prod"), []tsdb.Sample{{T: 1, V: 1}}); err != nil {
		t.Fatal(err)
	}
	// The process dies here: the temp directory exists, meta.json does not.
	blocks, err := OpenAll(parent)
	if err != nil || len(blocks) != 0 {
		t.Fatalf("OpenAll saw %d blocks mid-write, err %v", len(blocks), err)
	}
	removed, err := CleanTmp(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 {
		t.Errorf("CleanTmp removed %v, want the one temp dir", removed)
	}
	if entries, _ := os.ReadDir(parent); len(entries) != 0 {
		t.Errorf("%d entries survived the cleanup", len(entries))
	}
}

func TestWriter_ADirectoryWithoutMetaIsGarbage(t *testing.T) {
	// Not only .tmp: a directory that looks like a block but has no meta.json
	// is the wreckage of a crash between the index write and the meta write.
	parent, _, _ := writeFixture(t, 2)
	broken := filepath.Join(parent, "01JXXXXXXXXXXXXXXXXXXXXXXX")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, ChunksFilename), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := CleanTmp(parent)
	if err != nil || len(removed) != 1 {
		t.Errorf("CleanTmp removed %v, err %v", removed, err)
	}
}

func TestWriter_ACompleteTempDirectoryIsStillGarbage(t *testing.T) {
	// The nastiest of the three, and the one TestDB_CrashLoop caught: the
	// process died in the window between meta.json and the rename, so the
	// .tmp directory is a complete, valid, readable block. It is still not a
	// block, because it was never renamed into place — OpenAll will never look
	// at it, so keeping it would leak the disk it occupies forever while the
	// size cap went on charging for it.
	parent := t.TempDir()
	meta, err := Write(parent, []tsdb.SeriesSamples{
		{Series: ref("m", "env:prod"), Samples: []tsdb.Sample{{T: 1, V: 1}}},
	}, WriterOptions{Now: epoch})
	if err != nil {
		t.Fatal(err)
	}
	// Undo the rename: exactly the state a crash one instruction earlier left.
	live := filepath.Join(parent, meta.ULID.String())
	orphan := live + tmpSuffix
	if err := os.Rename(live, orphan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(orphan, MetaFilename)); err != nil {
		t.Fatalf("the fixture is not the case under test: %v", err)
	}

	removed, err := CleanTmp(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 {
		t.Errorf("CleanTmp removed %v, want the orphaned temp dir", removed)
	}
	if entries, _ := os.ReadDir(parent); len(entries) != 0 {
		t.Errorf("%d entries survived the cleanup", len(entries))
	}
}

func TestWriter_AnEmptyBlockIsNotWritten(t *testing.T) {
	parent := t.TempDir()
	_, err := Write(parent, nil, WriterOptions{Now: epoch})
	if !errors.Is(err, ErrEmpty) {
		t.Errorf("err = %v, want ErrEmpty", err)
	}
	if entries, _ := os.ReadDir(parent); len(entries) != 0 {
		t.Errorf("an empty write left %d entries behind", len(entries))
	}
}

func TestWriter_RejectsANonCanonicalSeries(t *testing.T) {
	parent := t.TempDir()
	// Tags out of order: the store rejects rather than fixes, so a caller bug
	// cannot create two spellings of one series.
	bad := tsdb.SeriesRef{Metric: "m", Tags: []tsdb.Tag{{Key: "z", Value: "1"}, {Key: "a", Value: "2"}}}
	_, err := Write(parent, []tsdb.SeriesSamples{{Series: bad, Samples: []tsdb.Sample{{T: 1}}}},
		WriterOptions{Now: epoch})
	if err == nil {
		t.Fatal("a non-canonical series was accepted")
	}
	if entries, _ := os.ReadDir(parent); len(entries) != 0 {
		t.Errorf("the failed write left %d entries behind", len(entries))
	}
}

func TestBlock_DeleteIsCrashSafe(t *testing.T) {
	parent, b, _ := writeFixture(t, 2)
	dir := b.Dir()
	_ = b.Close()

	// Simulate dying after the tombstone is written but before the files go.
	if err := writeFileSync(filepath.Join(dir, tombstoneFilename), []byte("deleted\n")); err != nil {
		t.Fatal(err)
	}
	if !Condemned(dir) {
		t.Fatal("a tombstoned block is not reported as condemned")
	}
	removed, err := CleanCondemned(parent)
	if err != nil || len(removed) != 1 {
		t.Fatalf("CleanCondemned removed %v, err %v", removed, err)
	}
	if blocks, err := OpenAll(parent); err != nil || len(blocks) != 0 {
		t.Errorf("OpenAll gave %d blocks after the cleanup, err %v", len(blocks), err)
	}
	// And the ordinary path works too.
	if _, err := Write(parent, fixture(2), WriterOptions{Now: epoch}); err != nil {
		t.Fatal(err)
	}
	blocks, _ := OpenAll(parent)
	if len(blocks) != 1 {
		t.Fatalf("expected one block, got %d", len(blocks))
	}
	dir = blocks[0].Dir()
	_ = blocks[0].Close()
	if err := Delete(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the block directory survived Delete: %v", err)
	}
}

// --- corruption ------------------------------------------------------------

func TestBlock_CorruptionIsDetected(t *testing.T) {
	t.Run("a flipped bit in a chunk", func(t *testing.T) {
		_, b, _ := writeFixture(t, 250)
		dir := b.Dir()
		_ = b.Close()
		flip(t, filepath.Join(dir, ChunksFilename), 40)

		reopened, err := Open(dir)
		if err != nil {
			t.Fatalf("the block should still open: %v", err)
		}
		defer func() { _ = reopened.Close() }()
		// The damage is caught when the chunk is read, not silently decoded.
		if _, err := reopened.All(); err == nil {
			t.Error("a corrupt chunk was read without complaint")
		}
	})

	t.Run("a flipped bit in the index", func(t *testing.T) {
		_, b, _ := writeFixture(t, 5)
		dir := b.Dir()
		_ = b.Close()
		flip(t, filepath.Join(dir, IndexFilename), 30)
		if _, err := Open(dir); err == nil {
			t.Error("a corrupt index opened without complaint")
		}
	})

	t.Run("a truncated index", func(t *testing.T) {
		_, b, _ := writeFixture(t, 5)
		dir := b.Dir()
		_ = b.Close()
		path := filepath.Join(dir, IndexFilename)
		if err := os.Truncate(path, fileSize(t, path)/2); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir); err == nil {
			t.Error("a truncated index opened without complaint")
		}
	})

	t.Run("a file that is not a block", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{MetaFilename, IndexFilename, ChunksFilename} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := Open(dir); err == nil {
			t.Error("a directory of junk opened as a block")
		}
	})

	t.Run("a block from another version", func(t *testing.T) {
		_, b, _ := writeFixture(t, 2)
		dir := b.Dir()
		_ = b.Close()
		if err := os.WriteFile(filepath.Join(dir, MetaFilename),
			[]byte(`{"version": 99}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir); err == nil {
			t.Error("a block claiming version 99 was opened")
		}
	})
}

func flip(t *testing.T, path string, at int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var b [1]byte
	if _, err := f.ReadAt(b[:], at); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0x40
	if _, err := f.WriteAt(b[:], at); err != nil {
		t.Fatal(err)
	}
}

func TestMeta_SameMillisecondIDsAreStillDistinct(t *testing.T) {
	// Two blocks can be written inside one millisecond — a compaction
	// finishing right after a head cut, or any test on a fake clock. If the
	// entropy came from the timestamp the ids would be equal and the second
	// block would collide with the first's directory.
	seen := map[string]bool{}
	var prev string
	for i := 0; i < 100; i++ {
		id := NewULID(epoch).String()
		if seen[id] {
			t.Fatalf("id %s repeated within one millisecond after %d draws", id, i)
		}
		if prev != "" && id <= prev {
			t.Fatalf("id %s does not sort after %s; same-ms ids must stay ordered", id, prev)
		}
		seen[id], prev = true, id
	}
}

func TestMeta_ULIDsSortByTime(t *testing.T) {
	// `ls blocks/` in creation order is what lets compaction find adjacent
	// blocks without opening every meta.json.
	a := NewULID(epoch)
	b := NewULID(epoch.Add(time.Second))
	if a.String() >= b.String() {
		t.Errorf("%s does not sort before %s", a, b)
	}
	if a.Compare(b) >= 0 {
		t.Errorf("Compare says %s is not before %s", a, b)
	}
}

func TestOpenAll_SortsByTimeAndSkipsIncomplete(t *testing.T) {
	parent := t.TempDir()
	for i := 3; i >= 1; i-- { // written newest-first on purpose
		s := []tsdb.SeriesSamples{{
			Series:  ref("m", "env:prod"),
			Samples: []tsdb.Sample{{T: int64(i) * 1000, V: 1}},
		}}
		if _, err := Write(parent, s, WriterOptions{Now: epoch.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(parent, "01JUNK.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	blocks, err := OpenAll(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, b := range blocks {
			_ = b.Close()
		}
	}()
	if len(blocks) != 3 {
		t.Fatalf("opened %d blocks, want 3", len(blocks))
	}
	for i := 1; i < len(blocks); i++ {
		if blocks[i-1].Meta().MinTime > blocks[i].Meta().MinTime {
			t.Errorf("blocks are not in time order: %v", blocks)
		}
	}
	if _, err := OpenAll(filepath.Join(parent, "does-not-exist")); err != nil {
		t.Errorf("OpenAll of a missing directory should be empty, not an error: %v", err)
	}
}

func TestWriter_RecordsProvenance(t *testing.T) {
	parent := t.TempDir()
	src := NewULID(epoch).String()
	m, err := Write(parent, fixture(2), WriterOptions{
		Now: epoch.Add(time.Hour), Level: 2, Sources: []string{src}, ResolutionS: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.Compaction.Level != 2 || len(m.Compaction.Sources) != 1 ||
		m.Compaction.Sources[0].String() != src {
		t.Errorf("compaction = %+v", m.Compaction)
	}
	if m.ResolutionS != 60 {
		t.Errorf("ResolutionS = %d", m.ResolutionS)
	}
	// And it survives a round trip through JSON.
	got, err := ReadMeta(filepath.Join(parent, m.ULID.String()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, m) {
		t.Errorf("meta.json round trip: %+v != %+v", got, m)
	}
	if _, err := Write(parent, fixture(1), WriterOptions{Sources: []string{"not-a-ulid"}}); err == nil {
		t.Error("a malformed source id was accepted")
	}
}

func TestBlock_SeriesAndSamplesFor(t *testing.T) {
	// The streaming read compaction uses: ask what a block holds without
	// touching the chunks file, then pull one series at a time.
	_, b, want := writeFixture(t, 250)

	got := b.Series()
	if len(got) != len(want) {
		t.Fatalf("Series listed %d, the block holds %d", len(got), len(want))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Key() >= got[i].Key() {
			t.Fatalf("Series is not in key order at %d: %s then %s",
				i, got[i-1].Key(), got[i].Key())
		}
	}

	key := got[0].Key()
	samples, err := b.SamplesFor(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 250 {
		t.Errorf("SamplesFor(%s) returned %d samples, want 250", key, len(samples))
	}
	// It agrees with the whole-block read, which is the one that is already
	// checked against what was written.
	all, err := b.All()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range all {
		one, err := b.SamplesFor(s.Series.Key())
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(one, s.Samples) {
			t.Errorf("SamplesFor(%s) disagrees with All()", s.Series.Key())
		}
	}

	// A series the block does not hold is absent, not an error: compaction
	// asks every source for every key in the union.
	for _, missing := range []string{"nope|", "http.request.count|env:nope", ""} {
		got, err := b.SamplesFor(missing)
		if err != nil || got != nil {
			t.Errorf("SamplesFor(%q) = %v, %v; want nil, nil", missing, got, err)
		}
	}
}

func TestBlock_SamplesForReportsACorruptChunk(t *testing.T) {
	_, b, _ := writeFixture(t, 4)
	dir := b.Dir()
	key := b.Series()[0].Key()
	cr := b.ix.series[0].chunks[0]
	_ = b.Close()
	flip(t, filepath.Join(dir, ChunksFilename), int64(cr.off+3))

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if _, err := reopened.SamplesFor(key); err == nil {
		t.Error("SamplesFor read a corrupt chunk without complaint")
	}
}

func TestBlock_AReaderKeepsTheFilesOpenPastClose(t *testing.T) {
	// Compaction and retention close a block the moment it leaves the
	// database's list, and a query that started a moment earlier is still
	// reading chunks.dat. Before the reader count existed, that query failed
	// with "file already closed" — an error returned to a user for a block
	// that was merged, not lost. The close has to wait its turn.
	_, b, _ := writeFixture(t, 4)

	if !b.Acquire() {
		t.Fatal("a fresh block refused a reader")
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close while held: %v", err)
	}
	if _, err := b.Select(tsdb.Selector{}, math.MinInt64, math.MaxInt64); err != nil {
		t.Errorf("the held block became unreadable before its reader left: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Errorf("Close is not idempotent: %v", err)
	}
	if b.Acquire() {
		t.Error("a closed block handed out a new reader")
	}

	b.Release() // the last one out turns off the lights
	if _, err := b.Select(tsdb.Selector{}, math.MinInt64, math.MaxInt64); err == nil {
		t.Error("the file was still open after the last reader left")
	}
}

func TestBlock_ReadersNestAndOnlyTheLastOneCloses(t *testing.T) {
	_, b, _ := writeFixture(t, 4)
	for i := 0; i < 3; i++ {
		if !b.Acquire() {
			t.Fatalf("reader %d refused", i)
		}
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		b.Release()
		if _, err := b.Select(tsdb.Selector{}, math.MinInt64, math.MaxInt64); err != nil {
			t.Fatalf("closed with %d readers still holding it: %v", 2-i, err)
		}
	}
	b.Release()
	if _, err := b.Select(tsdb.Selector{}, math.MinInt64, math.MaxInt64); err == nil {
		t.Error("the file outlived its last reader")
	}
}
