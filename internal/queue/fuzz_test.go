package queue

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/tsdb/wal"
)

// FuzzScan runs Open's recovery read over a segment of arbitrary bytes. This
// is the startup path after a crash: the segment is whatever the disk holds,
// and the scan decides how many records the topic has and where each segment
// starts. It is fuzzed directly rather than through Open because Open
// fsyncs (Repair, the directory, the new segment), which on macOS is
// F_FULLFSYNC and held the fuzzer to about fifteen inputs a second.
//
// The properties: no panic and no hang; and anything the scan accepts must be
// a topic a consumer can read end to end, in offset order, without error —
// the scan is the only check Open makes, so whatever it lets through is
// served.
func FuzzScan(f *testing.F) {
	good := func(recs ...wal.Record) []byte {
		dir := f.TempDir()
		w, err := wal.Open(wal.Options{Dir: dir})
		if err != nil {
			f.Fatal(err)
		}
		if err := w.Log(recs...); err != nil {
			f.Fatal(err)
		}
		_ = w.Close()
		b, err := os.ReadFile(filepath.Join(dir, wal.SegmentName(0)))
		if err != nil {
			f.Fatal(err)
		}
		return b
	}
	at := time.Unix(1, 0)
	seed := good(
		wal.Record{Type: recordType, Data: encodeRecord(0, at, []byte("a"))},
		wal.Record{Type: recordType, Data: encodeRecord(1, at, nil)},
	)
	f.Add(seed)
	f.Add(seed[:len(seed)-3])
	f.Add([]byte{})
	f.Add(good(wal.Record{Type: recordType, Data: encodeRecord(7, at, []byte("starts late"))}))
	f.Add(good(wal.Record{Type: 2, Data: encodeRecord(0, at, nil)}))

	f.Fuzz(func(t *testing.T, seg []byte) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, wal.SegmentName(0)), seg, 0o644); err != nil {
			t.Fatal(err)
		}
		segs, next, err := scan(dir)
		if err != nil {
			return
		}
		if len(segs) != 1 {
			t.Fatalf("scan of one file returned %d segments", len(segs))
		}
		if segs[0].size == 0 {
			return
		}
		c := &Consumer{t: &Topic{dir: dir, segs: segs}, name: "f", cursor: segs[0].first}
		var out []Record
		got, err := c.readSegment(&out, 1<<20, next)
		if err != nil || !got && c.cursor != next {
			t.Fatalf("scan accepted the segment (records %d..%d) but reading it stopped at %d: %v",
				segs[0].first, next, c.cursor, err)
		}
	})
}

// FuzzDecodeOffset: an offset file is 12 bytes read at startup to decide
// where to resume. Anything it accepts must re-encode to the same bytes, so a
// damaged file can never be read as a different offset.
func FuzzDecodeOffset(f *testing.F) {
	f.Add(encodeOffset(0))
	f.Add(encodeOffset(1 << 40))
	f.Add([]byte{1, 2, 3})
	f.Fuzz(func(t *testing.T, b []byte) {
		next, err := decodeOffset(b)
		if err != nil {
			return
		}
		if got := encodeOffset(next); string(got) != string(b) {
			t.Fatalf("decodeOffset(%x) = %d, which encodes as %x", b, next, got)
		}
	})
}
