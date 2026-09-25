package head

import (
	"encoding/binary"
	"runtime"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
)

// TestDecodeSeries_ATagCountCannotReserveMoreThanTheRecord.
//
// The count was checked against len(b), which stops a wild *number* but not a
// wild *allocation*. A tag costs at least two bytes on the wire — two
// length-prefixed strings — while tsdb.Tag is 32 bytes wide, so a record
// claiming len(b) tags passed the check and then reserved sixteen times the
// record's own size. At the 16 MiB wal.MaxRecordSize ceiling that is half a
// gigabyte, taken before a single string had been parsed, from a record whose
// only validation so far was the WAL's crc — which says the bytes arrived as
// written, not that they mean anything.
//
// Measuring the allocation rather than the error, because the old code
// returned an error too: it ran out of bytes partway through the tags and
// failed *after* reserving the memory. The error was never the problem.
func TestDecodeSeries_ATagCountCannotReserveMoreThanTheRecord(t *testing.T) {
	const size = 1 << 20
	// id, an empty metric name, then a tag count equal to the record length —
	// the largest the old bound would accept. The rest is zero bytes, which
	// decode as empty strings, so nothing else here allocates.
	rec := binary.AppendUvarint(nil, 1)
	rec = binary.AppendUvarint(rec, 0)
	rec = binary.AppendUvarint(rec, size)
	rec = append(rec, make([]byte, size-len(rec))...)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, _, _ = decodeSeries(rec)
	runtime.ReadMemStats(&after)

	// Generous: the point is 32x versus a small constant, not a tight bound.
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 4*size {
		t.Errorf("decoding a %d-byte record allocated %d bytes (%.1fx); the tag count "+
			"it claimed was reserved before any of it was parsed", size, grew, float64(grew)/size)
	}
}

// TestDecodeSeries_TheBoundStillAcceptsRealRecords guards the other side: the
// tightened bound must not reject anything encodeSeries produces. Two
// single-character tags in a record with an empty metric name is the densest
// shape there is — the case that sits closest to the limit.
func TestDecodeSeries_TheBoundStillAcceptsRealRecords(t *testing.T) {
	ref := tsdb.SeriesRef{Metric: "", Tags: []tsdb.Tag{{Key: "a", Value: "b"}, {Key: "c", Value: "d"}}}
	id, got, err := decodeSeries(encodeSeries(nil, 7, ref))
	if err != nil {
		t.Fatal(err)
	}
	if id != 7 || len(got.Tags) != 2 {
		t.Errorf("decoded id %d with %d tags, want 7 and 2", id, len(got.Tags))
	}
}

// FuzzRecordDecoders. decodeSeries and decodeSamples read bytes that came off
// a disk and have only been checked by the WAL's own crc, which says the
// record arrived as it was written — not that what was written made sense. A
// bug in the encoder, or a log from a future version, produces exactly that.
//
// They were the last decoders in the TSDB without a fuzz target, alongside
// chunks.dat, and that is where both of this round's allocation and overflow
// bugs were. An error is always fine; a panic and an over-allocation are not.
func FuzzRecordDecoders(f *testing.F) {
	f.Add(encodeSeries(nil, 1, tsdb.SeriesRef{Metric: "m", Tags: []tsdb.Tag{{Key: "host", Value: "a"}}}))
	f.Add(encodeSamples(nil, []Sample{{ID: 1, T: 1, V: 1}, {ID: 2, T: 2, V: 2}}))
	f.Add([]byte{})
	f.Add(binary.AppendUvarint(binary.AppendUvarint(binary.AppendUvarint(nil, 1), 0), 1<<40))
	f.Add(binary.AppendUvarint(nil, ^uint64(0)))

	f.Fuzz(func(t *testing.T, rec []byte) {
		if _, ref, err := decodeSeries(rec); err == nil {
			// It parsed. Then what it claimed has to fit in what it was given,
			// because the bound on the count is the only thing standing
			// between a corrupt record and an arbitrary allocation.
			if 2*len(ref.Tags) > len(rec) {
				t.Fatalf("accepted %d tags from a %d-byte record", len(ref.Tags), len(rec))
			}
		}
		if out, err := decodeSamples(rec, nil); err == nil {
			if 3*len(out) > len(rec)+3 {
				t.Fatalf("accepted %d samples from a %d-byte record", len(out), len(rec))
			}
		}
	})
}
