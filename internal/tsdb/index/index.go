package index

import (
	"sort"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
)

// MetricName is the pseudo tag key under which a series' metric name is
// indexed, so "which series are named http.request.count" is the same lookup
// as "which series have env:prod" — one postings list, no special case.
const MetricName = "__name__"

// MemPostings is an in-memory inverted index: for every (key, value) pair, the
// sorted ids of the series carrying it.
//
// This is the data structure that makes a tag query cheap. Answering
// `{env:prod, service:web}` without it means examining every series; with it,
// it is an intersection of two sorted id lists, and the cost is proportional
// to the smaller one rather than to the database.
//
// It is also where cardinality hurts: every distinct value is another list,
// and a tag like `user_id` makes one list per user, each holding a single id.
// The head enforces a series limit for exactly this reason.
//
// MemPostings is not safe for concurrent use; the head serializes access.
type MemPostings struct {
	// m[key][value] = sorted series ids.
	m map[string]map[string][]uint64
	// all is every id, sorted — the universe that a negative matcher
	// subtracts from.
	all []uint64
}

// NewMemPostings returns an empty index.
func NewMemPostings() *MemPostings {
	return &MemPostings{m: map[string]map[string][]uint64{}}
}

// Add indexes a series under its metric name and every tag.
//
// Ids must arrive in increasing order, which the head guarantees by assigning
// them from a counter. That keeps every postings list sorted by construction:
// appending is O(1) and no list ever needs re-sorting.
func (p *MemPostings) Add(id uint64, s tsdb.SeriesRef) {
	p.add(MetricName, s.Metric, id)
	for _, t := range s.Tags {
		p.add(t.Key, t.Value, id)
	}
	p.all = appendSorted(p.all, id)
}

func (p *MemPostings) add(key, value string, id uint64) {
	vals, ok := p.m[key]
	if !ok {
		vals = map[string][]uint64{}
		p.m[key] = vals
	}
	vals[value] = appendSorted(vals[value], id)
}

// appendSorted keeps a list sorted when ids arrive in order, and falls back to
// an insert when they do not (replay can re-add an id).
//
// A postings list handed to a reader is immutable from that moment: readers
// take a slice header under the index's read lock and then walk it with the
// lock released, so anything that rewrites elements in place corrupts a query
// that is already running. Appending past the end is the one exception and the
// reason this stays cheap — a reader's slice header stops at the old length,
// so the new element is in memory no reader can see. Inserting in the middle
// has no such excuse and builds a new list.
func appendSorted(list []uint64, id uint64) []uint64 {
	if n := len(list); n == 0 || list[n-1] < id {
		return append(list, id)
	}
	i := sort.Search(len(list), func(i int) bool { return list[i] >= id })
	if i < len(list) && list[i] == id {
		return list // already present
	}
	out := make([]uint64, 0, len(list)+1)
	out = append(out, list[:i]...)
	out = append(out, id)
	return append(out, list[i:]...)
}

// Delete removes a series from every list it appears in. Used by head GC
// after a block cut, when a series has no chunks left.
func (p *MemPostings) Delete(id uint64, s tsdb.SeriesRef) {
	p.remove(MetricName, s.Metric, id)
	for _, t := range s.Tags {
		p.remove(t.Key, t.Value, id)
	}
	p.all = removeID(p.all, id)
}

func (p *MemPostings) remove(key, value string, id uint64) {
	vals, ok := p.m[key]
	if !ok {
		return
	}
	list := removeID(vals[value], id)
	if len(list) == 0 {
		// Drop empty containers, or a high-cardinality tag would leave its
		// map entries behind forever after the series expired.
		delete(vals, value)
		if len(vals) == 0 {
			delete(p.m, key)
		}
		return
	}
	vals[value] = list
}

// removeID returns list without id, as a new slice.
//
// Shifting the tail down in place, which is the obvious way to write this, is
// a write to elements a reader may be walking right now: head GC runs after
// every block cut while queries are being served. It is also the reason this
// allocates once per removed id — one pass per series, the same shape the
// in-place version had, and a cost worth paying to make a postings list
// immutable once it has been handed out.
func removeID(list []uint64, id uint64) []uint64 {
	i := sort.Search(len(list), func(i int) bool { return list[i] >= id })
	if i >= len(list) || list[i] != id {
		return list
	}
	out := make([]uint64, 0, len(list)-1)
	out = append(out, list[:i]...)
	return append(out, list[i+1:]...)
}

// Size reports how many series and how many distinct (key, value) pairs are
// indexed — the two numbers that describe a cardinality problem.
func (p *MemPostings) Size() (series, pairs int) {
	for _, vals := range p.m {
		pairs += len(vals)
	}
	return len(p.all), pairs
}

// TagKeys implements [Lookup].
func (p *MemPostings) TagKeys() []string {
	out := make([]string, 0, len(p.m))
	for key := range p.m {
		out = append(out, key)
	}
	return out
}

// Keys returns the indexed tag keys, sorted, excluding the metric-name
// pseudo key. Optionally restricted to series of one metric.
func (p *MemPostings) Keys(metric string) []string { return Keys(p, metric) }

// Values returns the values seen for a tag key, sorted, optionally restricted
// to series of one metric.
func (p *MemPostings) Values(metric, key string) []string { return Values(p, metric, key) }

// Metrics returns the indexed metric names with the given prefix, sorted.
func (p *MemPostings) Metrics(prefix string) []string { return Metrics(p, prefix) }

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// Postings implements [Lookup].
func (p *MemPostings) Postings(key, value string) []uint64 { return p.m[key][value] }

// TagValues implements [Lookup].
func (p *MemPostings) TagValues(key string) []string {
	vals, ok := p.m[key]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(vals))
	for v := range vals {
		out = append(out, v)
	}
	return out
}

// AllSeries implements [Lookup].
func (p *MemPostings) AllSeries() []uint64 { return p.all }

// Select returns the sorted ids of series matching sel. The set logic lives in
// [Select] so that this index and a block's on-disk index cannot drift apart.
func (p *MemPostings) Select(sel tsdb.Selector) []uint64 { return Select(p, sel) }

// intersect returns the ids present in both sorted lists.
func intersect(a, b []uint64) []uint64 {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	out := make([]uint64, 0, min(len(a), len(b)))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case a[i] < b[j]:
			// Gallop: skip ahead rather than stepping, so intersecting a tiny
			// list with a huge one costs about log(huge), not huge.
			i += gallop(a[i:], b[j])
		default:
			j += gallop(b[j:], a[i])
		}
	}
	return out
}

// gallop returns how far into list to advance to reach the first id >= target.
func gallop(list []uint64, target uint64) int {
	step := 1
	i := 0
	for i+step < len(list) && list[i+step] < target {
		i += step
		step *= 2
	}
	hi := min(i+step+1, len(list))
	return i + sort.Search(hi-i, func(k int) bool { return list[i+k] >= target })
}

// subtract returns the ids in a that are not in b.
func subtract(a, b []uint64) []uint64 {
	if len(b) == 0 {
		return a
	}
	out := make([]uint64, 0, len(a))
	j := 0
	for _, id := range a {
		for j < len(b) && b[j] < id {
			j++
		}
		if j < len(b) && b[j] == id {
			continue
		}
		out = append(out, id)
	}
	return out
}

// merge unions sorted lists, de-duplicating.
func merge(lists [][]uint64) []uint64 {
	switch len(lists) {
	case 0:
		return nil
	case 1:
		return lists[0]
	}
	var total int
	for _, l := range lists {
		total += len(l)
	}
	out := make([]uint64, 0, total)
	idx := make([]int, len(lists))
	for {
		var best uint64
		found := false
		for i, l := range lists {
			if idx[i] < len(l) && (!found || l[idx[i]] < best) {
				best, found = l[idx[i]], true
			}
		}
		if !found {
			return out
		}
		for i, l := range lists {
			if idx[i] < len(l) && l[idx[i]] == best {
				idx[i]++
			}
		}
		out = append(out, best)
	}
}

func intersects(a, b []uint64) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			return true
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return false
}
