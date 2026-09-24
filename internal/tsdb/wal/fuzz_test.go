package wal

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// FuzzReader replays arbitrary bytes as a log segment.
//
// The reader's framing is `u32 len | u8 type | u32 crc32c | payload`, and the
// length is the first thing it reads — on a corrupt segment it is whatever the
// disk says, up to 4 GiB. This is the input a torn write, a bit flip or a
// filesystem that returned someone else's block produces, and the reader runs
// on it at every startup, before anything else in the process is up.
//
// What is asserted is the safety property, not a parse: no panic, no hang, and
// no allocation driven by a length the file chose. A record that survives must
// have passed its checksum, so anything the reader *returns* is trustworthy;
// the question is only what it does on the way there.
func FuzzReader(f *testing.F) {
	dir := f.TempDir()
	w, err := Open(Options{Dir: dir})
	if err != nil {
		f.Fatal(err)
	}
	if err := w.Log(Record{Type: 1, Data: []byte("hello")}, Record{Type: 2, Data: nil}); err != nil {
		f.Fatal(err)
	}
	if err := w.Close(); err != nil {
		f.Fatal(err)
	}
	good, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("%08d%s", 0, segmentExt)))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(good)
	f.Add(good[:len(good)/2]) // a torn tail
	f.Add([]byte{})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0x01, 0, 0, 0, 0}) // a 4 GiB record
	f.Add(make([]byte, headerSize))

	f.Fuzz(func(t *testing.T, seg []byte) {
		dir := t.TempDir()
		path := filepath.Join(dir, fmt.Sprintf("%08d%s", 0, segmentExt))
		if err := os.WriteFile(path, seg, 0o644); err != nil {
			t.Fatal(err)
		}
		r, err := NewReader(dir)
		if err != nil {
			return
		}
		defer func() { _ = r.Close() }()
		for r.Next() {
			rec := r.Record()
			if len(rec.Data) > len(seg) {
				t.Fatalf("a %d byte segment yielded a %d byte record", len(seg), len(rec.Data))
			}
		}
		_ = r.Err()
		_ = r.End()
	})
}
