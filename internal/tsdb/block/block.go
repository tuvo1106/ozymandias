package block

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/chunkenc"
	"github.com/tuvo1106/ozymandias/internal/tsdb/index"
)

// Block is an open, immutable block: its index in memory and its chunks file
// held open for reads.
//
// Immutability is what makes everything else simple. A block can be read by
// any number of goroutines with no locking on the data itself (the only lock
// here counts readers, so a block being deleted underneath one stays open long
// enough to finish), and compaction never edits — it writes a new block and
// deletes the sources. The only mutable data in the TSDB is the head.
type Block struct {
	dir    string
	meta   Meta
	ix     *indexReader
	chunks *chunkReader

	// mu guards the reader count and the closing flag. A block outlives the
	// list it was removed from: compaction and retention take blocks out of
	// the database's slice and close them, but a query that snapshotted that
	// slice a moment earlier is still reading chunks.dat. The count is what
	// keeps the file descriptor alive until it is finished. See [Block.Read].
	mu      sync.Mutex
	readers int
	closing bool
}

// ErrCorrupt marks damage to a block's own files: a meta.json that does not parse, an index that fails its
// checksum, a chunks.dat that is not one or that ends before the index says it does, a chunk that fails its
// checksum, a file that has gone missing. It is what [OpenAll] quarantines on, and nothing else.
//
// What is not ErrCorrupt matters as much. A block written by a different version of this software
// is fine and a build that cannot read it must not hide it. An I/O error (EIO, EMFILE, EACCES) says
// nothing about the block, which may be perfectly healthy. Both still stop the open, as they always
// did: moving a good block aside because of a transient fault is the one mistake a quarantine
// cannot afford.
var ErrCorrupt = errors.New("block: corrupt")

// missingIsCorrupt turns a file that is not there into damage, and leaves every other error as the
// I/O error it is. The caller has already seen the block's meta.json, so a block without its index or its
// chunks has lost them.
func missingIsCorrupt(err error, path string) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s is missing", ErrCorrupt, path)
	}
	return err
}

// Open reads a block directory. An error that wraps [ErrCorrupt] means the block's files are damaged;
// any other error is not a verdict on the block.
//
// What it checks is what opening reads: the meta, the whole index (checksummed, then decoded, so every
// chunk reference is known), the chunks file's header, and that the chunks file is as long as the index
// needs it to be. It does not read the chunks. A flipped bit inside one is found when a query reads that
// chunk, whose checksum then fails with an error wrapping ErrCorrupt (never a wrong sample), which is the
// price of an open that does not read the whole block.
func Open(dir string) (*Block, error) {
	m, err := ReadMeta(dir)
	if err != nil {
		return nil, missingIsCorrupt(err, filepath.Join(dir, MetaFilename))
	}
	ix, err := openIndexReader(dir)
	if err != nil {
		return nil, err
	}
	cr, err := openChunkReader(dir)
	if err != nil {
		return nil, err
	}
	if end := ix.chunksEnd(); end > uint64(cr.size) {
		_ = cr.close()
		return nil, fmt.Errorf("%w: %s is %d bytes and its index needs %d: it was cut short",
			ErrCorrupt, filepath.Join(dir, ChunksFilename), cr.size, end)
	}
	return &Block{dir: dir, meta: m, ix: ix, chunks: cr}, nil
}

// QuarantineMark is in the name of a block directory that was set aside as damaged:
// "<ulid>.corrupt-<unix seconds>". Nothing opens, compacts, expires or deletes such a directory. It is the
// operator's to inspect, restore (rename it back and restart) or remove.
const QuarantineMark = ".corrupt-"

// IsQuarantined reports whether a directory name is a quarantined block's.
func IsQuarantined(name string) bool { return strings.Contains(name, QuarantineMark) }

// Quarantined is a block that [OpenAll] set aside.
type Quarantined struct {
	// Name is the directory's name before it was moved, MovedTo its path now.
	Name, MovedTo string
	// Cause is what was wrong; it wraps [ErrCorrupt].
	Cause error
	// MinTime and MaxTime (Unix milliseconds) are the time the block covered, if its meta.json could
	// still be read. They say what data is no longer served.
	MinTime, MaxTime int64
	HasRange         bool
}

// Quarantine moves a damaged block directory aside, to "<dir>.corrupt-<unix seconds of now>", and makes
// the move durable. It never deletes anything: the files stay exactly as they were found. If the block
// cannot be moved, that is an error, and the caller must not carry on as if it had been.
func Quarantine(dir string, cause error, now time.Time) (Quarantined, error) {
	q := Quarantined{Name: filepath.Base(dir), Cause: cause}
	if m, err := ReadMeta(dir); err == nil {
		q.MinTime, q.MaxTime, q.HasRange = m.MinTime, m.MaxTime, true
	}
	to := fmt.Sprintf("%s%s%d", dir, QuarantineMark, now.Unix())
	if err := os.Rename(dir, to); err != nil {
		return q, fmt.Errorf("block: quarantining %s: %w", dir, err)
	}
	q.MovedTo = to
	if err := syncDir(filepath.Dir(dir)); err != nil {
		return q, fmt.Errorf("block: syncing after quarantining %s: %w", dir, err)
	}
	return q, nil
}

// ListQuarantined is the names of the quarantined block directories under parent, in name order. They
// outlive a restart, so this is how a count of them survives one.
func ListQuarantined(parent string) ([]string, error) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && IsQuarantined(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// OpenAll opens every complete block under parent, sorted by time, and quarantines the damaged ones
// (see [ErrCorrupt]) instead of failing: the rest are served and the caller is told what was set aside,
// so that it can say so loudly. now names the quarantine directories.
//
// It still fails on anything that is not damage: an I/O error, a block of a version this build does
// not read, a damaged block it cannot move aside. Directories that are already quarantined are left
// alone. ".tmp" directories are skipped rather than reported: [CleanTmp] is what removes them.
//
// A directory with files in it and no meta.json is a block that lost its meta. Nothing else explains one
// here: a crashed write is a ".tmp" directory, and a crashed deletion is tombstoned and is finished by
// [CleanCondemned] before this runs. [CleanTmp] used to delete such a directory; it is quarantined.
func OpenAll(parent string, now time.Time) (blocks []*Block, quarantined []Quarantined, err error) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	// Close what is already open when giving up. Returning them alongside the error reads as
	// helpful and is not: every caller checks err first and drops the slice, and each of those blocks
	// is holding a chunks.dat descriptor.
	fail := func(err error) ([]*Block, []Quarantined, error) {
		for _, opened := range blocks {
			_ = opened.Close()
		}
		return nil, nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || filepath.Ext(e.Name()) == tmpSuffix || IsQuarantined(e.Name()) {
			continue
		}
		dir := filepath.Join(parent, e.Name())
		var cause error
		if _, err := os.Stat(filepath.Join(dir, MetaFilename)); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return fail(fmt.Errorf("block: looking for the meta of %s: %w", e.Name(), err))
			}
			inside, err := os.ReadDir(dir)
			if err != nil {
				return fail(fmt.Errorf("block: reading %s: %w", e.Name(), err))
			}
			if len(inside) == 0 {
				continue // nothing in it to lose
			}
			cause = fmt.Errorf("%w: %s has files and no %s", ErrCorrupt, e.Name(), MetaFilename)
		} else {
			b, err := Open(dir)
			if err == nil {
				blocks = append(blocks, b)
				continue
			}
			if !errors.Is(err, ErrCorrupt) {
				return fail(fmt.Errorf("block: opening %s: %w", e.Name(), err))
			}
			cause = err
		}
		q, err := Quarantine(dir, cause, now)
		if err != nil {
			return fail(errors.Join(fmt.Errorf("block: %s is damaged and could not be set aside", e.Name()), cause, err))
		}
		quarantined = append(quarantined, q)
	}
	sort.Slice(blocks, func(i, j int) bool {
		if blocks[i].meta.MinTime != blocks[j].meta.MinTime {
			return blocks[i].meta.MinTime < blocks[j].meta.MinTime
		}
		return blocks[i].meta.ULID.Compare(blocks[j].meta.ULID) < 0
	})
	return blocks, quarantined, nil
}

// Meta returns the block's metadata.
func (b *Block) Meta() Meta { return b.meta }

// Dir returns the block's directory.
func (b *Block) Dir() string { return b.dir }

// Lookup exposes the block's index for queries and metadata.
func (b *Block) Lookup() index.Lookup { return b.ix }

// Overlaps reports whether the block holds any time in [from, to].
func (b *Block) Overlaps(from, to int64) bool {
	return b.meta.MinTime <= to && from <= b.meta.MaxTime
}

// Close releases the block's file handles.
func (b *Block) Close() error {
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return nil // idempotent: retention and shutdown can both reach a block
	}
	b.closing = true
	idle := b.readers == 0
	b.mu.Unlock()
	if idle {
		return b.chunks.close()
	}
	// A reader has it. The last one out closes the file; the error from that
	// close is lost, which is the right trade for a file being discarded
	// anyway — the alternative is blocking compaction on a query.
	return nil
}

// Acquire registers a reader and reports whether the block is still readable.
// Every Acquire that returns true is paired with a [Block.Release].
//
// False means the block has been closed, and a caller must read that as "this
// block is gone", never as "this block is empty" — the difference is silently
// wrong query results. A caller that cannot afford to miss the data acquires
// while holding whatever lock guards the list it found the block in: a block
// still in that list has not been closed, because removing it from the list
// comes first, so the acquire cannot fail. That is the discipline the
// database's query path follows.
func (b *Block) Acquire() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closing {
		return false
	}
	b.readers++
	return true
}

// Release ends a read started by [Block.Acquire], closing the block's files if
// it was closed while this reader held them.
func (b *Block) Release() {
	b.mu.Lock()
	b.readers--
	last := b.readers == 0 && b.closing
	b.mu.Unlock()
	if last {
		_ = b.chunks.close()
	}
}

// Select returns the series matching sel with samples in [from, to], sorted by
// series key.
//
// Chunk references carry their own time range, so a query reads only the
// chunks that overlap the window: a 5-minute query against a 2-hour block
// touches a handful of the block's chunks, not all of them. That is what the
// per-chunk minT/maxT in the index buy.
func (b *Block) Select(sel tsdb.Selector, from, to int64) ([]tsdb.SeriesSamples, error) {
	if !b.Overlaps(from, to) {
		return nil, nil
	}
	ids := index.Select(b.ix, sel)
	out := make([]tsdb.SeriesSamples, 0, len(ids))
	for _, id := range ids {
		if id >= uint64(len(b.ix.series)) {
			return nil, fmt.Errorf("block: %s: postings name series %d of %d",
				b.meta.ULID, id, len(b.ix.series))
		}
		s := b.ix.series[id]
		samples, err := b.samples(s, from, to)
		if err != nil {
			return nil, err
		}
		if len(samples) == 0 {
			continue
		}
		out = append(out, tsdb.SeriesSamples{Series: s.ref, Samples: samples})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Series.Key() < out[j].Series.Key() })
	return out, nil
}

func (b *Block) samples(s blockSeries, from, to int64) ([]tsdb.Sample, error) {
	var out []tsdb.Sample
	for _, cr := range s.chunks {
		if cr.maxT < from || cr.minT > to {
			continue
		}
		enc, data, err := b.chunks.at(cr.off, cr.recLen)
		if err != nil {
			return nil, fmt.Errorf("block: %s: %s: %w", b.meta.ULID, s.ref.Key(), err)
		}
		if enc != EncodingGorilla {
			return nil, fmt.Errorf("block: %s: %s: chunk encoding %d is not supported",
				b.meta.ULID, s.ref.Key(), enc)
		}
		c, err := chunkenc.FromBytes(data)
		if err != nil {
			return nil, fmt.Errorf("block: %s: %s: %w", b.meta.ULID, s.ref.Key(), err)
		}
		it := c.Iterator()
		for it.Next() {
			t, v := it.At()
			if t < from {
				continue
			}
			if t > to {
				break // samples ascend, so the rest of this chunk is out too
			}
			out = append(out, tsdb.Sample{T: t, V: v})
		}
		if err := it.Err(); err != nil {
			return nil, fmt.Errorf("block: %s: %s: decoding: %w", b.meta.ULID, s.ref.Key(), err)
		}
	}
	return out, nil
}

// All returns every series in the block with its samples, in key order. It is
// what compaction reads, and what a differential test compares against the
// head it was written from.
func (b *Block) All() ([]tsdb.SeriesSamples, error) {
	out := make([]tsdb.SeriesSamples, 0, len(b.ix.series))
	for _, s := range b.ix.series {
		samples, err := b.samples(s, b.meta.MinTime, b.meta.MaxTime)
		if err != nil {
			return nil, err
		}
		out = append(out, tsdb.SeriesSamples{Series: s.ref, Samples: samples})
	}
	return out, nil
}

// Delete removes a block from disk. It is used by compaction (after its
// replacement is durable) and by retention.
//
// A tombstone marks the block as condemned before anything is unlinked, so a
// crash halfway through leaves a directory that startup finishes removing
// rather than a block missing half its files.
//
// Deleting a block that is already gone succeeds. Both callers retry after a
// crash, and they cannot tell "I deleted this last time" from "this was never
// here" — making the second attempt an error would turn a finished job into a
// permanent failure.
func Delete(dir string) error {
	tomb := filepath.Join(dir, tombstoneFilename)
	if err := writeFileSync(tomb, []byte("deleted\n")); err != nil && !os.IsNotExist(err) {
		return err
	}
	// The tombstone's *directory entry* has to be durable too, not just its
	// contents. writeFileSync fsyncs the file; without this the entry is still
	// only in the page cache when the unlinking below starts, so a crash can
	// persist a removed chunks.dat and lose the tombstone that was supposed to
	// explain it. Startup then finds a block with missing files and no mark:
	// CleanCondemned skips it, CleanTmp keeps it because meta.json is there,
	// and Open fails — ozyd will not start, which is the exact outcome the
	// tombstone exists to prevent. Writer.Close already syncs its directory
	// for the same reason; this was the asymmetry.
	if err := syncDir(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := removeCondemned(dir, os.RemoveAll, syncDir); err != nil {
		return fmt.Errorf("block: deleting %s: %w", dir, err)
	}
	return syncDir(filepath.Dir(dir))
}

// removeCondemned removes a tombstoned block directory, meta.json first.
//
// meta.json is what makes a directory a block: OpenAll opens only directories
// that have one, and CleanTmp removes those that do not. So it goes first, and
// the directory is fsynced before anything else is unlinked. From then on a
// crash leaves a directory startup removes whatever else is still in it;
// before then the block is whole and tombstoned. The rest can go in any order.
// The fsync is what carries that across a power cut, where unlinks can reach
// the disk out of order; the tests check it is called in that place, but no
// test can cut the power.
//
// Not os.RemoveAll on its own: that unlinks in the order the filesystem lists
// entries. Where the tombstone was listed before meta.json, a crash between
// the two left meta.json, no chunks.dat and no tombstone: a block startup
// tried to open and failed on. The crash loop began failing that way when CI's
// runner image changed; a new listing order is the likely reason, not one we
// reproduced.
//
// remove is os.RemoveAll and sync is syncDir outside tests, which stop remove
// partway to stand in for a crash and record when sync runs. Like os.RemoveAll,
// it keeps going past an entry it cannot remove, so one bad entry does not
// leave the others behind.
func removeCondemned(dir string, remove, sync func(string) error) error {
	if err := remove(filepath.Join(dir, MetaFilename)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := sync(dir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if err := remove(filepath.Join(dir, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return remove(dir)
}

const tombstoneFilename = "tombstone"

// Condemned reports whether a block directory was being deleted when the
// process died, in which case finishing the job is the only safe move: the
// block may already be missing files.
func Condemned(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, tombstoneFilename))
	return err == nil
}

// CleanCondemned finishes interrupted deletions under parent.
func CleanCondemned(parent string) (removed []string, err error) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	for _, e := range entries {
		dir := filepath.Join(parent, e.Name())
		if !e.IsDir() || !Condemned(dir) {
			continue
		}
		if err := removeCondemned(dir, os.RemoveAll, syncDir); err != nil {
			return removed, errors.Join(fmt.Errorf("block: finishing deletion of %s", dir), err)
		}
		removed = append(removed, e.Name())
	}
	if len(removed) > 0 {
		return removed, syncDir(parent)
	}
	return removed, nil
}

// Series returns the block's series identities in key order, without touching
// the chunks file. Compaction needs to know what a block holds before it
// decides to read any of it.
func (b *Block) Series() []tsdb.SeriesRef {
	out := make([]tsdb.SeriesRef, len(b.ix.series))
	for i, s := range b.ix.series {
		out[i] = s.ref
	}
	return out
}

// SeriesOf returns the identity of every series of one metric in the block,
// from the index alone. An id past the series table is the same corruption
// [Block.Select] refuses, and is refused the same way.
func (b *Block) SeriesOf(metric string) ([]tsdb.SeriesRef, error) {
	ids := b.ix.Postings(index.MetricName, metric)
	out := make([]tsdb.SeriesRef, 0, len(ids))
	for _, id := range ids {
		if id >= uint64(len(b.ix.series)) {
			return nil, fmt.Errorf("block: %s: postings name series %d of %d", b.meta.ULID, id, len(b.ix.series))
		}
		out = append(out, b.ix.series[id].ref)
	}
	return out, nil
}

// SamplesFor returns every sample of one series, or nil if the block does not
// hold it. Series are stored in key order, so the lookup is a binary search.
//
// This is what lets compaction merge k blocks while holding one series in
// memory rather than all of them: the alternative, reading each block whole,
// would make the memory cost of a compaction the size of its output.
func (b *Block) SamplesFor(key string) ([]tsdb.Sample, error) {
	i := sort.Search(len(b.ix.series), func(i int) bool {
		return b.ix.series[i].ref.Key() >= key
	})
	if i == len(b.ix.series) || b.ix.series[i].ref.Key() != key {
		return nil, nil
	}
	return b.samples(b.ix.series[i], b.meta.MinTime, b.meta.MaxTime)
}
