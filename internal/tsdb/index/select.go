package index

import (
	"sort"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
)

// Lookup is the read side of an inverted index: everything the set operations
// need, and nothing about how the lists are stored.
//
// It exists because there are two indexes — the head's in-memory one and the
// one inside an on-disk block — and only one definition of what a matcher
// means. Sharing [Select] between them is what guarantees that a query does
// not change its answer when its data ages from the head into a block, which
// would be a very hard bug to see.
type Lookup interface {
	// Postings returns the sorted ids carrying key:value, or nil. The returned
	// slice must not be modified by the caller.
	Postings(key, value string) []uint64
	// TagValues returns the values seen for a tag key. Only wildcard matchers
	// need it, which is why they cost more than exact ones.
	TagValues(key string) []string
	// TagKeys returns the keys in the index, including [MetricName]. Only the
	// metadata queries need it; a matcher always knows its own key.
	TagKeys() []string
	// AllSeries returns every id, sorted: the universe a negative matcher
	// subtracts from when there is no positive matcher to start from.
	AllSeries() []uint64
}

// Select returns the sorted ids of the series in l matching sel.
//
// Positive matchers are intersected smallest-list-first, because the result
// can never be larger than the smallest input and starting there does the
// least work. Negative matchers are applied afterwards as subtractions, since
// they can only shrink the set and computing them eagerly would mean building
// the complement of a possibly huge list.
func Select(l Lookup, sel tsdb.Selector) []uint64 {
	var positive [][]uint64
	// An empty metric is "no restriction" here, which is how a block cut asks
	// for every series in the head. It is *not* what [tsdb.Selector.Matches]
	// does with the same input — see that type's doc for why the two are
	// allowed to differ and why nothing can reach the difference.
	if sel.Metric != "" {
		positive = append(positive, l.Postings(MetricName, sel.Metric))
	}
	var negative []tsdb.Matcher

	for _, m := range sel.Matchers {
		switch m.Type {
		case tsdb.Equal:
			positive = append(positive, l.Postings(m.Key, m.Value))
		case tsdb.Wildcard:
			positive = append(positive, union(l, m.Key, m.Value))
		case tsdb.NotEqual, tsdb.NotWildcard:
			negative = append(negative, m)
		}
	}
	var ids []uint64
	if len(positive) == 0 {
		ids = l.AllSeries()
	} else {
		sort.Slice(positive, func(i, j int) bool { return len(positive[i]) < len(positive[j]) })
		ids = positive[0]
		for _, next := range positive[1:] {
			ids = intersect(ids, next)
			if len(ids) == 0 {
				return nil
			}
		}
	}
	for _, m := range negative {
		switch m.Type {
		case tsdb.NotEqual:
			ids = subtract(ids, l.Postings(m.Key, m.Value))
		case tsdb.NotWildcard:
			ids = subtract(ids, union(l, m.Key, m.Value))
		}
	}
	// Copy: callers must not be able to mutate the index's own slices, and
	// `ids` may still alias a postings list when no work was done.
	return append([]uint64(nil), ids...)
}

// union merges the lists of every value of key matching the glob pattern.
func union(l Lookup, key, pattern string) []uint64 {
	var lists [][]uint64
	for _, v := range l.TagValues(key) {
		if tsdb.GlobMatch(pattern, v) {
			lists = append(lists, l.Postings(key, v))
		}
	}
	return merge(lists)
}
