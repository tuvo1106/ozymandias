package block

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
)

// TestBlock_WriteReadRoundTrip is the block's central property: reading a
// block returns exactly the series and samples that were written to it, for
// any input the writer accepts. Everything else in the format — symbols,
// postings, chunk references — is an optimization that must not change it.
func TestBlock_WriteReadRoundTrip(t *testing.T) {
	root := t.TempDir()
	var n int
	rapid.Check(t, func(t *rapid.T) {
		series := rapid.SliceOfNDistinct(genSeries(), 1, 12,
			func(s tsdb.SeriesSamples) string { return s.Series.Key() }).Draw(t, "series")

		n++
		parent := filepath.Join(root, fmt.Sprint(n))
		if err := os.MkdirAll(parent, 0o755); err != nil {
			t.Fatal(err)
		}
		m, err := Write(parent, series, WriterOptions{Now: epoch})
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		b, err := Open(filepath.Join(parent, m.ULID.String()))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = b.Close() }()

		got, err := b.All()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		want := append([]tsdb.SeriesSamples(nil), series...)
		sort.Slice(want, func(i, j int) bool { return want[i].Series.Key() < want[j].Series.Key() })
		if len(got) != len(want) {
			t.Fatalf("read %d series, wrote %d", len(got), len(want))
		}
		var minT, maxT int64 = math.MaxInt64, math.MinInt64
		var samples int
		for i := range want {
			if got[i].Series.Key() != want[i].Series.Key() {
				t.Fatalf("series %d is %s, want %s", i, got[i].Series.Key(), want[i].Series.Key())
			}
			if !reflect.DeepEqual(got[i].Samples, want[i].Samples) {
				t.Fatalf("series %s: read %v, wrote %v",
					want[i].Series.Key(), got[i].Samples, want[i].Samples)
			}
			samples += len(want[i].Samples)
			minT = min(minT, want[i].Samples[0].T)
			maxT = max(maxT, want[i].Samples[len(want[i].Samples)-1].T)
		}
		// The meta must describe what is actually inside, because compaction
		// and the querier prune on it without opening the block.
		if m.MinTime != minT || m.MaxTime != maxT {
			t.Fatalf("meta range [%d, %d], data range [%d, %d]",
				m.MinTime, m.MaxTime, minT, maxT)
		}
		if m.Stats.Series != len(want) || m.Stats.Samples != samples {
			t.Fatalf("meta stats %+v for %d series and %d samples",
				m.Stats, len(want), samples)
		}
	})
}

func genSeries() *rapid.Generator[tsdb.SeriesSamples] {
	return rapid.Custom(func(t *rapid.T) tsdb.SeriesSamples {
		tags := rapid.SliceOfNDistinct(
			rapid.SampledFrom([]string{
				"env:prod", "env:dev", "route:/a", "route:/b", "host:h1", "host:h2",
			}), 0, 3, func(s string) string { return s[:4] },
		).Draw(t, "tags")
		metric := rapid.SampledFrom([]string{"a.count", "b.gauge", "c.duration"}).Draw(t, "metric")

		// Timestamps must ascend; deltas exercise the encoder's buckets.
		n := rapid.IntRange(1, 300).Draw(t, "samples")
		out := tsdb.SeriesSamples{Series: tsdb.NewSeriesRef(metric, tags)}
		ts := rapid.Int64Range(-1<<40, 1<<40).Draw(t, "t0")
		for i := 0; i < n; i++ {
			out.Samples = append(out.Samples, tsdb.Sample{
				T: ts,
				V: rapid.Float64().Draw(t, "v"),
			})
			ts += rapid.Int64Range(1, 60_000).Draw(t, "dt")
		}
		return out
	})
}

// TestBlock_TruncatedIndexNeverPanics walks every prefix of a real index file.
// Corruption arrives as arbitrary bytes, so every length check in the parser
// has to hold on input it was not designed for; the requirement is an error,
// never a panic and never a silently wrong block.
func TestBlock_TruncatedIndexNeverPanics(t *testing.T) {
	_, b, _ := writeFixture(t, 4)
	full, err := os.ReadFile(filepath.Join(b.Dir(), IndexFilename))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, IndexFilename)
	for n := 0; n < len(full); n++ {
		if err := os.WriteFile(path, full[:n], 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := openIndexReader(dir); err == nil {
			t.Fatalf("a %d-byte prefix of the index opened successfully", n)
		}
	}
	// And the complete file still works, so the loop above proves something.
	if err := os.WriteFile(path, full, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openIndexReader(dir); err != nil {
		t.Fatalf("the intact index failed to open: %v", err)
	}
}

// TestBlock_CorruptIndexBodyIsRejected flips bytes throughout the file. The
// whole-file checksum is what makes this cheap to get right: a reader does not
// have to be robust against every possible bad value, only against reading a
// file whose checksum it has not verified.
func TestBlock_CorruptIndexBodyIsRejected(t *testing.T) {
	_, b, _ := writeFixture(t, 4)
	full, err := os.ReadFile(filepath.Join(b.Dir(), IndexFilename))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, IndexFilename)
	for at := 0; at < len(full); at += 7 {
		damaged := append([]byte(nil), full...)
		damaged[at] ^= 0x20
		if err := os.WriteFile(path, damaged, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := openIndexReader(dir); err == nil {
			t.Fatalf("a byte flip at offset %d went undetected", at)
		}
	}
}

func TestChunkReader_RejectsBadReferences(t *testing.T) {
	// The index could point anywhere if it were damaged in a way the checksum
	// missed (it cannot, but the reader must not rely on that).
	_, b, _ := writeFixture(t, 4)
	r := b.chunks
	good := b.ix.series[0].chunks[0]

	cases := []struct {
		name        string
		off, recLen uint64
	}{
		{"length of zero", good.off, 0},
		{"absurd length", good.off, 1 << 30},
		{"offset past the end", uint64(r.size) + 1, good.recLen},
		{"record running past the end", uint64(r.size) - 2, good.recLen},
		{"misaligned offset", good.off + 1, good.recLen},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := r.at(c.off, c.recLen); err == nil {
				t.Errorf("at(%d, %d) succeeded", c.off, c.recLen)
			}
		})
	}
	if _, _, err := r.at(good.off, good.recLen); err != nil {
		t.Errorf("the good reference failed: %v", err)
	}
}

func TestBlock_MissingFiles(t *testing.T) {
	_, b, _ := writeFixture(t, 2)
	src := b.Dir()
	_ = b.Close()

	for _, missing := range []string{MetaFilename, IndexFilename, ChunksFilename} {
		t.Run("without "+missing, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range []string{MetaFilename, IndexFilename, ChunksFilename} {
				if name == missing {
					continue
				}
				data, err := os.ReadFile(filepath.Join(src, name))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Open(dir); err == nil {
				t.Errorf("a block without %s opened", missing)
			}
		})
	}
	t.Run("a chunks file that is not one", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{MetaFilename, IndexFilename} {
			data, err := os.ReadFile(filepath.Join(src, name))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		// Right length, wrong magic.
		if err := os.WriteFile(filepath.Join(dir, ChunksFilename),
			[]byte("nope!"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir); err == nil {
			t.Error("a block whose chunks file has the wrong magic opened")
		}
		if err := os.WriteFile(filepath.Join(dir, ChunksFilename),
			[]byte{0x44, 0x44, 0x43, 0x48, 99}, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir); err == nil {
			t.Error("a chunks file from another format version opened")
		}
		if err := os.WriteFile(filepath.Join(dir, ChunksFilename), []byte{1}, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir); err == nil {
			t.Error("a chunks file too short for a header opened")
		}
	})
}

func TestBlock_SelectWithoutAMetricUsesTheWholeBlock(t *testing.T) {
	// A selector with no metric and only a negative matcher has no positive
	// list to start from, so it subtracts from the universe — the one case
	// that reads AllSeries.
	_, b, want := writeFixture(t, 2)
	got, err := b.Select(tsdb.Selector{
		Matchers: []tsdb.Matcher{{Key: "env", Value: "staging", Type: tsdb.NotEqual}},
	}, math.MinInt64, math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	var expect int
	for _, s := range want {
		if v, _ := s.Series.Get("env"); v != "staging" {
			expect++
		}
	}
	if len(got) != expect {
		t.Errorf("selected %d series, want %d", len(got), expect)
	}
}

func TestWriter_ErrorsOnAClosedOrUnwritableBlock(t *testing.T) {
	parent := t.TempDir()
	w, err := NewWriter(parent, WriterOptions{Now: epoch})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddSeries(ref("m", "env:prod"), []tsdb.Sample{{T: 1, V: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := w.AddSeries(ref("m", "env:dev"), nil); err != nil {
		t.Errorf("an empty series should be a no-op, got %v", err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err == nil {
		t.Error("closing twice succeeded")
	}
	if err := w.AddSeries(ref("m", "env:prod"), []tsdb.Sample{{T: 2}}); err == nil {
		t.Error("adding to a closed writer succeeded")
	}
	if err := w.Abort(); err != nil {
		t.Errorf("aborting a closed writer: %v", err)
	}

	t.Run("an unwritable parent", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root; permissions do not apply")
		}
		locked := filepath.Join(t.TempDir(), "blocks")
		if err := os.MkdirAll(locked, 0o500); err != nil {
			t.Fatal(err)
		}
		if _, err := NewWriter(locked, WriterOptions{Now: epoch}); err == nil {
			t.Error("a writer was created under a read-only directory")
		}
	})
}

func TestWriter_AbortLeavesNothingBehind(t *testing.T) {
	parent := t.TempDir()
	w, err := NewWriter(parent, WriterOptions{Now: epoch})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddSeries(ref("m", "env:prod"), []tsdb.Sample{{T: 1, V: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(parent); len(entries) != 0 {
		t.Errorf("Abort left %d entries behind", len(entries))
	}
}

func TestCleanup_OnADirectoryThatIsNotThere(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	for name, fn := range map[string]func(string) ([]string, error){
		"CleanTmp":       CleanTmp,
		"CleanCondemned": CleanCondemned,
	} {
		removed, err := fn(missing)
		if err != nil || removed != nil {
			t.Errorf("%s(%q) = %v, %v; want nothing and no error", name, missing, removed, err)
		}
	}
	// A file where a block directory would be is left alone, not deleted.
	parent := t.TempDir()
	stray := filepath.Join(parent, "notes.txt")
	if err := os.WriteFile(stray, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if removed, err := CleanTmp(parent); err != nil || len(removed) != 0 {
		t.Errorf("CleanTmp removed %v, err %v", removed, err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("CleanTmp deleted a plain file: %v", err)
	}
}

func TestReadMeta_Errors(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadMeta(dir); err == nil {
		t.Error("ReadMeta of a directory without meta.json succeeded")
	}
	if err := os.WriteFile(filepath.Join(dir, MetaFilename), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMeta(dir); err == nil {
		t.Error("ReadMeta of malformed JSON succeeded")
	}
}

func TestIndexReader_LookupsForAbsentKeys(t *testing.T) {
	_, b, _ := writeFixture(t, 2)
	ix := b.ix
	for _, c := range []struct{ key, value string }{
		{"nope", "prod"},  // unknown key
		{"env", "nope"},   // known key, unknown value
		{"route", "prod"}, // both known, pair absent
		{"", ""},          // empty
	} {
		if got := ix.Postings(c.key, c.value); got != nil {
			t.Errorf("Postings(%q, %q) = %v, want nil", c.key, c.value, got)
		}
	}
	if got := ix.TagValues("nope"); got != nil {
		t.Errorf("TagValues of an unknown key = %v", got)
	}
	if got := ix.AllSeries(); len(got) != len(ix.series) {
		t.Errorf("AllSeries has %d ids for %d series", len(got), len(ix.series))
	}
	keys := ix.TagKeys()
	if !sort.StringsAreSorted(keys) {
		t.Errorf("TagKeys are not sorted: %v", keys)
	}
	if len(keys) != 3 { // __name__, env, route
		t.Errorf("TagKeys = %v, want three", keys)
	}
}

func TestBlock_StringOfTheIDIsStableAcrossAReopen(t *testing.T) {
	parent, b, _ := writeFixture(t, 2)
	first := b.Meta().ULID
	_ = b.Close()
	blocks, err := OpenAll(parent)
	if err != nil || len(blocks) != 1 {
		t.Fatalf("OpenAll gave %d blocks, err %v", len(blocks), err)
	}
	defer func() { _ = blocks[0].Close() }()
	if got := blocks[0].Meta().ULID; got != first {
		t.Errorf("block id changed across a reopen: %s != %s", got, first)
	}
	if got := blocks[0].Dir(); got != filepath.Join(parent, first.String()) {
		t.Errorf("Dir = %s", got)
	}
}

func TestOpenAll_ReportsABrokenBlock(t *testing.T) {
	parent, b, _ := writeFixture(t, 2)
	dir := b.Dir()
	_ = b.Close()
	if err := os.WriteFile(filepath.Join(dir, IndexFilename), []byte("broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAll(parent); err == nil {
		t.Error("OpenAll ignored a block it could not read")
	} else if !strings.Contains(err.Error(), filepath.Base(dir)) {
		t.Errorf("the error does not name the block: %v", err)
	}
}

// patchChunk rewrites a chunk record in place and fixes its checksum, so the
// damage is invisible to the crc and has to be caught further in.
func patchChunk(t *testing.T, path string, off, recLen uint64, mutate func(body []byte)) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rec := make([]byte, recLen)
	if _, err := f.ReadAt(rec, int64(off)); err != nil {
		t.Fatal(err)
	}
	length, used := binary.Uvarint(rec)
	body := rec[used : uint64(used)+length]
	mutate(body)
	binary.BigEndian.PutUint32(rec[uint64(used)+length:], crc32.Checksum(body, castagnoli))
	if _, err := f.WriteAt(rec, int64(off)); err != nil {
		t.Fatal(err)
	}
}

func TestBlock_AChecksumIsNotAGuaranteeOfMeaning(t *testing.T) {
	// A crc proves the bytes are the ones that were written. It says nothing
	// about whether they decode, so the layers above still have to check —
	// and must report, not panic or return nonsense.
	t.Run("an encoding this build does not know", func(t *testing.T) {
		_, b, _ := writeFixture(t, 4)
		dir, cr := b.Dir(), b.ix.series[0].chunks[0]
		_ = b.Close()
		patchChunk(t, filepath.Join(dir, ChunksFilename), cr.off, cr.recLen,
			func(body []byte) { body[0] = 99 })

		reopened, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = reopened.Close() }()
		_, err = reopened.Select(tsdb.Selector{Metric: "db.query.count"}, math.MinInt64, math.MaxInt64)
		if err == nil {
			t.Error("an unknown chunk encoding was decoded anyway")
		} else if !strings.Contains(err.Error(), "encoding") {
			t.Errorf("the error does not mention the encoding: %v", err)
		}
	})

	t.Run("gorilla bytes that are not a gorilla chunk", func(t *testing.T) {
		_, b, _ := writeFixture(t, 4)
		dir, cr := b.Dir(), b.ix.series[0].chunks[0]
		_ = b.Close()
		patchChunk(t, filepath.Join(dir, ChunksFilename), cr.off, cr.recLen,
			func(body []byte) {
				for i := range body[1:] {
					body[1+i] = 0xff // a sample count and a bitstream that disagree
				}
			})
		reopened, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = reopened.Close() }()
		if _, err := reopened.All(); err == nil {
			t.Error("an undecodable chunk was read without complaint")
		}
	})
}

func TestDelete_IsIdempotent(t *testing.T) {
	// Retention and compaction both retry after a crash, and the block they
	// meant to delete may already be gone. Failing there would turn a
	// completed deletion into a permanent error.
	parent, b, _ := writeFixture(t, 2)
	dir := b.Dir()
	_ = b.Close()
	for i := 0; i < 2; i++ {
		if err := Delete(dir); err != nil {
			t.Fatalf("Delete call %d: %v", i+1, err)
		}
	}
	if err := Delete(filepath.Join(parent, "01JNEVEREXISTED")); err != nil {
		t.Errorf("deleting a block that never existed: %v", err)
	}
}

// The error paths that only a failing disk reaches. A read-only directory is
// the cheapest honest way to produce one: ENOSPC and EIO arrive at the same
// places in the code.
func TestBlock_DiskFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permissions do not apply")
	}

	t.Run("a block that cannot be finished leaves nothing behind", func(t *testing.T) {
		parent := t.TempDir()
		w, err := NewWriter(parent, WriterOptions{Now: epoch})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.AddSeries(ref("m", "env:prod"), []tsdb.Sample{{T: 1, V: 1}}); err != nil {
			t.Fatal(err)
		}
		// The disk fills between the chunks and the index.
		if err := os.Chmod(w.tmpDir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(w.tmpDir, 0o700) })
		if _, err := w.Close(); err == nil {
			t.Error("Close succeeded although the index could not be written")
		}
		// Whatever happened, no half-block is visible.
		blocks, err := OpenAll(parent)
		if err != nil || len(blocks) != 0 {
			t.Errorf("OpenAll gave %d blocks, err %v", len(blocks), err)
		}
	})

	t.Run("cleanup reports what it could not remove", func(t *testing.T) {
		parent := t.TempDir()
		stuck := filepath.Join(parent, "01JSTUCK.tmp")
		if err := os.MkdirAll(filepath.Join(stuck, "inner"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(stuck, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(stuck, 0o700) })
		if _, err := CleanTmp(parent); err == nil {
			t.Error("CleanTmp reported success on a directory it could not remove")
		}

		condemned := filepath.Join(parent, "01JCONDEMNED")
		if err := os.MkdirAll(filepath.Join(condemned, "inner"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(condemned, tombstoneFilename), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(condemned, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(condemned, 0o700) })
		if _, err := CleanCondemned(parent); err == nil {
			t.Error("CleanCondemned reported success on a directory it could not remove")
		}
	})

	t.Run("a block that cannot be deleted", func(t *testing.T) {
		parent, b, _ := writeFixture(t, 2)
		dir := b.Dir()
		_ = b.Close()
		if err := os.Chmod(parent, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
		if err := Delete(dir); err == nil {
			t.Error("Delete succeeded on a read-only parent")
		}
	})
}

// FuzzIndexReader feeds the parser arbitrary bodies under a *valid* checksum.
//
// Corrupted files are the easy case: the whole-file crc catches them before a
// single length prefix is read, which is why the reader's own bounds checks
// are nearly unreachable from a bit flip. This target reaches them anyway, by
// constructing files that are internally inconsistent but checksum correctly —
// the shape a bug in the *writer*, or a file from a future version, would
// have. The requirement is the same either way: an error, never a panic,
// never a silently wrong block.
func FuzzIndexReader(f *testing.F) {
	full, err := os.ReadFile(filepath.Join(seedBlock(f), IndexFilename))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(full[indexHeadLen : len(full)-4]) // a real body
	f.Add([]byte{})
	f.Add(make([]byte, tocLen))
	f.Add(full[indexHeadLen : indexHeadLen+20])

	dir := f.TempDir()
	path := filepath.Join(dir, IndexFilename)
	f.Fuzz(func(t *testing.T, body []byte) {
		file := make([]byte, 0, len(body)+indexHeadLen+4)
		file = binary.BigEndian.AppendUint32(file, indexMagic)
		file = append(file, indexVersion)
		file = append(file, body...)
		file = binary.BigEndian.AppendUint32(file, crc32.Checksum(file, castagnoli))
		if err := os.WriteFile(path, file, 0o644); err != nil {
			t.Fatal(err)
		}
		r, err := openIndexReader(dir)
		if err != nil {
			return
		}
		// If it parsed, every lookup it offers must also be safe: the postings
		// section is only decoded on demand, so a file can pass the header
		// checks and still hold nonsense there.
		for _, key := range r.TagKeys() {
			for _, v := range r.TagValues(key) {
				for _, id := range r.Postings(key, v) {
					if id >= uint64(len(r.series)) {
						return // a bad id is fine to report, not to follow
					}
				}
			}
		}
		_ = r.AllSeries()
	})
}

// seedBlock writes a block and returns its directory, for fuzz seeds.
func seedBlock(f *testing.F) string {
	f.Helper()
	parent := f.TempDir()
	m, err := Write(parent, fixture(4), WriterOptions{Now: epoch})
	if err != nil {
		f.Fatal(err)
	}
	return filepath.Join(parent, m.ULID.String())
}

// patchIndex rewrites bytes of a real index file and fixes the checksum, so
// the file is internally wrong but passes the crc. mutate receives the body
// (everything after the header, before the crc) and the four TOC offsets.
func patchIndex(t *testing.T, dir string, mutate func(body []byte, offs []uint64)) {
	t.Helper()
	path := filepath.Join(dir, IndexFilename)
	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := file[:len(file)-4]
	toc := body[len(body)-tocLen:]
	offs := make([]uint64, 4)
	for i := range offs {
		offs[i] = binary.BigEndian.Uint64(toc[i*8:])
	}
	mutate(body, offs)
	file = append(body, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(file[len(file)-4:], crc32.Checksum(body, castagnoli))
	if err := os.WriteFile(path, file, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestIndexReader_RejectsAWellFormedLie covers what the checksum cannot: a
// file whose bytes are exactly what was written, but whose contents point at
// things that are not there. Only the parser's own bounds checks stand between
// this and a panic, so each one gets a case.
func TestIndexReader_RejectsAWellFormedLie(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(body []byte, offs []uint64)
	}{
		{"more symbols than bytes", func(body []byte, offs []uint64) {
			// The count is a uvarint; 0xff 0xff 0x7f is a large one.
			copy(body[offs[0]:], []byte{0xff, 0xff, 0x7f})
		}},
		{"a symbol longer than the section", func(body []byte, offs []uint64) {
			copy(body[offs[0]+1:], []byte{0xff, 0xff, 0x7f})
		}},
		{"a series naming a symbol that does not exist", func(body []byte, offs []uint64) {
			copy(body[offs[1]+1:], []byte{0xfe, 0x7f})
		}},
		{"a series claiming more tags than bytes", func(body []byte, offs []uint64) {
			copy(body[offs[1]+2:], []byte{0xff, 0xff, 0x7f})
		}},
		{"a postings table entry pointing past the file", func(body []byte, offs []uint64) {
			copy(body[offs[3]+3:], []byte{0xff, 0xff, 0xff, 0x7f})
		}},
		{"a TOC section offset past the file", func(body []byte, offs []uint64) {
			binary.BigEndian.PutUint64(body[len(body)-tocLen:], uint64(len(body))+1)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, b, _ := writeFixture(t, 4)
			dir := b.Dir()
			_ = b.Close()
			patchIndex(t, dir, c.mutate)
			if _, err := openIndexReader(dir); err == nil {
				t.Error("the reader accepted it")
			}
		})
	}
}

func TestWriter_RejectsSamplesOutOfOrder(t *testing.T) {
	// A chunk is an append-only bitstream of deltas, so the writer cannot fix
	// an unsorted input — it can only refuse it. Callers (the head cut and
	// compaction's merge) both produce sorted samples; this is the check that
	// says so out loud.
	parent := t.TempDir()
	_, err := Write(parent, []tsdb.SeriesSamples{{
		Series:  ref("m", "env:prod"),
		Samples: []tsdb.Sample{{T: 200, V: 1}, {T: 100, V: 2}},
	}}, WriterOptions{Now: epoch})
	if err == nil {
		t.Fatal("unsorted samples were accepted")
	}
	if entries, _ := os.ReadDir(parent); len(entries) != 0 {
		t.Errorf("the failed write left %d entries behind", len(entries))
	}
}
