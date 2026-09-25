package db

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
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
// segment rolls at 32 MiB, and the head holds hours of data, so on any real
// traffic the head's oldest samples are several segments back — and dropping
// those segments drops the only copy of them, because the head is memory.
//
// Nothing else catches this. Every other restart test writes a few kilobytes,
// so the log never rolls and the premise is accidentally true.
func TestDB_LogTruncationKeepsWhatIsStillOnlyInTheHead(t *testing.T) {
	if testing.Short() {
		t.Skip("writes ~40 MiB to roll a log segment")
	}
	dir := t.TempDir()
	db, err := Open(Options{
		Dir: dir,
		// Long enough that one cut leaves most of the data in the head.
		BlockRange: 30 * time.Second,
		Retention:  -1,
	})
	if err != nil {
		t.Fatal(err)
	}

	const series = 10
	refs := make([]tsdb.SeriesRef, series)
	for i := range refs {
		refs[i] = ref("m", fmt.Sprintf("host:h%d", i))
	}
	batch := make([]tsdb.SeriesSamples, series)
	var acked int64
	var ts int64
	for walBytes(t, dir) < 40<<20 {
		for i, r := range refs {
			batch[i] = tsdb.SeriesSamples{Series: r, Samples: []tsdb.Sample{{T: ts, V: float64(ts)}}}
		}
		res, err := db.Append(ctx, batch)
		if err != nil {
			t.Fatal(err)
		}
		if res.Samples != series {
			t.Fatalf("at t=%d stored %d of %d: %+v", ts, res.Samples, series, res.Rejected)
		}
		acked += int64(res.Samples)
		ts++
	}
	segments, err := os.ReadDir(filepath.Join(dir, walDirName))
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) < 2 {
		t.Fatalf("the log never rolled (%d segment), so this test proves nothing", len(segments))
	}

	// One cut: the oldest 30s goes to a block, the rest stays in the head —
	// and the log is truncated behind it.
	if err := db.CutBlock(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(Options{Dir: dir, BlockRange: 30 * time.Second, Retention: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if got := countSamples(t, reopened); got != acked {
		t.Errorf("%d samples survived a clean restart, %d were acknowledged — %d lost",
			got, acked, acked-got)
	}
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
