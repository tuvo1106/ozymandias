package spool

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"pgregory.net/rapid"

	"github.com/tuvo1106/ozymandias/internal/tsdb/wal"
)

var quiet = slog.New(slog.DiscardHandler)

// tb is what the helpers need of a test, so they work under *testing.T and *rapid.T.
type tb interface {
	Helper()
	Fatal(args ...any)
}

func open(t tb, dir string, segSize, maxBytes int64) *Spool {
	t.Helper()
	s, err := Open(Options{Dir: dir, SegmentSize: segSize, MaxBytes: maxBytes, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func body(i int) []byte { return bytes.Repeat([]byte{byte('a' + i%26)}, 100+i) }

func drain(t tb, s *Spool) (out [][]byte) {
	t.Helper()
	for {
		it, ok, _, _ := s.Peek()
		if !ok {
			return out
		}
		out = append(out, it.Body)
		s.Ack(it)
	}
}

func TestSpool_IsFIFOAcrossSegmentsAndReopens(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, 1024, 1<<20)
	for i := range 40 {
		if _, err := s.Put(i+1, body(i)); err != nil {
			t.Fatal(err)
		}
	}
	if st := s.Stats(); st.Segments < 3 || st.Payloads != 40 || st.Series != 40*41/2 {
		t.Fatalf("stats %+v", st)
	}
	// Deliver ten, restart, deliver the rest: the cursor must survive.
	for i := range 10 {
		it, ok, _, _ := s.Peek()
		if !ok || !bytes.Equal(it.Body, body(i)) || it.Series != i+1 {
			t.Fatalf("item %d: ok=%v series=%d", i, ok, it.Series)
		}
		s.Ack(it)
	}
	_ = s.Close()
	s = open(t, dir, 1024, 1<<20)
	defer func() { _ = s.Close() }()
	if st := s.Stats(); st.Payloads != 30 {
		t.Fatalf("after reopen: %+v, want 30 payloads (the cursor was not restored)", st)
	}
	got := drain(t, s)
	for i, b := range got {
		if !bytes.Equal(b, body(10+i)) {
			t.Fatalf("payload %d out of order", 10+i)
		}
	}
	if len(got) != 30 {
		t.Fatalf("%d payloads delivered, want 30", len(got))
	}
}

func TestSpool_DeliveredSegmentsAreDeleted(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, 1024, 1<<20)
	defer func() { _ = s.Close() }()
	for i := range 40 {
		if _, err := s.Put(1, body(i)); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := wal.Segments(dir)
	drain(t, s)
	after, _ := wal.Segments(dir)
	if len(after) != 1 || len(before) < 3 {
		t.Fatalf("segments %v -> %v: a drained spool should keep only the active one", before, after)
	}
}

func TestSpool_OverTheCapDropsTheOldestAndCountsIt(t *testing.T) {
	s := open(t, t.TempDir(), 1024, 8*1024)
	defer func() { _ = s.Close() }()
	var dropped int
	for i := range 200 {
		d, err := s.Put(1, body(i%50))
		if err != nil {
			t.Fatal(err)
		}
		dropped += d
	}
	st := s.Stats()
	if st.Bytes > 8*1024+1024 {
		t.Fatalf("%d bytes held against a cap of %d", st.Bytes, 8*1024)
	}
	if dropped == 0 || dropped+st.Series != 200 {
		t.Fatalf("dropped %d + held %d != 200 put: every payload must be delivered, held or counted", dropped, st.Series)
	}
	// What is left is the newest, in order.
	first, _, _, _ := s.Peek()
	if bytes.Equal(first.Body, body(0)) {
		t.Fatal("the oldest payload survived a drop-oldest")
	}
}

// A damaged record ends its segment; the payloads in later segments still arrive.
func TestSpool_ADamagedRecordSkipsTheRestOfItsSegmentOnly(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, 1024, 1<<20)
	for i := range 30 {
		if _, err := s.Put(1, body(i)); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close()
	segs, _ := wal.Segments(dir)
	path := filepath.Join(dir, wal.SegmentName(segs[0]))
	b, _ := os.ReadFile(path)
	b[len(b)/2] ^= 0xff // flip a byte inside the first segment
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir, 1024, 1<<20)
	defer func() { _ = s.Close() }()
	got := drain(t, s)
	if len(got) >= 30 || len(got) < 10 {
		t.Fatalf("%d payloads delivered of 30: want the damaged segment's tail lost and the other segments kept", len(got))
	}
	last := got[len(got)-1]
	if !bytes.Equal(last, body(29)) {
		t.Fatal("the newest payload did not arrive")
	}
}

func TestSpool_ATornTailIsCutAndAppendingContinues(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, 1<<20, 8<<20)
	for i := range 3 {
		_, _ = s.Put(1, body(i))
	}
	_ = s.Close()
	segs, _ := wal.Segments(dir)
	path := filepath.Join(dir, wal.SegmentName(segs[len(segs)-1]))
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.Write([]byte{0, 0, 1, 0, 1, 9, 9}) // half a header and no payload
	_ = f.Close()

	s = open(t, dir, 1<<20, 8<<20)
	defer func() { _ = s.Close() }()
	if _, err := s.Put(1, body(3)); err != nil {
		t.Fatal(err)
	}
	if got := drain(t, s); len(got) != 4 {
		t.Fatalf("%d payloads, want 4: an append behind a torn tail would be unreachable", len(got))
	}
}

func TestSpool_ABadCursorMeansStartAgainNotLoss(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, 1<<20, 8<<20)
	for i := range 5 {
		_, _ = s.Put(1, body(i))
	}
	it, _, _, _ := s.Peek()
	s.Ack(it)
	_ = s.Close()
	for _, junk := range []string{"garbage", "1 2 3\n", "0 99999999 1\n"} {
		_ = os.WriteFile(filepath.Join(dir, headFile), []byte(junk), 0o644)
		s = open(t, dir, 1<<20, 8<<20)
		if got := len(drain(t, s)); got < 4 {
			t.Fatalf("cursor %q: %d payloads, want at least the 4 undelivered", junk, got)
		}
		_ = s.Close()
		s = open(t, dir, 1<<20, 8<<20)
		for i := range 5 {
			_, _ = s.Put(1, body(i))
		}
		it, _, _, _ := s.Peek()
		s.Ack(it)
		_ = s.Close()
	}
}

func TestOpen_RefusesACapSmallerThanFourSegments(t *testing.T) {
	if _, err := Open(Options{Dir: t.TempDir(), SegmentSize: 1024, MaxBytes: 2048}); err == nil {
		t.Fatal("accepted a cap that would drop most of the spool per segment")
	}
}

// Model test: any interleaving of put, deliver and restart delivers exactly the
// put sequence, in order, once (the cap is out of reach here).
func TestSpool_PutDeliverRestartDeliversEverythingOnceInOrder(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		dir := t.TempDir()
		s := open(rt, dir, 700, 1<<20)
		defer func() { _ = s.Close() }()
		var put, got []int
		next := 0
		for range rapid.IntRange(1, 60).Draw(rt, "ops") {
			switch rapid.IntRange(0, 3).Draw(rt, "op") {
			case 0, 1:
				if _, err := s.Put(next+1, []byte(fmt.Sprintf("payload-%d", next))); err != nil {
					rt.Fatal(err)
				}
				put = append(put, next)
				next++
			case 2:
				if it, ok, _, _ := s.Peek(); ok {
					var n int
					_, _ = fmt.Sscanf(string(it.Body), "payload-%d", &n)
					got = append(got, n)
					s.Ack(it)
				}
			case 3:
				_ = s.Close()
				s = open(rt, dir, 700, 1<<20)
			}
		}
		for {
			it, ok, _, _ := s.Peek()
			if !ok {
				break
			}
			var n int
			_, _ = fmt.Sscanf(string(it.Body), "payload-%d", &n)
			got = append(got, n)
			s.Ack(it)
		}
		if fmt.Sprint(put) != fmt.Sprint(got) {
			rt.Fatalf("put %v\ngot %v", put, got)
		}
	})
}

// L4: whatever bytes are in a segment, opening and draining must not panic or hang.
func FuzzSpool_OpenAndDrainArbitrarySegmentBytes(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 4, 1, 0, 0, 0, 0, 0, 0, 0, 1, 'x'})
	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, wal.SegmentName(0)), data, 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(dir, headFile), data, 0o644)
		s, err := Open(Options{Dir: dir, SegmentSize: 1024, MaxBytes: 1 << 20, Logger: quiet})
		if err != nil {
			return
		}
		defer func() { _ = s.Close() }()
		for range 10_000 {
			it, ok, _, _ := s.Peek()
			if !ok {
				return
			}
			s.Ack(it)
		}
		t.Fatal("drain did not end")
	})
}

// A payload is acknowledged to the caller only after its fsync: a Put whose
// sync fails must say so, and every Put must sync.
func TestSpool_PutSyncsAndReportsAFailedSync(t *testing.T) {
	calls := 0
	var failNext bool
	s, err := Open(Options{Dir: t.TempDir(), SegmentSize: 1 << 20, MaxBytes: 8 << 20, Logger: quiet,
		syncFn: func(w *wal.WAL) error {
			calls++
			if failNext {
				return errors.New("disk on fire")
			}
			return w.Sync()
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Put(1, body(0)); err != nil || calls != 1 {
		t.Fatalf("err=%v, syncs=%d: a Put must fsync once", err, calls)
	}
	failNext = true
	if _, err := s.Put(1, body(1)); err == nil {
		t.Fatal("a failed fsync was reported as success")
	}
}

// Damage that appears while the spool is running (not found by Open's scan).
func TestSpool_DamageFoundWhileRunningSkipsTheRestOfItsSegment(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, 1024, 1<<20)
	defer func() { _ = s.Close() }()
	for i := range 30 {
		if _, err := s.Put(1, body(i)); err != nil {
			t.Fatal(err)
		}
	}
	segs, _ := wal.Segments(dir)
	path := filepath.Join(dir, wal.SegmentName(segs[0]))
	b, _ := os.ReadFile(path)
	b[len(b)/2] ^= 0xff
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	got := drain(t, s)
	if len(got) >= 30 || len(got) < 10 || !bytes.Equal(got[len(got)-1], body(29)) {
		t.Fatalf("%d payloads, last ok=%v: want the damaged segment's tail lost and the later segments kept",
			len(got), len(got) > 0 && bytes.Equal(got[len(got)-1], body(29)))
	}
	if st := s.Stats(); st.Payloads != 0 || st.Series != 0 {
		t.Fatalf("a drained spool reports %+v", st)
	}
}

func TestOpen_RejectsAMissingOrUnusableDir(t *testing.T) {
	if _, err := Open(Options{}); err == nil {
		t.Error("an empty Dir was accepted")
	}
	if _, err := Open(Options{Dir: "/dev/null/spool"}); err == nil {
		t.Error("a Dir that cannot be created was accepted")
	}
}

func TestSpool_ClosedPutFailsAndCloseIsIdempotent(t *testing.T) {
	s := open(t, t.TempDir(), 1<<20, 8<<20)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if _, err := s.Put(1, body(0)); err == nil {
		t.Fatal("Put on a closed spool succeeded")
	}
}

func TestSpool_APayloadOverTheRecordLimitIsRefused(t *testing.T) {
	s := open(t, t.TempDir(), 1<<20, 8<<20)
	defer func() { _ = s.Close() }()
	if _, err := s.Put(1, make([]byte, wal.MaxRecordSize+1)); err == nil {
		t.Fatal("an oversize payload was accepted")
	}
	if st := s.Stats(); st.Payloads != 0 {
		t.Fatalf("a refused Put left %+v behind", st)
	}
}

// A record that passes its CRC but is not ours (wrong type) is a different
// format, not a payload: skipped like damage, not delivered as garbage.
func TestSpool_ARecordOfTheWrongTypeIsSkipped(t *testing.T) {
	dir := t.TempDir()
	w, err := wal.Open(wal.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Log(wal.Record{Type: 9, Data: []byte("not a spool record")}); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	s := open(t, dir, 1<<20, 8<<20)
	defer func() { _ = s.Close() }()
	if _, err := s.Put(1, body(1)); err != nil {
		t.Fatal(err)
	}
	got := drain(t, s)
	if len(got) != 1 || !bytes.Equal(got[0], body(1)) {
		t.Fatalf("%d payloads delivered, want only the real one", len(got))
	}
}

// An Ack for a payload the cap already dropped must not move the cursor or the counts.
func TestSpool_AckOfADroppedPayloadChangesNothing(t *testing.T) {
	s := open(t, t.TempDir(), 1024, 8*1024)
	defer func() { _ = s.Close() }()
	if _, err := s.Put(1, body(0)); err != nil {
		t.Fatal(err)
	}
	first, _, _, _ := s.Peek()
	for i := range 200 { // roll over and drop the segment the item came from
		if _, err := s.Put(1, body(i%50)); err != nil {
			t.Fatal(err)
		}
	}
	before := s.Stats()
	s.Ack(first)
	if after := s.Stats(); after != before {
		t.Fatalf("a stale Ack changed the spool: %+v -> %+v", before, after)
	}
}

func TestSpool_ACursorThatCannotBeWrittenOnlyCostsRepeats(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, 1<<20, 8<<20)
	defer func() { _ = s.Close() }()
	if _, err := s.Put(1, body(0)); err != nil {
		t.Fatal(err)
	}
	// A directory where the cursor's temp file goes makes the write fail.
	if err := os.Mkdir(filepath.Join(dir, headFile+".tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	it, _, _, _ := s.Peek()
	s.Ack(it) // must not panic or lose the in-memory state
	if st := s.Stats(); st.Payloads != 0 {
		t.Fatalf("%+v: the delivery was not recorded in memory", st)
	}
}

// A failed fsync stops the spool (fsyncgate): the record that was written but
// not synced is not indexed, nothing more is accepted, and the counts stay
// true. Before, the next Put raised the good end over it and Peek delivered a
// payload the caller had been told was dropped.
func TestSpool_AFailedFsyncStopsTheSpoolAndKeepsTheCountsTrue(t *testing.T) {
	fail := false
	s, err := Open(Options{Dir: t.TempDir(), SegmentSize: 1 << 20, MaxBytes: 8 << 20, Logger: quiet,
		syncFn: func(w *wal.WAL) error {
			if fail {
				return fmt.Errorf("%w: injected", wal.ErrSync)
			}
			return w.Sync()
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Put(1, body(0)); err != nil {
		t.Fatal(err)
	}
	fail = true
	if _, err := s.Put(1, body(1)); err == nil {
		t.Fatal("a failed fsync was reported as success")
	}
	fail = false
	if _, err := s.Put(1, body(2)); err == nil {
		t.Fatal("the spool kept accepting writes after a failed fsync")
	}
	if !s.Broken() {
		t.Fatal("Broken() = false after a failed fsync")
	}
	got := drain(t, s)
	if len(got) != 1 || !bytes.Equal(got[0], body(0)) {
		t.Fatalf("%d payloads delivered, want only the one that was synced", len(got))
	}
	if st := s.Stats(); st.Payloads != 0 || st.Series != 0 {
		t.Fatalf("counts went wrong: %+v", st)
	}
}

// Damage in the segment being appended to must be skipped for good, not
// re-found at the same record after every later Put.
func TestSpool_DamageInTheActiveSegmentIsSkippedAndAppendingContinues(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, 1<<20, 8<<20) // one segment holds everything
	defer func() { _ = s.Close() }()
	for i := range 6 {
		if _, err := s.Put(1, body(i)); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 { // deliver two, then damage the third
		it, _, _, _ := s.Peek()
		s.Ack(it)
	}
	segs, _ := wal.Segments(dir)
	path := filepath.Join(dir, wal.SegmentName(segs[0]))
	b, _ := os.ReadFile(path)
	third := 2 * (len(b) / 6)
	b[third+20] ^= 0xff
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	_, ok, skipped, _ := s.Peek()
	if ok || skipped != 4 {
		t.Fatalf("ok=%v skipped=%d: want the four undelivered series counted and no item", ok, skipped)
	}
	for i := 10; i < 13; i++ {
		if _, err := s.Put(1, body(i)); err != nil {
			t.Fatal(err)
		}
	}
	got := drain(t, s)
	if len(got) != 3 || !bytes.Equal(got[0], body(10)) {
		t.Fatalf("%d payloads after the damage, want the 3 written after it", len(got))
	}
	if st := s.Stats(); st.Payloads != 0 || st.Series != 0 || st.Bytes < 0 {
		t.Fatalf("stats %+v", st)
	}
}

// A read that fails for a reason that is not damage (here: the segment file is
// gone for a moment) must leave the spool as it was.
func TestSpool_ATransientReadErrorDropsNothing(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, 1024, 1<<20)
	defer func() { _ = s.Close() }()
	for i := range 20 {
		if _, err := s.Put(1, body(i)); err != nil {
			t.Fatal(err)
		}
	}
	segs, _ := wal.Segments(dir)
	path := filepath.Join(dir, wal.SegmentName(segs[0]))
	hidden := path + ".hidden"
	if err := os.Rename(path, hidden); err != nil {
		t.Fatal(err)
	}
	before := s.Stats()
	if _, ok, skipped, _ := s.Peek(); ok || skipped != 0 {
		t.Fatalf("ok=%v skipped=%d with the file missing", ok, skipped)
	}
	if after := s.Stats(); after != before {
		t.Fatalf("a failed read changed the spool: %+v -> %+v", before, after)
	}
	if err := os.Rename(hidden, path); err != nil {
		t.Fatal(err)
	}
	if got := drain(t, s); len(got) != 20 {
		t.Fatalf("%d payloads once the file was back, want all 20", len(got))
	}
}

func TestSpool_APayloadLargerThanASegmentIsRefused(t *testing.T) {
	s := open(t, t.TempDir(), 1024, 1<<20)
	defer func() { _ = s.Close() }()
	if _, err := s.Put(1, make([]byte, 2048)); err == nil {
		t.Fatal("a payload bigger than a segment was accepted: the cap could never evict it")
	}
}

// The cursor is written every few deliveries and on Close; a kill between
// writes repeats at most those few, and never loses one.
func TestSpool_AKillBetweenCursorWritesRepeatsAtMostAFewPayloads(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, 1<<20, 8<<20)
	for i := range 30 {
		if _, err := s.Put(1, body(i)); err != nil {
			t.Fatal(err)
		}
	}
	for range 20 {
		it, _, _, _ := s.Peek()
		s.Ack(it)
	}
	// No Close: a kill. A second spool reads the directory as it is.
	s2 := open(t, dir, 1<<20, 8<<20)
	defer func() { _ = s2.Close(); _ = s.Close() }()
	got := drain(t, s2)
	if len(got) < 10 || len(got) > 10+headEvery {
		t.Fatalf("%d payloads after a kill with 10 undelivered: want 10 to %d", len(got), 10+headEvery)
	}
	if !bytes.Equal(got[len(got)-1], body(29)) {
		t.Fatal("the newest payload was lost")
	}
}

// A log that has stopped itself (a rollover's fsync failed, a partial write that
// could not be cut back) is a stopped spool: Broken() and spool_enabled say so.
func TestSpool_AStoppedLogStopsTheSpool(t *testing.T) {
	stop := false
	s, err := Open(Options{Dir: t.TempDir(), SegmentSize: 1 << 20, MaxBytes: 8 << 20, Logger: quiet,
		logFn: func(w *wal.WAL, r wal.Record) error {
			if stop {
				return fmt.Errorf("%w: injected", wal.ErrBroken)
			}
			return w.Log(r)
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Put(1, body(0)); err != nil || s.Broken() {
		t.Fatalf("err=%v broken=%v", err, s.Broken())
	}
	stop = true
	if _, err := s.Put(1, body(1)); err == nil || !s.Broken() {
		t.Fatalf("err=%v broken=%v: a stopped log must stop the spool", err, s.Broken())
	}
}

// The segment must hold the record with its framing, not just the body.
func TestSpool_TheSizeGuardCountsTheFraming(t *testing.T) {
	s := open(t, t.TempDir(), 1024, 1<<20)
	defer func() { _ = s.Close() }()
	fits := 1024 - wal.RecordOverhead - headerLen
	if _, err := s.Put(1, make([]byte, fits)); err != nil {
		t.Fatalf("a payload that exactly fills a segment was refused: %v", err)
	}
	if _, err := s.Put(1, make([]byte, fits+1)); err == nil {
		t.Fatal("a payload one byte too big for its segment was accepted")
	}
}

// Holds is how the forwarder learns that the cap dropped what it was about to send.
func TestSpool_HoldsIsFalseOnceTheCapDropsThePayload(t *testing.T) {
	s := open(t, t.TempDir(), 1024, 8*1024)
	defer func() { _ = s.Close() }()
	if _, err := s.Put(1, body(0)); err != nil {
		t.Fatal(err)
	}
	it, _, _, _ := s.Peek()
	if !s.Holds(it) {
		t.Fatal("a freshly peeked item is not held")
	}
	for i := range 200 {
		if _, err := s.Put(1, body(i%50)); err != nil {
			t.Fatal(err)
		}
	}
	if s.Holds(it) {
		t.Fatal("the payload's segment was dropped, but Holds still says yes: it would be sent and counted lost")
	}
}

// BenchmarkOpen measures the startup scan: every segment is read and CRC-checked
// to rebuild the index. Reported as MB/s of spool, so a 512 MiB spool's start-up
// is 512 / that.
func BenchmarkOpen(b *testing.B) {
	dir := b.TempDir()
	s, err := Open(Options{Dir: dir, SegmentSize: 8 << 20, MaxBytes: 1 << 30, Logger: quiet})
	if err != nil {
		b.Fatal(err)
	}
	payload := make([]byte, 100<<10)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	for range 1300 { // about 130 MB
		if _, err := s.Put(100, payload); err != nil {
			b.Fatal(err)
		}
	}
	size := s.Stats().Bytes
	_ = s.Close()
	b.SetBytes(size)
	b.ResetTimer()
	for range b.N {
		s2, err := Open(Options{Dir: dir, SegmentSize: 8 << 20, MaxBytes: 1 << 30, Logger: quiet})
		if err != nil {
			b.Fatal(err)
		}
		_ = s2.Close()
	}
}
