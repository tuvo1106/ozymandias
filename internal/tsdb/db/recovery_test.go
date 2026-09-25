package db

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
)

// TestDB_LogTruncationKeepsWhatIsStillOnlyInTheHead is the one that hurts.
//
// Cutting a block truncates the log, on the reasoning that everything older
// than the cut is now in a block. That reasoning has a hidden premise: that
// the samples still in the head were written to the *current* segment. A
// segment rolls at WALSegmentSize, and the head holds hours of data, so on any
// real traffic the head's oldest samples are several segments back — and
// dropping those segments drops the only copy of them, because the head is
// memory.
//
// Nothing else catches this. Every other restart test writes a few kilobytes,
// so the log never rolls and the premise is accidentally true.
//
// # Why this does a fixed amount of work
//
// It used to fill until `walBytes(dir) >= 40 MiB`, against the default 32 MiB
// segment size, with the maintenance loop truncating the log underneath it the
// whole time. The log therefore shrank while the loop tried to grow it, and
// how much got written was decided by how fast the machine was: ~3 million
// samples in 6s here, ~10.6 million in 151s on a CI runner. Worse, on a slow
// enough runner the truncator can keep up with the appender indefinitely, and
// the loop stops terminating at all — the test hangs rather than fails.
//
// Lowering WALSegmentSize reaches the same premise — several rolled segments
// behind a head that still holds their samples — for a bounded, known number
// of appends. The assertions below check the premise was actually reached
// rather than assuming it.
//
// # What the full scale caught
//
// The same assertion used to fail intermittently on CI and never locally,
// losing samples in round numbers (issue #3). The difference turned out not to
// be the hardware but the pace: CI ran this about twelve times slower than a
// laptop, and `-race` slows a laptop into the same regime. `make soak` runs it
// with the race detector for exactly that reason, and it reproduced on the
// first attempt and every attempt after.
//
// What it was is nothing to do with restarts. [DB.Select] read the blocks
// before the head, so a block cut landing inside a query published its block
// after the block snapshot and truncated the head before the head read — and
// the range it moved appeared in neither. The count was short while the
// process was still up; the restart was a bystander.
// TestDB_AQueryNeverFallsIntoTheGapBetweenTheHeadAndANewBlock covers that
// directly and in two seconds. This stays as the end-to-end check, because it
// is what found it: nothing smaller had a block cut and a long query
// overlapping by accident.
//
// The `liveCount` assertion below is the part that pointed at it. Counting
// before the close as well as after splits "lost while running" from "lost
// across the restart", and those have no causes in common.
func TestDB_LogTruncationKeepsWhatIsStillOnlyInTheHead(t *testing.T) {
	nSeries, segmentSize, totalTs := 10, int64(256<<10), int64(90_000)
	const blockRange = 30 * time.Second
	if os.Getenv("OZY_SOAK") == "1" {
		// The shape that has actually failed on CI: the stock 32 MiB segment
		// size and ~35 block ranges of data behind it.
		segmentSize, totalTs = 0, 1_065_000
		t.Logf("soak: %d timestamps x %d series at the default segment size", totalTs, nSeries)
	}
	dir := t.TempDir()
	// The store's own account of what it did. When this fails it fails once,
	// after a minute of work, somewhere nobody can attach a debugger — and the
	// interesting events (which ranges were cut, what replay applied and what
	// it refused) have all already happened by then.
	journal := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(journal, &slog.HandlerOptions{Level: slog.LevelInfo}))
	db, err := Open(Options{
		Dir:            dir,
		BlockRange:     blockRange,
		Retention:      -1,
		WALSegmentSize: segmentSize,
		Logger:         logger,
	})
	if err != nil {
		t.Fatal(err)
	}

	refs := make([]tsdb.SeriesRef, nSeries)
	for i := range refs {
		refs[i] = ref("m", fmt.Sprintf("host:h%d", i))
	}
	batch := make([]tsdb.SeriesSamples, nSeries)
	var acked int64
	for ts := int64(0); ts < totalTs; ts++ {
		for i, r := range refs {
			batch[i] = tsdb.SeriesSamples{Series: r, Samples: []tsdb.Sample{{T: ts, V: float64(ts)}}}
		}
		res, err := db.Append(ctx, batch)
		if err != nil {
			t.Fatal(err)
		}
		if res.Samples != nSeries {
			t.Fatalf("at t=%d stored %d of %d: %+v", ts, res.Samples, nSeries, res.Rejected)
		}
		acked += int64(res.Samples)
	}

	logger.Info("test: the fill is finished", "acked", acked)
	if hs := db.HeadStats(); hs.Samples == 0 {
		t.Fatal("the head is empty before the cut")
	}

	// One cut: the oldest 30s goes to a block, the rest stays in the head —
	// and the log is truncated behind it.
	if err := db.CutBlock(); err != nil {
		t.Fatal(err)
	}

	// The premise, checked rather than assumed. A checkpoint exists only
	// because the truncation found records it had to carry forward — samples
	// still in the head whose segment was being dropped. Without one, the
	// head's data was all in the current segment, the case this test is about
	// was never reached, and a pass means nothing. Asserting on the segment
	// count instead does not survive the soak run, where the background
	// maintenance loop truncates while the fill is still going.
	if !hasCheckpoint(t, dir) {
		t.Fatalf("no checkpoint after the cut: nothing in the head was behind a truncated "+
			"segment, so this test proves nothing. wal=%v", walFiles(t, dir))
	}
	// Counted *before* the restart, because "lost" has two very different
	// causes and the restart is the obvious suspect for both. If the store is
	// already short here, the cut dropped acknowledged samples while the
	// process was still up and replay never had them to lose; if it is whole
	// here and short after the reopen, the log or the checkpoint is where they
	// went. Only one of those is a recovery bug.
	logger.Info("test: the explicit cut is done")
	liveCount := countSamples(t, db)
	logger.Info("test: counted the live store", "samples", liveCount)
	before := describe(t, db, dir)
	logger.Info("test: described the live store", "state", before)
	if liveCount != acked {
		t.Errorf("%d samples readable before any restart, %d were acknowledged — %d lost while "+
			"the process was still up\nstate: %s\nmissing ranges: %s",
			liveCount, acked, acked-liveCount, before, missingRanges(t, db, totalTs, nSeries))
	}
	logger.Info("test: closing")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	logger.Info("test: closed")

	reopened, err := Open(Options{
		Dir: dir, BlockRange: blockRange, Retention: -1,
		WALSegmentSize: segmentSize, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if got := countSamples(t, reopened); got != acked {
		// Everything a diagnosis needs, because this has only ever failed
		// somewhere nobody can attach a debugger.
		t.Errorf("%d samples survived a clean restart, %d were acknowledged (%d readable "+
			"before the close) — %d lost\nbefore close: %s\nafter reopen: %s\nmissing ranges: %s",
			got, acked, liveCount, acked-got, before, describe(t, reopened, dir),
			missingRanges(t, reopened, totalTs, nSeries))
		// The whole journal, in order. Roughly one line per block range plus
		// the test's own markers: enough to place every measurement against
		// the maintenance loop that was running underneath it, which is the
		// one thing a snapshot taken afterwards cannot tell you.
		t.Logf("what the store did, in order:\n%s", journal.all())
	}
}

// walFiles names the log directory's contents, tolerating one vanishing under
// the maintenance loop.
func walFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, walDirName))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// hasCheckpoint reports whether the log holds a checkpoint — the artefact that
// carries head-only records across a truncation.
func hasCheckpoint(t *testing.T, dir string) bool {
	t.Helper()
	for _, name := range walFiles(t, dir) {
		if strings.HasPrefix(name, "checkpoint.") {
			return true
		}
	}
	return false
}

// describe renders what a store is holding, for a failure message.
func describe(t *testing.T, db *DB, dir string) string {
	t.Helper()
	var b strings.Builder
	hs := db.HeadStats()
	fmt.Fprintf(&b, "head[%d,%d] series=%d chunks=%d; blocks=[", hs.MinT, hs.MaxT, hs.Series, hs.Chunks)
	for i, blk := range db.Blocks() {
		if i > 0 {
			b.WriteString(" ")
		}
		m := blk.Meta()
		fmt.Fprintf(&b, "L%d(%d,%d)x%d", m.Compaction.Level, m.MinTime, m.MaxTime, m.Stats.Samples)
	}
	fmt.Fprintf(&b, "]; wal=%v", walFiles(t, dir))
	return b.String()
}

// missingRanges reports which timestamps are not present exactly once per
// series, collapsed into runs, so a failure says *where* the hole is rather
// than only how big it was — and, per run, how many copies of each timestamp
// were actually found. The count matters: a run of `seen=0` is data that was
// lost, a run of `seen=1..9` against ten series is a partial loss inside the
// window, and a run above nSeries is duplication that a dedup should have
// removed. They are different bugs and the sample shortfall alone cannot tell
// them apart.
func missingRanges(t *testing.T, db *DB, totalTs int64, nSeries int) string {
	t.Helper()
	set, err := db.Select(ctx, tsdb.Selector{Metric: "m"}, math.MinInt64, math.MaxInt64)
	if err != nil {
		return "select failed: " + err.Error()
	}
	defer func() { _ = set.Close() }()
	seen := make([]int, totalTs)
	for set.Next() {
		it := set.Iterator()
		for it.Next() {
			if s := it.At(); s.T >= 0 && s.T < totalTs {
				seen[s.T]++
			}
		}
	}
	var b strings.Builder
	runs := 0
	for ts := int64(0); ts < totalTs; {
		if seen[ts] == nSeries {
			ts++
			continue
		}
		start, lo, hi := ts, seen[ts], seen[ts]
		for ts < totalTs && seen[ts] != nSeries {
			lo, hi = min(lo, seen[ts]), max(hi, seen[ts])
			ts++
		}
		if runs++; runs > 8 {
			b.WriteString(" ...")
			break
		}
		fmt.Fprintf(&b, " [%d,%d)seen=%d..%d", start, ts, lo, hi)
	}
	if runs == 0 {
		return "(none - every timestamp is present exactly nSeries times, so the shortfall is elsewhere)"
	}
	return strings.TrimSpace(b.String())
}

// TestDB_ATornTailIsRepairedOnOpen: a crash can leave a partial record at the
// end of the log. Replay correctly stops there. What must not happen is for
// the next process to append *behind* it — everything after an unreadable
// record is unreachable forever, so the store would acknowledge writes it
// silently loses at every subsequent restart.
func TestDB_ATornTailIsRepairedOnOpen(t *testing.T) {
	dir := t.TempDir()
	db, _, _ := open(t, Options{Dir: dir, BlockRange: time.Hour, Retention: -1, SyncOnAppend: true})
	r := ref("m", "host:a")
	for i := int64(0); i < 5; i++ {
		if _, err := db.Append(ctx, []tsdb.SeriesSamples{{Series: r, Samples: []tsdb.Sample{{T: i, V: float64(i)}}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// The crash: the last record never finished landing.
	seg := filepath.Join(dir, walDirName, "00000000.wal")
	info, err := os.Stat(seg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(seg, info.Size()-5); err != nil {
		t.Fatal(err)
	}

	second, err := Open(Options{Dir: dir, BlockRange: time.Hour, Retention: -1, SyncOnAppend: true})
	if err != nil {
		t.Fatal(err)
	}
	survived := countSamples(t, second)
	if survived != 4 {
		t.Fatalf("replay found %d samples after the torn write, want the 4 that landed whole", survived)
	}
	for i := int64(100); i < 105; i++ {
		if _, err := second.Append(ctx, []tsdb.SeriesSamples{{Series: r, Samples: []tsdb.Sample{{T: i, V: float64(i)}}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	third, err := Open(Options{Dir: dir, BlockRange: time.Hour, Retention: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = third.Close() }()
	if got := countSamples(t, third); got != survived+5 {
		t.Errorf("%d samples after the second restart, want %d — writes went in behind the torn record",
			got, survived+5)
	}
}

// TestDB_MetadataQueriesAreSafeDuringIntake. The head's index is documented as
// "not safe for concurrent use; the head serializes access", and the metadata
// path handed the live pointer out and called it with no lock held. Under
// -race this reports a data race; without it, it is a map read racing a map
// write, which Go turns into a fatal error that takes the process down rather
// than into a wrong answer.
//
// Creating a series is the write, so the appender makes a new one every
// iteration — but a bounded number of them. An unbounded appender would grow
// the index without limit while every metadata query scans and copies the
// whole thing, which is quadratic and pegs every core without testing
// anything the first few iterations did not.
func TestDB_MetadataQueriesAreSafeDuringIntake(t *testing.T) {
	const (
		newSeries    = 2000
		queriesEach  = 200
		queryWorkers = 4
	)
	db, _, _ := open(t, Options{BlockRange: time.Hour, Retention: -1})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < newSeries; i++ {
			_, err := db.Append(ctx, []tsdb.SeriesSamples{{
				Series:  ref("m", fmt.Sprintf("host:h%d", i), fmt.Sprintf("pod:p%d", i)),
				Samples: []tsdb.Sample{{T: int64(i), V: 1}},
			}})
			if err != nil {
				t.Error(err)
				return
			}
		}
	}()

	for g := 0; g < queryWorkers; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < queriesEach; i++ {
				if _, err := db.MetricNames(ctx, "", 0); err != nil {
					t.Error(err)
					return
				}
				if _, err := db.TagKeys(ctx, "m"); err != nil {
					t.Error(err)
					return
				}
				if _, err := db.TagValues(ctx, "m", "host", 0); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func countSamples(t *testing.T, db *DB) int64 {
	t.Helper()
	set, err := db.Select(ctx, tsdb.Selector{Metric: "m"}, math.MinInt64, math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	for set.Next() {
		it := set.Iterator()
		for it.Next() {
			it.At()
			n++
		}
		if err := it.Err(); err != nil {
			t.Fatal(err)
		}
	}
	if err := set.Err(); err != nil {
		t.Fatal(err)
	}
	if err := set.Close(); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDB_RetentionIsMutuallyExclusiveWithCompaction(t *testing.T) {
	// Retention closes and unlinks blocks; compaction spends seconds reading
	// the ones it is merging. Retention used not to take cutMu, so the only
	// thing keeping them apart was that maintain() calls them one after the
	// other — and both are exported precisely so a test or an operator can
	// call them when it does not. A retention pass landing inside a
	// compaction closes chunks.dat under the read.
	//
	// Asserting on the lock rather than on a race, because the race is timing
	// and this is the property.
	db, _, _ := open(t, Options{BlockRange: time.Minute, Retention: -1})

	db.cutMu.Lock()
	done := make(chan error, 1)
	go func() { done <- db.ApplyRetention() }()

	select {
	case <-done:
		db.cutMu.Unlock()
		t.Fatal("ApplyRetention ran while a cut or compaction held the lock")
	case <-time.After(50 * time.Millisecond):
	}

	db.cutMu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ApplyRetention: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ApplyRetention never finished once the lock was released")
	}
}

// TestDB_ASeriesThatComesBackAfterABlockCutSurvivesARestart.
//
// The ordinary lifecycle of a series that reports in bursts: it is written to
// a block, the head forgets it (that is what a block cut is for), and then it
// reports again and gets a *new* id, because the old one went with it. Both
// series records are in the log, and replay has to end up with both ids
// resolvable — the samples after the gap point at the second one.
func TestDB_ASeriesThatComesBackAfterABlockCutSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(Options{Dir: dir, BlockRange: 30 * time.Second, Retention: -1})
	if err != nil {
		t.Fatal(err)
	}
	r := ref("m", "host:a")
	// A burst, then silence long enough for the cut to take the whole series.
	for _, ts := range []int64{0, 10_000, 20_000} {
		if _, err := db.Append(ctx, []tsdb.SeriesSamples{{Series: r, Samples: []tsdb.Sample{{T: ts, V: float64(ts)}}}}); err != nil {
			t.Fatal(err)
		}
	}
	// Another series carries the head forward so there is something to cut.
	other := ref("m", "host:b")
	for ts := int64(0); ts <= 90_000; ts += 10_000 {
		if _, err := db.Append(ctx, []tsdb.SeriesSamples{{Series: other, Samples: []tsdb.Sample{{T: ts, V: 1}}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CutBlock(); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.head.Series(1); ok {
		t.Log("note: the head still holds series id 1; the cut did not drop it")
	}

	// It reports again. getOrCreate has no memory of the old id, so this is a
	// new one, and the log now defines the same key twice.
	if _, err := db.Append(ctx, []tsdb.SeriesSamples{{Series: r, Samples: []tsdb.Sample{{T: 60_000, V: 60_000}}}}); err != nil {
		t.Fatal(err)
	}
	before := countSamples(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(Options{Dir: dir, BlockRange: 30 * time.Second, Retention: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if after := countSamples(t, reopened); after != before {
		t.Errorf("%d samples after the restart, %d before — replay could not resolve the second id",
			after, before)
	}
}

// TestDB_MetadataQueriesAreSafeDuringABlockCut is the other half of
// TestDB_MetadataQueriesAreSafeDuringIntake, and the one that shows why taking
// the head's lock per map read is not enough: a postings list handed to a
// caller is the index's own slice, and head GC edits those in place.
func TestDB_MetadataQueriesAreSafeDuringABlockCut(t *testing.T) {
	db, _, _ := open(t, Options{BlockRange: time.Second, Retention: -1})

	// Enough series, then enough time, that a cut drops most of them.
	for i := 0; i < 400; i++ {
		if _, err := db.Append(ctx, []tsdb.SeriesSamples{{
			Series:  ref("m", fmt.Sprintf("host:h%d", i), "env:prod"),
			Samples: []tsdb.Sample{{T: int64(i), V: 1}},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Append(ctx, []tsdb.SeriesSamples{{
		Series:  ref("m", "host:carry", "env:prod"),
		Samples: []tsdb.Sample{{T: 5_000, V: 1}},
	}}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if _, err := db.TagValues(ctx, "m", "host", 0); err != nil {
				t.Error(err)
				return
			}
			if _, err := db.TagKeys(ctx, "m"); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	if err := db.CutBlock(); err != nil {
		t.Error(err)
	}
	wg.Wait()
}

func TestDB_CloseIsSafeFromSeveralGoroutinesAtOnce(t *testing.T) {
	// SIGTERM handling and a failed startup path can both reach Close, and the
	// check-then-act this used to do let two goroutines both decide to close
	// the same channel — the second one panics and takes the process with it
	// during shutdown, which is the worst time to lose the final log sync.
	dir := t.TempDir()
	db, err := Open(Options{Dir: dir, BlockRange: time.Hour, Retention: -1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Append(ctx, []tsdb.SeriesSamples{{
		Series: ref("m", "host:a"), Samples: []tsdb.Sample{{T: 1, V: 1}},
	}}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 8)
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = db.Close()
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("Close %d returned %v", i, err)
		}
	}
	// And what it was closing is still on disk.
	reopened, err := Open(Options{Dir: dir, BlockRange: time.Hour, Retention: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if n := countSamples(t, reopened); n != 1 {
		t.Errorf("%d samples after a racing shutdown, want 1", n)
	}
}

// TestDB_ACrashBetweenAMergeAndItsCleanupIsResolvedAtStartup.
//
// Compaction writes the merged block, renames it into place, and only then
// deletes its sources — the safe direction to fail, because a crash in between
// leaves the data twice rather than not at all. But nothing was finishing the
// job. The sources carry no tombstone (a tombstone is written inside the
// delete that never ran), so they sat on disk forever: invisible to queries,
// because Select dedups the overlap, and fully charged against MaxBytes. The
// next compaction would then merge the same run again into a second identical
// block that no later pass can reduce.
func TestDB_ACrashBetweenAMergeAndItsCleanupIsResolvedAtStartup(t *testing.T) {
	dir := t.TempDir()
	db, fake, _ := open(t, Options{
		Dir: dir, BlockRange: time.Minute, Retention: -1, MaxBlockRange: time.Hour,
	})
	_ = fake

	// Three blocks, so there is a run worth compacting.
	r := ref("m", "host:a")
	for i := 0; i < 4; i++ {
		fill(t, db, r, int64(i)*60_000, 1000, 55)
		if err := db.CutBlock(); err != nil && !errors.Is(err, errNothingToCut) {
			t.Fatal(err)
		}
	}
	if n := len(db.Blocks()); n < 3 {
		t.Fatalf("only %d blocks; nothing to compact", n)
	}
	sources := make([]string, 0, 3)
	for _, b := range db.Blocks()[:3] {
		sources = append(sources, b.Meta().ULID.String())
	}
	before := countSamples(t, db)

	// The compaction, but stopped the instant the merged block is in place:
	// run it for real, then put the sources back.
	saved := t.TempDir()
	for _, id := range sources {
		if err := os.Rename(filepath.Join(dir, blocksDirName, id), filepath.Join(saved, id)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Compact(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, id := range sources {
		if err := os.Rename(filepath.Join(saved, id), filepath.Join(dir, blocksDirName, id)); err != nil {
			t.Fatal(err)
		}
	}

	reopened, err := Open(Options{Dir: dir, BlockRange: time.Minute, Retention: -1, MaxBlockRange: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()

	for _, b := range reopened.Blocks() {
		for _, id := range sources {
			if b.Meta().ULID.String() == id {
				t.Errorf("source block %s survived the restart alongside its replacement", id)
			}
		}
	}
	entries, err := os.ReadDir(filepath.Join(dir, blocksDirName))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		for _, id := range sources {
			if e.Name() == id {
				t.Errorf("source directory %s is still on disk", id)
			}
		}
	}
	if after := countSamples(t, reopened); after != before {
		t.Errorf("%d samples after the cleanup, %d before", after, before)
	}
}

// lockedBuffer is an io.Writer for a slog handler shared by the maintenance
// goroutine and the test's own opens. slog serializes each record's Write, but
// not across handlers, and the two Opens here share one.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// all returns everything logged, in order.
func (b *lockedBuffer) all() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(b.buf.String())
}
