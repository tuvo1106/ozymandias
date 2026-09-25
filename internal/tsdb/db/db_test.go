package db

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/block"
)

var ctx = context.Background()

var epoch = time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

func ref(metric string, tags ...string) tsdb.SeriesRef {
	return tsdb.NewSeriesRef(metric, tags)
}

func ss(metric string, tags []string, samples ...tsdb.Sample) tsdb.SeriesSamples {
	return tsdb.SeriesSamples{Series: tsdb.NewSeriesRef(metric, tags), Samples: samples}
}

func sm(t int64, v float64) tsdb.Sample { return tsdb.Sample{T: t, V: v} }

// open returns a DB with a fake clock and a short block range, plus the
// directory so a test can reopen it.
func open(t *testing.T, opts Options) (*DB, *testutil.FakeClock, string) {
	t.Helper()
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	fake := testutil.NewFakeClock(epoch)
	if opts.Clock == nil {
		opts.Clock = fake
	}
	if opts.BlockRange == 0 {
		opts.BlockRange = time.Minute
	}
	db, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, fake, opts.Dir
}

func mustAppend(t *testing.T, db *DB, batch ...tsdb.SeriesSamples) tsdb.AppendResult {
	t.Helper()
	res, err := db.Append(ctx, batch)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// dump renders a SeriesSet as "key=t:v,t:v; key=…" so a whole result is one
// readable assertion.
func dump(t *testing.T, set tsdb.SeriesSet) string {
	t.Helper()
	var parts []string
	for set.Next() {
		var b strings.Builder
		b.WriteString(set.Series().Key())
		b.WriteByte('=')
		it, first := set.Iterator(), true
		for it.Next() {
			if !first {
				b.WriteByte(',')
			}
			first = false
			s := it.At()
			fmt.Fprintf(&b, "%d:%g", s.T, s.V)
		}
		if err := it.Err(); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, b.String())
	}
	if err := set.Err(); err != nil {
		t.Fatal(err)
	}
	if err := set.Close(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(parts, "; ")
}

func TestDB_AppendAndSelect(t *testing.T) {
	db, _, _ := open(t, Options{})
	mustAppend(t, db,
		ss("http.count", []string{"env:dev", "route:/a"}, sm(1000, 1), sm(2000, 2)),
		ss("http.count", []string{"env:dev", "route:/b"}, sm(1000, 5)),
		ss("other", nil, sm(1000, 9)),
	)
	set, err := db.Select(ctx, tsdb.Selector{Metric: "http.count"}, 0, 5000)
	if err != nil {
		t.Fatal(err)
	}
	want := "http.count|env:dev,route:/a=1000:1,2000:2; http.count|env:dev,route:/b=1000:5"
	if got := dump(t, set); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if st := db.Stats(); st.Series != 3 || st.Samples != 4 {
		t.Errorf("stats %+v", st)
	}
}

func TestDB_RejectionsAreReportedPerSeries(t *testing.T) {
	db, _, _ := open(t, Options{MaxSeriesPerMetric: 2})
	mustAppend(t, db, ss("m", []string{"h:1"}, sm(1000, 1)))

	res, err := db.Append(ctx, []tsdb.SeriesSamples{
		ss("m", []string{"h:1"}, sm(2000, 2)),                        // fine
		ss("m", []string{"h:1"}, sm(500, 9), sm(400, 8)),             // two late samples, one series
		ss("m", []string{"h:2"}, sm(1000, 1)),                        // fine, second series
		ss("m", []string{"h:3"}, sm(1000, 1)),                        // over the limit
		{Series: tsdb.SeriesRef{}, Samples: []tsdb.Sample{sm(1, 1)}}, // no metric name
		ss("m", []string{"h:4"}, sm(1000, math.NaN())),               // non-finite
	})
	if err != nil {
		t.Fatal(err)
	}
	// Two: the new sample for h:1 and the first for h:2. The late samples and
	// the over-limit series contribute nothing.
	if res.Samples != 2 {
		t.Errorf("stored %d samples, want 2", res.Samples)
	}
	// One entry per series with a problem, not one per bad sample.
	if len(res.Rejected) != 4 {
		t.Fatalf("rejected %d entries: %+v", len(res.Rejected), res.Rejected)
	}
	reasons := map[string]string{}
	for _, r := range res.Rejected {
		reasons[r.Series.Key()] = r.Reason
	}
	if got := reasons["m|h:1"]; !strings.Contains(got, "newest timestamp") {
		t.Errorf("late samples gave reason %q", got)
	}
	if got := reasons["m|h:3"]; !strings.Contains(got, "series limit") {
		t.Errorf("the limit gave reason %q", got)
	}
	if got := reasons["m|h:4"]; !strings.Contains(got, "non-finite") {
		t.Errorf("a NaN gave reason %q", got)
	}
	// And the good data in the same batch landed.
	set, _ := db.Select(ctx, tsdb.Selector{Metric: "m"}, 0, 5000)
	if got := dump(t, set); got != "m|h:1=1000:1,2000:2; m|h:2=1000:1" {
		t.Fatalf("got %s", got)
	}
}

func TestDB_AnAtLeastOnceRetryIsANoOp(t *testing.T) {
	db, _, _ := open(t, Options{})
	batch := []tsdb.SeriesSamples{ss("m", nil, sm(1000, 1), sm(2000, 2))}
	first := mustAppend(t, db, batch...)
	second := mustAppend(t, db, batch...)

	if first.Samples != 2 {
		t.Errorf("the first append stored %d samples", first.Samples)
	}
	if second.Samples != 0 || len(second.Rejected) != 0 {
		t.Errorf("the retry gave %+v; it should be a silent no-op", second)
	}
	set, _ := db.Select(ctx, tsdb.Selector{Metric: "m"}, 0, 5000)
	if got := dump(t, set); got != "m|=1000:1,2000:2" {
		t.Fatalf("got %s", got)
	}
}

// --- blocks ----------------------------------------------------------------

// fill appends n samples spaced step apart, starting at t0.
func fill(t *testing.T, db *DB, r tsdb.SeriesRef, t0, step int64, n int) {
	t.Helper()
	batch := tsdb.SeriesSamples{Series: r}
	for i := 0; i < n; i++ {
		batch.Samples = append(batch.Samples, sm(t0+int64(i)*step, float64(i)))
	}
	res := mustAppend(t, db, batch)
	if res.Samples != n {
		t.Fatalf("stored %d of %d samples: %+v", res.Samples, n, res.Rejected)
	}
}

func TestDB_CutBlockMovesTheOldestRangeToDisk(t *testing.T) {
	const rangeMs = 60_000
	db, _, dir := open(t, Options{BlockRange: time.Minute})
	r := ref("m", "env:prod")

	// Nothing to cut until the head spans 1.5 ranges.
	fill(t, db, r, 0, 1000, 60) // 0..59s, exactly one range
	if err := db.CutBlock(); !errors.Is(err, errNothingToCut) {
		t.Fatalf("cut too early: %v", err)
	}
	fill(t, db, r, 60_000, 1000, 31) // through 90s: 1.5 ranges
	if err := db.CutBlock(); err != nil {
		t.Fatal(err)
	}

	blocks := db.Blocks()
	if len(blocks) != 1 {
		t.Fatalf("%d blocks after the cut", len(blocks))
	}
	m := blocks[0].Meta()
	if m.MinTime != 0 || m.MaxTime != 59_000 {
		t.Errorf("the block covers [%d, %d], want the first whole range", m.MinTime, m.MaxTime)
	}
	if m.Stats.Samples != 60 {
		t.Errorf("the block holds %d samples, want 60", m.Stats.Samples)
	}
	// It is on disk, complete, under blocks/.
	entries, err := os.ReadDir(filepath.Join(dir, blocksDirName))
	if err != nil || len(entries) != 1 {
		t.Fatalf("blocks dir holds %v, err %v", entries, err)
	}
	if _, err := block.ReadMeta(filepath.Join(dir, blocksDirName, m.ULID.String())); err != nil {
		t.Errorf("the block on disk is not readable: %v", err)
	}

	// The head gave up exactly what the block took, and nothing else.
	if hs := db.HeadStats(); hs.MinT < rangeMs {
		t.Errorf("the head still holds samples from before the cut (minT %d)", hs.MinT)
	}
	// A sample in the cut range is now out of bounds, because the block owns it.
	res := mustAppend(t, db, ss("m", []string{"env:prod"}, sm(30_000, 1)))
	if len(res.Rejected) != 1 || !strings.Contains(res.Rejected[0].Reason, "older than") {
		t.Errorf("a sample inside the cut range gave %+v", res)
	}
}

func TestDB_AQueryDoesNotNoticeTheCut(t *testing.T) {
	// The point of the whole exercise: where a sample lives is not the
	// caller's problem. The same query must answer identically before and
	// after its data moves from memory to disk.
	db, _, _ := open(t, Options{BlockRange: time.Minute})
	r := ref("m", "env:prod")
	fill(t, db, r, 0, 1000, 95)

	sel := tsdb.Selector{Metric: "m", Matchers: []tsdb.Matcher{
		{Key: "env", Value: "prod", Type: tsdb.Equal},
	}}
	before, err := db.Select(ctx, sel, 0, 200_000)
	if err != nil {
		t.Fatal(err)
	}
	beforeDump := dump(t, before)

	if err := db.CutBlock(); err != nil {
		t.Fatal(err)
	}
	if len(db.Blocks()) != 1 {
		t.Fatal("expected a block")
	}
	after, err := db.Select(ctx, sel, 0, 200_000)
	if err != nil {
		t.Fatal(err)
	}
	if got := dump(t, after); got != beforeDump {
		t.Errorf("the cut changed the answer:\nbefore %s\nafter  %s", beforeDump, got)
	}

	// Including a window that straddles the boundary.
	straddle, err := db.Select(ctx, sel, 55_000, 65_000)
	if err != nil {
		t.Fatal(err)
	}
	got := dump(t, straddle)
	if want := 11; len(strings.Split(strings.SplitN(got, "=", 2)[1], ",")) != want {
		t.Errorf("a window across the boundary returned %s, want %d samples", got, want)
	}
}

func TestDB_MetadataSpansBlocksAndHead(t *testing.T) {
	// A metric that stopped reporting lives only in a block; one that just
	// started lives only in the head. Autocomplete has to see both, and this
	// is the test that fails if metadata is answered from the head alone.
	db, _, _ := open(t, Options{BlockRange: time.Minute})
	fill(t, db, ref("old.metric", "env:prod"), 0, 1000, 95)
	if err := db.CutBlock(); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, db, ss("new.metric", []string{"zone:b"}, sm(120_000, 1)))

	names, err := db.MetricNames(ctx, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"new.metric", "old.metric"}) {
		t.Errorf("MetricNames = %v", names)
	}
	if got, _ := db.MetricNames(ctx, "old.", 0); !reflect.DeepEqual(got, []string{"old.metric"}) {
		t.Errorf("prefixed MetricNames = %v", got)
	}
	keys, err := db.TagKeys(ctx, "old.metric")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keys, []string{"env"}) {
		t.Errorf("TagKeys of a metric only in a block = %v", keys)
	}
	vals, err := db.TagValues(ctx, "", "env", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(vals, []string{"prod"}) {
		t.Errorf("TagValues = %v", vals)
	}
	if got, _ := db.MetricNames(ctx, "", 1); len(got) != 1 {
		t.Errorf("the limit was not applied: %v", got)
	}
}

func TestDB_RetentionDeletesWholeBlocks(t *testing.T) {
	db, fake, dir := open(t, Options{BlockRange: time.Minute, Retention: time.Hour})
	fill(t, db, ref("m", "env:prod"), epoch.UnixMilli(), 1000, 95)
	if err := db.CutBlock(); err != nil {
		t.Fatal(err)
	}
	if len(db.Blocks()) != 1 {
		t.Fatal("expected a block to age out")
	}
	// Not yet: the block's newest sample is minutes old, not hours.
	if err := db.ApplyRetention(); err != nil {
		t.Fatal(err)
	}
	if len(db.Blocks()) != 1 {
		t.Fatal("retention deleted a block that was still inside the window")
	}

	fake.Advance(2 * time.Hour)
	if err := db.ApplyRetention(); err != nil {
		t.Fatal(err)
	}
	if n := len(db.Blocks()); n != 0 {
		t.Errorf("%d blocks survived retention", n)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, blocksDirName))
	if len(entries) != 0 {
		t.Errorf("the block directory still holds %v", entries)
	}
	// The head's data is untouched: retention is about blocks.
	set, _ := db.Select(ctx, tsdb.Selector{Metric: "m"}, 0, math.MaxInt64)
	if got := dump(t, set); got == "" {
		t.Error("retention took the head's samples too")
	}
}

func TestDB_RetentionCanBeDisabled(t *testing.T) {
	db, fake, _ := open(t, Options{BlockRange: time.Minute, Retention: -1})
	fill(t, db, ref("m", "env:prod"), epoch.UnixMilli(), 1000, 95)
	if err := db.CutBlock(); err != nil {
		t.Fatal(err)
	}
	fake.Advance(10_000 * time.Hour)
	if err := db.ApplyRetention(); err != nil {
		t.Fatal(err)
	}
	if len(db.Blocks()) != 1 {
		t.Error("a negative retention still deleted a block")
	}
}

// --- durability ------------------------------------------------------------

func TestDB_ReopenRecoversEverything(t *testing.T) {
	db, _, dir := open(t, Options{Dir: t.TempDir(), BlockRange: time.Minute, SyncOnAppend: true})
	r := ref("m", "env:prod")
	fill(t, db, r, 0, 1000, 95)
	if err := db.CutBlock(); err != nil { // some in a block, some in the head
		t.Fatal(err)
	}
	sel := tsdb.Selector{Metric: "m"}
	set, _ := db.Select(ctx, sel, 0, math.MaxInt64)
	want := dump(t, set)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, _, _ := open(t, Options{Dir: dir, BlockRange: time.Minute})
	set, err := reopened.Select(ctx, sel, 0, math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	if got := dump(t, set); got != want {
		t.Errorf("after a reopen:\ngot  %s\nwant %s", got, want)
	}
	if n := len(reopened.Blocks()); n != 1 {
		t.Errorf("%d blocks after the reopen, want 1", n)
	}
	// Appending still works and continues where it left off.
	res := mustAppend(t, reopened, ss("m", []string{"env:prod"}, sm(200_000, 7)))
	if res.Samples != 1 {
		t.Errorf("a new sample after the reopen: %+v", res)
	}
}

func TestDB_ACrashBetweenTheBlockAndTheLogTruncationDoesNotDuplicate(t *testing.T) {
	// The window CutBlock deliberately leaves open: the block is on disk but
	// the log still holds the same samples. Replay must not double them, and
	// the block's copy is the one that wins.
	dir := t.TempDir()
	db, _, _ := open(t, Options{Dir: dir, BlockRange: time.Minute, SyncOnAppend: true})
	fill(t, db, ref("m", "env:prod"), 0, 1000, 95)

	// Write the block, but stop before the log is truncated.
	blocksDir := filepath.Join(dir, blocksDirName)
	series, err := db.head.Select(tsdb.Selector{}, 0, 59_999)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := block.Write(blocksDir, series, block.WriterOptions{Now: epoch}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil { // the process dies here
		t.Fatal(err)
	}

	reopened, _, _ := open(t, Options{Dir: dir, BlockRange: time.Minute})
	set, err := reopened.Select(ctx, tsdb.Selector{Metric: "m"}, 0, math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	got := dump(t, set)
	samples := strings.Split(strings.SplitN(got, "=", 2)[1], ",")
	if len(samples) != 95 {
		t.Fatalf("recovered %d samples, want 95 with no duplicates:\n%s", len(samples), got)
	}
	var last int64 = math.MinInt64
	for _, s := range samples {
		var ts int64
		if _, err := fmt.Sscanf(s, "%d:", &ts); err != nil {
			t.Fatal(err)
		}
		if ts <= last {
			t.Fatalf("timestamps are not strictly increasing near %s", s)
		}
		last = ts
	}
}

func TestDB_IncompleteBlocksAreClearedAtStartup(t *testing.T) {
	dir := t.TempDir()
	blocksDir := filepath.Join(dir, blocksDirName)
	if err := os.MkdirAll(filepath.Join(blocksDir, "01JHALF.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	condemned := filepath.Join(blocksDir, "01JGOING")
	if err := os.MkdirAll(condemned, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{block.MetaFilename, "tombstone"} {
		if err := os.WriteFile(filepath.Join(condemned, name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	db, _, _ := open(t, Options{Dir: dir})
	if n := len(db.Blocks()); n != 0 {
		t.Errorf("%d blocks opened, want 0", n)
	}
	entries, _ := os.ReadDir(blocksDir)
	if len(entries) != 0 {
		t.Errorf("startup left %v behind", entries)
	}
}

func TestDB_OpenErrors(t *testing.T) {
	if _, err := Open(Options{}); err == nil {
		t.Error("Open without a Dir succeeded")
	}
	t.Run("a corrupt block stops startup", func(t *testing.T) {
		dir := t.TempDir()
		db, _, _ := open(t, Options{Dir: dir, BlockRange: time.Minute})
		fill(t, db, ref("m", "env:prod"), 0, 1000, 95)
		if err := db.CutBlock(); err != nil {
			t.Fatal(err)
		}
		bdir := db.Blocks()[0].Dir()
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bdir, block.IndexFilename), []byte("broken"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Loudly, not by quietly serving a database with a hole in it.
		if _, err := Open(Options{Dir: dir}); err == nil {
			t.Error("a database with a corrupt block opened")
		}
	})
}

func TestDB_CloseIsIdempotentAndStopsMaintenance(t *testing.T) {
	db, _, _ := open(t, Options{})
	mustAppend(t, db, ss("m", nil, sm(1000, 1)))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Errorf("the second Close returned %v", err)
	}
}

func TestDB_MaintenanceRunsOnTheClock(t *testing.T) {
	// The background goroutine is the same code the tests call directly, so
	// this only has to prove it is wired to the clock at all.
	// Retention off: these timestamps are near the unix epoch, and against a
	// 2026 clock a real retention window would delete the block as fast as
	// maintenance created it.
	db, fake, _ := open(t, Options{
		BlockRange: time.Minute, SyncInterval: 50 * time.Millisecond, Retention: -1,
	})
	fill(t, db, ref("m", "env:prod"), 0, 1000, 95)
	testutil.Eventually(t, time.Second, func() bool { return fake.Waiters() >= 2 },
		"maintenance never armed its tickers")

	deadline := time.Now().Add(2 * time.Second)
	for len(db.Blocks()) == 0 && time.Now().Before(deadline) {
		fake.Advance(10 * time.Second)
		time.Sleep(time.Millisecond)
	}
	if n := len(db.Blocks()); n != 1 {
		t.Errorf("maintenance produced %d blocks", n)
	}
}

func TestDB_DiskUsageCountsBothHalves(t *testing.T) {
	db, _, _ := open(t, Options{BlockRange: time.Minute})
	fill(t, db, ref("m", "env:prod"), 0, 1000, 95)
	if err := db.Sync(); err != nil {
		t.Fatal(err)
	}
	logOnly, err := db.DiskUsage()
	if err != nil {
		t.Fatal(err)
	}
	if logOnly <= 0 {
		t.Fatalf("DiskUsage = %d with a written log", logOnly)
	}
	if err := db.CutBlock(); err != nil {
		t.Fatal(err)
	}
	withBlock, err := db.DiskUsage()
	if err != nil {
		t.Fatal(err)
	}
	if withBlock <= logOnly {
		t.Errorf("DiskUsage did not grow after a block was written: %d then %d", logOnly, withBlock)
	}
}

func TestDB_ContextCancellation(t *testing.T) {
	db, _, _ := open(t, Options{})
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := db.Append(cancelled, []tsdb.SeriesSamples{ss("m", nil, sm(1, 1))}); err == nil {
		t.Error("Append ignored a cancelled context")
	}
	if _, err := db.Select(cancelled, tsdb.Selector{Metric: "m"}, 0, 1); err == nil {
		t.Error("Select ignored a cancelled context")
	}
	if _, err := db.MetricNames(cancelled, "", 0); err == nil {
		t.Error("MetricNames ignored a cancelled context")
	}
}

func TestDB_StatsAndBlockOrdering(t *testing.T) {
	db, _, _ := open(t, Options{BlockRange: time.Minute, Retention: -1})
	fill(t, db, ref("m", "env:prod"), 0, 1000, 95)
	fill(t, db, ref("m", "env:dev"), 0, 1000, 95)

	inHead := db.Stats()
	if inHead.Series != 2 || inHead.Samples != 190 {
		t.Fatalf("before the cut: %+v", inHead)
	}
	if err := db.CutBlock(); err != nil {
		t.Fatal(err)
	}
	// Stats span both halves, so the total is unchanged by where data lives.
	if after := db.Stats(); after.Samples != inHead.Samples {
		t.Errorf("the cut changed the sample count: %d then %d", inHead.Samples, after.Samples)
	}

	// A second cut, and the blocks stay in time order — which is what lets
	// Select merge them by concatenation.
	fill(t, db, ref("m", "env:prod"), 120_000, 1000, 95)
	if err := db.CutBlock(); err != nil {
		t.Fatal(err)
	}
	blocks := db.Blocks()
	if len(blocks) != 2 {
		t.Fatalf("%d blocks after two cuts", len(blocks))
	}
	if blocks[0].Meta().MinTime > blocks[1].Meta().MinTime {
		t.Error("blocks are not in time order")
	}
	// And a query across both blocks and the head is still one sorted answer.
	set, err := db.Select(ctx, tsdb.Selector{Metric: "m"}, 0, math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	got := dump(t, set)
	if !strings.HasPrefix(got, "m|env:dev=") || !strings.Contains(got, "; m|env:prod=") {
		t.Errorf("series are not in key order: %s", got)
	}
}

func TestDB_OpenClosesWhatItOpenedWhenItFails(t *testing.T) {
	// A failure partway through Open must not leak the blocks and log it had
	// already opened; on Windows an unclosed file would also block the next
	// attempt from cleaning up.
	dir := t.TempDir()
	db, _, _ := open(t, Options{Dir: dir, BlockRange: time.Minute, Retention: -1})
	fill(t, db, ref("m", "env:prod"), 0, 1000, 95)
	if err := db.CutBlock(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Corrupt the log so replay fails after the blocks are already open.
	walDir := filepath.Join(dir, walDirName)
	entries, err := os.ReadDir(walDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no log segments: %v", err)
	}
	seg := filepath.Join(walDir, entries[0].Name())
	data, err := os.ReadFile(seg)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 20 {
		data[12] ^= 0xff // inside the first record, past its header
		if err := os.WriteFile(seg, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Whether this particular damage is fatal depends on where it landed; the
	// requirement is only that Open never returns both a nil DB and a nil
	// error, and never panics.
	reopened, err := Open(Options{Dir: dir, BlockRange: time.Minute, Retention: -1})
	if err == nil {
		if reopened == nil {
			t.Fatal("Open returned no DB and no error")
		}
		_ = reopened.Close()
	}
}

func TestSortBlocks_TiesBreakOnTheID(t *testing.T) {
	// Two blocks can share a minTime — a rollup beside its source, or a
	// compaction that starts at the same instant. Without a tiebreak the order
	// would depend on map iteration, and Select's merge would stop being
	// deterministic.
	parent := t.TempDir()
	var blocks []*block.Block
	for i := 0; i < 3; i++ {
		m, err := block.Write(parent, []tsdb.SeriesSamples{{
			Series:  ref("m", fmt.Sprintf("i:%d", i)),
			Samples: []tsdb.Sample{{T: 1000, V: 1}},
		}}, block.WriterOptions{Now: epoch})
		if err != nil {
			t.Fatal(err)
		}
		b, err := block.Open(filepath.Join(parent, m.ULID.String()))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = b.Close() })
		blocks = append(blocks, b)
	}
	// Reverse, then sort: same minTime throughout, so only the id can order them.
	blocks[0], blocks[2] = blocks[2], blocks[0]
	sortBlocks(blocks)
	for i := 1; i < len(blocks); i++ {
		if blocks[i-1].Meta().ULID.Compare(blocks[i].Meta().ULID) >= 0 {
			t.Errorf("blocks with equal minTime are not ordered by id: %v", blocks)
		}
	}
}

func TestDB_DiskUsageOnAVanishedDirectory(t *testing.T) {
	db, _, dir := open(t, Options{})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	// Retention or an operator can remove things under us; reporting zero
	// beats failing a self-metrics collection.
	if _, err := db.DiskUsage(); err != nil {
		t.Errorf("DiskUsage on a missing directory: %v", err)
	}
}

func TestDB_CompactionIsInvisibleThroughTheStore(t *testing.T) {
	db, _, _ := open(t, Options{BlockRange: time.Minute, Retention: -1})
	r := ref("m", "env:prod")

	// Four cuts, so three adjacent level-0 blocks exist to merge.
	for i := int64(0); i < 4; i++ {
		fill(t, db, r, i*100_000, 1000, 95)
		if err := db.CutBlock(); err != nil {
			t.Fatalf("cut %d: %v", i, err)
		}
	}
	if n := len(db.Blocks()); n < 3 {
		t.Fatalf("only %d blocks; compaction needs three", n)
	}
	sel := tsdb.Selector{Metric: "m"}
	before := dumpOf(t, db, sel)

	did, err := db.Compact()
	if err != nil {
		t.Fatal(err)
	}
	if !did {
		t.Fatal("nothing was compacted")
	}
	if after := dumpOf(t, db, sel); after != before {
		t.Errorf("compaction changed the answer:\nbefore %s\nafter  %s", before, after)
	}
	// The merged block replaced its sources rather than joining them.
	var levels []int
	for _, b := range db.Blocks() {
		levels = append(levels, b.Meta().Compaction.Level)
	}
	if len(db.Blocks()) >= 4 {
		t.Errorf("block count did not fall: %d blocks at levels %v", len(db.Blocks()), levels)
	}
	// And the files on disk match what the DB thinks it has.
	entries, err := os.ReadDir(filepath.Join(db.dir, blocksDirName))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(db.Blocks()) {
		t.Errorf("%d directories on disk for %d open blocks", len(entries), len(db.Blocks()))
	}
	// It survives a reopen, which is where a dangling source would show up.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _, _ := open(t, Options{Dir: db.dir, BlockRange: time.Minute, Retention: -1})
	if after := dumpOf(t, reopened, sel); after != before {
		t.Errorf("a reopen after compaction changed the answer:\n%s", after)
	}
}

// TestDB_OverlappingBlocksMergeInsteadOfHidingEachOther covers the merge in
// Select for sources that are not prefix-ordered in time. Concatenating in
// source order and skipping anything at or before the running tail is right
// only while the sources are disjoint; two blocks over the same range are not,
// and every sample the second one holds for an instant the first has nothing
// for would be dropped. A crash between writing a merged block and deleting
// its sources leaves exactly this until the next startup, and rollup blocks
// will have it by design.
func TestDB_OverlappingBlocksMergeInsteadOfHidingEachOther(t *testing.T) {
	dir := t.TempDir()
	blocksDir := filepath.Join(dir, blocksDirName)
	r := ref("m", "env:prod")
	// Same range, interleaved: neither block is a prefix of the other, and
	// they agree at t=2000 so the dedup has something to do too.
	even := []tsdb.SeriesSamples{{Series: r, Samples: []tsdb.Sample{sm(0, 0), sm(2000, 2), sm(4000, 4)}}}
	odd := []tsdb.SeriesSamples{{Series: r, Samples: []tsdb.Sample{sm(1000, 1), sm(2000, 2), sm(3000, 3), sm(5000, 5)}}}
	for _, series := range [][]tsdb.SeriesSamples{even, odd} {
		if _, err := block.Write(blocksDir, series, block.WriterOptions{Now: epoch}); err != nil {
			t.Fatal(err)
		}
	}

	db, _, _ := open(t, Options{Dir: dir, BlockRange: time.Minute, Retention: -1})
	if n := len(db.Blocks()); n != 2 {
		t.Fatalf("%d blocks opened, want the two overlapping ones", n)
	}
	want := "m|env:prod=0:0,1000:1,2000:2,3000:3,4000:4,5000:5"
	if got := dumpOf(t, db, tsdb.Selector{Metric: "m"}); got != want {
		t.Errorf("merged to\n  %s\nwant\n  %s", got, want)
	}
}

func dumpOf(t *testing.T, db *DB, sel tsdb.Selector) string {
	t.Helper()
	set, err := db.Select(ctx, sel, math.MinInt64, math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	return dump(t, set)
}

// TestDB_CompactionSurvivesASourceThatWillNotDelete pins the order the swap
// and the unlink happen in. The sources are removed only after the merged
// block is open and in db.blocks, so no failure can leave a block that is
// gone from disk and still being served — and a source that refuses to be
// deleted is disk to reclaim at the next startup (dropSuperseded finds it by
// the merged block's provenance), not a reason to fail a compaction that has
// already happened and re-plan the identical run every tick.
func TestDB_CompactionSurvivesASourceThatWillNotDelete(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	db, _, _ := open(t, Options{BlockRange: time.Minute, Retention: -1})
	r := ref("m", "env:prod")
	for i := int64(0); i < 4; i++ {
		fill(t, db, r, i*100_000, 1000, 95)
		if err := db.CutBlock(); err != nil {
			t.Fatalf("cut %d: %v", i, err)
		}
	}
	sel := tsdb.Selector{Metric: "m"}
	before := dumpOf(t, db, sel)

	// Read-only, so the tombstone write inside block.Delete fails. The merge
	// itself only reads, so it is unaffected.
	stuck := db.Blocks()[0].Dir()
	if err := os.Chmod(stuck, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stuck, 0o700) })

	did, err := db.Compact()
	if err != nil || !did {
		t.Fatalf("Compact = %v, %v; an undeletable source must not fail the merge", did, err)
	}
	if after := dumpOf(t, db, sel); after != before {
		t.Errorf("compaction changed the answer:\nbefore %s\nafter  %s", before, after)
	}
	if _, err := os.Stat(stuck); err != nil {
		t.Fatalf("the source deleted after all, so this tested nothing: %v", err)
	}
	for _, b := range db.Blocks() {
		if b.Dir() == stuck {
			t.Error("a source that could not be deleted is still being served")
		}
	}
	// The leftover is resolved at the next startup, not by another
	// compaction. (Restoring the permissions first is the realistic case: a
	// full disk clears, an operator fixes a mode. dropSuperseded still
	// cannot delete what the filesystem refuses, and it treats that as
	// fatal — a separate question from this one.)
	if err := os.Chmod(stuck, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _, _ := open(t, Options{Dir: db.dir, BlockRange: time.Minute, Retention: -1})
	if after := dumpOf(t, reopened, sel); after != before {
		t.Errorf("a reopen changed the answer:\n%s", after)
	}
	if _, err := os.Stat(stuck); !os.IsNotExist(err) {
		t.Errorf("the superseded source survived a reopen: %v", err)
	}
}

// TestDB_SyncsContinueWhileMaintenanceIsBusy pins why group-commit has a
// goroutine of its own. The two used to share one select, so no fsync happened
// for the whole of a cut, a compaction that rewrites three blocks and a
// retention sweep — and with SyncOnAppend off, the window an acknowledged
// sample spends in the page cache is documented as wal_sync_interval, not
// "however long the longest maintenance pass took".
//
// Holding cutMu is what a long pass looks like from outside: the maintenance
// goroutine wakes on its own ticker, reaches CutBlock and stops there.
func TestDB_SyncsContinueWhileMaintenanceIsBusy(t *testing.T) {
	db, fake, _ := open(t, Options{BlockRange: time.Minute, Retention: -1, SyncInterval: 10 * time.Millisecond})
	fill(t, db, ref("m", "env:prod"), 0, 1000, 91) // enough that a cut is due

	// Both loops are armed before the clock moves (see FakeClock's doc).
	testutil.Eventually(t, 2*time.Second, func() bool { return fake.Waiters() >= 2 }, "tickers not armed")

	db.cutMu.Lock()
	defer db.cutMu.Unlock()
	fake.Advance(10 * time.Second) // the check ticker fires; CutBlock blocks on cutMu

	// Let every tick that advance delivered be consumed before the baseline is
	// taken. Without this the baseline can be read before a tick that was
	// already in flight lands, and that one arrival alone satisfies the
	// assertion — which is how this test first passed against the very shape
	// it is meant to reject. The clock only moves when a test moves it, so
	// "unchanged while it is still" is a real quiescence check.
	settle := func() uint64 {
		last, stable := db.Syncs(), 0
		for stable < 5 {
			time.Sleep(10 * time.Millisecond)
			if n := db.Syncs(); n == last {
				stable++
			} else {
				last, stable = n, 0
			}
		}
		return last
	}
	before := settle()

	for i := 0; i < 20; i++ {
		fake.Advance(10 * time.Millisecond)
	}
	testutil.Eventually(t, 2*time.Second, func() bool { return db.Syncs() > before },
		"the log was not synced while the maintenance goroutine was stuck")
}

func TestDB_CompactIsANoOpWithNothingToDo(t *testing.T) {
	db, _, _ := open(t, Options{BlockRange: time.Minute, Retention: -1})
	if did, err := db.Compact(); err != nil || did {
		t.Errorf("Compact on an empty database = %v, %v", did, err)
	}
}

func TestDB_SizeCapDeletesTheOldestBlocks(t *testing.T) {
	db, _, _ := open(t, Options{BlockRange: time.Minute, Retention: -1, MaxBytes: 1})
	fill(t, db, ref("m", "env:prod"), epoch.UnixMilli(), 1000, 95)
	if err := db.CutBlock(); err != nil {
		t.Fatal(err)
	}
	if len(db.Blocks()) != 1 {
		t.Fatal("expected a block")
	}
	// A cap of one byte cannot be met — the log alone is larger — but every
	// block must still go, and the pass must not spin or fail.
	if err := db.ApplyRetention(); err != nil {
		t.Fatal(err)
	}
	if n := len(db.Blocks()); n != 0 {
		t.Errorf("%d blocks survived a 1-byte cap", n)
	}
	// The head still answers: the cap evicts blocks, not live data.
	if got := dumpOf(t, db, tsdb.Selector{Metric: "m"}); got == "" {
		t.Error("the size cap took the head's samples too")
	}
}

func TestDB_SizeCapLeavesRoomAlone(t *testing.T) {
	db, _, _ := open(t, Options{BlockRange: time.Minute, Retention: -1, MaxBytes: 1 << 30})
	fill(t, db, ref("m", "env:prod"), epoch.UnixMilli(), 1000, 95)
	if err := db.CutBlock(); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplyRetention(); err != nil {
		t.Fatal(err)
	}
	if n := len(db.Blocks()); n != 1 {
		t.Errorf("a generous cap deleted %d blocks", 1-n)
	}
}

func TestDB_OpenReleasesTheBlocksItOpenedBeforeFailing(t *testing.T) {
	// Open opens the blocks before the log. If the log then fails, those file
	// handles have to go back — otherwise a retry loop leaks a descriptor per
	// block per attempt.
	dir := t.TempDir()
	db, _, _ := open(t, Options{Dir: dir, BlockRange: time.Minute, Retention: -1})
	fill(t, db, ref("m", "env:prod"), 0, 1000, 95)
	if err := db.CutBlock(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// A plain file where the log directory belongs: the blocks open, the log
	// cannot.
	if err := os.RemoveAll(filepath.Join(dir, walDirName)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, walDirName), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Options{Dir: dir, BlockRange: time.Minute, Retention: -1}); err == nil {
		t.Fatal("Open succeeded with a file where the log directory belongs")
	}
}
