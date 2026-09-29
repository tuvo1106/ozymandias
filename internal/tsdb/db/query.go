package db

import (
	"context"
	"slices"
	"sort"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/block"
	"github.com/tuvo1106/ozymandias/internal/tsdb/index"
)

// Select implements [tsdb.MetricStore].
//
// A query is answered from however many sources overlap its window: the blocks
// whose time range intersects it, plus the head. Each source is asked the same
// question and the answers are merged by series identity — which is why series
// identity is a string key rather than a per-source id.
//
// The sources are *usually* disjoint in time (a block covers what the head has
// already given up), so merging is usually concatenation in source order. Two
// things break that, and the merge below handles both rather than assuming
// neither:
//
//   - A block has just been written and the head has not yet been truncated,
//     so the same samples are briefly in both.
//   - Two blocks genuinely overlap. A crash between writing a merged block and
//     deleting its sources leaves exactly that until the next startup, and
//     rollup blocks will sit beside their sources at the same range by design.
//
// Overlap is not the same as duplication: an overlapping source can hold
// samples at instants the first source has nothing for, and those have to
// survive. So samples are collected from every source and, if any arrived out
// of order, sorted and deduplicated by timestamp. The sort is stable and the
// sources are added oldest first, so the first source to offer an instant is
// the one whose value is kept — which keeps a query stable across the block
// handover instead of flickering.
func (db *DB) Select(ctx context.Context, sel tsdb.Selector, fromMs, toMs int64) (tsdb.SeriesSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The head is read first, and this is a correctness requirement, not a
	// preference.
	//
	// A block cut publishes the block to db.blocks and only then truncates the
	// head, so that the samples it moves are in both places for a moment
	// rather than in neither. That ordering protects a reader that looks at
	// the head before it looks at the blocks: whichever side of the cut it
	// lands on, it sees the samples at least once, and the merge below drops
	// the duplicate. Reading the blocks first inverts it — the snapshot is
	// taken before the new block is published, the head is read after it has
	// been truncated, and the range the cut just moved is in neither half of
	// the answer. The query then succeeds and returns a hole, which is the
	// worst way for a database to be wrong.
	//
	// Reading it early cannot lose anything in the other direction, because
	// samples only ever travel from the head to a block: an earlier head read
	// and a later block snapshot can each only hold *more* than they would
	// have, never less.
	fromHead, err := db.head.Select(sel, fromMs, toMs)
	if err != nil {
		return nil, err
	}

	blocks, release := db.acquireBlocks()
	defer release()

	type merging struct {
		tsdb.SeriesSamples
		// unordered records that some sample did not land after the one
		// before it, which is the only case that needs the sort below. The
		// common query touches one source, or several that really are
		// disjoint, and pays nothing for this.
		unordered bool
	}
	merged := map[string]*merging{}
	add := func(src []tsdb.SeriesSamples) {
		for _, s := range src {
			key := s.Series.Key()
			into, ok := merged[key]
			if !ok {
				into = &merging{SeriesSamples: tsdb.SeriesSamples{Series: s.Series}}
				merged[key] = into
			}
			for _, smp := range s.Samples {
				if n := len(into.Samples); n > 0 && smp.T <= into.Samples[n-1].T {
					into.unordered = true
				}
				into.Samples = append(into.Samples, smp)
			}
		}
	}
	// Merged oldest first, head last — that is time order, so each source only
	// ever appends to the end of what is already there, and the stable sort
	// below keeps the older source's value for an instant two of them claim.
	// This is the order they are *combined* in, which is independent of the
	// order they were read in above.
	for _, b := range blocks {
		if !b.Overlaps(fromMs, toMs) {
			continue
		}
		got, err := b.Select(sel, fromMs, toMs)
		if err != nil {
			return nil, err
		}
		add(got)
	}
	add(fromHead)

	out := make([]tsdb.SeriesSamples, 0, len(merged))
	for _, s := range merged {
		if len(s.Samples) == 0 {
			continue
		}
		if s.unordered {
			// Stable, so equal timestamps stay in source order and Compact
			// keeps the oldest source's value for an instant two sources
			// both claim.
			sort.SliceStable(s.Samples, func(i, j int) bool { return s.Samples[i].T < s.Samples[j].T })
			s.Samples = slices.CompactFunc(s.Samples, func(a, b tsdb.Sample) bool { return a.T == b.T })
		}
		out = append(out, s.SeriesSamples)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Series.Key() < out[j].Series.Key() })
	return tsdb.NewSliceSet(out), nil
}

// MetricNames implements [tsdb.MetricStore].
func (db *DB) MetricNames(ctx context.Context, prefix string, limit int) ([]string, error) {
	return db.metadata(ctx, limit, func(l index.Lookup) []string {
		return index.Metrics(l, prefix)
	})
}

// TagKeys implements [tsdb.MetricStore].
func (db *DB) TagKeys(ctx context.Context, metric string) ([]string, error) {
	return db.metadata(ctx, 0, func(l index.Lookup) []string {
		return index.Keys(l, metric)
	})
}

// TagValues implements [tsdb.MetricStore].
func (db *DB) TagValues(ctx context.Context, metric, key string, limit int) ([]string, error) {
	return db.metadata(ctx, limit, func(l index.Lookup) []string {
		return index.Values(l, metric, key)
	})
}

// metadata runs one metadata query against every index and unions the results.
//
// Every source has to be asked: a metric that stopped reporting an hour ago is
// in a block and not in the head, and a metric that started a minute ago is
// the other way round. Answering from the head alone is the bug that makes a
// dashboard's autocomplete forget everything after a restart.
func (db *DB) metadata(ctx context.Context, limit int, ask func(index.Lookup) []string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	// The head first, and its answer taken before the block list is even
	// looked at — the same ordering requirement as [DB.Select], for the same
	// reason. A block cut publishes the block and then drops the series it
	// moved out of the head's index, so a reader that snapshots the block list
	// first and asks the head last can miss a series that spent the whole
	// query in exactly one of them. A series seen twice is free here: these
	// answers go into a set.
	for _, v := range ask(db.head.Lookup()) {
		seen[v] = struct{}{}
	}
	for _, l := range db.blockLookups() {
		for _, v := range ask(l) {
			seen[v] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// acquireBlocks returns the current blocks, each acquired, and the function
// that releases them; a caller holds them for its whole read.
//
// Acquired under the same lock that guards the list. Compaction and retention
// take a block out of this list and only then close it, so a block still in
// the list here cannot be closed underneath the read that follows — without
// this, a compaction landing mid-query closes chunks.dat out from under it and
// the query fails with "file already closed".
func (db *DB) acquireBlocks() ([]*block.Block, func()) {
	db.mu.RLock()
	blocks := make([]*block.Block, 0, len(db.blocks))
	for _, b := range db.blocks {
		if b.Acquire() {
			blocks = append(blocks, b)
		}
	}
	db.mu.RUnlock()
	return blocks, func() {
		for _, b := range blocks {
			b.Release()
		}
	}
}

// SeriesCounts implements [tsdb.MetricStore].
//
// A count of distinct series, which is not the sum of the sources' postings:
// a series still being written after a block cut is in the block *and* the
// head, and one that has been compacted is briefly in a merged block beside
// its sources. Summing lengths would count it once per place — the same
// overcount [DB.Stats] makes, and here it would be the number the page exists
// to show. So each metric's series are gathered by key.
//
// The work is the index: every series of every matching metric, resolved to
// its identity once per source that holds it, and no sample read. The head
// is read for every metric first and the blocks are acquired once after it
// (see [DB.metricSeries] for why that order), so what is held at once is the
// head's keys for the matching metrics — no more than the head already holds —
// plus one metric's block keys.
func (db *DB) SeriesCounts(ctx context.Context, prefix string) ([]tsdb.MetricSeriesCount, error) {
	names, err := db.MetricNames(ctx, prefix, 0)
	if err != nil {
		return nil, err
	}
	fromHead := make([]map[string]struct{}, len(names))
	for i, name := range names {
		fromHead[i] = map[string]struct{}{}
		for _, ref := range db.head.SeriesOf(name) {
			fromHead[i][ref.Key()] = struct{}{}
		}
	}
	blocks, release := db.acquireBlocks()
	defer release()
	out := make([]tsdb.MetricSeriesCount, 0, len(names))
	for i, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		keys := fromHead[i]
		fromHead[i] = nil
		for _, b := range blocks {
			refs, err := b.SeriesOf(name)
			if err != nil {
				return nil, err
			}
			for _, ref := range refs {
				keys[ref.Key()] = struct{}{}
			}
		}
		if len(keys) > 0 {
			out = append(out, tsdb.MetricSeriesCount{Metric: name, Series: len(keys)})
		}
	}
	return out, nil
}

// TagCardinality implements [tsdb.MetricStore], over the same deduplicated
// series as [DB.SeriesCounts]. The series count is the size of the set the
// keys were counted over, so no key can be on more series than the metric.
func (db *DB) TagCardinality(ctx context.Context, metric string) (tsdb.MetricTagCardinality, error) {
	series, err := db.metricSeries(ctx, metric)
	if err != nil {
		return tsdb.MetricTagCardinality{}, err
	}
	type acc struct {
		series int
		values map[string]struct{}
	}
	byKey := map[string]*acc{}
	for _, ref := range series {
		counted := map[string]bool{} // a series may carry one key twice
		for _, t := range ref.Tags {
			a := byKey[t.Key]
			if a == nil {
				a = &acc{values: map[string]struct{}{}}
				byKey[t.Key] = a
			}
			if !counted[t.Key] {
				a.series++
				counted[t.Key] = true
			}
			if t.Value != "" {
				a.values[t.Value] = struct{}{}
			}
		}
	}
	keys := make([]tsdb.TagKeyCardinality, 0, len(byKey))
	for k, a := range byKey {
		keys = append(keys, tsdb.TagKeyCardinality{Key: k, Series: a.series, Values: len(a.values)})
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Key < keys[j].Key })
	return tsdb.MetricTagCardinality{Series: len(series), Keys: keys}, nil
}

// metricSeries returns one metric's series, by key, from the head and every
// block. The head is read before the block list is taken, for the reason
// [DB.Select] gives: a block cut publishes the block before it drops the
// series from the head, so this order sees a moving series at least once, and
// the map makes "more than once" harmless.
func (db *DB) metricSeries(ctx context.Context, metric string) (map[string]tsdb.SeriesRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := map[string]tsdb.SeriesRef{}
	for _, ref := range db.head.SeriesOf(metric) {
		out[ref.Key()] = ref
	}
	blocks, release := db.acquireBlocks()
	defer release()
	for _, b := range blocks {
		refs, err := b.SeriesOf(metric)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			out[ref.Key()] = ref
		}
	}
	return out, nil
}
