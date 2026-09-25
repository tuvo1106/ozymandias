package tsdb

import (
	"cmp"
	"context"
	"errors"
	"path"
	"slices"
	"strings"
)

// Tag is one key/value pair of a series' identity. A bare tag ("canary")
// has an empty Value.
type Tag struct{ Key, Value string }

// String renders the tag the way the wire does: "key:value", or "key".
func (t Tag) String() string {
	if t.Value == "" {
		return t.Key
	}
	return t.Key + ":" + t.Value
}

// ParseTag is String's inverse.
func ParseTag(s string) Tag {
	if i := strings.IndexByte(s, ':'); i >= 0 {
		return Tag{Key: s[:i], Value: s[i+1:]}
	}
	return Tag{Key: s}
}

// Sample is one value at one instant. T is unix milliseconds: seconds are
// enough for 10s buckets, but traces (M5) and logs (M4) carry sub-second
// times, and one unit across stores keeps the query layer simple.
type Sample struct {
	T int64
	V float64
}

// SeriesRef is a series' identity: metric name plus a sorted, de-duplicated
// tag set. Two refs with the same Key are the same series.
type SeriesRef struct {
	Metric string
	Tags   []Tag
}

// NewSeriesRef builds a ref from wire-style "k:v" tags, sorting and
// de-duplicating them so the result is canonical.
func NewSeriesRef(metric string, tags []string) SeriesRef {
	ts := make([]Tag, len(tags))
	for i, t := range tags {
		ts[i] = ParseTag(t)
	}
	SortTags(ts)
	return SeriesRef{Metric: metric, Tags: slices.Compact(ts)}
}

// SortTags orders tags by key, then value — the canonical order.
func SortTags(tags []Tag) {
	slices.SortFunc(tags, func(a, b Tag) int {
		if c := cmp.Compare(a.Key, b.Key); c != 0 {
			return c
		}
		return cmp.Compare(a.Value, b.Value)
	})
}

// Key is the series' identity as one string: "metric|k1:v1,k2". It is what
// stores index by and what SeriesSet orders by.
func (s SeriesRef) Key() string {
	var b strings.Builder
	b.WriteString(s.Metric)
	b.WriteByte('|')
	for i, t := range s.Tags {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(t.String())
	}
	return b.String()
}

// Get returns the value of tag key and whether the series has it.
func (s SeriesRef) Get(key string) (string, bool) {
	for _, t := range s.Tags {
		if t.Key == key {
			return t.Value, true
		}
	}
	return "", false
}

// Validate checks that the ref is canonical: a metric name, and tags sorted
// and unique. Stores reject non-canonical refs rather than fixing them, so
// a caller bug can't create two spellings of one series.
func (s SeriesRef) Validate() error {
	if s.Metric == "" {
		return errors.New("empty metric name")
	}
	for i := 1; i < len(s.Tags); i++ {
		a, b := s.Tags[i-1], s.Tags[i]
		if a.Key > b.Key || (a.Key == b.Key && a.Value >= b.Value) {
			return errors.New("tags are not sorted and unique")
		}
	}
	return nil
}

// SeriesSamples is a batch entry for Append.
type SeriesSamples struct {
	Series  SeriesRef
	Samples []Sample
}

// AppendResult reports what Append stored. Per-series problems are reported
// here, not as Append's error, so one bad series doesn't fail a batch.
type AppendResult struct {
	Series, Samples int // stored
	Rejected        []Rejected
}

// Rejected names a series Append refused, and why.
type Rejected struct {
	Series SeriesRef
	Reason string
}

// MatchType is how a Matcher compares a tag.
type MatchType int

// Matcher types.
const (
	// Equal: the series has the tag key:value.
	Equal MatchType = iota
	// NotEqual: the series does not have key:value (series without key match).
	NotEqual
	// Wildcard: the series has a key whose value matches the glob ('*' only).
	Wildcard
	// NotWildcard: the series has no value for key matching the glob.
	NotWildcard
)

// Matcher selects series by one tag.
type Matcher struct {
	Key, Value string
	Type       MatchType
}

// Matches reports whether a series with tags satisfies m. Stores may use
// indexes instead, but must agree with this — it is the reference semantics
// the differential tests (M2) compare against.
func (m Matcher) Matches(tags []Tag) bool {
	has := false
	for _, t := range tags {
		if t.Key != m.Key {
			continue
		}
		switch m.Type {
		case Equal, NotEqual:
			has = has || t.Value == m.Value
		case Wildcard, NotWildcard:
			has = has || GlobMatch(m.Value, t.Value)
		}
	}
	if m.Type == NotEqual || m.Type == NotWildcard {
		return !has
	}
	return has
}

// GlobMatch matches s against pattern, where '*' matches any run of
// characters and everything else is literal.
func GlobMatch(pattern, s string) bool {
	// path.Match treats '?', '[' and '\' specially; escape them so only '*'
	// is a wildcard, as in tag search generally.
	esc := strings.NewReplacer(`\`, `\\`, `?`, `\?`, `[`, `\[`, `/`, "\x00").Replace(pattern)
	ok, _ := path.Match(esc, strings.ReplaceAll(s, "/", "\x00"))
	return ok
}

// Selector picks the series of one metric that satisfy every matcher.
//
// An empty Metric is deliberately unspecified, and the two stores read it
// differently: [Selector.Matches] and the naive store take it literally, as a
// metric whose name is "", so nothing matches; the TSDB's index reads it as
// "no metric restriction", which is what a block cut needs in order to sweep
// the whole head. Nothing can reach either behaviour from outside — the query
// API rejects a request without a metric before a store sees it — so rather
// than bend one of them to the other and lose the sweep, the case is named
// here as the gap it is. The differential test excludes it for the same
// reason, and the M2 notes record the decision.
type Selector struct {
	Metric   string
	Matchers []Matcher
}

// Matches reports whether ref is selected. See [Selector] on the empty Metric.
func (sel Selector) Matches(ref SeriesRef) bool {
	if ref.Metric != sel.Metric {
		return false
	}
	for _, m := range sel.Matchers {
		if !m.Matches(ref.Tags) {
			return false
		}
	}
	return true
}

// SeriesIterator walks one series' samples in [from, to], ascending by time.
type SeriesIterator interface {
	Next() bool
	At() Sample
	Err() error
}

// SeriesSet walks selected series in ascending Key order — deterministic, so
// query results and differential tests are stable.
type SeriesSet interface {
	Next() bool
	Series() SeriesRef
	Iterator() SeriesIterator
	Err() error
	Close() error
}

// StoreStats is a store's size, for self-metrics and the UI.
type StoreStats struct {
	Series  int64
	Samples int64
}

// MetricStore is the storage contract every metric store implements. The
// naive SQLite store (M1) and the real TSDB (M2) sit behind it; everything
// above — intake, query, monitors — only knows this interface.
//
// Semantics every implementation must share (M2's differential tests hold
// the TSDB to the naive store on exactly these):
//   - Samples are append-only (ADR-0011). A sample newer than the series'
//     newest is stored; one with the same T and the same V is a no-op, so an
//     at-least-once retry is safe; anything else at or before the newest T is
//     rejected and reported in AppendResult.Rejected.
//   - Select returns series in Key order, samples in T order, both bounds
//     inclusive, and only series with at least one sample in range.
//   - MetricNames, TagKeys and TagValues are sorted ascending.
type MetricStore interface {
	Append(ctx context.Context, batch []SeriesSamples) (AppendResult, error)
	Select(ctx context.Context, sel Selector, fromMs, toMs int64) (SeriesSet, error)
	MetricNames(ctx context.Context, prefix string, limit int) ([]string, error)
	TagKeys(ctx context.Context, metric string) ([]string, error)
	TagValues(ctx context.Context, metric, key string, limit int) ([]string, error)
	Stats() StoreStats
	Close() error
}

// SliceSet is a SeriesSet over in-memory data, for stores that materialize
// results and for tests.
type SliceSet struct {
	items []SeriesSamples
	i     int
}

// NewSliceSet returns a set over items, which must already be in Key order.
func NewSliceSet(items []SeriesSamples) *SliceSet { return &SliceSet{items: items, i: -1} }

// Next advances to the next series.
func (s *SliceSet) Next() bool { s.i++; return s.i < len(s.items) }

// Series returns the current series' identity.
func (s *SliceSet) Series() SeriesRef { return s.items[s.i].Series }

// Iterator returns the current series' samples.
func (s *SliceSet) Iterator() SeriesIterator { return &sliceIter{samples: s.items[s.i].Samples, i: -1} }

// Err is always nil.
func (s *SliceSet) Err() error { return nil }

// Close does nothing.
func (s *SliceSet) Close() error { return nil }

type sliceIter struct {
	samples []Sample
	i       int
}

func (it *sliceIter) Next() bool { it.i++; return it.i < len(it.samples) }
func (it *sliceIter) At() Sample { return it.samples[it.i] }
func (it *sliceIter) Err() error { return nil }

// BlockOf returns the index of the block range t falls in, for a range of
// rangeMs milliseconds.
//
// It exists so that the two places that decide a block boundary — where the
// head cuts a chunk, and where the database cuts a block — cannot disagree.
// They did: one used Go's division, which truncates toward zero, and the other
// rounded toward negative infinity. For any timestamp before the unix epoch
// the two land on different sides of zero, so a chunk could straddle the
// boundary the cut used, and its older half would end up both in the new block
// and still in the head. Pre-epoch timestamps are rare and entirely legal, and
// a query that returns a sample twice is not a rounding detail.
func BlockOf(t, rangeMs int64) int64 {
	q := t / rangeMs
	if (t%rangeMs != 0) && ((t < 0) != (rangeMs < 0)) {
		q--
	}
	return q
}
