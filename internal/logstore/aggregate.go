package logstore

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tuvo1106/ozymandias/internal/query/logql"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Bounds that keep an aggregation's memory independent of the data.
const (
	// MaxGroups bounds the distinct values a histogram's group-by tracks;
	// later values count under OtherGroup. A group-by on a high-cardinality
	// attribute would otherwise build one series per value.
	MaxGroups  = 100
	OtherGroup = "(other)"
	// MaxFacetValues bounds the distinct values counted per facet key. Past
	// it new values are not tracked, so counts for values first seen late
	// can be low: the result says which keys hit the cap.
	MaxFacetValues = 10_000
)

// scan runs fn over every log matching q in [from, to], in no particular
// order, which is all counting needs and is cheaper than the ordered merge:
// no heap, no sorting, no early stop. The scan budget applies as in Search.
func (s *Store) scan(ctx context.Context, q logql.Node, from, to, budget int64, fn func(*wire.Log)) (truncated bool, st Stats, err error) {
	if budget <= 0 {
		budget = DefaultScanBudget
	}
	if from > to {
		return false, st, nil
	}
	runs, selected := s.snapshot(logql.Split(q), from, to)
	st.Streams = selected
	files := fileCache{}
	defer files.close()
	for _, r := range runs {
		if err := ctx.Err(); err != nil {
			return false, st, err
		}
		if r.ruledOut(files) {
			st.BlocksSkipped++
			continue
		}
		if r.isBlock {
			if budget < int64(r.meta.RawLen) {
				return true, st, nil
			}
			budget -= int64(r.meta.RawLen)
			st.BlocksRead++
			st.BytesRead += int64(r.meta.RawLen)
		}
		es, err := r.load(files)
		if err != nil {
			return false, st, err
		}
		for _, e := range es {
			if e.Ts < from || e.Ts > to {
				continue
			}
			st.EntriesExamined++
			l, err := decodeLog(e.Body)
			if err != nil {
				return false, st, fmt.Errorf("logstore: decoding a stored log: %w", err)
			}
			if r.pred(l) {
				fn(l)
			}
		}
	}
	return false, st, nil
}

// AggSpec describes a histogram: logs counted per time bucket and, optionally,
// per value of one field.
type AggSpec struct {
	// Interval is the bucket width; at least a millisecond. Buckets are
	// aligned to multiples of it, so two queries over different windows draw
	// the same bars.
	Interval time.Duration
	// By is "" (one count per bucket), a reserved key (status, service, ...)
	// or "@attr.path". Logs without a value count under "".
	By string
	// ScanBudget as in SearchOpts.
	ScanBudget int64
}

// Bucket is one bar of the histogram: Ts is its start (unix ms).
type Bucket struct {
	Ts     int64
	Counts map[string]int64 // by group value; "" when AggSpec.By is empty
}

// AggResult is a sparse histogram: only buckets that hold a log appear, in
// time order. Filling the gaps is a drawing concern.
type AggResult struct {
	Buckets   []Bucket
	Truncated bool
	Stats     Stats
}

// groupValue is a log's value for an AggSpec.By.
func groupValue(l *wire.Log, by string) string {
	if by == "" {
		return ""
	}
	if path, ok := strings.CutPrefix(by, "@"); ok {
		for _, v := range logql.AttrValues(l.Attrs, path) {
			return logql.ValueText(v)
		}
		return ""
	}
	return logql.LabelValue(l, by)
}

// Aggregate counts the logs matching q in [from, to] per time bucket.
func (s *Store) Aggregate(ctx context.Context, q logql.Node, from, to int64, a AggSpec) (*AggResult, error) {
	step := a.Interval.Milliseconds()
	if step < 1 {
		return nil, fmt.Errorf("logstore: aggregate interval %v is below a millisecond", a.Interval)
	}
	if a.By != "" && !strings.HasPrefix(a.By, "@") && !isReservedKey(a.By) {
		return nil, fmt.Errorf("logstore: cannot group by %q: want one of %s or an @attribute", a.By, strings.Join(logql.ReservedKeys, ", "))
	}
	buckets := map[int64]map[string]int64{}
	seen := map[string]bool{}
	trunc, st, err := s.scan(ctx, q, from, to, a.ScanBudget, func(l *wire.Log) {
		g := groupValue(l, a.By)
		if !seen[g] {
			if len(seen) >= MaxGroups {
				g = OtherGroup
			} else {
				seen[g] = true
			}
		}
		ts := l.Ts - mod(l.Ts, step)
		b := buckets[ts]
		if b == nil {
			b = map[string]int64{}
			buckets[ts] = b
		}
		b[g]++
	})
	if err != nil {
		return nil, err
	}
	res := &AggResult{Truncated: trunc, Stats: st}
	for ts, c := range buckets {
		res.Buckets = append(res.Buckets, Bucket{Ts: ts, Counts: c})
	}
	sort.Slice(res.Buckets, func(i, j int) bool { return res.Buckets[i].Ts < res.Buckets[j].Ts })
	return res, nil
}

// mod is the non-negative remainder, so a timestamp before 1970 still rounds down.
func mod(a, b int64) int64 {
	m := a % b
	if m < 0 {
		m += b
	}
	return m
}

func isReservedKey(k string) bool {
	for _, r := range logql.ReservedKeys {
		if r == k {
			return true
		}
	}
	return false
}

// FacetCount is one value of a facet and how many logs have it.
type FacetCount struct {
	Value string
	Count int64
}

// FacetResult holds the most frequent values per key.
type FacetResult struct {
	Facets map[string][]FacetCount
	// Capped lists keys that had more than MaxFacetValues distinct values, so
	// their counts for late-appearing values may be low.
	Capped    []string
	Truncated bool
	Stats     Stats
}

// Facets counts, for each key ("status", "service", "@http.status", ...), the
// values among the logs matching q, and returns the top `limit` per key by
// count (ties by value). A log with several values for an attribute (an
// array) counts once for each distinct value.
func (s *Store) Facets(ctx context.Context, q logql.Node, from, to int64, keys []string, limit int) (*FacetResult, error) {
	if limit <= 0 {
		limit = 10
	}
	for _, k := range keys {
		if !strings.HasPrefix(k, "@") && !isReservedKey(k) {
			return nil, fmt.Errorf("logstore: cannot facet on %q: want one of %s or an @attribute", k, strings.Join(logql.ReservedKeys, ", "))
		}
	}
	counts := make(map[string]map[string]int64, len(keys))
	for _, k := range keys {
		counts[k] = map[string]int64{}
	}
	capped := map[string]bool{}
	trunc, st, err := s.scan(ctx, q, from, to, 0, func(l *wire.Log) {
		for _, k := range keys {
			for _, v := range facetValues(l, k) {
				m := counts[k]
				if _, tracked := m[v]; !tracked && len(m) >= MaxFacetValues {
					capped[k] = true
					continue
				}
				m[v]++
			}
		}
	})
	if err != nil {
		return nil, err
	}
	res := &FacetResult{Facets: make(map[string][]FacetCount, len(keys)), Truncated: trunc, Stats: st}
	for _, k := range keys {
		var fc []FacetCount
		for v, c := range counts[k] {
			fc = append(fc, FacetCount{v, c})
		}
		sort.Slice(fc, func(i, j int) bool {
			if fc[i].Count != fc[j].Count {
				return fc[i].Count > fc[j].Count
			}
			return fc[i].Value < fc[j].Value
		})
		if len(fc) > limit {
			fc = fc[:limit]
		}
		res.Facets[k] = fc
		if capped[k] {
			res.Capped = append(res.Capped, k)
		}
	}
	sort.Strings(res.Capped)
	return res, nil
}

// facetValues are the distinct values of a key in a log: one for a reserved
// key (none when it is empty), every distinct element for an attribute.
func facetValues(l *wire.Log, key string) []string {
	if path, ok := strings.CutPrefix(key, "@"); ok {
		var out []string
		dup := map[string]bool{}
		for _, v := range logql.AttrValues(l.Attrs, path) {
			t := logql.ValueText(v)
			if !dup[t] {
				dup[t] = true
				out = append(out, t)
			}
		}
		return out
	}
	if v := logql.LabelValue(l, key); v != "" {
		return []string{v}
	}
	return nil
}
