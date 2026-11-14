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
		it, ok, _ := s.Peek()
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
		it, ok, _ := s.Peek()
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
	first, _, _ := s.Peek()
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
	it, _, _ := s.Peek()
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
		it, _, _ := s.Peek()
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
				if it, ok, _ := s.Peek(); ok {
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
			it, ok, _ := s.Peek()
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
			it, ok, _ := s.Peek()
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
	first, _, _ := s.Peek()
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
	it, _, _ := s.Peek()
	s.Ack(it) // must not panic or lose the in-memory state
	if st := s.Stats(); st.Payloads != 0 {
		t.Fatalf("%+v: the delivery was not recorded in memory", st)
	}
}
