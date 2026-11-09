package wal

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSegmentReader_StartsMidSegmentAtARecordBoundary pins the one thing a
// queue consumer needs from the log: resuming at a byte offset the writer
// reported (via End) yields exactly the records written after it.
func TestSegmentReader_StartsMidSegmentAtARecordBoundary(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Log(Record{Type: 1, Data: []byte("first")}); err != nil {
		t.Fatal(err)
	}
	mid := w.End()
	if mid.Segment != 0 || mid.Offset != int64(headerSize+len("first")) || mid.File != SegmentName(0) {
		t.Fatalf("End() after one record = %+v", mid)
	}
	for _, s := range []string{"second", "third"} {
		if err := w.Log(Record{Type: 2, Data: []byte(s)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	r := NewSegmentReader(dir, mid.Segment, mid.Offset)
	defer func() { _ = r.Close() }()
	var got []string
	for r.Next() {
		got = append(got, string(r.Record().Data))
	}
	if r.Err() != nil {
		t.Fatal(r.Err())
	}
	if len(got) != 2 || got[0] != "second" || got[1] != "third" {
		t.Fatalf("records from mid-segment = %q, want [second third]", got)
	}
	if end := r.End(); end.Offset != w.size {
		t.Errorf("reader End = %d, want the file end %d", end.Offset, w.size)
	}
}

// TestSegmentReader_IsStrictAboutATornTail: the queue only reads below the
// synced end, so a short record there is lost data, not a crash in progress.
// The ordinary reader forgives it in the last file; this one must not.
func TestSegmentReader_IsStrictAboutATornTail(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Log(Record{Type: 1, Data: []byte("whole")}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, SegmentName(0)), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0, 0, 0, 9, 1}); err != nil { // a header cut short
		t.Fatal(err)
	}
	_ = f.Close()

	r := NewSegmentReader(dir, 0, 0)
	defer func() { _ = r.Close() }()
	n := 0
	for r.Next() {
		n++
	}
	if n != 1 {
		t.Errorf("read %d records before the damage, want 1", n)
	}
	if !errors.Is(r.Err(), ErrCorrupt) {
		t.Errorf("Err() = %v, want ErrCorrupt", r.Err())
	}

	// The forgiving reader, for contrast, treats the same bytes as a torn tail.
	lenient, err := NewReader(dir)
	if err != nil {
		t.Fatal(err)
	}
	for lenient.Next() {
	}
	_ = lenient.Close()
	if lenient.Err() != nil {
		t.Errorf("NewReader on a torn tail: Err() = %v, want nil", lenient.Err())
	}
}

func TestSegmentReader_MissingSegmentAndBadSeekAreErrors(t *testing.T) {
	dir := t.TempDir()
	r := NewSegmentReader(dir, 7, 0)
	if r.Next() || r.Err() == nil {
		t.Fatalf("missing segment: Next=true or Err=nil (%v)", r.Err())
	}
	w, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Log(Record{Type: 1, Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	// A negative offset is a caller bug; reading from 0 instead would
	// silently redeliver the whole segment.
	r = NewSegmentReader(dir, 0, -5)
	if r.Next() || r.Err() == nil {
		t.Fatalf("negative start offset: Next=true or Err=nil (%v)", r.Err())
	}
	_ = r.Close()
}

// TestLog_AFailedRolloverFsyncIsErrSync: the queue must be able to tell a
// failed fsync (never retry) from a failed write (retry is fine).
func TestLog_AFailedRolloverFsyncIsErrSync(t *testing.T) {
	w, err := Open(Options{Dir: t.TempDir(), SegmentSize: 16})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Log(Record{Type: 1, Data: []byte("fills it")}); err != nil {
		t.Fatal(err)
	}
	_ = w.f.Close() // the rollover's fsync of this segment now fails
	err = w.Log(Record{Type: 1, Data: []byte("rolls")})
	if !errors.Is(err, ErrSync) || !errors.Is(err, ErrBroken) {
		t.Fatalf("Log over a failing rollover fsync = %v, want ErrSync and ErrBroken", err)
	}
}

func TestSegments_ListsInOrderAndIgnoresOtherFiles(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{SegmentName(3), SegmentName(1), "checkpoint.00000001", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Segments(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("Segments = %v, want [1 3]", got)
	}
	if _, err := Segments(filepath.Join(dir, "absent")); err == nil {
		t.Error("Segments on a missing dir = nil error")
	}
}

// TestSync_AFailedFsyncStopsTheLog: after a failed fsync a retry can report
// success over pages the kernel dropped, so the log must refuse everything
// after it (ErrBroken), not just report the one failure.
func TestSync_AFailedFsyncStopsTheLog(t *testing.T) {
	w, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Log(Record{Type: 1, Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	_ = w.f.Close()
	if err := w.Sync(); !errors.Is(err, ErrSync) || !errors.Is(err, ErrBroken) {
		t.Fatalf("failed Sync = %v, want ErrSync and ErrBroken", err)
	}
	if err := w.Log(Record{Type: 1, Data: []byte("y")}); !errors.Is(err, ErrBroken) {
		t.Fatalf("Log after a failed Sync = %v, want ErrBroken", err)
	}
}

// TestLog_ARolloverThatCannotOpenTheNextSegmentStopsTheLog: the old segment
// is closed by then, so no later write has anywhere to go.
func TestLog_ARolloverThatCannotOpenTheNextSegmentStopsTheLog(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(Options{Dir: dir, SegmentSize: 16})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Log(Record{Type: 1, Data: []byte("fills it")}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o755) }()
	if err := w.Log(Record{Type: 1, Data: []byte("rolls")}); !errors.Is(err, ErrBroken) {
		t.Fatalf("Log over a failed rollover open = %v, want ErrBroken", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := w.Log(Record{Type: 1, Data: []byte("again")}); !errors.Is(err, ErrBroken) {
		t.Fatalf("Log after a failed rollover = %v, want the log to stay stopped", err)
	}
}
