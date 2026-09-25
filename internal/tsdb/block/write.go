package block

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/chunkenc"
)

// tmpSuffix marks a block under construction. Startup deletes any directory
// carrying it, and so does a failed [Writer.Close].
const tmpSuffix = ".tmp"

// WriterOptions describe the block being produced.
type WriterOptions struct {
	// Now supplies the block's ulid timestamp. Injected rather than read from
	// the clock so tests are deterministic; zero means time.Now.
	Now time.Time
	// Level and Sources record provenance: 0 and empty for a head cut, and for
	// a compaction one more than the sources' level.
	Level   int
	Sources []string
	// ResolutionS marks a rollup block; 0 is raw.
	ResolutionS int
}

// Writer builds one block. Series are added in any order — the index sorts
// them — and each series' samples must be in ascending time order.
//
// Chunks stream to disk as they fill, so writing a block costs one chunk of
// memory per call plus the index; only the index is held whole. That is the
// asymmetry the format is built around: the index is small because of the
// symbol table, and the data is large but write-once.
type Writer struct {
	parent string
	tmpDir string
	id     string
	opts   WriterOptions

	chunks *chunkWriter
	ix     indexWriter
	stats  Stats
	minT   int64
	maxT   int64
	closed bool
}

// NewWriter creates a block under parent (the blocks directory). Nothing in
// parent changes until [Writer.Close] renames the finished block into place.
func NewWriter(parent string, opts WriterOptions) (*Writer, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	id := NewULID(opts.Now).String()
	tmpDir := filepath.Join(parent, id+tmpSuffix)
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return nil, fmt.Errorf("block: creating %s: %w", tmpDir, err)
	}
	cw, err := newChunkWriter(tmpDir)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, err
	}
	return &Writer{
		parent: parent, tmpDir: tmpDir, id: id, opts: opts, chunks: cw,
		minT: math.MaxInt64, maxT: math.MinInt64,
	}, nil
}

// AddSeries writes one series' samples, which must be sorted by time and
// non-empty.
func (w *Writer) AddSeries(ref tsdb.SeriesRef, samples []tsdb.Sample) error {
	if w.closed {
		return errors.New("block: writer is closed")
	}
	if len(samples) == 0 {
		return nil
	}
	if err := ref.Validate(); err != nil {
		return fmt.Errorf("block: %s: %w", ref.Key(), err)
	}
	bs := blockSeries{ref: ref}
	// Re-encode into full chunks rather than copying the head's. A head chunk
	// is cut early by a block boundary or by a restart, and a block is written
	// once and read many times, so packing them full here is paid for by every
	// later query.
	for start := 0; start < len(samples); start += chunkenc.MaxSamplesPerChunk {
		end := min(start+chunkenc.MaxSamplesPerChunk, len(samples))
		ref, err := w.writeChunk(samples[start:end])
		if err != nil {
			return err
		}
		bs.chunks = append(bs.chunks, ref)
	}
	w.ix.add(bs)
	w.stats.Series++
	w.stats.Samples += len(samples)
	w.stats.Chunks += len(bs.chunks)
	w.minT = min(w.minT, samples[0].T)
	w.maxT = max(w.maxT, samples[len(samples)-1].T)
	return nil
}

func (w *Writer) writeChunk(samples []tsdb.Sample) (chunkRef, error) {
	c := chunkenc.NewChunk()
	app, err := c.Appender()
	if err != nil {
		return chunkRef{}, err
	}
	for _, s := range samples {
		if err := app.Append(s.T, s.V); err != nil {
			return chunkRef{}, fmt.Errorf("block: encoding sample at %d: %w", s.T, err)
		}
	}
	off, recLen, err := w.chunks.write(EncodingGorilla, c.Bytes())
	if err != nil {
		return chunkRef{}, err
	}
	return chunkRef{
		minT: samples[0].T, maxT: samples[len(samples)-1].T,
		off: off, recLen: recLen,
	}, nil
}

// Close finalizes the block and makes it visible, returning its meta.
//
// The order is the whole durability argument: chunks and index are written and
// fsynced, then meta.json, then the temp directory is renamed. Rename is
// atomic, so a block either appears complete or does not appear at all. A
// crash before the rename leaves a `.tmp` directory, which startup deletes;
// nothing can observe a half-written block.
func (w *Writer) Close() (Meta, error) {
	if w.closed {
		return Meta{}, errors.New("block: writer is closed")
	}
	w.closed = true
	if err := w.chunks.close(); err != nil {
		return Meta{}, w.abort(err)
	}
	if w.stats.Series == 0 {
		// An empty block is not worth a directory, and a meta with
		// minT > maxT would confuse every consumer.
		return Meta{}, w.abort(ErrEmpty)
	}
	if err := w.ix.writeTo(w.tmpDir); err != nil {
		return Meta{}, w.abort(err)
	}
	m := Meta{
		Version: MetaVersion,
		MinTime: w.minT, MaxTime: w.maxT,
		Stats:       w.stats,
		Compaction:  Compaction{Level: w.opts.Level},
		ResolutionS: w.opts.ResolutionS,
	}
	if err := m.ULID.UnmarshalText([]byte(w.id)); err != nil {
		return Meta{}, w.abort(err)
	}
	for _, src := range w.opts.Sources {
		var u ulid.ULID
		if err := u.UnmarshalText([]byte(src)); err != nil {
			return Meta{}, w.abort(fmt.Errorf("block: source id %q: %w", src, err))
		}
		m.Compaction.Sources = append(m.Compaction.Sources, u)
	}
	if err := writeMeta(w.tmpDir, m); err != nil {
		return Meta{}, w.abort(err)
	}
	if err := syncDir(w.tmpDir); err != nil {
		return Meta{}, w.abort(err)
	}
	final := filepath.Join(w.parent, w.id)
	if err := os.Rename(w.tmpDir, final); err != nil {
		return Meta{}, w.abort(fmt.Errorf("block: publishing %s: %w", final, err))
	}
	// The rename itself needs the parent synced, or a crash can lose the
	// directory entry for a block whose files are all safely on disk.
	if err := syncDir(w.parent); err != nil {
		return Meta{}, err
	}
	return m, nil
}

// ErrEmpty is returned when a block would contain no series. Callers treat it
// as "nothing to do", not as a failure: a head with no data is normal.
var ErrEmpty = errors.New("block: no series to write")

func (w *Writer) abort(cause error) error {
	if err := os.RemoveAll(w.tmpDir); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// Abort discards a block under construction.
func (w *Writer) Abort() error {
	if w.closed {
		return nil
	}
	w.closed = true
	_ = w.chunks.close()
	return os.RemoveAll(w.tmpDir)
}

// Write is [Writer] for callers that already hold every series.
func Write(parent string, series []tsdb.SeriesSamples, opts WriterOptions) (Meta, error) {
	w, err := NewWriter(parent, opts)
	if err != nil {
		return Meta{}, err
	}
	for _, s := range series {
		if err := w.AddSeries(s.Series, s.Samples); err != nil {
			return Meta{}, errors.Join(err, w.Abort())
		}
	}
	return w.Close()
}

// CleanTmp removes the leftovers of interrupted writes: any directory whose
// name carries the .tmp suffix, and any directory without a meta.json. Both
// mean a process died mid-block. It runs at startup, before blocks are opened.
//
// The suffix has to be enough on its own. meta.json is written last *inside*
// the temporary directory, so a crash in the window between that write and the
// rename leaves a .tmp directory that is complete in every respect except the
// one that counts — and judging it by its contents would keep it forever,
// invisible to [OpenAll] and to queries but charged against the size cap.
// The rename is the commit, not the meta.json, and the data is still in the
// write-ahead log either way: the log is only truncated once the block is in
// place.
func CleanTmp(parent string) (removed []string, err error) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(parent, e.Name())
		if filepath.Ext(e.Name()) != tmpSuffix {
			if _, statErr := os.Stat(filepath.Join(dir, MetaFilename)); statErr == nil {
				continue
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			return removed, fmt.Errorf("block: removing incomplete %s: %w", dir, err)
		}
		removed = append(removed, e.Name())
	}
	if len(removed) > 0 {
		return removed, syncDir(parent)
	}
	return removed, nil
}
