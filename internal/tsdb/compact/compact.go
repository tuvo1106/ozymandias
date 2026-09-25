package compact

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/block"
)

// DefaultMinBlocks is how many adjacent same-level blocks make a compaction
// worth doing. Three is Prometheus's number and the reasoning is the same:
// merging two blocks halves the file count for one full rewrite of the data,
// which is a poor trade, while waiting for more means a query in the meantime
// touches more files than it should.
const DefaultMinBlocks = 3

// DefaultMaxBlockRange caps how much time one block may cover. Past some size
// a block stops helping: a query for five minutes still has to open its index,
// and retention can only drop data a whole block at a time, so a very wide
// block pins old data long past its window.
const DefaultMaxBlockRange = 54 * time.Hour

// Options tune the planner.
type Options struct {
	// MinBlocks is how many adjacent blocks of one level trigger a merge.
	MinBlocks int
	// MaxBlockRange is the widest time span a merged block may cover.
	MaxBlockRange time.Duration
}

func (o *Options) setDefaults() {
	if o.MinBlocks <= 0 {
		o.MinBlocks = DefaultMinBlocks
	}
	if o.MaxBlockRange <= 0 {
		o.MaxBlockRange = DefaultMaxBlockRange
	}
}

// Plan is one compaction: the blocks to merge and the level the result gets.
type Plan struct {
	Sources []*block.Block
	Level   int
}

// Span is the time the plan's output will cover.
func (p *Plan) Span() (minT, maxT int64) {
	minT, maxT = p.Sources[0].Meta().MinTime, p.Sources[0].Meta().MaxTime
	for _, b := range p.Sources[1:] {
		m := b.Meta()
		minT = min(minT, m.MinTime)
		maxT = max(maxT, m.MaxTime)
	}
	return minT, maxT
}

// Next returns the compaction to run now, or nil if there is nothing worth
// doing.
//
// Blocks are grouped by level and merged within a level, so the tree stays
// balanced: three level-0 blocks become one level-1, three level-1 become one
// level-2, and the total number of files grows with the logarithm of the data
// rather than linearly. Merging across levels instead would keep rewriting the
// same large block every time a small one arrived, which is the write
// amplification an LSM tree is built to avoid.
//
// Only the oldest eligible run is returned, and only one at a time: compaction
// competes with queries for disk, and doing it in small steps keeps that
// competition bounded.
func Next(blocks []*block.Block, opts Options) *Plan {
	opts.setDefaults()
	maxRange := opts.MaxBlockRange.Milliseconds()

	// Oldest first, so the run found is the oldest one.
	sorted := append([]*block.Block(nil), blocks...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i].Meta(), sorted[j].Meta()
		if a.MinTime != b.MinTime {
			return a.MinTime < b.MinTime
		}
		return a.ULID.Compare(b.ULID) < 0
	})

	for start := 0; start < len(sorted); start++ {
		m := sorted[start].Meta()
		// A rollup block sits beside its source at the same time range, so it
		// must only ever merge with blocks of its own resolution.
		level, resolution := m.Compaction.Level, m.ResolutionS
		run := []*block.Block{sorted[start]}
		minT, maxT := m.MinTime, m.MaxTime

		for i := start + 1; i < len(sorted); i++ {
			next := sorted[i].Meta()
			if next.Compaction.Level != level || next.ResolutionS != resolution {
				// Skip it, do not stop. The list is ordered by MinTime alone,
				// and a rollup block sits beside its source at the same time
				// range, so once rollups exist the order interleaves:
				// raw(t0), roll(t0), raw(t1), roll(t1), ... Breaking here ends
				// every run at length one, no run ever reaches MinBlocks, and
				// compaction stops permanently for *both* resolutions while
				// the file count keeps growing. Today levels happen to be
				// segregated in time, which is the only reason breaking works.
				continue
			}
			if max(maxT, next.MaxTime)-minT > maxRange {
				break // merging this one would make the result too wide
			}
			run = append(run, sorted[i])
			maxT = max(maxT, next.MaxTime)
			if len(run) >= opts.MinBlocks {
				return &Plan{Sources: run, Level: level + 1}
			}
		}
	}
	return nil
}

// Run executes a plan: it writes the merged block and returns its meta. The
// sources are left alone; [DeleteSources] removes them, and *when* is the
// caller's decision, not this package's.
//
// That split is the same argument every durable operation in this package
// makes, carried one step further. The new block is complete and renamed into
// place before a single source is touched, so a crash in between leaves the
// data twice over — the merged block and its sources, briefly overlapping —
// and never leaves a gap. But "complete on disk" is not "in service": the
// caller still has to open the merged block and swap it into the set queries
// read from, and if it deleted the sources here, a failure in either of those
// steps would leave blocks that are gone from disk and still being served,
// and a plan that repeats identically on the next tick, writing another full
// copy of the merged block every time.
//
// Resolving the brief duplication is the *database's* job at startup, not this
// package's, and not [block.CleanCondemned]'s: a tombstone is written inside
// [DeleteSources], so a crash before the first one leaves no tombstone to
// find. What it leaves is a merged block whose meta names its sources, and the
// sources still on disk beside it. See dropSuperseded in the db package.
//
// The caller reopens the directory rather than being handed an open block:
// compaction should not decide when a reader starts using its output.
func Run(parent string, p *Plan, now time.Time) (block.Meta, error) {
	if p == nil || len(p.Sources) < 2 {
		return block.Meta{}, errors.New("compact: a plan needs at least two blocks")
	}
	sources := make([]string, len(p.Sources))
	for i, b := range p.Sources {
		sources[i] = b.Meta().ULID.String()
	}
	return merge(parent, p, sources, now)
}

// DeleteSources removes a plan's inputs. Call it once the merged block is
// open and serving; until then the sources are the only copy anyone can read.
//
// It attempts every source and joins the failures, because a directory that
// resists deletion is disk to reclaim later, not a reason to leave the rest
// behind. Each is tombstoned first, so [block.CleanCondemned] finishes at
// startup whatever this could not.
func DeleteSources(p *Plan) error {
	var errs []error
	for _, b := range p.Sources {
		if err := block.Delete(b.Dir()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func merge(parent string, p *Plan, sources []string, now time.Time) (block.Meta, error) {
	w, err := block.NewWriter(parent, block.WriterOptions{
		Now:         now,
		Level:       p.Level,
		Sources:     sources,
		ResolutionS: p.Sources[0].Meta().ResolutionS,
	})
	if err != nil {
		return block.Meta{}, err
	}
	for _, ref := range mergedSeries(p.Sources) {
		samples, err := gather(p.Sources, ref.Key())
		if err != nil {
			return block.Meta{}, errors.Join(err, w.Abort())
		}
		if len(samples) == 0 {
			continue
		}
		if err := w.AddSeries(ref, samples); err != nil {
			return block.Meta{}, errors.Join(err, w.Abort())
		}
	}
	m, err := w.Close()
	if err != nil {
		return block.Meta{}, fmt.Errorf("compact: writing the merged block: %w", err)
	}
	return m, nil
}

// mergedSeries is the union of the sources' series, in key order. Each block's
// own list is already sorted, so this is a k-way merge rather than a sort, and
// a series present in several sources is yielded once.
func mergedSeries(sources []*block.Block) []tsdb.SeriesRef {
	lists := make([][]tsdb.SeriesRef, len(sources))
	total := 0
	for i, b := range sources {
		lists[i] = b.Series()
		total += len(lists[i])
	}
	at := make([]int, len(lists))
	out := make([]tsdb.SeriesRef, 0, total)
	for {
		var smallest tsdb.SeriesRef
		key, found := "", false
		for i, l := range lists {
			if at[i] >= len(l) {
				continue
			}
			if k := l[at[i]].Key(); !found || k < key {
				smallest, key, found = l[at[i]], k, true
			}
		}
		if !found {
			return out
		}
		for i, l := range lists {
			if at[i] < len(l) && l[at[i]].Key() == key {
				at[i]++
			}
		}
		out = append(out, smallest)
	}
}

// gather concatenates one series' samples across the sources, in time order.
//
// Sources are adjacent and disjoint in time, so this is a concatenation rather
// than a merge — but the duplicate guard stays, because a crash between
// writing a merged block and deleting its sources can leave two blocks
// covering the same instant, and the next compaction would otherwise encode
// the same timestamp twice into one chunk. [chunkenc] would reject it and the
// whole compaction would fail.
func gather(sources []*block.Block, key string) ([]tsdb.Sample, error) {
	var out []tsdb.Sample
	for _, b := range sources {
		samples, err := b.SamplesFor(key)
		if err != nil {
			return nil, fmt.Errorf("compact: reading %s from %s: %w", key, b.Meta().ULID, err)
		}
		for _, s := range samples {
			if n := len(out); n > 0 && s.T <= out[n-1].T {
				continue
			}
			out = append(out, s)
		}
	}
	return out, nil
}
