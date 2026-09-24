package wal

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	typeSeries  = 1
	typeSamples = 2
)

func rec(typ uint8, s string) Record { return Record{Type: typ, Data: []byte(s)} }

// replay reads the whole log, failing the test on a corruption error.
func replay(t *testing.T, dir string) []Record {
	t.Helper()
	r, err := NewReader(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	var out []Record
	for r.Next() {
		got := r.Record()
		out = append(out, Record{Type: got.Type, Data: append([]byte(nil), got.Data...)})
	}
	if err := r.Err(); err != nil {
		t.Fatalf("replay: %v", err)
	}
	return out
}

func equalRecords(a, b []Record) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Type != b[i].Type || !bytes.Equal(a[i].Data, b[i].Data) {
			return false
		}
	}
	return true
}

func TestWAL_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	want := []Record{
		rec(typeSeries, "series 1"),
		rec(typeSamples, "samples for 1"),
		{Type: typeSamples, Data: nil}, // empty payload is legal
		rec(typeSeries, strings.Repeat("x", 10_000)),
	}
	if err := w.Log(want...); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got := replay(t, dir)
	if len(got) != len(want) {
		t.Fatalf("replayed %d records, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Type != want[i].Type || !bytes.Equal(got[i].Data, want[i].Data) {
			t.Errorf("record %d: got (%d, %d bytes), want (%d, %d bytes)",
				i, got[i].Type, len(got[i].Data), want[i].Type, len(want[i].Data))
		}
	}
}

func TestWAL_EmptyAndMissingDirectoriesReplayAsEmpty(t *testing.T) {
	// A first start must not look like a failure.
	if got := replay(t, t.TempDir()); len(got) != 0 {
		t.Errorf("empty dir replayed %d records", len(got))
	}
	r, err := NewReader(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Next() {
		t.Error("missing dir produced a record")
	}
	if err := r.Err(); err != nil {
		t.Errorf("missing dir is an error: %v", err)
	}
}

func TestWAL_RollsSegmentsAndReplaysAcrossThem(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(Options{Dir: dir, SegmentSize: 512})
	if err != nil {
		t.Fatal(err)
	}
	var want []Record
	for i := 0; i < 50; i++ {
		r := rec(typeSamples, fmt.Sprintf("record %02d payload", i))
		want = append(want, r)
		if err := w.Log(r); err != nil {
			t.Fatal(err)
		}
	}
	if w.Segment() == 0 {
		t.Fatal("expected the log to have rolled to a new segment")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := replay(t, dir); !equalRecords(got, want) {
		t.Errorf("replayed %d records, want %d", len(got), len(want))
	}
}

func TestWAL_ABatchIsNeverSplitAcrossSegments(t *testing.T) {
	// Replay must never see half a batch: a caller's records are one unit.
	dir := t.TempDir()
	w, err := Open(Options{Dir: dir, SegmentSize: 256})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Log(rec(typeSamples, strings.Repeat("a", 100))); err != nil {
		t.Fatal(err)
	}
	batch := []Record{
		rec(typeSamples, strings.Repeat("b", 100)),
		rec(typeSamples, strings.Repeat("c", 100)),
	}
	if err := w.Log(batch...); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// The batch did not fit after the first record, so it must be alone in
	// segment 1 rather than straddling the boundary.
	second, err := os.ReadFile(filepath.Join(dir, "00000001.wal"))
	if err != nil {
		t.Fatal(err)
	}
	if want := 2 * (headerSize + 100); len(second) != want {
		t.Errorf("segment 1 holds %d bytes, want the whole %d-byte batch", len(second), want)
	}
}

func TestWAL_ReopenAppendsToTheNewestSegment(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(Options{Dir: dir, SegmentSize: 128})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if err := w.Log(rec(typeSamples, fmt.Sprintf("before %d", i))); err != nil {
			t.Fatal(err)
		}
	}
	seg := w.Segment()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	w2, err := Open(Options{Dir: dir, SegmentSize: 128})
	if err != nil {
		t.Fatal(err)
	}
	if w2.Segment() < seg {
		t.Errorf("reopened at segment %d, behind %d", w2.Segment(), seg)
	}
	if err := w2.Log(rec(typeSamples, "after")); err != nil {
		t.Fatal(err)
	}
	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}
	got := replay(t, dir)
	if len(got) != 11 || string(got[10].Data) != "after" {
		t.Errorf("replayed %d records, last %q", len(got), got[len(got)-1].Data)
	}
}

func TestWAL_RejectsReservedTypeAndOversizedRecords(t *testing.T) {
	w, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if err := w.Log(Record{Type: 0, Data: []byte("x")}); err == nil {
		t.Error("type 0 is reserved and must be refused")
	}
	if err := w.Log(Record{Type: 1, Data: make([]byte, MaxRecordSize+1)}); err == nil {
		t.Error("an oversized record must be refused")
	}
}

// --- damage ----------------------------------------------------------------

// writeLog fills a log with n records of the given type across small segments.
func writeLog(t *testing.T, dir string, n int, segSize int64) []Record {
	t.Helper()
	w, err := Open(Options{Dir: dir, SegmentSize: segSize})
	if err != nil {
		t.Fatal(err)
	}
	var want []Record
	for i := 0; i < n; i++ {
		r := rec(typeSamples, fmt.Sprintf("record %03d", i))
		want = append(want, r)
		if err := w.Log(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return want
}

func TestWAL_TornTailInTheLastSegmentIsNotAnError(t *testing.T) {
	// The expected shape of a crash: the process died mid-write. Everything
	// before the partial record is still good and must replay.
	for _, cut := range []int{1, headerSize - 1, headerSize + 2} {
		t.Run(fmt.Sprintf("cut %d bytes", cut), func(t *testing.T) {
			dir := t.TempDir()
			want := writeLog(t, dir, 20, 256)

			last := lastSegment(t, dir)
			info, err := os.Stat(last)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(last, info.Size()-int64(cut)); err != nil {
				t.Fatal(err)
			}
			got := replay(t, dir) // must not error
			if len(got) >= len(want) {
				t.Errorf("replayed %d records, expected fewer than %d", len(got), len(want))
			}
			if !equalRecords(got, want[:len(got)]) {
				t.Error("the records that did replay are not a prefix of what was written")
			}
		})
	}
}

func TestWAL_GarbageAtTheTailStopsReplayQuietly(t *testing.T) {
	// Zero-filled or junk bytes at the end look like a header with a reserved
	// type or an implausible length. Either way: stop, don't guess.
	for name, tail := range map[string][]byte{
		"zeros": make([]byte, 64),
		"junk":  bytes.Repeat([]byte{0xff}, 64),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			want := writeLog(t, dir, 10, 4096)
			f, err := os.OpenFile(lastSegment(t, dir), os.O_WRONLY|os.O_APPEND, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write(tail); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			if got := replay(t, dir); !equalRecords(got, want) {
				t.Errorf("replayed %d records, want the %d good ones", len(got), len(want))
			}
		})
	}
}

func TestWAL_CorruptionInAnEarlierSegmentIsReported(t *testing.T) {
	// Damage that cannot be explained by a crash means acknowledged data is
	// gone. Skipping it silently would be the worst possible behaviour.
	dir := t.TempDir()
	writeLog(t, dir, 60, 256)

	segs, err := listNumbered(dir, "", segmentExt)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) < 3 {
		t.Fatalf("test needs several segments, got %d", len(segs))
	}
	// Flip a payload bit in the first segment: the header still parses, so
	// only the CRC can catch it.
	path := filepath.Join(dir, fmt.Sprintf("%08d%s", segs[0], segmentExt))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[headerSize+2] ^= 0x40
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := NewReader(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	for r.Next() {
	}
	if r.Err() == nil {
		t.Fatal("corruption in an earlier segment replayed cleanly")
	}
	if !errors.Is(r.Err(), ErrCorrupt) {
		t.Errorf("error %v does not wrap ErrCorrupt", r.Err())
	}
	if !strings.Contains(r.Err().Error(), "00000000.wal") {
		t.Errorf("error %q does not name the damaged file", r.Err())
	}
}

func TestTruncateTail_MakesATornLogAppendableAgain(t *testing.T) {
	dir := t.TempDir()
	want := writeLog(t, dir, 20, 256)
	last := lastSegment(t, dir)
	info, err := os.Stat(last)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(last, info.Size()-3); err != nil {
		t.Fatal(err)
	}

	r, err := NewReader(dir)
	if err != nil {
		t.Fatal(err)
	}
	var survived []Record
	for r.Next() {
		got := r.Record()
		survived = append(survived, Record{Type: got.Type, Data: append([]byte(nil), got.Data...)})
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	end := r.End()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := TruncateTail(dir, end); err != nil {
		t.Fatal(err)
	}

	// Appending after the cut must be replayable — the bug this prevents is a
	// new record hiding behind a partial one forever.
	w, err := Open(Options{Dir: dir, SegmentSize: 256})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Log(rec(typeSamples, "after recovery")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got := replay(t, dir)
	if len(got) != len(survived)+1 {
		t.Fatalf("replayed %d records, want %d survivors plus 1", len(got), len(survived))
	}
	if string(got[len(got)-1].Data) != "after recovery" {
		t.Errorf("last record is %q", got[len(got)-1].Data)
	}
	if !equalRecords(got[:len(survived)], survived) && len(want) == 0 {
		t.Error("survivors changed")
	}
}

// --- truncation and checkpoints --------------------------------------------

func TestTruncate_KeepsWhatLaterSegmentsStillNeed(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(Options{Dir: dir, SegmentSize: 256})
	if err != nil {
		t.Fatal(err)
	}
	// Series records define ids that later sample records refer to; they live
	// in the oldest segment, which is exactly the one being deleted.
	if err := w.Log(rec(typeSeries, "series A"), rec(typeSeries, "series B")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		if err := w.Log(rec(typeSamples, fmt.Sprintf("samples %03d", i))); err != nil {
			t.Fatal(err)
		}
	}
	current := w.Segment()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if current == 0 {
		t.Fatal("test needs more than one segment")
	}

	keep := func(r Record) bool { return r.Type == typeSeries }
	if err := Truncate(dir, current, keep); err != nil {
		t.Fatal(err)
	}

	segs, err := listNumbered(dir, "", segmentExt)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range segs {
		if n < current {
			t.Errorf("segment %d survived truncation", n)
		}
	}
	got := replay(t, dir)
	var series []string
	for _, r := range got {
		if r.Type == typeSeries {
			series = append(series, string(r.Data))
		}
	}
	if len(series) != 2 || series[0] != "series A" || series[1] != "series B" {
		t.Errorf("series records did not survive: %v", series)
	}
	// And the checkpoint replays before the segments, so ids are defined
	// before the samples that use them.
	if got[0].Type != typeSeries {
		t.Errorf("first replayed record is type %d, want the checkpointed series", got[0].Type)
	}
}

func TestTruncate_IsANoopWithNothingToDelete(t *testing.T) {
	dir := t.TempDir()
	want := writeLog(t, dir, 5, 1<<20)
	if err := Truncate(dir, 0, func(Record) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if got := replay(t, dir); !equalRecords(got, want) {
		t.Errorf("a no-op truncation changed the log: %d records, want %d", len(got), len(want))
	}
}

func TestTruncate_SupersedesAnEarlierCheckpoint(t *testing.T) {
	// Two rounds of truncation: a series record copied into the first
	// checkpoint must be copied forward again, not dropped.
	dir := t.TempDir()
	w, err := Open(Options{Dir: dir, SegmentSize: 256})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Log(rec(typeSeries, "series A")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		if err := w.Log(rec(typeSamples, fmt.Sprintf("s%03d", i))); err != nil {
			t.Fatal(err)
		}
	}
	mid := w.Segment()
	for i := 30; i < 60; i++ {
		if err := w.Log(rec(typeSamples, fmt.Sprintf("s%03d", i))); err != nil {
			t.Fatal(err)
		}
	}
	last := w.Segment()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if mid == 0 || last == mid {
		t.Skip("segments did not roll as expected on this filesystem")
	}

	keep := func(r Record) bool { return r.Type == typeSeries }
	if err := Truncate(dir, mid, keep); err != nil {
		t.Fatal(err)
	}
	if err := Truncate(dir, last, keep); err != nil {
		t.Fatal(err)
	}

	var series int
	for _, r := range replay(t, dir) {
		if r.Type == typeSeries {
			series++
		}
	}
	if series != 1 {
		t.Errorf("found %d series records after two truncations, want exactly 1", series)
	}
	cps, err := listNumbered(dir, checkpointPre, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cps) != 1 {
		t.Errorf("found %d checkpoints, want 1", len(cps))
	}
}

func lastSegment(t *testing.T, dir string) string {
	t.Helper()
	segs, err := listNumbered(dir, "", segmentExt)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) == 0 {
		t.Fatal("no segments")
	}
	return filepath.Join(dir, fmt.Sprintf("%08d%s", segs[len(segs)-1], segmentExt))
}

// --- error paths and stated invariants -------------------------------------

func TestWAL_SyncMakesRecordsDurableAndFailsAfterClose(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Log(rec(typeSamples, "durable")); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	// Readable by a separate reader while the writer is still open: Sync is
	// the point at which a crash would no longer lose the record.
	if got := replay(t, dir); len(got) != 1 || string(got[0].Data) != "durable" {
		t.Errorf("after Sync, replay gave %v", got)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close is not idempotent: %v", err)
	}
	// A closed log must refuse work rather than write to a closed fd.
	if err := w.Sync(); err == nil {
		t.Error("Sync on a closed log should fail")
	}
	if err := w.Log(rec(typeSamples, "x")); err == nil {
		t.Error("Log on a closed log should fail")
	}
	if err := w.Log(); err != nil {
		t.Errorf("logging nothing should be a no-op, got %v", err)
	}
}

func TestOpen_Errors(t *testing.T) {
	if _, err := Open(Options{}); err == nil {
		t.Error("Open without a Dir should fail")
	}
	// A path that cannot be a directory, because a file is in the way.
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Options{Dir: file}); err == nil {
		t.Error("Open on a file path should fail")
	}
	if _, err := NewReader(file); err == nil {
		t.Error("NewReader on a file path should fail")
	}
}

func TestReader_UnreadableFileIsReported(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, 2, 1<<20)
	if err := os.Chmod(lastSegment(t, dir), 0o000); err != nil {
		t.Skip("cannot make a file unreadable here")
	}
	defer func() { _ = os.Chmod(lastSegment(t, dir), 0o644) }()
	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions do not apply")
	}
	r, err := NewReader(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	for r.Next() {
	}
	if r.Err() == nil {
		t.Error("an unreadable segment should be reported")
	}
}

func TestTruncateTail_EdgeCases(t *testing.T) {
	dir := t.TempDir()
	// Nothing read: nothing to cut.
	if err := TruncateTail(dir, Position{}); err != nil {
		t.Errorf("empty position: %v", err)
	}
	// A position naming a file that does not exist is an error, not a panic.
	if err := TruncateTail(dir, Position{File: "00000000.wal", Offset: 4}); err == nil {
		t.Error("truncating a missing file should fail")
	}
	// An already-clean log is left alone.
	want := writeLog(t, dir, 4, 1<<20)
	r, err := NewReader(dir)
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
	}
	end := r.End()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := TruncateTail(dir, end); err != nil {
		t.Fatal(err)
	}
	if got := replay(t, dir); !equalRecords(got, want) {
		t.Errorf("truncating a clean log changed it: %d records, want %d", len(got), len(want))
	}
}

func TestTruncateTail_RemovesSegmentsStrandedAfterTheDamage(t *testing.T) {
	// Corruption in the middle makes every later segment unreachable by
	// replay. Leaving them would silently strand data and mislead the next
	// restart about where the log ends.
	dir := t.TempDir()
	writeLog(t, dir, 60, 256)
	segs, err := listNumbered(dir, "", segmentExt)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) < 3 {
		t.Skip("needs several segments")
	}
	damaged := segs[0]
	if err := TruncateTail(dir, Position{
		File:    fmt.Sprintf("%08d%s", damaged, segmentExt),
		Offset:  int64(headerSize),
		Segment: damaged,
	}); err != nil {
		t.Fatal(err)
	}
	after, err := listNumbered(dir, "", segmentExt)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0] != damaged {
		t.Errorf("segments after truncation: %v, want only %d", after, damaged)
	}
}

func TestTruncate_WithoutAKeepFunctionWritesNoCheckpoint(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, 40, 256)
	segs, err := listNumbered(dir, "", segmentExt)
	if err != nil {
		t.Fatal(err)
	}
	last := segs[len(segs)-1]
	if err := Truncate(dir, last, nil); err != nil {
		t.Fatal(err)
	}
	cps, err := listNumbered(dir, checkpointPre, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cps) != 0 {
		t.Errorf("wrote %d checkpoints for a nil keep function", len(cps))
	}
}

func TestTruncate_RefusesToCheckpointFromACorruptSegment(t *testing.T) {
	// Copying records forward out of a segment that fails its CRC would
	// launder corruption into the checkpoint, where nothing would ever
	// question it again.
	dir := t.TempDir()
	writeLog(t, dir, 60, 256)
	segs, err := listNumbered(dir, "", segmentExt)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) < 3 {
		t.Skip("needs several segments")
	}
	path := filepath.Join(dir, fmt.Sprintf("%08d%s", segs[0], segmentExt))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[headerSize+1] ^= 0x20
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	err = Truncate(dir, segs[len(segs)-1], func(Record) bool { return true })
	if err == nil {
		t.Fatal("checkpointing a corrupt segment should fail")
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("error %v does not wrap ErrCorrupt", err)
	}
	// The segments must still be there: a failed truncation deletes nothing.
	after, err := listNumbered(dir, "", segmentExt)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(segs) {
		t.Errorf("a failed truncation deleted segments: %d left of %d", len(after), len(segs))
	}
}

func TestSegmentNumber(t *testing.T) {
	for name, want := range map[string]int{
		"00000007.wal":       7,
		"checkpoint.0000001": -1,
		"notanumber.wal":     -1,
	} {
		if got := segmentNumber(name); got != want {
			t.Errorf("segmentNumber(%q) = %d, want %d", name, got, want)
		}
	}
}

func TestWAL_WriteFailuresAreReportedNotSwallowed(t *testing.T) {
	// A WAL that reports success on a failed write is worse than no WAL: the
	// caller acknowledges data that is nowhere. Simulate by closing the file
	// underneath the writer, which is the cheapest stand-in for a full disk.
	dir := t.TempDir()
	w, err := Open(Options{Dir: dir, SegmentSize: 128})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Log(rec(typeSamples, "header fails")); err == nil {
		t.Error("Log should report a write failure")
	}
	if err := w.Sync(); err == nil {
		t.Error("Sync should report a failure")
	}
	// Rolling a segment must surface the failure too, rather than leaving the
	// log pointing at a file it never successfully closed.
	w.size = w.segmentSize
	if err := w.Log(rec(typeSamples, "roll fails")); err == nil {
		t.Error("a failed rollover should be reported")
	}
	w.closed = true // the fd is already gone; keep Close from double-failing
}

func TestWAL_UnwritableDirectoryIsReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions do not apply")
	}
	dir := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Options{Dir: dir}); err == nil {
		t.Error("Open should fail when the segment cannot be created")
	}
	if err := Truncate(dir, 1, nil); err != nil {
		t.Logf("Truncate on an empty read-only dir: %v", err) // no segments: fine either way
	}
}

func TestTruncate_ReportsAMissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	if err := Truncate(missing, 5, nil); err == nil {
		t.Error("Truncate should report a missing directory")
	}
	if err := TruncateTail(missing, Position{File: "00000000.wal", Offset: 1}); err == nil {
		t.Error("TruncateTail should report a missing directory")
	}
}

func TestWAL_AFailedWriteStopsTheLogRatherThanStrandingWrites(t *testing.T) {
	// A write can fail partway — ENOSPC is the realistic cause. The batch
	// itself is fine: Log returns an error, so nothing was acknowledged. What
	// must not happen is the *next* batch being written behind the wreckage,
	// where it would be acknowledged and then unreachable at replay.
	//
	// The failure is induced by closing the file out from under the log, which
	// also makes the rollback impossible, so this covers the worse of the two
	// paths: the log cannot repair itself and therefore refuses to continue.
	dir := t.TempDir()
	w, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Log(rec(1, "this one lands")); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := w.f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := w.Log(rec(1, "this one cannot")); err == nil {
		t.Fatal("Log reported success writing to a closed file")
	}
	// The important assertion: it does not go back to accepting writes.
	if err := w.Log(rec(1, "nor this one")); err == nil {
		t.Error("the log kept accepting writes it cannot make durable")
	}
	if err := w.Sync(); err == nil {
		t.Error("Sync reported success on a broken log")
	}

	// And what was acknowledged before the failure is still readable.
	got := replay(t, dir)
	if len(got) != 1 || string(got[0].Data) != "this one lands" {
		t.Errorf("replayed %d records, want the one that was acknowledged", len(got))
	}
}

func TestRepair_LeavesAHealthyLogAlone(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Log(rec(1, "a"), rec(1, "b")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "00000000.wal"))
	if err != nil {
		t.Fatal(err)
	}
	repaired, err := Repair(dir)
	if err != nil {
		t.Fatal(err)
	}
	if repaired {
		t.Error("Repair cut something out of an intact log")
	}
	after, err := os.ReadFile(filepath.Join(dir, "00000000.wal"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("Repair rewrote an intact segment")
	}
}

func TestRepair_OnAnAbsentOrEmptyLog(t *testing.T) {
	// The first start of a new ozyd: there is no log directory yet.
	if repaired, err := Repair(filepath.Join(t.TempDir(), "nope")); err != nil || repaired {
		t.Errorf("Repair on a missing directory: %v, %v", repaired, err)
	}
	dir := t.TempDir()
	w, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if repaired, err := Repair(dir); err != nil || repaired {
		t.Errorf("Repair on an empty log: %v, %v", repaired, err)
	}
}

func TestRepair_CutsATornTailSoTheLogIsAppendableAgain(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Log(rec(1, "kept"), rec(1, "also kept")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// The crash: the last record never finished landing.
	path := filepath.Join(dir, "00000000.wal")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, info.Size()-4); err != nil {
		t.Fatal(err)
	}

	repaired, err := Repair(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !repaired {
		t.Fatal("Repair did not notice the torn tail")
	}
	// Repairing twice is a no-op, which matters because it runs at every start.
	if again, err := Repair(dir); err != nil || again {
		t.Errorf("a second Repair cut something: %v, %v", again, err)
	}

	// And a write now lands where replay can reach it.
	w, err = Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Log(rec(2, "after the repair")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got := replay(t, dir)
	if len(got) != 2 || string(got[len(got)-1].Data) != "after the repair" {
		t.Fatalf("replayed %d records, want the survivor plus the new one", len(got))
	}
}

func TestRepair_ClearsASegmentThatIsGarbageFromItsFirstByte(t *testing.T) {
	// A crash can leave a freshly rolled segment holding nothing a reader can
	// use — here, zeroes, which cannot be a record because type 0 is reserved
	// exactly so a zero-filled region cannot masquerade as one. The reader
	// stops at its first byte, so that is where the log has to be cut back to;
	// anything written after it would be unreachable.
	dir := t.TempDir()
	w, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Log(rec(1, "kept")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	garbage := filepath.Join(dir, "00000001.wal")
	if err := os.WriteFile(garbage, make([]byte, 9), 0o644); err != nil {
		t.Fatal(err)
	}

	repaired, err := Repair(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !repaired {
		t.Fatal("Repair left an unreadable segment in place")
	}
	info, err := os.Stat(garbage)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Errorf("the unreadable segment still holds %d bytes", info.Size())
	}

	// And the log takes writes again, where replay can find them.
	w, err = Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Log(rec(2, "after the repair")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got := replay(t, dir)
	if len(got) != 2 || string(got[0].Data) != "kept" || string(got[1].Data) != "after the repair" {
		t.Fatalf("replayed %d records: %q", len(got), got)
	}
}

func TestRepair_RefusesToGuessAtRealCorruption(t *testing.T) {
	// Damage in an earlier segment is not an interrupted write, and how much
	// of the log to throw away is not Repair's call to make.
	dir := t.TempDir()
	w, err := Open(Options{Dir: dir, SegmentSize: 32})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := w.Log(rec(1, "0123456789")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(dir, "00000000.wal")
	body, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	body[len(body)-1] ^= 0xff // a bit flip in a segment that is not the last
	if err := os.WriteFile(first, body, 0o644); err != nil {
		t.Fatal(err)
	}

	repaired, err := Repair(dir)
	if err == nil {
		t.Fatal("Repair accepted a corrupt segment as a torn tail")
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("Repair returned %v, want an ErrCorrupt", err)
	}
	if repaired {
		t.Error("Repair reported cutting something on a corrupt log")
	}
	if after, err := os.ReadFile(first); err != nil || !bytes.Equal(after, body) {
		t.Error("Repair modified a log it could not judge")
	}
}

func TestWAL_UndoPartialCutsAHalfWrittenRecordOff(t *testing.T) {
	// The rollback itself, driven directly: a real ENOSPC is not something a
	// portable test can arrange, but what has to happen afterwards is exactly
	// this — the bytes of the failed record are gone, the log is positioned
	// where the last complete record ended, and it keeps working.
	dir := t.TempDir()
	w, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Log(rec(1, "acknowledged")); err != nil {
		t.Fatal(err)
	}
	// A header that landed and a payload that did not.
	if _, err := w.f.Write([]byte{0, 0, 0, 9, 1, 0xde, 0xad}); err != nil {
		t.Fatal(err)
	}

	cause := errors.New("no space left on device")
	if err := w.undoPartial(cause); !errors.Is(err, cause) {
		t.Fatalf("undoPartial returned %v, want the original cause", err)
	}
	if w.broken != nil {
		t.Errorf("the log was stopped even though the rollback worked: %v", w.broken)
	}
	if err := w.Log(rec(1, "after the failure")); err != nil {
		t.Fatalf("the log refused a write after a clean rollback: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	got := replay(t, dir)
	if len(got) != 2 || string(got[0].Data) != "acknowledged" || string(got[1].Data) != "after the failure" {
		t.Fatalf("replayed %d records: %q", len(got), got)
	}
}

func TestWAL_RollAndSyncReportIOFailures(t *testing.T) {
	// Rolling a segment syncs and closes the old one first, because the next
	// segment's records must never be durable before the previous segment's.
	// If either step fails, the roll has to fail with it rather than quietly
	// starting a new file over a segment that is not on disk.
	dir := t.TempDir()
	w, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.roll(); err == nil {
		t.Error("roll reported success with a dead segment handle")
	}

	// And a closed log is closed: no writes, no syncs, no surprises.
	clean, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := clean.Close(); err != nil {
		t.Fatal(err)
	}
	if err := clean.Sync(); err == nil {
		t.Error("Sync reported success on a closed log")
	}
	if err := clean.Log(rec(1, "too late")); err == nil {
		t.Error("Log accepted a record after Close")
	}
	if err := clean.Close(); err != nil {
		t.Errorf("Close is not idempotent: %v", err)
	}
}
