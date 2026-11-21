package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/block"
)

// threeBlocks writes 0..300 s of one series at one-second spacing, cuts three one-minute blocks
// ([0,60), [60,120), [120,180)), leaves 180..300 s in the log, and closes the database. It returns the
// directory and the three blocks' directories in time order.
func threeBlocks(t *testing.T) (dir string, blocks []string) {
	t.Helper()
	dir = t.TempDir()
	db, _, _ := open(t, Options{Dir: dir, BlockRange: time.Minute})
	fill(t, db, ref("m", "env:prod"), 0, 1000, 301)
	for i := 0; i < 3; i++ {
		if err := db.CutBlock(); err != nil {
			t.Fatalf("cut %d: %v", i, err)
		}
	}
	for _, b := range db.Blocks() {
		blocks = append(blocks, b.Dir())
	}
	if len(blocks) != 3 {
		t.Fatalf("%d blocks, want 3", len(blocks))
	}
	sort.Strings(blocks)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, blocks
}

// secondsOf is the whole seconds of every sample a query returned, in order.
func secondsOf(t *testing.T, db *DB, fromS, toS int64) []int64 {
	t.Helper()
	set, err := db.Select(ctx, tsdb.Selector{Metric: "m"}, fromS*1000, toS*1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []int64
	for set.Next() {
		it := set.Iterator()
		for it.Next() {
			out = append(out, it.At().T/1000)
		}
		if err := it.Err(); err != nil {
			t.Fatal(err)
		}
	}
	if err := set.Err(); err != nil {
		t.Fatal(err)
	}
	_ = set.Close()
	return out
}

func seconds(from, to int64) []int64 {
	var out []int64
	for s := from; s <= to; s++ {
		out = append(out, s)
	}
	return out
}

func equalSeconds(a, b []int64) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

func openLogged(t *testing.T, dir string) (*DB, *lockedBuffer) {
	t.Helper()
	journal := &lockedBuffer{}
	db, _, _ := open(t, Options{Dir: dir, BlockRange: time.Minute,
		Logger: slog.New(slog.NewTextHandler(journal, &slog.HandlerOptions{Level: slog.LevelInfo}))})
	return db, journal
}

// The reason for all of it: one damaged block must not take the database down. The rest is served, the
// time the block covered is simply missing (and said to be, in the log and the count), the block is
// not deleted, and a restart does not lose track of it.
func TestDB_StartsWithoutADamagedBlockAndServesTheRest(t *testing.T) {
	dir, blocks := threeBlocks(t)
	victim := blocks[1] // [60, 120)
	original, err := os.ReadFile(filepath.Join(victim, block.IndexFilename))
	if err != nil {
		t.Fatal(err)
	}
	damaged := append([]byte(nil), original...)
	damaged[40] ^= 0xFF
	if err := os.WriteFile(filepath.Join(victim, block.IndexFilename), damaged, 0o644); err != nil {
		t.Fatal(err)
	}

	db, journal := openLogged(t, dir)
	if n := db.QuarantinedBlocks(); n != 1 {
		t.Fatalf("QuarantinedBlocks = %d, want 1", n)
	}
	if n := len(db.Blocks()); n != 2 {
		t.Fatalf("%d blocks open, want the 2 that are whole", n)
	}
	// Everything but the minute the damaged block held, exactly: no sample invented, none lost beside it.
	want := append(seconds(0, 59), seconds(120, 300)...)
	if got := secondsOf(t, db, 0, 300); !equalSeconds(got, want) {
		t.Fatalf("a query over 0..300 s returned %d samples, want the %d outside 60..119: %v", len(got), len(want), got)
	}
	// Loud: an Error line that says which block, where it went, and what time is missing.
	log := journal.all()
	for _, must := range []string{"level=ERROR", "block quarantined", "block=" + filepath.Base(victim), "moved_to=", ".corrupt-", "min_time=1970-01-01T00:01:00Z", "max_time=1970-01-01T00:01:59Z"} {
		if !strings.Contains(log, must) {
			t.Errorf("the log lacks %q:\n%s", must, log)
		}
	}
	// Not deleted: the files are where the log says, and the block is not at its own name.
	if _, err := os.Stat(victim); !os.IsNotExist(err) {
		t.Errorf("the damaged block is still at its own name: %v", err)
	}
	set, _ := filepath.Glob(victim + block.QuarantineMark + "*")
	if len(set) != 1 {
		t.Fatalf("%d quarantined directories, want 1", len(set))
	}
	for _, f := range []string{block.MetaFilename, block.IndexFilename, block.ChunksFilename} {
		if _, err := os.Stat(filepath.Join(set[0], f)); err != nil {
			t.Errorf("the quarantined block lost %s: %v", f, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart: the set-aside block is still counted, not moved again, and not logged as a new event.
	db, journal = openLogged(t, dir)
	if n := db.QuarantinedBlocks(); n != 1 {
		t.Errorf("after a restart QuarantinedBlocks = %d, want it still 1", n)
	}
	if strings.Contains(journal.all(), "block quarantined") {
		t.Errorf("a restart quarantined it again:\n%s", journal.all())
	}
	if got := secondsOf(t, db, 0, 300); !equalSeconds(got, want) {
		t.Errorf("after a restart a query returned %d samples, want %d", len(got), len(want))
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Restoring it is a rename and a restart: put the good index back, take the mark off the name.
	if err := os.WriteFile(filepath.Join(set[0], block.IndexFilename), original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(set[0], victim); err != nil {
		t.Fatal(err)
	}
	db, _ = openLogged(t, dir)
	if n := db.QuarantinedBlocks(); n != 0 {
		t.Errorf("after restoring it QuarantinedBlocks = %d, want 0", n)
	}
	if got := secondsOf(t, db, 0, 300); !equalSeconds(got, seconds(0, 300)) {
		t.Errorf("after restoring it a query returned %d samples, want all 301", len(got))
	}
}

// What the open cannot see: a flipped bit inside a chunk. The block is not quarantined (the open did not
// read the chunk), the query that does read it fails with an error and no samples (never a wrong value),
// every such failure is counted, and the log says so once per block, not once per request. Queries that do
// not reach the block go on working.
func TestDB_ADamagedChunkFailsTheQueryLoudlyNotWithAWrongAnswer(t *testing.T) {
	dir, blocks := threeBlocks(t)
	victim := filepath.Join(blocks[1], block.ChunksFilename)
	b, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-8] ^= 0xFF // inside the last chunk, before its checksum
	if err := os.WriteFile(victim, b, 0o644); err != nil {
		t.Fatal(err)
	}

	db, journal := openLogged(t, dir)
	if n := db.QuarantinedBlocks(); n != 0 {
		t.Fatalf("a chunk-level flip was quarantined at open (%d): the open cannot have seen it", n)
	}
	if n := db.CorruptReads(); n != 0 {
		t.Fatalf("CorruptReads = %d before any read", n)
	}
	for i := 1; i <= 3; i++ {
		set, err := db.Select(ctx, tsdb.Selector{Metric: "m"}, 0, 300_000)
		if !errors.Is(err, block.ErrCorrupt) {
			t.Fatalf("query %d: err %v, want one wrapping ErrCorrupt", i, err)
		}
		if set != nil {
			t.Fatalf("query %d returned a result set along with the error", i)
		}
		if n := db.CorruptReads(); n != uint64(i) {
			t.Errorf("after %d failed queries CorruptReads = %d", i, n)
		}
	}
	if n := strings.Count(journal.all(), "a query read a damaged block"); n != 1 {
		t.Errorf("the damaged block was logged %d times for 3 queries, want once:\n%s", n, journal.all())
	}
	// A range that never touches the damaged block is unaffected, and does not count.
	if got := secondsOf(t, db, 120, 300); !equalSeconds(got, seconds(120, 300)) {
		t.Errorf("a query outside the damaged block returned %d samples", len(got))
	}
	if n := db.CorruptReads(); n != 3 {
		t.Errorf("a query that read nothing damaged moved CorruptReads to %d", n)
	}
}

// Only damage is counted as damage: a read that fails for any other reason (a cancelled query, an
// I/O error) must not move a gauge that an operator alerts on.
func TestDB_OnlyDamageIsCountedAsCorruptRead(t *testing.T) {
	dir, _ := threeBlocks(t)
	db, journal := openLogged(t, dir)
	b := db.Blocks()[0]
	db.noteCorruptRead(b, errors.New("read tcp: i/o timeout"))
	db.noteCorruptRead(b, context.Canceled)
	if n := db.CorruptReads(); n != 0 {
		t.Fatalf("errors that are not damage moved CorruptReads to %d", n)
	}
	if strings.Contains(journal.all(), "a query read a damaged block") {
		t.Errorf("an error that is not damage was logged as damage:\n%s", journal.all())
	}
	db.noteCorruptRead(b, fmt.Errorf("block: %w: a chunk", block.ErrCorrupt))
	if n := db.CorruptReads(); n != 1 {
		t.Errorf("a damaged read moved CorruptReads to %d, want 1", n)
	}
}
