package tracestore

import (
	"bytes"
	"container/heap"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/cockroachdb/pebble"
)

// Search limits.
const (
	DefaultLimit = 50
	MaxLimit     = 500
	// DefaultMaxExamined bounds the index entries one Search reads. Filters that
	// the index cannot answer (name, duration, status) are applied while
	// scanning, so a selective filter over a long window could otherwise read it
	// all; at the bound the search stops and returns a cursor to resume from.
	DefaultMaxExamined = 100_000
)

// Filter selects entry spans. Zero fields do not filter.
type Filter struct {
	Env, Service string
	// Resource matches ignoring case, answered from the resource index.
	Resource string
	// Name is an exact match on the span name, applied while scanning.
	Name string
	// ErrorsOnly keeps entry spans that failed or belong to a trace with a failed span.
	ErrorsOnly bool
	// MinDurationUs and MaxDurationUs bound the entry span's duration; zero means unbounded.
	MinDurationUs, MaxDurationUs int64
	StatusCode                   int
	// MaxExamined overrides DefaultMaxExamined.
	MaxExamined int
}

// TraceSummary is one entry span as a search result.
type TraceSummary struct {
	TraceID, SpanID, Env, Service string
	StartUs                       int64
	Summary
}

// SearchResult is a page of results, newest first.
type SearchResult struct {
	Traces []TraceSummary
	// Next resumes the search where this page stopped; empty when the window is exhausted.
	Next string
	// Examined is how many index entries were read, so a caller can see a search that was expensive.
	Examined int
	lastKey  []byte
}

type pair struct{ env, service string }

// pairs lists the (env, service) combinations a filter covers.
func (s *Store) pairs(f Filter) []pair {
	if f.Env != "" && f.Service != "" {
		return []pair{{f.Env, f.Service}}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []pair
	for k := range s.known {
		env, svc, err := splitPair([]byte(k), false)
		if err != nil || (f.Env != "" && env != f.Env) || (f.Service != "" && svc != f.Service) {
			continue
		}
		out = append(out, pair{env, svc})
	}
	slices.SortFunc(out, func(a, b pair) int {
		if c := cmpStr(a.env, b.env); c != 0 {
			return c
		}
		return cmpStr(a.service, b.service)
	})
	return out
}

// Search lists entry spans in [fromUs, toUs] newest first.
//
// One iterator per (env, service) walks its index in time order and a heap
// merges them, so a search across services is a merge of sorted runs rather than
// a sort of everything. The cursor is the position in that merged order (a start
// time, trace and span id), which is unique to one entry, so resuming does not
// repeat or skip a result even when many share a timestamp.
func (s *Store) Search(ctx context.Context, f Filter, fromUs, toUs int64, limit int, cursor string) (*SearchResult, error) {
	if s.closed.Load() {
		return nil, errors.New("tracestore: closed")
	}
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)
	budget := f.MaxExamined
	if budget <= 0 {
		budget = DefaultMaxExamined
	}
	if toUs <= 0 {
		toUs = 1<<62 - 1
	}
	var after []byte
	if cursor != "" {
		b, err := hex.DecodeString(cursor)
		if err != nil || len(b) != suffixLen {
			return nil, fmt.Errorf("tracestore: bad cursor %q", cursor)
		}
		after = b
	}

	kind := prefixEntry
	switch {
	case f.Resource != "":
		kind = prefixResource
	case f.ErrorsOnly:
		kind = prefixError
	}
	h := &mergeHeap{}
	defer h.closeAll()
	for _, p := range s.pairs(f) {
		prefix := pairPrefix(kind, p.env, p.service)
		if kind == prefixResource {
			prefix = resourcePrefix(p.env, p.service, resourceHash(f.Resource))
		}
		lo := append(slices.Clone(prefix), binary.BigEndian.AppendUint64(nil, invert(toUs))...)
		hi := successor(prefix)
		if fromUs > 0 {
			hi = append(slices.Clone(prefix), binary.BigEndian.AppendUint64(nil, invert(fromUs-1))...)
		}
		if after != nil {
			if c := bytes.Compare(append(slices.Clone(prefix), after...), lo); c > 0 {
				lo = append(slices.Clone(prefix), after...)
			}
		}
		it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
		if err != nil {
			return nil, err
		}
		src := &source{it: it, pair: p, prefixLen: len(prefix)}
		src.valid = it.First()
		if src.valid && after != nil && bytes.Equal(src.key()[src.prefixLen:], after) {
			src.valid = it.Next() // the cursor entry itself was the last one returned
		}
		if src.valid {
			*h = append(*h, src)
		} else if err := it.Close(); err != nil {
			return nil, err
		}
	}
	heap.Init(h)

	res := &SearchResult{}
	for h.Len() > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(res.Traces) == limit || res.Examined >= budget {
			// The cursor is the last entry consumed, matched or not; resuming starts
			// strictly after it. Reporting an unread entry instead would make a
			// resumed page repeat or skip depending on which side of it a filter fell.
			res.Next = hex.EncodeToString(res.lastKey)
			return res, nil
		}
		src := (*h)[0]
		res.Examined++
		last := src.key()[src.prefixLen:]
		sum, ok, err := s.summaryFor(src, kind)
		if err != nil {
			return nil, err
		}
		if ok && matches(f, sum) {
			start, trace, span, _ := splitSuffix(src.key())
			res.Traces = append(res.Traces, TraceSummary{
				TraceID: hex.EncodeToString(trace), SpanID: hex.EncodeToString(span), Env: src.pair.env, Service: src.pair.service,
				StartUs: start, Summary: sum,
			})
		}
		res.lastKey = append(res.lastKey[:0], last...)
		src.valid = src.it.Next()
		if src.valid {
			heap.Fix(h, 0)
		} else {
			if err := src.it.Close(); err != nil {
				return nil, err
			}
			heap.Pop(h)
		}
	}
	return res, nil
}

func matches(f Filter, s Summary) bool {
	switch {
	case f.Name != "" && s.Name != f.Name:
		return false
	case f.Resource != "" && !strings.EqualFold(s.Resource, truncate(f.Resource, summaryResource)):
		return false
	case f.ErrorsOnly && s.Error == 0 && s.TraceError == 0:
		return false
	case f.MinDurationUs > 0 && s.Duration < f.MinDurationUs:
		return false
	case f.MaxDurationUs > 0 && s.Duration > f.MaxDurationUs:
		return false
	case f.StatusCode != 0 && s.StatusCode != f.StatusCode:
		return false
	}
	return true
}

// summaryFor returns the entry's Summary: from the value for the 'e' index, from a
// point read of the 'e' key for the 'r' and 'x' indexes, which hold no value.
func (s *Store) summaryFor(src *source, kind byte) (Summary, bool, error) {
	var raw []byte
	if kind == prefixEntry {
		raw = src.it.Value()
	} else {
		start, trace, span, err := splitSuffix(src.key())
		if err != nil {
			return Summary{}, false, nil
		}
		v, closer, err := s.db.Get(entryKey(src.pair.env, src.pair.service, start, trace, span))
		if errors.Is(err, pebble.ErrNotFound) {
			return Summary{}, false, nil // swept between the index read and this one
		}
		if err != nil {
			return Summary{}, false, err
		}
		raw = slices.Clone(v)
		_ = closer.Close()
	}
	var sum Summary
	if err := json.Unmarshal(raw, &sum); err != nil {
		return Summary{}, false, nil
	}
	return sum, true, nil
}

// source is one (env, service) iterator in the merge.
type source struct {
	it        *pebble.Iterator
	pair      pair
	prefixLen int
	valid     bool
}

func (s *source) key() []byte { return s.it.Key() }

type mergeHeap []*source

func (h mergeHeap) Len() int { return len(h) }
func (h mergeHeap) Less(i, j int) bool {
	return bytes.Compare(h[i].key()[h[i].prefixLen:], h[j].key()[h[j].prefixLen:]) < 0
}
func (h mergeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *mergeHeap) Push(x any)   { *h = append(*h, x.(*source)) }
func (h *mergeHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}
func (h *mergeHeap) closeAll() {
	for _, s := range *h {
		_ = s.it.Close()
	}
}

// ServiceEdges sums the service-to-service counters for the hours overlapping
// [fromUs, toUs], for one env (empty means every env).
func (s *Store) ServiceEdges(ctx context.Context, env string, fromUs, toUs int64) ([]Edge, error) {
	if s.closed.Load() {
		return nil, errors.New("tracestore: closed")
	}
	lo := binary.BigEndian.AppendUint32([]byte{prefixEdge}, hourOf(max(fromUs, 0)))
	hi := binary.BigEndian.AppendUint32([]byte{prefixEdge}, hourOf(max(toUs, 0))+1)
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
	if err != nil {
		return nil, err
	}
	sums := map[[3]string]*Edge{}
	for ok := it.First(); ok; ok = it.Next() {
		if err := ctx.Err(); err != nil {
			_ = it.Close()
			return nil, err
		}
		_, e, parent, child, err := splitEdgeKey(it.Key())
		if err != nil || (env != "" && e != env) {
			continue
		}
		v, ok := decodeEdge(it.Value())
		if !ok {
			continue
		}
		k := [3]string{e, parent, child}
		x := sums[k]
		if x == nil {
			x = &Edge{Env: e, Parent: parent, Child: child}
			sums[k] = x
		}
		x.Calls += v.calls
		x.Errors += v.errors
		x.DurationSumUs += v.durSum
	}
	if err := errors.Join(it.Error(), it.Close()); err != nil {
		return nil, err
	}
	out := make([]Edge, 0, len(sums))
	for _, e := range sums {
		out = append(out, *e)
	}
	slices.SortFunc(out, func(a, b Edge) int {
		return cmpStr(a.Env+"\x00"+a.Parent+"\x00"+a.Child, b.Env+"\x00"+b.Parent+"\x00"+b.Child)
	})
	return out, nil
}

// Services lists the (env, service) pairs that have entry spans.
func (s *Store) Services() [][2]string {
	ps := s.pairs(Filter{})
	out := make([][2]string, len(ps))
	for i, p := range ps {
		out[i] = [2]string{p.env, p.service}
	}
	return out
}
