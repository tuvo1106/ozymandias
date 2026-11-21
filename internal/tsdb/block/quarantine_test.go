package block

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
)

// twoBlocks writes two blocks under one parent and returns their directories: the first is the one
// a test damages, the second its healthy neighbour.
func twoBlocks(t *testing.T) (parent, victim, neighbour string, want []tsdb.SeriesSamples) {
	t.Helper()
	parent = t.TempDir()
	want = fixture(4)
	for i := 0; i < 2; i++ {
		if _, err := Write(parent, want, WriterOptions{Now: epoch}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 2 {
		t.Fatalf("expected two blocks, got %d, %v", len(entries), err)
	}
	return parent, filepath.Join(parent, entries[0].Name()), filepath.Join(parent, entries[1].Name()), want
}

// snapshot is every file of a directory, by name, so that a quarantine can be shown to have kept all
// of them as they were.
func snapshot(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = b
	}
	return out
}

func flipByte(t *testing.T, path string, at int) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if at < 0 {
		at = len(b) + at
	}
	b[at] ^= 0xFF
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func truncateTo(t *testing.T, path string, n int64) {
	t.Helper()
	if err := os.Truncate(path, n); err != nil {
		t.Fatal(err)
	}
}

// Every way the files OpenAll reads can be damaged: the block is set aside, nothing is deleted, the
// neighbour is opened and answers, and a second start leaves it all as it was.
func TestOpenAll_QuarantinesEachKindOfDamage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		damage   func(t *testing.T, dir string)
		hasRange bool // whether meta.json still parses, so the quarantine can say what time it claimed
		min, max int64
	}{
		{"meta.json is not JSON", func(t *testing.T, d string) {
			if err := os.WriteFile(filepath.Join(d, MetaFilename), []byte("{not json"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, false, 0, 0},
		{"meta.json ends before it starts", func(t *testing.T, d string) {
			m, err := ReadMeta(d)
			if err != nil {
				t.Fatal(err)
			}
			m.MinTime, m.MaxTime = 500, 100
			if err := writeMeta(d, m); err != nil {
				t.Fatal(err)
			}
		}, true, 500, 100}, // the numbers are the damage, and are still reported as what it claimed
		{"index.idx has a flipped bit", func(t *testing.T, d string) { flipByte(t, filepath.Join(d, IndexFilename), 40) }, true, 0, 30_000},
		{"index.idx is cut short", func(t *testing.T, d string) { truncateTo(t, filepath.Join(d, IndexFilename), 10) }, true, 0, 30_000},
		{"index.idx is missing", func(t *testing.T, d string) {
			if err := os.Remove(filepath.Join(d, IndexFilename)); err != nil {
				t.Fatal(err)
			}
		}, true, 0, 30_000},
		{"chunks.dat is missing", func(t *testing.T, d string) {
			if err := os.Remove(filepath.Join(d, ChunksFilename)); err != nil {
				t.Fatal(err)
			}
		}, true, 0, 30_000},
		{"chunks.dat has the wrong magic", func(t *testing.T, d string) { flipByte(t, filepath.Join(d, ChunksFilename), 0) }, true, 0, 30_000},
		{"chunks.dat is shorter than its header", func(t *testing.T, d string) { truncateTo(t, filepath.Join(d, ChunksFilename), 2) }, true, 0, 30_000},
		{"chunks.dat is cut short, which only the index can say", func(t *testing.T, d string) {
			info, err := os.Stat(filepath.Join(d, ChunksFilename))
			if err != nil {
				t.Fatal(err)
			}
			truncateTo(t, filepath.Join(d, ChunksFilename), info.Size()-3)
		}, true, 0, 30_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, victim, neighbour, want := twoBlocks(t)
			tc.damage(t, victim)
			before := snapshot(t, victim)

			blocks, quarantined, err := OpenAll(parent, epoch)
			if err != nil {
				t.Fatalf("OpenAll failed on a damaged block instead of setting it aside: %v", err)
			}
			defer func() {
				for _, b := range blocks {
					_ = b.Close()
				}
			}()
			if len(quarantined) != 1 || len(blocks) != 1 {
				t.Fatalf("%d blocks opened, %d quarantined; want 1 and 1", len(blocks), len(quarantined))
			}
			q := quarantined[0]
			if !errors.Is(q.Cause, ErrCorrupt) {
				t.Errorf("the cause does not wrap ErrCorrupt: %v", q.Cause)
			}
			if q.Name != filepath.Base(victim) || !strings.HasPrefix(filepath.Base(q.MovedTo), q.Name+QuarantineMark) {
				t.Errorf("the quarantine names %q moved to %q; want %q set aside as %q<unix>", q.Name, q.MovedTo, filepath.Base(victim), filepath.Base(victim)+QuarantineMark)
			}
			if q.HasRange != tc.hasRange {
				t.Errorf("HasRange = %v, want %v", q.HasRange, tc.hasRange)
			}
			if tc.hasRange && (q.MinTime != tc.min || q.MaxTime != tc.max) {
				t.Errorf("the time lost is %d..%d, want %d..%d", q.MinTime, q.MaxTime, tc.min, tc.max)
			}
			if _, err := os.Stat(victim); !os.IsNotExist(err) {
				t.Errorf("the damaged block is still at its own name: %v", err)
			}
			// Nothing deleted, nothing edited: every file is there with the same bytes.
			after := snapshot(t, q.MovedTo)
			if len(after) != len(before) {
				t.Errorf("the quarantine holds %d files, the block had %d", len(after), len(before))
			}
			for name, b := range before {
				if !bytes.Equal(after[name], b) {
					t.Errorf("%s changed in the move", name)
				}
			}
			// The neighbour is whole and answers.
			if blocks[0].Dir() != neighbour {
				t.Errorf("the block that opened is %s, want the neighbour %s", blocks[0].Dir(), neighbour)
			}
			got, err := blocks[0].Select(tsdb.Selector{Metric: "db.query.count"}, math.MinInt64, math.MaxInt64)
			if err != nil || len(got) != 4 {
				t.Fatalf("the neighbour answered %d series, err %v; want its 4 of db.query.count", len(got), err)
			}
			_ = want

			// A second start: the set-aside block is left alone and still counted.
			for _, b := range blocks {
				_ = b.Close()
			}
			blocks2, again, err := OpenAll(parent, epoch)
			if err != nil || len(again) != 0 || len(blocks2) != 1 {
				t.Fatalf("second start: %d blocks, %d newly quarantined, err %v", len(blocks2), len(again), err)
			}
			blocks = blocks2
			if list, err := ListQuarantined(parent); err != nil || len(list) != 1 || list[0] != filepath.Base(q.MovedTo) {
				t.Errorf("ListQuarantined = %v, %v", list, err)
			}
			if !bytes.Equal(snapshot(t, q.MovedTo)["meta.json"], before["meta.json"]) {
				t.Error("a second start changed the quarantined block")
			}
		})
	}
}

// What is not damage must not be quarantined. A block written by another version of this software
// is whole, and a build that cannot read it would hide good data by moving it aside; an I/O error says
// nothing about the block. Both stop the start, as before, and leave the block where it is.
func TestOpenAll_DoesNotQuarantineWhatIsNotDamage(t *testing.T) {
	t.Run("a meta.json of another version", func(t *testing.T) {
		parent, victim, _, _ := twoBlocks(t)
		m, err := ReadMeta(victim)
		if err != nil {
			t.Fatal(err)
		}
		m.Version = MetaVersion + 1
		if err := writeMeta(victim, m); err != nil {
			t.Fatal(err)
		}
		blocks, quarantined, err := OpenAll(parent, epoch)
		if err == nil || errors.Is(err, ErrCorrupt) {
			t.Fatalf("OpenAll = %d blocks, %d quarantined, err %v; want a plain error that stops the start", len(blocks), len(quarantined), err)
		}
		if _, statErr := os.Stat(victim); statErr != nil {
			t.Errorf("a block of another version was moved: %v", statErr)
		}
	})
	t.Run("a chunks.dat of another version", func(t *testing.T) {
		parent, victim, _, _ := twoBlocks(t)
		b, err := os.ReadFile(filepath.Join(victim, ChunksFilename))
		if err != nil {
			t.Fatal(err)
		}
		b[4] = chunksVersion + 1
		if err := os.WriteFile(filepath.Join(victim, ChunksFilename), b, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, q, err := OpenAll(parent, epoch); err == nil || len(q) != 0 || errors.Is(err, ErrCorrupt) {
			t.Fatalf("OpenAll quarantined %d, err %v; want a plain error", len(q), err)
		}
		if _, statErr := os.Stat(victim); statErr != nil {
			t.Errorf("a block of another version was moved: %v", statErr)
		}
	})
	t.Run("an unreadable file", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can read anything")
		}
		parent, victim, _, _ := twoBlocks(t)
		p := filepath.Join(victim, IndexFilename)
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
		if _, q, err := OpenAll(parent, epoch); err == nil || len(q) != 0 || errors.Is(err, ErrCorrupt) {
			t.Fatalf("OpenAll quarantined %d, err %v; want a plain I/O error", len(q), err)
		}
		if _, statErr := os.Stat(victim); statErr != nil {
			t.Errorf("a block that could not be read was moved: %v", statErr)
		}
	})
}

// A damaged block that cannot be set aside stops the start: carrying on as if it had been would serve a
// database with a hole in it and say nothing. Nothing is left open.
func TestOpenAll_AFailedQuarantineIsAnErrorNotASkip(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can rename anywhere")
	}
	parent, victim, _, _ := twoBlocks(t)
	flipByte(t, filepath.Join(victim, IndexFilename), 40)
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	blocks, quarantined, err := OpenAll(parent, epoch)
	if err == nil {
		t.Fatalf("OpenAll went on with %d blocks and %d quarantined after the move failed", len(blocks), len(quarantined))
	}
	if blocks != nil || quarantined != nil {
		t.Errorf("a failed OpenAll returned %d blocks and %d quarantined along with its error", len(blocks), len(quarantined))
	}
	if !strings.Contains(err.Error(), filepath.Base(victim)) {
		t.Errorf("the error does not name the block: %v", err)
	}
}

// A quarantined directory is nobody's to open, clean or delete, whatever it looks like inside.
func TestQuarantinedDirectoriesAreLeftAlone(t *testing.T) {
	parent, victim, neighbour, _ := twoBlocks(t)
	// Set the healthy one aside by hand: it still has a perfectly good meta.json.
	set := victim + QuarantineMark + "1"
	if err := os.Rename(victim, set); err != nil {
		t.Fatal(err)
	}
	// And an empty one and a meta-less one, which CleanTmp would otherwise remove.
	for _, name := range []string{"01JEMPTY" + QuarantineMark + "1", "01JNOMETA" + QuarantineMark + "1"} {
		if err := os.MkdirAll(filepath.Join(parent, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(parent, "01JNOMETA"+QuarantineMark+"1", ChunksFilename), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if removed, err := CleanTmp(parent); err != nil || len(removed) != 0 {
		t.Fatalf("CleanTmp removed %v, err %v", removed, err)
	}
	blocks, quarantined, err := OpenAll(parent, epoch)
	if err != nil || len(quarantined) != 0 || len(blocks) != 1 || blocks[0].Dir() != neighbour {
		t.Fatalf("OpenAll: %d blocks, %d quarantined, err %v; want only the neighbour", len(blocks), len(quarantined), err)
	}
	_ = blocks[0].Close()
	if list, _ := ListQuarantined(parent); len(list) != 3 {
		t.Errorf("ListQuarantined = %v, want the three set-aside directories", list)
	}
}

// What opening does not read: a flipped bit inside a chunk. The block opens, and the damage is found
// by the query that reads that chunk, which gets an error wrapping ErrCorrupt and never a wrong sample.
func TestBlock_ABitFlippedInAChunkIsFoundByTheReadNotTheOpen(t *testing.T) {
	parent := t.TempDir()
	if _, err := Write(parent, fixture(4), WriterOptions{Now: epoch}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(parent)
	dir := filepath.Join(parent, entries[0].Name())
	flipByte(t, filepath.Join(dir, ChunksFilename), -8) // inside the last chunk, before its checksum

	blocks, quarantined, err := OpenAll(parent, epoch)
	if err != nil || len(quarantined) != 0 || len(blocks) != 1 {
		t.Fatalf("the open should not see damage inside a chunk: %d blocks, %d quarantined, %v", len(blocks), len(quarantined), err)
	}
	defer func() { _ = blocks[0].Close() }()
	got, err := blocks[0].Select(tsdb.Selector{}, math.MinInt64, math.MaxInt64)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Select = %d series, err %v; want an error wrapping ErrCorrupt", len(got), err)
	}
	if got != nil {
		t.Errorf("a failed Select also returned %d series: partial data presented as an answer", len(got))
	}
}

// meta.json said nothing is wrong and the chunks file simply is not the length the index needs: this
// is a block that was never going to answer a query for its last series.
func TestChunksEnd(t *testing.T) {
	r := &indexReader{series: []blockSeries{
		{chunks: []chunkRef{{off: 5, recLen: 10}, {off: 15, recLen: 20}}},
		{chunks: []chunkRef{{off: 40, recLen: 7}}},
	}}
	if got := r.chunksEnd(); got != 47 {
		t.Errorf("chunksEnd = %d, want 47", got)
	}
	if got := (&indexReader{}).chunksEnd(); got != 0 {
		t.Errorf("an index with no chunks needs %d bytes", got)
	}
	huge := &indexReader{series: []blockSeries{{chunks: []chunkRef{{off: math.MaxUint64 - 2, recLen: 10}}}}}
	if got := huge.chunksEnd(); got != math.MaxUint64 {
		t.Errorf("a claim that wraps the sum gave %d, not the largest value", got)
	}
}

// The index is checksummed over the whole file, so most damage is a failed checksum. Damage that the
// checksum does not see (a file written wrong and then summed, or tampered with and re-summed) is
// caught by the structure that opening decodes. Both are damage, not an I/O error and not a version.
func TestOpenAll_QuarantinesAnIndexThatIsWrongInsideAValidChecksum(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(body []byte)
	}{
		{"the table of contents points outside the file", func(body []byte) {
			binary.BigEndian.PutUint64(body[len(body)-tocLen+8:], uint64(len(body))+100)
		}},
		{"the symbol table claims more symbols than the file has bytes", func(body []byte) {
			off := binary.BigEndian.Uint64(body[len(body)-tocLen:])
			copy(body[off:], []byte{0xFF, 0xFF, 0xFF, 0xFF, 0x7F})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, victim, _, _ := twoBlocks(t)
			path := filepath.Join(victim, IndexFilename)
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			body := b[:len(b)-4]
			tc.change(body)
			binary.BigEndian.PutUint32(b[len(b)-4:], crc32.Checksum(body, castagnoli))
			if err := os.WriteFile(path, b, 0o644); err != nil {
				t.Fatal(err)
			}
			blocks, quarantined, err := OpenAll(parent, epoch)
			if err != nil || len(quarantined) != 1 || len(blocks) != 1 {
				t.Fatalf("OpenAll: %d blocks, %d quarantined, err %v; want the damaged index set aside", len(blocks), len(quarantined), err)
			}
			_ = blocks[0].Close()
			if !errors.Is(quarantined[0].Cause, ErrCorrupt) {
				t.Errorf("the cause does not wrap ErrCorrupt: %v", quarantined[0].Cause)
			}
		})
	}
}

// A directory with nothing in it is not a block that lost anything: OpenAll passes over it, as it always
// did, and does not set it aside (CleanTmp is what removes it).
func TestOpenAll_PassesOverAnEmptyDirectory(t *testing.T) {
	parent, _, neighbour, _ := twoBlocks(t)
	if err := os.MkdirAll(filepath.Join(parent, "01JEMPTYEMPTYEMPTYEMPTYEMP"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(parent, filepath.Base(neighbour))); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(parent)
	blocks, quarantined, err := OpenAll(parent, epoch)
	if err != nil || len(blocks) != 1 || len(quarantined) != 0 {
		t.Fatalf("OpenAll over %d entries: %d blocks, %d quarantined, err %v", len(entries), len(blocks), len(quarantined), err)
	}
	_ = blocks[0].Close()
}

// Two quarantines of the same directory in the same second (a restored block damaged again) must not stop
// the start by renaming onto a directory that exists.
func TestQuarantine_ASecondOneInTheSameSecondGetsAFreeName(t *testing.T) {
	parent, victim, _, _ := twoBlocks(t)
	first, err := Quarantine(victim, ErrCorrupt, epoch)
	if err != nil {
		t.Fatal(err)
	}
	// "Restore" it under its old name and damage it again, then set it aside in the same second.
	if err := os.Rename(first.MovedTo, victim); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(first.MovedTo, 0o755); err != nil { // the name the first one took is in use again
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first.MovedTo, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := Quarantine(victim, ErrCorrupt, epoch)
	if err != nil {
		t.Fatalf("a name already in use stopped the quarantine: %v", err)
	}
	if second.MovedTo == first.MovedTo || !strings.HasPrefix(second.MovedTo, victim+QuarantineMark) {
		t.Errorf("the second went to %q, the first to %q", second.MovedTo, first.MovedTo)
	}
	if list, _ := ListQuarantined(parent); len(list) != 2 {
		t.Errorf("ListQuarantined = %v, want both", list)
	}
}

// A read that finds damage says which block it was, so that whoever counts it does not have to know.
func TestBlock_ADamagedReadNamesTheBlock(t *testing.T) {
	parent := t.TempDir()
	if _, err := Write(parent, fixture(4), WriterOptions{Now: epoch}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(parent)
	dir := filepath.Join(parent, entries[0].Name())
	flipByte(t, filepath.Join(dir, ChunksFilename), -8)
	blocks, _, err := OpenAll(parent, epoch)
	if err != nil || len(blocks) != 1 {
		t.Fatal(err)
	}
	defer func() { _ = blocks[0].Close() }()
	_, err = blocks[0].Select(tsdb.Selector{}, math.MinInt64, math.MaxInt64)
	var damaged *DamagedError
	if !errors.As(err, &damaged) {
		t.Fatalf("err %v is not a *DamagedError", err)
	}
	if damaged.Block != blocks[0].Meta().ULID || damaged.Dir != dir || !errors.Is(err, ErrCorrupt) {
		t.Errorf("the error names %v at %q, want %v at %q (and wrap ErrCorrupt)", damaged.Block, damaged.Dir, blocks[0].Meta().ULID, dir)
	}
	// An error that is not damage is passed through as it is, not dressed up as one.
	plain := errors.New("plain")
	if got := blocks[0].damaged(plain); !errors.Is(got, plain) || errors.As(got, new(*DamagedError)) {
		t.Errorf("a plain error became %v", got)
	}
}
