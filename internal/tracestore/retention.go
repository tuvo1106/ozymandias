package tracestore

import (
	"context"
	"encoding/binary"
	"errors"
	"time"

	"github.com/cockroachdb/pebble"
)

// sweepBatch is how many traces one write-locked deletion handles. Small, so
// appends wait milliseconds for a sweep, never seconds.
const sweepBatch = 200

// Sweep deletes traces first seen more than the retention ago, and edge counters
// older than that, and returns how many traces it deleted.
//
// A trace is deleted whole, found through the first-seen index ('t'), which is
// keyed by the hour its first span was seen. Its index entries live under
// (env, service, start) rather than under the trace, so they are recomputed from
// the spans before those are deleted; that is why the order is read spans,
// delete indexes, delete spans, delete the first-seen key, and why a crash in the
// middle is safe: the first-seen key goes last, so the next sweep finds the trace
// again and the deletes it repeats are no-ops.
func (s *Store) Sweep(ctx context.Context) (int, error) {
	if s.closed.Load() {
		return 0, errors.New("tracestore: closed")
	}
	cutoff := hourOf(s.clock.Now().Add(-s.retention).UnixMicro())
	total := 0
	for {
		n, err := s.sweepSome(ctx, cutoff)
		total += n
		if err != nil || n < sweepBatch {
			s.sweptTraces.Add(int64(total))
			if err != nil {
				return total, err
			}
			break
		}
	}
	// Edges are keyed by hour first, so they go with one range delete.
	s.sweepMu.Lock()
	defer s.sweepMu.Unlock()
	return total, s.db.DeleteRange([]byte{prefixEdge}, binary.BigEndian.AppendUint32([]byte{prefixEdge}, cutoff), s.writeOpts())
}

func (s *Store) sweepSome(ctx context.Context, cutoff uint32) (int, error) {
	s.sweepMu.Lock()
	defer s.sweepMu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: []byte{prefixSeen}, UpperBound: binary.BigEndian.AppendUint32([]byte{prefixSeen}, cutoff)})
	if err != nil {
		return 0, err
	}
	var seen [][]byte
	for ok := it.First(); ok && len(seen) < sweepBatch; ok = it.Next() {
		seen = append(seen, append([]byte(nil), it.Key()...))
	}
	if err := errors.Join(it.Error(), it.Close()); err != nil {
		return 0, err
	}
	if len(seen) == 0 {
		return 0, nil
	}
	b := s.db.NewBatch()
	defer func() { _ = b.Close() }()
	for _, k := range seen {
		trace := k[5:]
		prefix := tracePrefix(trace)
		sit, err := s.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: successor(prefix)})
		if err != nil {
			return 0, err
		}
		for ok := sit.First(); ok; ok = sit.Next() {
			sp, err := s.decodeSpan(sit.Value())
			if err != nil || !sp.TopLevel() {
				continue
			}
			span, _ := decodeID(sp.SpanID, spanLen)
			env := sp.Meta["env"]
			_ = b.Delete(entryKey(env, sp.Service, sp.Start, trace, span), nil)
			_ = b.Delete(errorKey(env, sp.Service, sp.Start, trace, span), nil)
			_ = b.Delete(resourceKey(env, sp.Service, truncate(sp.Resource, summaryResource), sp.Start, trace, span), nil)
		}
		if err := errors.Join(sit.Error(), sit.Close()); err != nil {
			return 0, err
		}
		_ = b.DeleteRange(prefix, successor(prefix), nil)
		_ = b.Delete(k, nil)
	}
	if err := b.Commit(s.writeOpts()); err != nil {
		return 0, err
	}
	return len(seen), nil
}

// Run sweeps every interval until ctx ends. It also gives up on orphaned edges.
func (s *Store) Run(ctx context.Context, every time.Duration) {
	t := s.clock.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C():
			s.ExpirePending()
			if _, err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("tracestore: sweep failed", "err", err)
			}
		}
	}
}
