package db

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/block"
	"github.com/tuvo1106/ozymandias/internal/tsdb/compact"
	"github.com/tuvo1106/ozymandias/internal/tsdb/head"
	"github.com/tuvo1106/ozymandias/internal/tsdb/wal"
)

// maintain runs the background work: group-commit syncs, block cuts and
// retention. One goroutine, owned by the DB, stopped by Close.
//
// Everything here is also callable directly ([DB.Sync], [DB.CutBlock],
// [DB.ApplyRetention]) so tests drive it without a clock racing them, and so
// an operator command can force it.
func (db *DB) maintain() {
	defer db.wg.Done()

	sync := db.opts.Clock.NewTicker(db.opts.SyncInterval)
	defer sync.Stop()
	// Checking for a cut every block range would mean a full range of latency
	// on the first cut after startup; a tenth of it is frequent enough to be
	// prompt and rare enough to cost nothing.
	check := db.opts.Clock.NewTicker(maxDuration(db.opts.BlockRange/10, time.Second))
	defer check.Stop()

	for {
		select {
		case <-db.closed:
			// A final sync on the way out: the log is what a restart reads,
			// and the samples of the last interval are only in the page cache.
			if err := db.Sync(); err != nil {
				db.opts.Logger.Error("final log sync failed", "error", err)
			}
			return
		case <-sync.C():
			if err := db.Sync(); err != nil {
				db.opts.Logger.Error("log sync failed", "error", err)
			}
		case <-check.C():
			if err := db.CutBlock(); err != nil && !errors.Is(err, errNothingToCut) {
				db.opts.Logger.Error("cutting a block failed", "error", err)
			}
			// Compaction before retention: a merge can produce a block that
			// retention then immediately deletes, which wastes the work, but
			// the reverse order can leave sources that retention would have
			// removed being merged into a block that outlives them.
			if _, err := db.Compact(); err != nil {
				db.opts.Logger.Error("compaction failed", "error", err)
			}
			if err := db.ApplyRetention(); err != nil {
				db.opts.Logger.Error("applying retention failed", "error", err)
			}
		}
	}
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// Sync flushes the write-ahead log. With SyncOnAppend off this is what makes
// an acknowledged sample durable, within one SyncInterval.
func (db *DB) Sync() error { return db.wal.Sync() }

// errNothingToCut means the head has not grown past the threshold yet. It is
// the normal case, not a failure.
var errNothingToCut = errors.New("tsdb: the head does not span a full block range yet")

// CutBlock writes the head's oldest complete block range to disk, drops it
// from memory, and truncates the log up to it.
//
// The head is cut once it spans 1.5 block ranges rather than exactly one. The
// extra half range is landing room: samples arrive slightly out of step with
// each other, and cutting the instant a range completed would start rejecting
// anything a moment behind as out of bounds.
//
// The order is what makes it crash-safe. The block is written and renamed into
// place first, so it exists before anything is dropped; then the head forgets
// what the block now holds; then the log is truncated. A crash at any point
// leaves either the log or the block holding the data, never neither. The cost
// is that a crash in the middle can leave both, which replay handles — the
// head refuses samples a block already covers.
func (db *DB) CutBlock() error {
	db.cutMu.Lock()
	defer db.cutMu.Unlock()

	rangeMs := db.opts.BlockRange.Milliseconds()
	st := db.head.Stats()
	if st.Samples == 0 || st.MaxT-st.MinT < rangeMs*cutThreshold/2 {
		return errNothingToCut
	}
	// Cut on a range boundary so blocks tile the timeline without gaps or
	// overlaps, and so no chunk has to be split: the head already cut its
	// chunks at these same boundaries.
	cutAt := tsdb.BlockOf(st.MinT, rangeMs)*rangeMs + rangeMs
	// Close the range before reading it. Everything below cutAt is refused
	// from here on, so the snapshot below cannot miss a sample that an
	// appender slips in while the block is being written — that sample would
	// be acknowledged, left out of the block, and then dropped by the
	// Truncate at the end. See [head.Head.Freeze].
	db.head.Freeze(cutAt)

	// The block must cover *exactly* what the Truncate at the end throws away,
	// and that is everything below cutAt, with no lower bound. Using st.MinT
	// here instead looks equivalent and is not: [head.Head.Stats] walks the
	// series without holding a lock across the whole walk, so a series created
	// while it ran contributes no samples to MinT, and its samples — already
	// acknowledged — would fall below the snapshot's floor and be truncated
	// away unwritten. st.MinT decides *whether* to cut; it does not get to
	// decide what the cut contains.
	series, err := db.head.Select(tsdb.Selector{}, math.MinInt64, cutAt-1)
	if err != nil {
		// Freeze has already happened, so the range below cutAt is closed to
		// new samples until a later cut succeeds — but nothing has been
		// written or forgotten. Aborting leaves every sample where it is, in
		// the head and in the log; writing a block from a partial read would
		// put it in neither, because the Truncate below trusts the block to
		// hold everything under cutAt.
		return fmt.Errorf("tsdb: reading the head for a block cut: %w", err)
	}
	if len(series) == 0 {
		return errNothingToCut
	}

	blocksDir := filepath.Join(db.dir, blocksDirName)
	meta, err := block.Write(blocksDir, series, block.WriterOptions{Now: db.opts.Clock.Now()})
	if err != nil {
		if errors.Is(err, block.ErrEmpty) {
			return errNothingToCut
		}
		return fmt.Errorf("tsdb: writing a block: %w", err)
	}
	b, err := block.Open(filepath.Join(blocksDir, meta.ULID.String()))
	if err != nil {
		return fmt.Errorf("tsdb: opening the block just written: %w", err)
	}
	// Visible before the head forgets anything, so no query can fall into a
	// gap between the two.
	db.mu.Lock()
	db.blocks = append(db.blocks, b)
	sortBlocks(db.blocks)
	db.mu.Unlock()

	droppedSeries, droppedChunks := db.head.Truncate(cutAt)
	db.opts.Logger.Info("cut a block",
		"ulid", meta.ULID.String(),
		"minTime", meta.MinTime, "maxTime", meta.MaxTime,
		"series", meta.Stats.Series, "samples", meta.Stats.Samples,
		"headSeriesDropped", droppedSeries, "headChunksDropped", droppedChunks)

	if err := db.truncateLog(); err != nil {
		// The data is safe — it is in the block. A log that is longer than it
		// needs to be only costs disk and replay time, so this is reported,
		// not fatal.
		db.opts.Logger.Error("truncating the log failed", "error", err)
	}
	return nil
}

// truncateLog drops log segments whose samples are all in blocks.
//
// Which segments those are is not something the log can know — it holds opaque
// records — so the decision is made by the filter, per record, against the
// head's new minimum valid time: everything below it is in the block that was
// just written, everything at or above it is still only in the head and in
// this log.
//
// Deleting a segment outright because it is older than the one being written
// is what this used to do, and it was wrong: segments roll at 32 MiB while the
// head holds up to 1.5 block ranges, so on any real write rate the head's
// oldest samples are several segments back. See [head.KeepForCheckpoint].
func (db *DB) truncateLog() error {
	return wal.Truncate(filepath.Join(db.dir, walDirName), db.wal.Segment(),
		head.KeepForCheckpoint(db.head.MinValidTime()))
}

// Compact merges one run of adjacent same-level blocks, if there is one worth
// merging. It reports whether it did anything.
//
// One run per call, deliberately. A compaction reads and rewrites every byte
// of its inputs while queries are being served off the same disk; doing all
// the available work at once would make the tail latency of a query depend on
// how long the database had been running.
func (db *DB) Compact() (bool, error) {
	db.cutMu.Lock()
	defer db.cutMu.Unlock()

	plan := compact.Next(db.Blocks(), compact.Options{MaxBlockRange: db.opts.MaxBlockRange})
	if plan == nil {
		return false, nil
	}
	minT, maxT := plan.Span()
	blocksDir := filepath.Join(db.dir, blocksDirName)
	meta, err := compact.Run(blocksDir, plan, db.opts.Clock.Now())
	if err != nil {
		return false, err
	}
	mergedDir := filepath.Join(blocksDir, meta.ULID.String())
	merged, err := block.Open(mergedDir)
	if err != nil {
		// The sources are untouched, so nothing is lost — but the merged block
		// is on disk with nothing referencing it, and the next tick would plan
		// the identical run and write another copy of it, every tick, all of
		// it charged against MaxBytes. Take it back.
		return false, errors.Join(fmt.Errorf("tsdb: opening the merged block: %w", err), block.Delete(mergedDir))
	}
	// Swap the sources out for their replacement in one step, so no query can
	// see both or neither.
	condemned := make(map[string]bool, len(plan.Sources))
	for _, b := range plan.Sources {
		condemned[b.Meta().ULID.String()] = true
	}
	db.mu.Lock()
	kept := make([]*block.Block, 0, len(db.blocks))
	var replaced []*block.Block
	for _, b := range db.blocks {
		if condemned[b.Meta().ULID.String()] {
			replaced = append(replaced, b)
			continue
		}
		kept = append(kept, b)
	}
	db.blocks = append(kept, merged)
	sortBlocks(db.blocks)
	db.mu.Unlock()

	// Only now are the sources expendable: until the swap above, they were
	// the blocks queries read. A failure here is disk that was not reclaimed,
	// not data that was lost — each source is tombstoned, so CleanCondemned
	// finishes the job at startup — and the merged block is already serving,
	// so failing the compaction over it would re-plan work that is done.
	if err := compact.DeleteSources(plan); err != nil {
		db.opts.Logger.Error("deleting compacted source blocks", "error", err)
	}

	// Unlinked but still open: closing after the swap means no *new* query can
	// find them; one already in flight is holding a reader reference, and the
	// close waits for it.
	for _, b := range replaced {
		_ = b.Close()
	}
	db.opts.Logger.Info("compacted blocks",
		"ulid", meta.ULID.String(), "level", meta.Compaction.Level,
		"sources", len(plan.Sources), "minTime", minT, "maxTime", maxT,
		"series", meta.Stats.Series, "samples", meta.Stats.Samples)
	return true, nil
}

// ApplyRetention deletes blocks whose newest sample has aged out, and then, if
// the database is still over MaxBytes, the oldest blocks until it is not.
//
// Retention is enforced on whole blocks, never on samples inside one: a block
// is immutable, so deleting part of it means rewriting it. That is why the
// oldest data can outlive the retention window by up to one block range, and
// why the block range is a retention-granularity decision as much as a
// compaction one.
func (db *DB) ApplyRetention() error {
	// The same lock a block cut and a compaction take. Retention closes and
	// unlinks blocks, and compaction spends seconds reading the ones it is
	// merging; without this they are only ever safe because maintain() happens
	// to call them one after another, and both are exported precisely so that
	// a test or an operator can call them when it does not.
	db.cutMu.Lock()
	defer db.cutMu.Unlock()

	// The two rules are independent: turning off time-based retention must not
	// also turn off the disk backstop, which is the one that keeps the process
	// alive when a metric explodes.
	var expired []*block.Block
	if db.opts.Retention >= 0 {
		cutoff := db.opts.Clock.Now().Add(-db.opts.Retention).UnixMilli()
		db.mu.Lock()
		var kept []*block.Block
		for _, b := range db.blocks {
			if b.Meta().MaxTime < cutoff {
				expired = append(expired, b)
				continue
			}
			kept = append(kept, b)
		}
		db.blocks = kept
		db.mu.Unlock()
	}

	// Out of the slice first, so no new query can find them. A query already
	// reading one holds a reader reference and keeps the file descriptor
	// alive; the unlink underneath it is safe, because on Unix an open file
	// outlives its directory entry.
	var errs []error
	for _, b := range expired {
		if err := db.drop(b, "past the retention window"); err != nil {
			errs = append(errs, err)
		}
	}
	if err := db.applySizeCap(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// applySizeCap deletes the oldest blocks until the database fits in MaxBytes.
//
// The head and the log are counted but cannot be evicted — they hold data that
// is not on disk anywhere else — so a cap smaller than they are will delete
// every block and still not be met. That is reported once per pass rather than
// looped on.
func (db *DB) applySizeCap() error {
	if db.opts.MaxBytes <= 0 {
		return nil
	}
	used, err := db.DiskUsage()
	if err != nil || used <= db.opts.MaxBytes {
		return err
	}
	var errs []error
	for _, b := range db.Blocks() { // oldest first
		if used <= db.opts.MaxBytes {
			break
		}
		size, err := dirSize(b.Dir())
		if err != nil {
			errs = append(errs, err)
			continue
		}
		db.mu.Lock()
		kept := make([]*block.Block, 0, len(db.blocks))
		for _, other := range db.blocks {
			if other != b {
				kept = append(kept, other)
			}
		}
		db.blocks = kept
		db.mu.Unlock()

		if err := db.drop(b, "over the size cap", "bytes", size, "cap", db.opts.MaxBytes); err != nil {
			errs = append(errs, err)
			continue
		}
		used -= size
	}
	if used > db.opts.MaxBytes {
		db.opts.Logger.Warn("still over the size cap after deleting every block; "+
			"the head and the log alone exceed it",
			"bytes", used, "cap", db.opts.MaxBytes)
	}
	return errors.Join(errs...)
}

// drop closes and deletes one block that is already out of db.blocks.
func (db *DB) drop(b *block.Block, why string, args ...any) error {
	dir := b.Dir()
	closeErr := b.Close()
	if err := block.Delete(dir); err != nil {
		return errors.Join(closeErr, err)
	}
	db.opts.Logger.Info("deleted a block",
		append([]any{"ulid", b.Meta().ULID.String(), "reason", why}, args...)...)
	return closeErr
}

func sortBlocks(blocks []*block.Block) {
	sort.Slice(blocks, func(i, j int) bool {
		a, b := blocks[i].Meta(), blocks[j].Meta()
		if a.MinTime != b.MinTime {
			return a.MinTime < b.MinTime
		}
		return a.ULID.Compare(b.ULID) < 0
	})
}

// DiskUsage reports the bytes the database occupies, for self-metrics.
func (db *DB) DiskUsage() (int64, error) { return dirSize(db.dir) }

func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil // deleted under us by retention; not an error
			}
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return total, err
	}
	return total, nil
}
