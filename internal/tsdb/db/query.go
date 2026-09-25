package db

import (
	"context"
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
// The sources are disjoint in time by construction (a block covers what the
// head has already given up), so merging is concatenation in source order. The
// duplicate check below is for the one moment that is not true: a block has
// just been written but the head has not yet been truncated, so the same
// samples are briefly in both. Preferring the first source seen — the block —
// keeps a query stable across that handover instead of flickering.
func (db *DB) Select(ctx context.Context, sel tsdb.Selector, fromMs, toMs int64) (tsdb.SeriesSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Acquired under the same lock that guards the list, and held for the
	// whole query. Compaction and retention take a block out of this list and
	// only then close it, so a block still in the list here cannot be closed
	// underneath the read that follows — without this, a compaction landing
	// mid-query closes chunks.dat out from under it and the query fails with
	// "file already closed".
	db.mu.RLock()
	blocks := make([]*block.Block, 0, len(db.blocks))
	for _, b := range db.blocks {
		if b.Acquire() {
			blocks = append(blocks, b)
		}
	}
	db.mu.RUnlock()
	defer func() {
		for _, b := range blocks {
			b.Release()
		}
	}()

	merged := map[string]*tsdb.SeriesSamples{}
	add := func(src []tsdb.SeriesSamples) {
		for _, s := range src {
			key := s.Series.Key()
			into, ok := merged[key]
			if !ok {
				cp := tsdb.SeriesSamples{Series: s.Series}
				merged[key] = &cp
				into = merged[key]
			}
			for _, smp := range s.Samples {
				if n := len(into.Samples); n > 0 && smp.T <= into.Samples[n-1].T {
					continue // already have this instant from an earlier source
				}
				into.Samples = append(into.Samples, smp)
			}
		}
	}
	// Oldest first, head last: that is time order, so each source only ever
	// appends to the end of what is already there.
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
	fromHead, err := db.head.Select(sel, fromMs, toMs)
	if err != nil {
		return nil, err
	}
	add(fromHead)

	out := make([]tsdb.SeriesSamples, 0, len(merged))
	for _, s := range merged {
		if len(s.Samples) == 0 {
			continue
		}
		out = append(out, *s)
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
	for _, l := range db.lookups() {
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
