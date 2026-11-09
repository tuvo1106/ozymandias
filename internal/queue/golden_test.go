package queue

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/internal/tsdb/wal"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite the committed golden files")

// goldenPayloads cover an empty payload, every byte value, and an ordinary one.
func goldenPayloads() [][]byte {
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	return [][]byte{[]byte("first"), nil, all}
}

var goldenStart = time.Unix(1_700_000_000, 123_456_789)

// TestTopic_Golden is the promise that a topic written by any past build
// still opens and replays on this one, with the same offsets, times and
// payloads, and resumes its consumer at the same place. The queue is read at
// the worst moment — the restart after a crash, with acknowledged data in it
// that exists nowhere else — so a format change has to be news. Regenerate
// with -update-golden, and treat needing to as that news.
func TestTopic_Golden(t *testing.T) {
	dir := filepath.Join("testdata", "golden")
	// The writer must still produce the committed bytes exactly (the fake
	// clock makes the times deterministic), and the committed bytes must
	// still read back: both halves of a format change are caught.
	out := dir
	if !*updateGolden {
		out = t.TempDir()
	}
	writeGolden(t, out)
	if !*updateGolden {
		for _, rel := range []string{wal.SegmentName(0), filepath.Join(consumersDir, "tsdb"+offsetExt)} {
			want, err := os.ReadFile(filepath.Join(dir, rel))
			if err != nil {
				t.Fatalf("golden file missing (run with -update-golden): %v", err)
			}
			got, err := os.ReadFile(filepath.Join(out, rel))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s: this build writes different bytes than the committed golden file", rel)
			}
		}
	}
	readGolden(t, dir)
}

func writeGolden(t *testing.T, dir string) {
	t.Helper()
	{
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		clk := testutil.NewFakeClock(goldenStart)
		tp, err := Open(Options{Dir: dir, Clock: clk, SyncInterval: -1})
		if err != nil {
			t.Fatal(err)
		}
		c, err := tp.Consumer("tsdb")
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range goldenPayloads() {
			if _, err := tp.Produce(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			clk.Advance(time.Second)
		}
		if _, err := c.Poll(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		if err := c.Commit(0); err != nil {
			t.Fatal(err)
		}
		if err := tp.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func readGolden(t *testing.T, dir string) {
	t.Helper()
	// Work on a copy: opening a topic may write (Repair, a new segment).
	work := t.TempDir()
	for _, rel := range []string{wal.SegmentName(0), filepath.Join(consumersDir, "tsdb"+offsetExt)} {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("golden file missing (run with -update-golden): %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, rel), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tp, err := Open(Options{Dir: work, SyncInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tp.Close() }()
	if s := tp.Stats(); s.Next != 3 || s.Lag["tsdb"] != 2 {
		t.Fatalf("golden topic stats = %+v, want next 3, lag 2", s)
	}
	c, err := tp.Consumer("tsdb")
	if err != nil {
		t.Fatal(err)
	}
	recs, err := c.Poll(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	want := goldenPayloads()[1:]
	if len(recs) != len(want) {
		t.Fatalf("golden replay returned %d records, want %d", len(recs), len(want))
	}
	for i, r := range recs {
		if r.Offset != uint64(i+1) || !bytes.Equal(r.Payload, want[i]) ||
			!r.Time.Equal(goldenStart.Add(time.Duration(i+1)*time.Second)) {
			t.Errorf("golden record %d = (%d, %v, %x)", i, r.Offset, r.Time, r.Payload)
		}
	}
}
