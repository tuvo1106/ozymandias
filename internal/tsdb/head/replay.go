package head

import (
	"fmt"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/wal"
)

// Replay rebuilds a head from a write-ahead log directory.
//
// It is the other half of the durability bargain: Append wrote the record
// before acknowledging, and this reads it back. What it restores is exactly
// what was acknowledged — samples logged but not yet synced may be missing,
// and that is the promise, not a bug.
//
// Series ids are taken from the log rather than reassigned, so ids stay stable
// across restarts and sample records keep pointing at the right series.
// Samples the head would now reject (out of order, out of bounds) are counted
// and skipped: replay is not a second chance to accept bad data.
func Replay(h *Head, dir string) (Stats, error) {
	r, err := wal.NewReader(dir)
	if err != nil {
		return Stats{}, err
	}
	defer func() { _ = r.Close() }()

	// The log's ids, not the head's. A series key can be named by more than one
	// id over the life of a log: a block cut makes the head forget a series,
	// and when it reports again it is given a fresh id, while the old series
	// record lives on in the checkpoint. Both ids mean the same series, so
	// replay keeps its own mapping and lets the head hold one identity per
	// key. Resolving through the head's own id map instead dropped every
	// sample logged under the second id.
	byLogID := map[uint64]*memSeries{}
	var samples []Sample
	var applied, skipped int64

	for r.Next() {
		rec := r.Record()
		switch rec.Type {
		case RecordSeries:
			id, ref, err := decodeSeries(rec.Data)
			if err != nil {
				return Stats{}, fmt.Errorf("head: replaying series record: %w", err)
			}
			byLogID[id] = h.restoreSeries(id, ref)
		case RecordSamples:
			samples = samples[:0]
			samples, err = decodeSamples(rec.Data, samples)
			if err != nil {
				return Stats{}, fmt.Errorf("head: replaying samples record: %w", err)
			}
			for _, s := range samples {
				ms := byLogID[s.ID]
				if ms == nil {
					// A sample whose series record is missing: the log lost
					// the definition, so the id means nothing.
					skipped++
					continue
				}
				stored, err := h.appendTo(ms, s.T, s.V)
				if err != nil {
					skipped++
					continue
				}
				if stored {
					applied++
				}
			}
		default:
			// An unknown record type is from a newer version of this code.
			// Skipping is the forward-compatible choice, and the type byte is
			// exactly why the format has one.
			continue
		}
	}
	if err := r.Err(); err != nil {
		return Stats{}, err
	}
	st := h.Stats()
	st.Samples = applied
	st.OOORejected = skipped
	return st, nil
}

// restoreSeries returns the head's series for ref, creating it with the id the
// log recorded if it is not there yet.
//
// It returns the series rather than nothing because the caller has to know
// which one this log id refers to: the same key can appear under several ids,
// and every one of them has to resolve to the single series the head keeps for
// that key. Bailing out on the second record — as this used to — left that id
// unresolvable and silently dropped every sample logged against it, which is
// the ordinary fate of any series that goes quiet long enough to be cut into a
// block and then reports again.
func (h *Head) restoreSeries(id uint64, ref tsdb.SeriesRef) *memSeries {
	key := ref.Key()
	sh := h.shardFor(key)

	sh.mu.Lock()
	defer sh.mu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()

	if id > h.nextID {
		// Keep handing out ids above anything the log used, or a new series
		// would collide with a restored one.
		h.nextID = id
	}
	if existing, ok := sh.series[key]; ok {
		return existing // a repeated record, or the same key under a new id
	}
	if _, taken := h.byID[id]; taken {
		// Two keys under one id is a log this code could not have written.
		// Give the series an id of its own rather than overwrite the other:
		// the caller resolves by log id, so nothing depends on keeping this
		// one, and a corrupt log should cost its own records, not someone
		// else's.
		h.nextID++
		id = h.nextID
	}
	ms := &memSeries{id: id, ref: ref, logged: true} // its record is in the log
	h.byID[id] = ms
	h.perMetric[ref.Metric]++
	h.postings.Add(id, ref)
	sh.series[key] = ms
	return ms
}
