package tracestore

import (
	"encoding/binary"
	"io"
	"time"

	"github.com/cockroachdb/pebble"
)

// edgeVal is the counter an edge key holds: calls, errors, and the sum of
// the callee's entry-span durations in microseconds.
type edgeVal struct{ calls, errors, durSum uint64 }

func (e edgeVal) encode() []byte {
	b := binary.AppendUvarint(nil, e.calls)
	b = binary.AppendUvarint(b, e.errors)
	return binary.AppendUvarint(b, e.durSum)
}

func decodeEdge(b []byte) (edgeVal, bool) {
	var v [3]uint64
	for i := range v {
		x, n := binary.Uvarint(b)
		if n <= 0 {
			return edgeVal{}, false
		}
		v[i], b = x, b[n:]
	}
	return edgeVal{v[0], v[1], v[2]}, len(b) == 0
}

// edgeMerger makes the edge counters Pebble merge operands: an Append adds a
// delta with Merge and never reads the current total, so concurrent appends do
// not need a lock around a read-modify-write, and the sums are folded in the
// background at compaction. The operation is commutative and associative
// (addition), so it does not matter in what order Pebble combines the operands.
var edgeMerger = &pebble.Merger{
	Name: "ozy.tracestore.edge.v1",
	Merge: func(_, value []byte) (pebble.ValueMerger, error) {
		m := &edgeMerge{}
		return m, m.MergeNewer(value)
	},
}

type edgeMerge struct{ sum edgeVal }

func (m *edgeMerge) add(v []byte) error {
	e, ok := decodeEdge(v)
	if !ok {
		// A value that is not ours cannot be merged; dropping it keeps a corrupt
		// operand from poisoning every later read of this key.
		return nil
	}
	m.sum.calls += e.calls
	m.sum.errors += e.errors
	m.sum.durSum += e.durSum
	return nil
}

func (m *edgeMerge) MergeNewer(v []byte) error { return m.add(v) }
func (m *edgeMerge) MergeOlder(v []byte) error { return m.add(v) }
func (m *edgeMerge) Finish(bool) ([]byte, io.Closer, error) {
	return m.sum.encode(), nil, nil
}

type edgeDelta struct {
	env, parent, child string
	hour               uint32
	err                bool
	dur                int64
}

type parkedEdge struct {
	key  string
	edge edgeDelta
}

// writeEdges folds deltas for the same key into one operand per key.
func writeEdges(b *pebble.Batch, deltas []edgeDelta) error {
	if len(deltas) == 0 {
		return nil
	}
	sums := map[string]*edgeVal{}
	for _, d := range deltas {
		if d.parent == d.child {
			continue // an entry span's parent in its own service is not an edge
		}
		k := string(edgeKey(d.hour, d.env, d.parent, d.child))
		v := sums[k]
		if v == nil {
			v = &edgeVal{}
			sums[k] = v
		}
		v.calls++
		if d.err {
			v.errors++
		}
		v.durSum += uint64(max(d.dur, 0))
	}
	for k, v := range sums {
		if err := b.Merge([]byte(k), v.encode(), nil); err != nil {
			return err
		}
	}
	return nil
}

// parentService finds the service of a span already stored, so a child that
// arrives after its parent resolves at once.
func (s *Store) parentService(trace []byte, parentID string) (string, bool) {
	span, ok := decodeID(parentID, spanLen)
	if !ok {
		return "", false
	}
	v, closer, err := s.db.Get(spanKey(trace, span))
	if err != nil {
		return "", false
	}
	defer closer.Close()
	sp, err := s.decodeSpan(v)
	if err != nil {
		return "", false
	}
	return sp.Service, true
}

// park holds children whose parent has not arrived. The map is keyed by
// trace+parent so the parent's arrival is one lookup. A parent that never comes
// (its process crashed, or the head sampler dropped that service's chunk) costs
// an unresolved edge after the TTL, which is counted rather than treated as an
// error: the service map is derived data, and a trace missing a link is normal.
func (s *Store) park(parked []parkedEdge) {
	if len(parked) == 0 {
		return
	}
	now := s.clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(now)
	for _, p := range parked {
		if s.npend >= maxPending {
			s.pendingDropped.Inc()
			continue
		}
		s.pending[p.key] = append(s.pending[p.key], pendingEdge{
			env: p.edge.env, child: p.edge.child, hour: p.edge.hour, err: p.edge.err, dur: p.edge.dur, expires: now.Add(s.pendTTL),
		})
		s.npend++
	}
}

func (s *Store) takePending(key string) []edgeDelta {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, ok := s.pending[key]
	if !ok {
		return nil
	}
	delete(s.pending, key)
	s.npend -= len(list)
	out := make([]edgeDelta, 0, len(list))
	for _, p := range list {
		out = append(out, edgeDelta{env: p.env, child: p.child, hour: p.hour, err: p.err, dur: p.dur})
	}
	return out
}

func (s *Store) expireLocked(now time.Time) {
	for k, list := range s.pending {
		kept := list[:0]
		for _, p := range list {
			if now.Before(p.expires) {
				kept = append(kept, p)
			} else {
				s.pendingExpired.Inc()
				s.npend--
			}
		}
		if len(kept) == 0 {
			delete(s.pending, k)
		} else {
			s.pending[k] = kept
		}
	}
}

// ExpirePending gives up on parked children older than the TTL. The server calls
// it on a timer so unresolved edges are counted even when no new orphan arrives.
func (s *Store) ExpirePending() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(s.clock.Now())
}

// Edge is one service-to-service call aggregate.
type Edge struct {
	Env, Parent, Child string
	Calls, Errors      uint64
	DurationSumUs      uint64
}
