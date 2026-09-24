package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/block"
	"github.com/tuvo1106/ozymandias/internal/tsdb/head"
	"github.com/tuvo1106/ozymandias/internal/tsdb/index"
	"github.com/tuvo1106/ozymandias/internal/tsdb/wal"
)

// Directory layout under Options.Dir.
const (
	walDirName    = "wal"
	blocksDirName = "blocks"
)

// Defaults. Block range and retention are the two numbers an operator is most
// likely to change; the rest exist so the zero Options is usable.
const (
	DefaultBlockRange = 2 * time.Hour
	DefaultRetention  = 15 * 24 * time.Hour
	// DefaultSyncInterval is the group-commit period: how long an
	// acknowledged sample can sit in the page cache when SyncOnAppend is off.
	DefaultSyncInterval = 100 * time.Millisecond
	// cutThreshold is how much wider than one block range the head may grow
	// before its oldest range is cut out. The margin exists so that a sample
	// arriving slightly late still has somewhere to land: cutting the instant
	// a range completes would reject anything a millisecond behind.
	cutThreshold = 3 // times BlockRange, halved — i.e. 1.5x
)

// Options configure a DB.
type Options struct {
	// Dir holds wal/ and blocks/. Created if missing.
	Dir string
	// BlockRange is how much time one block covers. Chunks are cut at its
	// boundaries, so changing it on an existing directory is safe but old
	// blocks keep their original range.
	BlockRange time.Duration
	// Retention deletes blocks whose newest sample is older than this. Zero
	// uses the default; negative keeps everything.
	Retention time.Duration
	// MaxBytes caps the database's total size. When it is exceeded the oldest
	// blocks are deleted until it is not. Zero means no cap.
	//
	// It is a backstop for a disk filling up, not a retention policy: it
	// deletes by age with no regard for what the data is, so a cardinality
	// spike can silently evict history. Retention is the setting to reach for
	// first.
	MaxBytes int64
	// MaxBlockRange caps how wide a compacted block may become; zero uses
	// [compact.DefaultMaxBlockRange].
	MaxBlockRange time.Duration
	// MaxSeriesPerMetric bounds cardinality; see [head.Options].
	MaxSeriesPerMetric int
	// SyncOnAppend fsyncs the WAL before Append returns, trading throughput
	// for a zero-length window of acknowledged-but-unsynced data.
	SyncOnAppend bool
	// SyncInterval is the group-commit period when SyncOnAppend is off.
	SyncInterval time.Duration
	// Clock is the time source; nil means [clock.Real].
	Clock clock.Clock
	// Logger receives block cuts, compactions and retention deletions. Nil
	// discards them.
	Logger *slog.Logger
}

func (o *Options) setDefaults() {
	if o.BlockRange <= 0 {
		o.BlockRange = DefaultBlockRange
	}
	if o.Retention == 0 {
		o.Retention = DefaultRetention
	}
	if o.SyncInterval <= 0 {
		o.SyncInterval = DefaultSyncInterval
	}
	if o.Clock == nil {
		o.Clock = clock.Real()
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
}

// DB is the real time series database: a write-ahead log, an in-memory head,
// and a set of immutable blocks on disk.
//
// It is the [tsdb.MetricStore] the rest of ozymandias talks to, and the place
// where the pieces below it meet. The division of labour is the one every
// LSM-shaped store uses: the WAL makes a write durable immediately but is
// useless for reading, the head makes it queryable but is lost on a crash, and
// blocks make it both — at the cost of being written only in batches.
type DB struct {
	opts      Options
	dir       string
	head      *head.Head
	wal       *wal.WAL
	closed    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	// mu guards blocks. Blocks themselves are immutable, so a reader takes a
	// snapshot of the slice and releases the lock before touching any of them.
	mu     sync.RWMutex
	blocks []*block.Block

	cutMu sync.Mutex // one block cut or compaction at a time
}

var _ tsdb.MetricStore = (*DB)(nil)

// Open opens or creates the database in opts.Dir.
//
// The startup sequence matters. Leftovers from an interrupted delete or block
// write are cleared first, because a half-written block must never be read.
// Then blocks are opened, so the head knows which time ranges are already
// persisted. Only then is the WAL replayed: its samples are the ones that had
// not yet reached a block, and any that a block already covers are refused by
// the head's own bounds check rather than duplicated.
func Open(opts Options) (*DB, error) {
	opts.setDefaults()
	if opts.Dir == "" {
		return nil, errors.New("tsdb: Dir is required")
	}
	blocksDir := filepath.Join(opts.Dir, blocksDirName)
	if err := os.MkdirAll(blocksDir, 0o755); err != nil {
		return nil, fmt.Errorf("tsdb: creating %s: %w", blocksDir, err)
	}
	if removed, err := block.CleanCondemned(blocksDir); err != nil {
		return nil, err
	} else if len(removed) > 0 {
		opts.Logger.Warn("finished interrupted block deletions", "blocks", removed)
	}
	if removed, err := block.CleanTmp(blocksDir); err != nil {
		return nil, err
	} else if len(removed) > 0 {
		opts.Logger.Warn("removed incomplete blocks", "blocks", removed)
	}
	blocks, err := block.OpenAll(blocksDir)
	if err != nil {
		return nil, err
	}
	blocks, err = dropSuperseded(blocks, opts.Logger)
	if err != nil {
		closeBlocks(blocks)
		return nil, err
	}

	// Before the log is opened for writing: a crash can leave a partial record
	// at its end, and appending behind one puts every later write somewhere
	// replay can never reach. See [wal.Repair].
	walDir := filepath.Join(opts.Dir, walDirName)
	repaired, err := wal.Repair(walDir)
	if err != nil {
		closeBlocks(blocks)
		return nil, fmt.Errorf("tsdb: repairing the log: %w", err)
	}
	if repaired {
		opts.Logger.Warn("discarded a torn record at the end of the write-ahead log",
			"dir", walDir)
	}

	w, err := wal.Open(wal.Options{Dir: walDir})
	if err != nil {
		closeBlocks(blocks)
		return nil, err
	}
	db := &DB{
		opts:   opts,
		dir:    opts.Dir,
		wal:    w,
		blocks: blocks,
		closed: make(chan struct{}),
	}
	db.head = head.New(head.Options{
		WAL:                w,
		BlockRange:         opts.BlockRange.Milliseconds(),
		MaxSeriesPerMetric: opts.MaxSeriesPerMetric,
		SyncOnAppend:       opts.SyncOnAppend,
	})
	// Everything a block already covers is off limits to the head, so replay
	// cannot re-admit a sample that is also on disk.
	if maxT, ok := db.blocksMaxTime(); ok {
		db.head.Truncate(maxT + 1)
	}
	st, err := head.Replay(db.head, filepath.Join(opts.Dir, walDirName))
	if err != nil {
		_ = w.Close()
		closeBlocks(blocks)
		return nil, fmt.Errorf("tsdb: replaying the log: %w", err)
	}
	if st.Samples > 0 || st.OOORejected > 0 {
		opts.Logger.Info("replayed the write-ahead log",
			"samples", st.Samples, "series", st.Series, "skipped", st.OOORejected)
	}

	db.wg.Add(1)
	go db.maintain()
	return db, nil
}

// dropSuperseded deletes any block that a surviving block was compacted from.
//
// Compaction writes the merged block, renames it into place, and only then
// deletes its sources — so a crash in between leaves the data twice over. That
// is the safe direction to fail, and it is deliberate, but nothing was
// finishing the job: the sources carry no tombstone, because a tombstone is
// written inside the delete that never ran. They would sit there forever,
// invisible to queries (Select dedups the overlap) and fully charged against
// MaxBytes — and the next compaction would merge the same run again, producing
// a second identical block that no later pass can reduce.
//
// A block's meta records what it was merged from, which makes the leftovers
// identifiable: anything named as a source by a block that is present has
// already been superseded by it. One pass is enough even for a chain of
// interrupted compactions, because the intermediate blocks are still on disk
// and their own source lists are read in the same sweep.
func dropSuperseded(blocks []*block.Block, log *slog.Logger) ([]*block.Block, error) {
	superseded := map[string]bool{}
	for _, b := range blocks {
		for _, src := range b.Meta().Compaction.Sources {
			superseded[src.String()] = true
		}
	}
	if len(superseded) == 0 {
		return blocks, nil
	}
	kept := make([]*block.Block, 0, len(blocks))
	var errs []error
	for _, b := range blocks {
		id := b.Meta().ULID.String()
		if !superseded[id] {
			kept = append(kept, b)
			continue
		}
		dir := b.Dir()
		if err := b.Close(); err != nil {
			errs = append(errs, err)
		}
		if err := block.Delete(dir); err != nil {
			errs = append(errs, fmt.Errorf("tsdb: removing superseded block %s: %w", id, err))
			continue
		}
		log.Warn("removed a block its replacement had already been compacted from",
			"ulid", id, "level", b.Meta().Compaction.Level)
	}
	if err := errors.Join(errs...); err != nil {
		return kept, err
	}
	return kept, nil
}

func closeBlocks(blocks []*block.Block) {
	for _, b := range blocks {
		_ = b.Close()
	}
}

// blocksMaxTime is the newest sample any block holds.
func (db *DB) blocksMaxTime() (int64, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	maxT, ok := int64(math.MinInt64), false
	for _, b := range db.blocks {
		if t := b.Meta().MaxTime; t > maxT {
			maxT, ok = t, true
		}
	}
	return maxT, ok
}

// Append implements [tsdb.MetricStore].
//
// Per-series problems are reported in the result rather than failing the
// batch: one series over the cardinality limit, or one carrying a late
// sample, must not cost the other 4999 series in an agent payload.
func (db *DB) Append(ctx context.Context, batch []tsdb.SeriesSamples) (tsdb.AppendResult, error) {
	var res tsdb.AppendResult
	if err := ctx.Err(); err != nil {
		return res, err
	}
	refs := make([]tsdb.SeriesRef, 0, len(batch))
	samples := make([]head.Sample, 0, len(batch))
	// counts[i] is how many samples of entry i are in the flattened slices, so
	// a rejection can be attributed back to the series that caused it.
	owner := make([]int, 0, len(batch))

	for i, ss := range batch {
		if reason := validate(ss); reason != "" {
			res.Rejected = append(res.Rejected, tsdb.Rejected{Series: ss.Series, Reason: reason})
			continue
		}
		for _, s := range ss.Samples {
			refs = append(refs, ss.Series)
			samples = append(samples, head.Sample{T: s.T, V: s.V})
			owner = append(owner, i)
		}
	}
	if len(samples) == 0 {
		return res, nil
	}
	stored, rejected := db.head.Append(refs, samples)

	// One entry per series, however many of its samples were refused: the
	// caller wants to know which series had a problem, not 120 copies of the
	// same sentence.
	refused := make([]bool, len(batch))
	for _, r := range rejected {
		if r.Index < 0 {
			// Not about any one series — a failed log write. Nothing in the
			// batch was applied, so the whole call failed.
			return tsdb.AppendResult{}, r.Err
		}
		idx := owner[r.Index]
		if refused[idx] {
			continue
		}
		refused[idx] = true
		res.Rejected = append(res.Rejected, tsdb.Rejected{
			Series: batch[idx].Series,
			Reason: reasonOf(r.Err),
		})
	}
	// Series counts the entries that stored something. An entry whose samples
	// were all duplicates of what is already there stored nothing, and saying
	// otherwise would make a retried batch look like fresh data.
	wrote := make([]bool, len(batch))
	for i, ok := range stored {
		if !ok {
			continue
		}
		res.Samples++
		wrote[owner[i]] = true
	}
	for _, ok := range wrote {
		if ok {
			res.Series++
		}
	}
	return res, nil
}

func reasonOf(err error) string {
	switch {
	case errors.Is(err, head.ErrOutOfOrder):
		return "sample at or before the series' newest timestamp"
	case errors.Is(err, head.ErrOutOfBounds):
		return "sample is older than the oldest writable block range"
	case errors.Is(err, head.ErrSeriesLimit):
		return "series limit for this metric reached"
	default:
		return err.Error()
	}
}

func validate(ss tsdb.SeriesSamples) string {
	if err := ss.Series.Validate(); err != nil {
		return err.Error()
	}
	for _, s := range ss.Samples {
		if math.IsNaN(s.V) || math.IsInf(s.V, 0) {
			return "non-finite sample value"
		}
	}
	return ""
}

// Stats implements [tsdb.MetricStore]: the head plus every block.
func (db *DB) Stats() tsdb.StoreStats {
	hs := db.head.Stats()
	out := tsdb.StoreStats{Series: hs.Series, Samples: hs.Samples}
	db.mu.RLock()
	defer db.mu.RUnlock()
	for _, b := range db.blocks {
		m := b.Meta()
		out.Series += int64(m.Stats.Series)
		out.Samples += int64(m.Stats.Samples)
	}
	return out
}

// HeadStats exposes the head's own counters for self-metrics.
func (db *DB) HeadStats() head.Stats { return db.head.Stats() }

// Blocks returns the currently open blocks, oldest first.
func (db *DB) Blocks() []*block.Block {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return append([]*block.Block(nil), db.blocks...)
}

// Close stops maintenance, flushes the log and releases every file.
//
// It deliberately does not cut a block. The WAL holds everything the head
// holds; replay is how that comes back, and writing a partial block on the way
// out would mean doing the riskiest operation in the codebase at the least
// convenient moment.
func (db *DB) Close() error {
	// sync.Once, not a check-then-act on the channel: two goroutines racing
	// through a select can both take the default branch, and the second
	// close of a closed channel panics.
	var ran bool
	db.closeOnce.Do(func() {
		ran = true
		close(db.closed)
	})
	if !ran {
		return nil
	}
	db.wg.Wait()

	err := db.wal.Close()
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, b := range db.blocks {
		if cerr := b.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	db.blocks = nil
	return err
}

// lookups returns the head's index and every block's, for metadata queries.
func (db *DB) lookups() []index.Lookup {
	db.mu.RLock()
	blocks := db.blocks
	out := make([]index.Lookup, 0, len(blocks)+1)
	for _, b := range blocks {
		out = append(out, b.Lookup())
	}
	db.mu.RUnlock()
	return append(out, db.head.Lookup())
}
