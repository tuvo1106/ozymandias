package queue

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/internal/tsdb/wal"
)

// open opens a topic that syncs immediately, so tests do not wait on a timer
// unless they mean to.
func open(t *testing.T, dir string, mod ...func(*Options)) *Topic {
	t.Helper()
	opts := Options{Dir: dir, SyncInterval: -1}
	for _, m := range mod {
		m(&opts)
	}
	tp, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tp.Close() })
	return tp
}

func produce(t *testing.T, tp *Topic, payloads ...string) []uint64 {
	t.Helper()
	var offs []uint64
	for _, p := range payloads {
		off, err := tp.Produce(context.Background(), []byte(p))
		if err != nil {
			t.Fatalf("Produce(%q): %v", p, err)
		}
		offs = append(offs, off)
	}
	return offs
}

func poll(t *testing.T, c *Consumer, max int) []Record {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	recs, err := c.Poll(ctx, max)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	return recs
}

func payloads(recs []Record) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = string(r.Payload)
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTopic_ProducePollCommitInOrder(t *testing.T) {
	tp := open(t, t.TempDir())
	offs := produce(t, tp, "a", "b", "c")
	if offs[0] != 0 || offs[1] != 1 || offs[2] != 2 {
		t.Fatalf("offsets = %v, want [0 1 2]", offs)
	}
	c, err := tp.Consumer("tsdb")
	if err != nil {
		t.Fatal(err)
	}
	got := poll(t, c, 2)
	if !eq(payloads(got), []string{"a", "b"}) || got[0].Offset != 0 || got[1].Offset != 1 {
		t.Fatalf("first poll = %+v", got)
	}
	if err := c.Commit(1); err != nil {
		t.Fatal(err)
	}
	got = poll(t, c, 10)
	if !eq(payloads(got), []string{"c"}) {
		t.Fatalf("second poll = %q", payloads(got))
	}
	if s := tp.Stats(); s.Lag["tsdb"] != 1 || s.Next != 3 || s.Durable != 3 {
		t.Errorf("stats = %+v, want lag 1, next 3, durable 3", s)
	}
}

func TestTopic_RecordsCarryTheProduceTime(t *testing.T) {
	clk := testutil.NewFakeClock(time.Unix(1_700_000_000, 42))
	tp := open(t, t.TempDir(), func(o *Options) { o.Clock = clk })
	produce(t, tp, "x")
	c, _ := tp.Consumer("c")
	if got := poll(t, c, 1)[0].Time; !got.Equal(clk.Now()) {
		t.Errorf("record time = %v, want %v", got, clk.Now())
	}
}

func TestPoll_WaitsForARecordAndHonoursCancel(t *testing.T) {
	tp := open(t, t.TempDir())
	c, _ := tp.Consumer("c")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Poll(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("Poll on a cancelled ctx with nothing to read = %v, want Canceled", err)
	}

	got := make(chan []Record, 1)
	go func() {
		recs, _ := c.Poll(context.Background(), 5)
		got <- recs
	}()
	time.Sleep(20 * time.Millisecond) // let it block
	produce(t, tp, "late")
	select {
	case recs := <-got:
		if !eq(payloads(recs), []string{"late"}) {
			t.Fatalf("woken poll = %q", payloads(recs))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Poll was not woken by a produce")
	}

	if _, err := c.Poll(context.Background(), 0); err == nil {
		t.Error("Poll(max=0) = nil error")
	}
}

func TestPoll_ClosedTopicWakesAndFailsPollers(t *testing.T) {
	tp := open(t, t.TempDir())
	c, _ := tp.Consumer("c")
	errc := make(chan error, 1)
	go func() {
		_, err := c.Poll(context.Background(), 1)
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if err := tp.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("Poll after Close = %v, want ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not wake a blocked Poll")
	}
	if _, err := tp.Produce(context.Background(), []byte("x")); !errors.Is(err, ErrClosed) {
		t.Errorf("Produce after Close = %v", err)
	}
	if err := c.Commit(0); err == nil {
		t.Error("Commit after Close (nothing read) = nil")
	}
	if _, err := tp.Consumer("d"); !errors.Is(err, ErrClosed) {
		t.Errorf("Consumer after Close = %v", err)
	}
	if err := tp.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
}

func TestCommit_RulesAndIdempotence(t *testing.T) {
	tp := open(t, t.TempDir())
	produce(t, tp, "a", "b")
	c, _ := tp.Consumer("c")
	if err := c.Commit(0); err == nil {
		t.Fatal("Commit before any Poll = nil, want an error (it would skip unread records)")
	}
	poll(t, c, 2)
	if err := c.Commit(2); err == nil {
		t.Fatal("Commit past the cursor = nil")
	}
	if err := c.Commit(1); err != nil {
		t.Fatal(err)
	}
	if err := c.Commit(0); err != nil {
		t.Fatalf("re-committing an older offset = %v, want a no-op", err)
	}
	if s := tp.Stats(); s.Lag["c"] != 0 {
		t.Errorf("lag after commit = %d", s.Lag["c"])
	}
	b, err := os.ReadFile(filepath.Join(tp.dir, consumersDir, "c"+offsetExt))
	if err != nil {
		t.Fatal(err)
	}
	if next, err := decodeOffset(b); err != nil || next != 2 {
		t.Errorf("offset file = %d, %v; want 2 (an older commit must not move it back)", next, err)
	}
}

func TestConsumer_NameRulesAndSingleRegistration(t *testing.T) {
	tp := open(t, t.TempDir())
	for _, bad := range []string{"", "Upper", "-lead", "a/b", "a.b", "../x"} {
		if _, err := tp.Consumer(bad); err == nil {
			t.Errorf("Consumer(%q) = nil error", bad)
		}
	}
	c, err := tp.Consumer("logs-1")
	if err != nil {
		t.Fatal(err)
	}
	if c.Name() != "logs-1" {
		t.Errorf("Name() = %q", c.Name())
	}
	if _, err := tp.Consumer("logs-1"); err == nil {
		t.Error("registering a name twice = nil error")
	}
}

// TestRestart_ResumesAtTheCommittedOffset is at-least-once in one test: what
// was committed is not delivered again, what was read but not committed is.
func TestRestart_ResumesAtTheCommittedOffset(t *testing.T) {
	dir := t.TempDir()
	tp := open(t, dir, func(o *Options) { o.SegmentSize = 64 })
	produce(t, tp, "r0", "r1", "r2", "r3", "r4", "r5")
	c, _ := tp.Consumer("c")
	poll(t, c, 4)
	if err := c.Commit(2); err != nil { // r3 read, not committed
		t.Fatal(err)
	}
	if err := tp.Close(); err != nil {
		t.Fatal(err)
	}

	tp = open(t, dir, func(o *Options) { o.SegmentSize = 64 })
	c, err := tp.Consumer("c")
	if err != nil {
		t.Fatal(err)
	}
	got := poll(t, c, 10)
	if !eq(payloads(got), []string{"r3", "r4", "r5"}) {
		t.Fatalf("after restart = %q, want [r3 r4 r5]", payloads(got))
	}
	if off := produce(t, tp, "r6")[0]; off != 6 {
		t.Errorf("first offset after restart = %d, want 6", off)
	}
}

func TestSegments_RollAndRetentionFreesSpace(t *testing.T) {
	dir := t.TempDir()
	// Each record is 9 + 16 + 4 = 29 bytes; 64-byte segments hold two.
	tp := open(t, dir, func(o *Options) { o.SegmentSize = 64 })
	c, _ := tp.Consumer("c")
	produce(t, tp, "aaaa", "bbbb", "cccc", "dddd", "eeee")
	s := tp.Stats()
	if s.Segments != 3 || s.Bytes != 5*29 {
		t.Fatalf("before retention: %d segments, %d bytes; want 3, 145", s.Segments, s.Bytes)
	}
	poll(t, c, 3)
	if err := c.Commit(2); err != nil { // aaaa..cccc done; segment 0 is wholly below
		t.Fatal(err)
	}
	s = tp.Stats()
	if s.Segments != 2 || s.Bytes != 3*29 || s.Oldest != 2 {
		t.Fatalf("after commit(2): %+v; want 2 segments, 87 bytes, oldest 2", s)
	}
	if _, err := os.Stat(filepath.Join(dir, wal.SegmentName(0))); !os.IsNotExist(err) {
		t.Errorf("segment 0 still on disk: %v", err)
	}
	// The rest still reads.
	if got := poll(t, c, 10); !eq(payloads(got), []string{"dddd", "eeee"}) {
		t.Fatalf("after retention = %q", payloads(got))
	}
	// The newest segment is never deleted, even when fully consumed.
	if err := c.Commit(4); err != nil {
		t.Fatal(err)
	}
	if s := tp.Stats(); s.Segments != 1 {
		t.Errorf("after committing everything: %d segments, want the newest kept", s.Segments)
	}
}

func TestRetention_WaitsForTheSlowestConsumer(t *testing.T) {
	tp := open(t, t.TempDir(), func(o *Options) { o.SegmentSize = 64 })
	fast, _ := tp.Consumer("fast")
	slow, _ := tp.Consumer("slow")
	produce(t, tp, "a", "b", "c", "d", "e")
	poll(t, fast, 10)
	if err := fast.Commit(4); err != nil {
		t.Fatal(err)
	}
	if s := tp.Stats(); s.Segments != 3 {
		t.Fatalf("one consumer done, the other not: %d segments, want all 3 kept", s.Segments)
	}
	if got := poll(t, slow, 10); len(got) != 5 {
		t.Fatalf("slow consumer read %d records, want 5", len(got))
	}
}

// TestRetention_ConsumerParkedOnARetiredSegment: a consumer that has read to
// the very end of a segment, whose next read would open that segment again,
// must move on when another consumer's commit deletes it.
func TestRetention_ConsumerParkedOnARetiredSegment(t *testing.T) {
	tp := open(t, t.TempDir(), func(o *Options) { o.SegmentSize = 64 })
	a, _ := tp.Consumer("a")
	b, _ := tp.Consumer("b")
	produce(t, tp, "s0r0", "s0r1")
	poll(t, a, 2)
	poll(t, b, 2) // b's file position is now the end of segment 0
	if err := b.Commit(1); err != nil {
		t.Fatal(err)
	}
	produce(t, tp, "s1r0", "s1r1", "s2r0")
	if err := a.Commit(1); err != nil { // both past segment 0: it goes
		t.Fatal(err)
	}
	if tp.retired(0) == false {
		t.Fatal("segment 0 was not retired; the test is not testing anything")
	}
	if got := poll(t, b, 10); !eq(payloads(got), []string{"s1r0", "s1r1", "s2r0"}) {
		t.Fatalf("parked consumer after retention = %q", payloads(got))
	}
}

func TestProduce_FullTopicRefusesUntilRetentionFreesSpace(t *testing.T) {
	tp := open(t, t.TempDir(), func(o *Options) { o.SegmentSize = 64; o.MaxBytes = 3 * 29 })
	c, _ := tp.Consumer("c")
	produce(t, tp, "aaaa", "bbbb", "cccc")
	if _, err := tp.Produce(context.Background(), []byte("dddd")); !errors.Is(err, ErrFull) {
		t.Fatalf("Produce past the cap = %v, want ErrFull", err)
	}
	if s := tp.Stats(); s.Next != 3 {
		t.Fatalf("a refused record used up an offset: next = %d", s.Next)
	}
	poll(t, c, 2)
	if err := c.Commit(1); err != nil {
		t.Fatal(err)
	}
	if off, err := tp.Produce(context.Background(), []byte("dddd")); err != nil || off != 3 {
		t.Fatalf("Produce after retention = %d, %v; want 3, nil", off, err)
	}
}

func TestProduce_RejectsAnOversizedPayload(t *testing.T) {
	tp := open(t, t.TempDir())
	if _, err := tp.Produce(context.Background(), make([]byte, MaxPayload+1)); err == nil {
		t.Fatal("oversized payload accepted")
	}
}

// TestProduce_DoesNotReturnBeforeTheFsync is the milestone's central promise
// for the queue: an ack never precedes durability. The sync is held; Produce
// must still be waiting, and the record must be invisible to consumers.
func TestProduce_DoesNotReturnBeforeTheFsync(t *testing.T) {
	gate := make(chan struct{})
	tp := open(t, t.TempDir(), func(o *Options) {
		o.syncFn = func(w *wal.WAL) error {
			<-gate
			return w.Sync()
		}
	})
	c, _ := tp.Consumer("c")
	done := make(chan error, 1)
	go func() {
		_, err := tp.Produce(context.Background(), []byte("x"))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("Produce returned (%v) before its fsync", err)
	case <-time.After(50 * time.Millisecond):
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if recs, err := c.Poll(ctx, 1); err == nil {
		t.Fatalf("a consumer saw an unsynced record: %q", payloads(recs))
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := poll(t, c, 1); !eq(payloads(got), []string{"x"}) {
		t.Fatalf("after the sync = %q", payloads(got))
	}
}

// TestProduce_AFailedFsyncFailsTheBatchAndStopsTheTopic: no 202 without
// durability, and no retrying a failed fsync (see syncOnce).
func TestProduce_AFailedFsyncFailsTheBatchAndStopsTheTopic(t *testing.T) {
	boom := errors.New("injected EIO")
	tp := open(t, t.TempDir(), func(o *Options) {
		o.syncFn = func(*wal.WAL) error { return boom }
	})
	if _, err := tp.Produce(context.Background(), []byte("x")); !errors.Is(err, boom) {
		t.Fatalf("Produce with a failing fsync = %v, want the fsync error", err)
	}
	if _, err := tp.Produce(context.Background(), []byte("y")); !errors.Is(err, boom) {
		t.Fatalf("Produce after a failed fsync = %v, want the topic to stay stopped", err)
	}
	if s := tp.Stats(); s.Broken == nil || s.Durable != 0 {
		t.Errorf("stats after a failed fsync = %+v", s)
	}
}

// TestProduce_WaitersDuringAFailedSyncAreReleased: a producer that appended
// while the failing fsync was in flight belongs to the next generation, which
// will never be synced. It must be failed, not left waiting forever.
func TestProduce_WaitersDuringAFailedSyncAreReleased(t *testing.T) {
	boom := errors.New("injected EIO")
	entered, release := make(chan struct{}), make(chan struct{})
	tp := open(t, t.TempDir(), func(o *Options) {
		o.syncFn = func(*wal.WAL) error {
			close(entered)
			<-release
			return boom
		}
	})
	first := make(chan error, 1)
	go func() { _, err := tp.Produce(context.Background(), []byte("a")); first <- err }()
	<-entered
	second := make(chan error, 1)
	go func() { _, err := tp.Produce(context.Background(), []byte("b")); second <- err }()
	testutil.Eventually(t, 5*time.Second, func() bool {
		tp.mu.Lock()
		defer tp.mu.Unlock()
		return tp.next == 2
	}, "second producer never appended")
	close(release)
	for name, ch := range map[string]chan error{"first": first, "second": second} {
		select {
		case err := <-ch:
			if !errors.Is(err, boom) {
				t.Errorf("%s producer = %v, want the fsync error", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s producer was never released", name)
		}
	}
}

func TestProduce_CancelledContextReturnsButTheRecordStillLands(t *testing.T) {
	gate := make(chan struct{})
	tp := open(t, t.TempDir(), func(o *Options) {
		o.syncFn = func(w *wal.WAL) error { <-gate; return w.Sync() }
	})
	c, _ := tp.Consumer("c")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := tp.Produce(ctx, []byte("x")); done <- err }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Produce = %v", err)
	}
	close(gate)
	if got := poll(t, c, 1); !eq(payloads(got), []string{"x"}) {
		t.Fatalf("the cancelled record = %q, want it delivered (at-least-once)", payloads(got))
	}
}

// TestGroupCommit_OneFsyncReleasesManyProducers: with a window, concurrent
// producers share a sync. Counted with a fake clock, so the window is exact.
func TestGroupCommit_OneFsyncReleasesManyProducers(t *testing.T) {
	clk := testutil.NewFakeClock(time.Unix(0, 0))
	var mu sync.Mutex
	syncs := 0
	tp := open(t, t.TempDir(), func(o *Options) {
		o.Clock = clk
		o.SyncInterval = 5 * time.Millisecond
		o.syncFn = func(w *wal.WAL) error {
			mu.Lock()
			syncs++
			mu.Unlock()
			return w.Sync()
		}
	})
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := tp.Produce(context.Background(), []byte(fmt.Sprint(i)))
			errs <- err
		}()
	}
	testutil.Eventually(t, 5*time.Second, func() bool {
		tp.mu.Lock()
		defer tp.mu.Unlock()
		return tp.next == n && clk.Waiters() == 1
	}, "producers never all appended under one armed timer")
	mu.Lock()
	before := syncs
	mu.Unlock()
	if before != 0 {
		t.Fatalf("%d syncs ran before the window closed", before)
	}
	clk.Advance(5 * time.Millisecond)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if syncs != 1 {
		t.Errorf("%d producers took %d fsyncs, want 1", n, syncs)
	}
}

// TestGroupCommit_SyncBytesEndsTheWindowEarly: the byte threshold does not
// wait for the timer.
func TestGroupCommit_SyncBytesEndsTheWindowEarly(t *testing.T) {
	clk := testutil.NewFakeClock(time.Unix(0, 0))
	tp := open(t, t.TempDir(), func(o *Options) {
		o.Clock = clk
		o.SyncInterval = time.Hour
		o.SyncBytes = 100
	})
	done := make(chan error, 1)
	go func() { _, err := tp.Produce(context.Background(), []byte("small")); done <- err }()
	testutil.Eventually(t, 5*time.Second, func() bool { return clk.Waiters() == 1 },
		"the window timer was never armed")
	if _, err := tp.Produce(context.Background(), make([]byte, 200)); err != nil {
		t.Fatal(err) // returns without the clock moving: bytes ended the window
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestClose_SyncsAndReleasesAPendingProducer: shutdown is a commit point.
func TestClose_SyncsAndReleasesAPendingProducer(t *testing.T) {
	clk := testutil.NewFakeClock(time.Unix(0, 0))
	dir := t.TempDir()
	tp, err := Open(Options{Dir: dir, Clock: clk, SyncInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := tp.Produce(context.Background(), []byte("x")); done <- err }()
	testutil.Eventually(t, 5*time.Second, func() bool { return clk.Waiters() == 1 }, "never armed")
	if err := tp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("producer pending at Close = %v, want its record synced", err)
	}
	tp = open(t, dir)
	c, _ := tp.Consumer("c")
	if got := poll(t, c, 1); !eq(payloads(got), []string{"x"}) {
		t.Fatalf("after reopen = %q", payloads(got))
	}
}

// TestOpen_TornTailIsCutAndItsOffsetReused: a crash mid-append leaves a
// partial record. It was never acknowledged, so its offset is free again.
func TestOpen_TornTailIsCutAndItsOffsetReused(t *testing.T) {
	dir := t.TempDir()
	tp := open(t, dir)
	produce(t, tp, "a", "b")
	if err := tp.Close(); err != nil {
		t.Fatal(err)
	}
	torn := wal.Record{Type: recordType, Data: encodeRecord(2, time.Unix(0, 0), []byte("never acked"))}
	appendTorn(t, dir, torn, 7)

	tp = open(t, dir)
	if off := produce(t, tp, "c")[0]; off != 2 {
		t.Fatalf("offset after a torn tail = %d, want 2", off)
	}
	c, _ := tp.Consumer("c")
	if got := poll(t, c, 10); !eq(payloads(got), []string{"a", "b", "c"}) {
		t.Fatalf("after recovery = %q", payloads(got))
	}
}

// appendTorn writes the first keep bytes of rec's framing to the newest
// segment, as a crash mid-write would.
func appendTorn(t *testing.T, dir string, rec wal.Record, keep int) {
	t.Helper()
	scratch := t.TempDir()
	w, err := wal.Open(wal.Options{Dir: scratch})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Log(rec); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	full, err := os.ReadFile(filepath.Join(scratch, wal.SegmentName(0)))
	if err != nil {
		t.Fatal(err)
	}
	segs, _ := wal.Segments(dir)
	f, err := os.OpenFile(filepath.Join(dir, wal.SegmentName(segs[len(segs)-1])), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(full[:keep]); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

// TestOpen_OffsetsAreNotReusedAfterEverythingIsRetained: retention can empty
// a topic down to a fresh segment. Numbering from 0 again would hand out
// offsets the consumer has committed past, and it would skip them.
func TestOpen_OffsetsAreNotReusedAfterEverythingIsRetained(t *testing.T) {
	dir := t.TempDir()
	tp := open(t, dir, func(o *Options) { o.SegmentSize = 30 }) // one record per segment
	c, _ := tp.Consumer("c")
	produce(t, tp, "aaaa", "bbbb")
	poll(t, c, 2)
	if err := c.Commit(1); err != nil {
		t.Fatal(err)
	}
	if err := tp.Close(); err != nil {
		t.Fatal(err)
	}
	// Leave only an empty newest segment, as a roll followed by a crash and
	// full retention would.
	segs, _ := wal.Segments(dir)
	for _, n := range segs {
		if err := os.Remove(filepath.Join(dir, wal.SegmentName(n))); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, wal.SegmentName(segs[len(segs)-1]+1)), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	tp = open(t, dir, func(o *Options) { o.SegmentSize = 30 })
	if off := produce(t, tp, "cccc")[0]; off != 2 {
		t.Fatalf("first offset on an emptied topic = %d, want 2", off)
	}
	c, _ = tp.Consumer("c")
	if got := poll(t, c, 10); !eq(payloads(got), []string{"cccc"}) {
		t.Fatalf("consumer after an emptied topic = %q", payloads(got))
	}
}

func TestOpen_RefusesTopicsItCouldNotHaveWritten(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string){
		"a foreign record type": func(t *testing.T, dir string) {
			writeWAL(t, dir, wal.Record{Type: 9, Data: encodeRecord(0, time.Unix(0, 0), nil)})
		},
		"a record shorter than its header": func(t *testing.T, dir string) {
			writeWAL(t, dir, wal.Record{Type: recordType, Data: []byte{1, 2, 3}})
		},
		"a gap in the offsets": func(t *testing.T, dir string) {
			writeWAL(t, dir,
				wal.Record{Type: recordType, Data: encodeRecord(0, time.Unix(0, 0), nil)},
				wal.Record{Type: recordType, Data: encodeRecord(2, time.Unix(0, 0), nil)})
		},
		"a gap in the segment numbers": func(t *testing.T, dir string) {
			writeWAL(t, dir, wal.Record{Type: recordType, Data: encodeRecord(0, time.Unix(0, 0), nil)})
			if err := os.WriteFile(filepath.Join(dir, wal.SegmentName(5)), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"an empty segment before the newest": func(t *testing.T, dir string) {
			sub := t.TempDir()
			writeWAL(t, sub, wal.Record{Type: recordType, Data: encodeRecord(0, time.Unix(0, 0), nil)})
			b, err := os.ReadFile(filepath.Join(sub, wal.SegmentName(0)))
			if err != nil {
				t.Fatal(err)
			}
			for n, data := range map[int][]byte{0: nil, 1: b} {
				if err := os.WriteFile(filepath.Join(dir, wal.SegmentName(n)), data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		},
		"a consumer behind the oldest record": func(t *testing.T, dir string) {
			writeWAL(t, dir, wal.Record{Type: recordType, Data: encodeRecord(5, time.Unix(0, 0), nil)})
			if err := os.MkdirAll(filepath.Join(dir, consumersDir), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := writeOffsetFile(filepath.Join(dir, consumersDir, "c"+offsetExt), 2); err != nil {
				t.Fatal(err)
			}
		},
		"a corrupt offset file": func(t *testing.T, dir string) {
			if err := os.MkdirAll(filepath.Join(dir, consumersDir), 0o755); err != nil {
				t.Fatal(err)
			}
			b := encodeOffset(3)
			b[0] ^= 1
			if err := os.WriteFile(filepath.Join(dir, consumersDir, "c"+offsetExt), b, 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			setup(t, dir)
			tp, err := Open(Options{Dir: dir})
			if err == nil {
				_ = tp.Close()
				t.Fatal("Open = nil error")
			}
			if !errors.Is(err, ErrCorrupt) {
				t.Errorf("Open = %v, want ErrCorrupt", err)
			}
		})
	}
}

func writeWAL(t *testing.T, dir string, recs ...wal.Record) {
	t.Helper()
	w, err := wal.Open(wal.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Log(recs...); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestOpen_LeftoverTempOffsetFileIsIgnored: a commit that crashed before its
// rename leaves the previous offset as the truth.
func TestOpen_LeftoverTempOffsetFileIsIgnored(t *testing.T) {
	dir := t.TempDir()
	tp := open(t, dir)
	produce(t, tp, "a", "b", "c")
	c, _ := tp.Consumer("c")
	poll(t, c, 3)
	if err := c.Commit(0); err != nil {
		t.Fatal(err)
	}
	if err := tp.Close(); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, consumersDir, "c"+offsetExt+".tmp")
	if err := os.WriteFile(tmp, encodeOffset(3)[:5], 0o644); err != nil { // torn, never renamed
		t.Fatal(err)
	}
	tp = open(t, dir)
	c, _ = tp.Consumer("c")
	if got := poll(t, c, 10); !eq(payloads(got), []string{"b", "c"}) {
		t.Fatalf("after a crashed commit = %q, want redelivery from b", payloads(got))
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("temp offset file left behind: %v", err)
	}
}

func TestOpen_Options(t *testing.T) {
	if _, err := Open(Options{}); err == nil {
		t.Error("Open without Dir = nil error")
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Options{Dir: filepath.Join(blocked, "topic")}); err == nil {
		t.Error("Open under a file = nil error")
	}
	tp := open(t, t.TempDir(), func(o *Options) { o.SyncInterval = 0 })
	if tp.syncInterval != DefaultSyncInterval || tp.maxBytes != DefaultMaxBytes || tp.syncBytes != DefaultSyncBytes {
		t.Errorf("defaults not applied: %+v", tp)
	}
}

func TestRecord_RoundTripAndRejects(t *testing.T) {
	at := time.Unix(12, 34)
	b := encodeRecord(7, at, []byte("hi"))
	got, err := decodeRecord(wal.Record{Type: recordType, Data: b})
	if err != nil || got.Offset != 7 || !got.Time.Equal(at) || !bytes.Equal(got.Payload, []byte("hi")) {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	b[recordHeader] = 'X' // the decoded payload is a copy
	if string(got.Payload) != "hi" {
		t.Error("decoded payload aliases the input")
	}
	if _, err := decodeOffset([]byte{1, 2}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("short offset file = %v", err)
	}
}

// TestGroupCommit_OneOversizedAppendDoesNotWaitOutTheWindow: a single append
// already past SyncBytes sends one wake-up. If the syncer then armed the
// window timer anyway, nothing would wake it before the timer.
func TestGroupCommit_OneOversizedAppendDoesNotWaitOutTheWindow(t *testing.T) {
	clk := testutil.NewFakeClock(time.Unix(0, 0))
	tp := open(t, t.TempDir(), func(o *Options) {
		o.Clock = clk
		o.SyncInterval = time.Hour
		o.SyncBytes = 100
	})
	done := make(chan error, 1)
	go func() { _, err := tp.Produce(context.Background(), make([]byte, 200)); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an append over SyncBytes waited for the window timer")
	}
}

// TestConsumer_ANewConsumerHoldsRetentionAcrossARestart: a consumer that has
// registered but never committed must still pin the records it has not read
// after a restart, or the other consumers' commits retire them under it.
func TestConsumer_ANewConsumerHoldsRetentionAcrossARestart(t *testing.T) {
	dir := t.TempDir()
	small := func(o *Options) { o.SegmentSize = 30 } // one record per segment
	tp := open(t, dir, small)
	if _, err := tp.Consumer("slow"); err != nil {
		t.Fatal(err)
	}
	if err := tp.Close(); err != nil {
		t.Fatal(err)
	}

	tp = open(t, dir, small)
	fast, _ := tp.Consumer("fast")
	produce(t, tp, "aaaa", "bbbb", "cccc")
	poll(t, fast, 3)
	if err := fast.Commit(2); err != nil {
		t.Fatal(err)
	}
	slow, err := tp.Consumer("slow")
	if err != nil {
		t.Fatal(err)
	}
	if got := poll(t, slow, 10); !eq(payloads(got), []string{"aaaa", "bbbb", "cccc"}) {
		t.Fatalf("slow consumer after restart = %q, want everything", payloads(got))
	}
}

// TestClose_ReleasesAProducerWhoseWakeUpIsStillQueued: Close can land while
// the syncer is idle with a wake-up pending. Whichever of the two it takes
// first, the pending record must be synced and its producer released.
// Repeated because select picks between ready cases at random.
func TestClose_ReleasesAProducerWhoseWakeUpIsStillQueued(t *testing.T) {
	for i := range 20 {
		entered, gate := make(chan struct{}), make(chan struct{})
		var once sync.Once
		tp, err := Open(Options{Dir: t.TempDir(), SyncInterval: -1, syncFn: func(w *wal.WAL) error {
			once.Do(func() { close(entered); <-gate })
			return w.Sync()
		}})
		if err != nil {
			t.Fatal(err)
		}
		first := make(chan error, 1)
		go func() { _, err := tp.Produce(context.Background(), []byte("a")); first <- err }()
		<-entered // the syncer is inside the first fsync
		second := make(chan error, 1)
		go func() { _, err := tp.Produce(context.Background(), []byte("b")); second <- err }()
		testutil.Eventually(t, 5*time.Second, func() bool {
			tp.mu.Lock()
			defer tp.mu.Unlock()
			return tp.next == 2
		}, "second producer never appended")
		closed := make(chan error, 1)
		go func() { closed <- tp.Close() }()
		testutil.Eventually(t, 5*time.Second, func() bool {
			select {
			case <-tp.stop:
				return true
			default:
				return false
			}
		}, "Close never signalled stop")
		close(gate)
		for name, ch := range map[string]chan error{"first": first, "second": second, "close": closed} {
			select {
			case err := <-ch:
				if err != nil {
					t.Fatalf("iteration %d: %s = %v", i, name, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("iteration %d: %s never returned", i, name)
			}
		}
	}
}

// TestRetention_APollInsideARetentionPassWaitsForIt: a retention pass unlinks
// files before it updates the index. A parked consumer that reads in that
// window finds its file gone while the segment is still listed; it must wait
// for the pass, not report corruption. The hook holds the window open.
func TestRetention_APollInsideARetentionPassWaitsForIt(t *testing.T) {
	inWindow, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	tp := open(t, t.TempDir(), func(o *Options) {
		o.SegmentSize = 64
		o.afterUnlink = func() { once.Do(func() { close(inWindow); <-release }) }
	})
	a, _ := tp.Consumer("a")
	b, _ := tp.Consumer("b")
	produce(t, tp, "s0r0", "s0r1")
	poll(t, b, 2) // b is parked at the end of segment 0
	if err := b.Commit(1); err != nil {
		t.Fatal(err)
	}
	produce(t, tp, "s1r0")
	poll(t, a, 2)
	committed := make(chan error, 1)
	go func() { committed <- a.Commit(1) }() // retires segment 0, then blocks in the window
	<-inWindow

	polled := make(chan []Record, 1)
	pollErr := make(chan error, 1)
	go func() {
		recs, err := b.Poll(context.Background(), 10)
		if err != nil {
			pollErr <- err
			return
		}
		polled <- recs
	}()
	select {
	case err := <-pollErr:
		t.Fatalf("Poll inside a retention pass = %v, want it to wait for the pass", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-committed; err != nil {
		t.Fatal(err)
	}
	select {
	case recs := <-polled:
		if !eq(payloads(recs), []string{"s1r0"}) {
			t.Fatalf("Poll after the pass = %q", payloads(recs))
		}
	case err := <-pollErr:
		t.Fatalf("Poll after the pass = %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Poll never returned")
	}
}

// TestConsumer_RegisteringDuringRetentionWaitsForThePass: a consumer that
// read its start offset before a running pass finished would start inside a
// segment being deleted, and its offset file would then make the topic
// unopenable. Registration must wait for the pass.
func TestConsumer_RegisteringDuringRetentionWaitsForThePass(t *testing.T) {
	dir := t.TempDir()
	inWindow, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	tp := open(t, dir, func(o *Options) {
		o.SegmentSize = 30 // one record per segment
		o.afterUnlink = func() { once.Do(func() { close(inWindow); <-release }) }
	})
	a, _ := tp.Consumer("a")
	produce(t, tp, "aaaa", "bbbb", "cccc")
	poll(t, a, 2)
	committed := make(chan error, 1)
	go func() { committed <- a.Commit(1) }()
	<-inWindow

	registered := make(chan *Consumer, 1)
	go func() {
		c, err := tp.Consumer("late")
		if err != nil {
			t.Error(err)
		}
		registered <- c
	}()
	select {
	case <-registered:
		t.Fatal("Consumer registered inside a retention pass")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-committed; err != nil {
		t.Fatal(err)
	}
	late := <-registered
	if got := poll(t, late, 10); !eq(payloads(got), []string{"cccc"}) {
		t.Fatalf("late consumer = %q, want [cccc]", payloads(got))
	}
	if err := tp.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Options{Dir: dir, SyncInterval: -1, SegmentSize: 30}); err != nil {
		t.Fatalf("topic does not reopen: %v", err)
	}
}

// TestProduce_AFailedRolloverFsyncStopsTheTopic: the fsync a rollover does is
// as final as a group commit's. It must stop the topic and fail the
// producers waiting on the current generation, not come back as an ordinary
// error that the next Produce retries.
func TestProduce_AFailedRolloverFsyncStopsTheTopic(t *testing.T) {
	clk := testutil.NewFakeClock(time.Unix(0, 0))
	var fail sync.Mutex
	failing := false
	tp := open(t, t.TempDir(), func(o *Options) {
		o.Clock = clk
		o.SyncInterval = time.Hour
		o.logFn = func(w *wal.WAL, r wal.Record) error {
			fail.Lock()
			defer fail.Unlock()
			if failing {
				return fmt.Errorf("%w before rollover: injected EIO", wal.ErrSync)
			}
			return w.Log(r)
		}
	})
	waiting := make(chan error, 1)
	go func() { _, err := tp.Produce(context.Background(), []byte("a")); waiting <- err }()
	testutil.Eventually(t, 5*time.Second, func() bool { return clk.Waiters() == 1 }, "never armed")
	fail.Lock()
	failing = true
	fail.Unlock()
	if _, err := tp.Produce(context.Background(), []byte("b")); !errors.Is(err, wal.ErrSync) {
		t.Fatalf("Produce over a failing rollover = %v", err)
	}
	select {
	case err := <-waiting:
		if !errors.Is(err, wal.ErrSync) {
			t.Fatalf("producer waiting on the group commit = %v, want the fsync error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("producer waiting on the group commit was never released")
	}
	fail.Lock()
	failing = false
	fail.Unlock()
	if _, err := tp.Produce(context.Background(), []byte("c")); !errors.Is(err, wal.ErrSync) {
		t.Fatalf("Produce after a failed rollover fsync = %v, want the topic stopped", err)
	}
}

// TestProduce_AGroupSyncDuringAFailedRolloverProvesNothing: a group fsync in
// flight when a rollover's fsync fails may have run after it, on the same
// file, and "succeeded" over dropped pages. Its waiters must not be acked.
func TestProduce_AGroupSyncDuringAFailedRolloverProvesNothing(t *testing.T) {
	entered, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var fail sync.Mutex
	failing := false
	tp := open(t, t.TempDir(), func(o *Options) {
		o.syncFn = func(w *wal.WAL) error {
			once.Do(func() { close(entered); <-gate })
			return w.Sync()
		}
		o.logFn = func(w *wal.WAL, r wal.Record) error {
			fail.Lock()
			defer fail.Unlock()
			if failing {
				return fmt.Errorf("%w before rollover: injected EIO", wal.ErrSync)
			}
			return w.Log(r)
		}
	})
	first := make(chan error, 1)
	go func() { _, err := tp.Produce(context.Background(), []byte("a")); first <- err }()
	<-entered
	fail.Lock()
	failing = true
	fail.Unlock()
	if _, err := tp.Produce(context.Background(), []byte("b")); !errors.Is(err, wal.ErrSync) {
		t.Fatalf("rollover Produce = %v", err)
	}
	close(gate) // the group fsync now "succeeds"
	if err := <-first; !errors.Is(err, wal.ErrSync) {
		t.Fatalf("producer of the in-flight group sync = %v, want the rollover's fsync error", err)
	}
	if s := tp.Stats(); s.Durable != 0 {
		t.Errorf("durable = %d after a failed rollover fsync, want 0", s.Durable)
	}
}

func TestProduce_AnOrdinaryWriteErrorIsRetryable(t *testing.T) {
	calls := 0
	tp := open(t, t.TempDir(), func(o *Options) {
		o.logFn = func(w *wal.WAL, r wal.Record) error {
			calls++
			if calls == 1 {
				return errors.New("injected ENOSPC")
			}
			return w.Log(r)
		}
	})
	if _, err := tp.Produce(context.Background(), []byte("a")); err == nil {
		t.Fatal("first Produce = nil error")
	}
	if off, err := tp.Produce(context.Background(), []byte("a")); err != nil || off != 0 {
		t.Fatalf("retry after a write error = %d, %v; want 0, nil", off, err)
	}
}

// TestOpen_SyncsRecoveredRecordsBeforeServingThem: records written but not
// synced by a crashed run read back from the page cache. Open must make
// them durable before a consumer can apply and commit them; if it cannot,
// it must not open.
func TestOpen_SyncsRecoveredRecordsBeforeServingThem(t *testing.T) {
	dir := t.TempDir()
	tp := open(t, dir)
	produce(t, tp, "a")
	if err := tp.Close(); err != nil {
		t.Fatal(err)
	}
	synced := 0
	tp = open(t, dir, func(o *Options) {
		o.openSyncFn = func(w *wal.WAL) error { synced++; return w.Sync() }
	})
	if synced != 1 {
		t.Errorf("Open fsynced %d times, want once", synced)
	}
	_ = tp.Close()
	boom := errors.New("injected EIO")
	if tp, err := Open(Options{Dir: dir, openSyncFn: func(*wal.WAL) error { return boom }}); !errors.Is(err, boom) {
		if tp != nil {
			_ = tp.Close()
		}
		t.Fatalf("Open with a failing fsync = %v, want it refused", err)
	}
}

// TestRetention_AFailedPassKeepsTheIndexHonestAndTheCommitDurable: a pass
// that cannot delete is reported in Stats, not as a failed Commit (the
// offset file is already durable), and the index keeps listing exactly the
// files still on disk.
func TestRetention_AFailedPassKeepsTheIndexHonestAndTheCommitDurable(t *testing.T) {
	dir := t.TempDir()
	tp := open(t, dir, func(o *Options) { o.SegmentSize = 30 })
	c, _ := tp.Consumer("c")
	produce(t, tp, "aaaa", "bbbb", "cccc")
	poll(t, c, 3)
	if err := os.Chmod(dir, 0o555); err != nil { // unlinking now fails
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o755) }()
	if err := c.Commit(0); err != nil {
		t.Fatalf("Commit with a failing retention = %v, want nil (the commit is durable)", err)
	}
	s := tp.Stats()
	if s.RetentionErr == nil || s.Segments != 3 || s.Bytes != 3*29 {
		t.Fatalf("after a failed pass: %+v; want RetentionErr set and all 3 segments still counted", s)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.Commit(1); err != nil {
		t.Fatal(err)
	}
	if s := tp.Stats(); s.RetentionErr != nil || s.Segments != 1 || s.Bytes != 29 {
		t.Fatalf("after a good pass: %+v; want the error cleared and 1 segment", s)
	}
}

// TestRetention_IndexFollowsTheFilesAfterAPartialPass: whatever a pass
// managed to delete is dropped from the index, and whatever it did not is
// kept. Simulated by putting one segment back before the index is updated.
func TestRetention_IndexFollowsTheFilesAfterAPartialPass(t *testing.T) {
	dir := t.TempDir()
	var saved []byte
	tp := open(t, dir, func(o *Options) {
		o.SegmentSize = 30
		o.afterUnlink = func() {
			if saved != nil {
				_ = os.WriteFile(filepath.Join(dir, wal.SegmentName(1)), saved, 0o644)
			}
		}
	})
	c, _ := tp.Consumer("c")
	produce(t, tp, "aaaa", "bbbb", "cccc")
	b, err := os.ReadFile(filepath.Join(dir, wal.SegmentName(1)))
	if err != nil {
		t.Fatal(err)
	}
	saved = b
	poll(t, c, 3)
	if err := c.Commit(1); err != nil { // segments 0 and 1 are due
		t.Fatal(err)
	}
	if s := tp.Stats(); s.Segments != 2 || s.Oldest != 1 || s.Bytes != 2*29 {
		t.Fatalf("after a partial pass: %+v; want segments 1 and 2 still indexed", s)
	}
}

// TestCommit_IsNotStuckBehindAnIdlePoll: Poll must not hold the consumer's
// lock while it waits, or an apply goroutine's Commit hangs until traffic.
func TestCommit_IsNotStuckBehindAnIdlePoll(t *testing.T) {
	tp := open(t, t.TempDir())
	c, _ := tp.Consumer("c")
	produce(t, tp, "a")
	poll(t, c, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = c.Poll(ctx, 1) }() // parks: nothing more to read
	time.Sleep(20 * time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- c.Commit(0) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Commit blocked behind an idle Poll")
	}
}
