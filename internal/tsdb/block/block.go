package block

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

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

// Open reads a block directory.
func Open(dir string) (*Block, error) {
	m, err := ReadMeta(dir)
	if err != nil {
		return nil, err
	}
	ix, err := openIndexReader(dir)
	if err != nil {
		return nil, err
	}
	cr, err := openChunkReader(dir)
	if err != nil {
		return nil, err
	}
	return &Block{dir: dir, meta: m, ix: ix, chunks: cr}, nil
}

// OpenAll opens every complete block under parent, sorted by time. Incomplete
// directories are skipped rather than reported: [CleanTmp] is what removes
// them, and a reader's job is to read what is there.
func OpenAll(parent string) ([]*Block, error) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Block
	for _, e := range entries {
		if !e.IsDir() || filepath.Ext(e.Name()) == tmpSuffix {
			continue
		}
		dir := filepath.Join(parent, e.Name())
		if _, err := os.Stat(filepath.Join(dir, MetaFilename)); err != nil {
			continue
		}
		b, err := Open(dir)
		if err != nil {
			// Close what is already open. Returning them alongside the error
			// reads as helpful and is not: every caller checks err first and
			// drops the slice, and each of those blocks is holding a
			// chunks.dat descriptor.
			for _, opened := range out {
				_ = opened.Close()
			}
			return nil, fmt.Errorf("block: opening %s: %w", e.Name(), err)
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].meta.MinTime != out[j].meta.MinTime {
			return out[i].meta.MinTime < out[j].meta.MinTime
		}
		return out[i].meta.ULID.Compare(out[j].meta.ULID) < 0
	})
	return out, nil
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
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("block: deleting %s: %w", dir, err)
	}
	return syncDir(filepath.Dir(dir))
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
		if err := os.RemoveAll(dir); err != nil {
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
