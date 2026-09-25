package wal

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite the committed golden files")

// goldenRecords is the fixture behind testdata/golden/00000000.wal: an empty
// payload, a payload with every byte value in it, and a type byte at the top
// of its range, because those are the three things a framing bug gets wrong.
func goldenRecords() []Record {
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	return []Record{
		{Type: 1, Data: []byte("the first record")},
		{Type: 2, Data: nil},
		{Type: 255, Data: all},
	}
}

// TestWAL_Golden is the promise that a log segment written by any past version
// of this code still replays on this one.
//
// This is the file an ozyd reads at startup after a crash, which is the
// worst possible moment to discover that the framing changed. A unit test
// cannot catch that, because it writes and reads with the same build; only
// committed bytes can. Regenerate with -update-golden, and treat needing to as
// the news that it is.
func TestWAL_Golden(t *testing.T) {
	dir := filepath.Join("testdata", "golden")
	name := fmt.Sprintf("%08d%s", 0, segmentExt)
	want := goldenRecords()

	if *updateGolden {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		w, err := Open(Options{Dir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Log(want...); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", filepath.Join(dir, name))
	}

	r, err := NewReader(dir)
	if err != nil {
		t.Fatalf("%v — regenerate with: go test ./internal/tsdb/wal -run Golden -update-golden", err)
	}
	defer func() { _ = r.Close() }()
	var got []Record
	for r.Next() {
		rec := r.Record()
		got = append(got, Record{Type: rec.Type, Data: append([]byte(nil), rec.Data...)})
	}
	if err := r.Err(); err != nil {
		t.Fatalf("replaying the committed segment: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("the committed segment replayed %d records, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Type != want[i].Type || !bytes.Equal(got[i].Data, want[i].Data) {
			t.Errorf("record %d is type %d / %d bytes, want type %d / %d bytes",
				i, got[i].Type, len(got[i].Data), want[i].Type, len(want[i].Data))
		}
	}

	// And the writer still produces those exact bytes.
	tmp := t.TempDir()
	w, err := Open(Options{Dir: tmp})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Log(want...); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	fresh, err := os.ReadFile(filepath.Join(tmp, name))
	if err != nil {
		t.Fatal(err)
	}
	committed, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fresh, committed) {
		t.Errorf("the writer no longer produces the committed segment (%d bytes now, %d committed).\n"+
			"If that is intended, regenerate with -update-golden and explain the format change.",
			len(fresh), len(committed))
	}
}
