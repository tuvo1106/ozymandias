package head

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/wal"
)

// WAL record types. The log itself is generic; these are the TSDB's.
const (
	// RecordSeries defines a series id. It must appear in the log before any
	// sample that refers to the id, which is what makes checkpointing keep
	// series records and drop sample records.
	RecordSeries uint8 = 1
	// RecordSamples carries one Append batch.
	RecordSamples uint8 = 2
)

// encodeSeries encodes id and its labels:
//
//	uvarint id | uvarint len(metric) | metric | uvarint ntags | (len|key, len|value)…
func encodeSeries(buf []byte, id uint64, ref tsdb.SeriesRef) []byte {
	buf = binary.AppendUvarint(buf, id)
	buf = appendString(buf, ref.Metric)
	buf = binary.AppendUvarint(buf, uint64(len(ref.Tags)))
	for _, t := range ref.Tags {
		buf = appendString(buf, t.Key)
		buf = appendString(buf, t.Value)
	}
	return buf
}

func decodeSeries(b []byte) (uint64, tsdb.SeriesRef, error) {
	d := decoder{b: b}
	id := d.uvarint()
	ref := tsdb.SeriesRef{Metric: d.str()}
	n := d.uvarint()
	if d.err != nil {
		return 0, ref, d.err
	}
	// A tag is two length-prefixed strings, so it costs at least two bytes.
	// Bounding by len(b) alone was enough to stop a wild count but not enough
	// to stop the allocation below from being wild: tsdb.Tag is 32 bytes, so a
	// 16 MiB record — the wal.MaxRecordSize ceiling — could ask for half a
	// gigabyte before the first d.str() failed and threw it away. The bound is
	// what makes the preallocation safe, so the two belong together.
	if n > uint64(len(b)/2) {
		return 0, ref, fmt.Errorf("head: series record claims %d tags in %d bytes", n, len(b))
	}
	ref.Tags = make([]tsdb.Tag, 0, n)
	for i := uint64(0); i < n; i++ {
		k, v := d.str(), d.str()
		ref.Tags = append(ref.Tags, tsdb.Tag{Key: k, Value: v})
	}
	return id, ref, d.err
}

// encodeSamples encodes a batch:
//
//	uvarint count | (uvarint id | varint t | float64 v)…
//
// Timestamps are absolute rather than delta-encoded: a WAL record is read
// once and thrown away, so the space saved would not pay for the coupling
// between records that delta encoding introduces.
func encodeSamples(buf []byte, samples []Sample) []byte {
	buf = binary.AppendUvarint(buf, uint64(len(samples)))
	for _, s := range samples {
		buf = binary.AppendUvarint(buf, s.ID)
		buf = binary.AppendVarint(buf, s.T)
		buf = binary.LittleEndian.AppendUint64(buf, math.Float64bits(s.V))
	}
	return buf
}

func decodeSamples(b []byte, out []Sample) ([]Sample, error) {
	d := decoder{b: b}
	n := d.uvarint()
	if d.err != nil {
		return out, d.err
	}
	// Each sample is at least 3 bytes, so a count larger than the record can
	// hold is corruption, not a huge batch. Without this a bad length would
	// make us allocate arbitrarily.
	if n > uint64(len(b)/3)+1 {
		return out, fmt.Errorf("head: samples record claims %d samples in %d bytes", n, len(b))
	}
	for i := uint64(0); i < n; i++ {
		s := Sample{ID: d.uvarint(), T: d.varint()}
		s.V = math.Float64frombits(d.uint64())
		if d.err != nil {
			return out, d.err
		}
		out = append(out, s)
	}
	return out, d.err
}

func appendString(buf []byte, s string) []byte {
	buf = binary.AppendUvarint(buf, uint64(len(s)))
	return append(buf, s...)
}

// decoder reads the encodings above, remembering the first error so callers
// can check once at the end instead of after every field.
type decoder struct {
	b   []byte
	err error
}

func (d *decoder) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.err = fmt.Errorf("head: truncated uvarint")
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *decoder) varint() int64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Varint(d.b)
	if n <= 0 {
		d.err = fmt.Errorf("head: truncated varint")
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *decoder) uint64() uint64 {
	if d.err != nil {
		return 0
	}
	if len(d.b) < 8 {
		d.err = fmt.Errorf("head: truncated float64")
		return 0
	}
	v := binary.LittleEndian.Uint64(d.b)
	d.b = d.b[8:]
	return v
}

func (d *decoder) str() string {
	n := d.uvarint()
	if d.err != nil {
		return ""
	}
	if n > uint64(len(d.b)) {
		d.err = fmt.Errorf("head: string of %d bytes exceeds the %d remaining", n, len(d.b))
		return ""
	}
	s := string(d.b[:n])
	d.b = d.b[n:]
	return s
}

// keepForCheckpoint is the policy Truncate uses, given the timestamp the head
// now refuses samples before.
//
// Series records always survive: a sample record names its series by an id
// that only a series record defines, so dropping one strands every sample
// pointing at it.
//
// Sample records survive if they still hold a sample the head has — which is
// the part that is easy to get wrong. It is tempting to drop them all on the
// grounds that a truncation only follows a block cut, so everything older is
// on disk. That is false the moment the log rolls a segment: the head keeps
// up to 1.5 block ranges of data, which at any real write rate spans several
// 32 MiB segments, and those samples exist *only* in the log and in memory.
// Dropping the segment drops the only durable copy.
//
// A record is kept whole if any of its samples is still live, so a record
// straddling the boundary is copied forward with a few dead samples in it.
// Replay refuses those by the same bounds check that refuses anything a block
// already covers, so the cost is bytes, not correctness.
func keepForCheckpoint(minValid int64) func(wal.Record) bool {
	var buf []Sample
	return func(r wal.Record) bool {
		switch r.Type {
		case RecordSeries:
			return true
		case RecordSamples:
			decoded, err := decodeSamples(r.Data, buf[:0])
			if err != nil {
				// Unreadable here, but it framed and checksummed correctly, so
				// something is wrong that this function is not equipped to
				// judge. Keep the bytes and let replay be the one to complain.
				return true
			}
			buf = decoded
			for _, s := range decoded {
				if s.T >= minValid {
					return true
				}
			}
			return false
		default:
			return false
		}
	}
}
