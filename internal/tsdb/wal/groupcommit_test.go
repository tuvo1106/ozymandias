package wal

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// syncProbe is a SyncFile that records the file length at the start of each fsync (what that fsync made
// durable) and how many fsyncs ran, and can stall or fail chosen calls.
type syncProbe struct {
	calls  atomic.Int64
	synced atomic.Int64 // the largest length any completed fsync covered
	delay  time.Duration
	// onCall, if set, runs inside call n (1-based) before the fsync and may block; it returns the error
	// that fsync should report.
	onCall func(n int64) error
}

func (p *syncProbe) fn(f *os.File) error {
	n := p.calls.Add(1)
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if p.onCall != nil {
		if err := p.onCall(n); err != nil {
			return err
		}
	}
	if p.delay > 0 {
		time.Sleep(p.delay)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	for {
		cur := p.synced.Load()
		if info.Size() <= cur || p.synced.CompareAndSwap(cur, info.Size()) {
			return nil
		}
	}
}

func openProbed(t *testing.T, p *syncProbe, mod func(*Options)) (*WAL, string) {
	t.Helper()
	dir := t.TempDir()
	o := Options{Dir: dir, StopOnSyncError: true, SyncFile: p.fn}
	if mod != nil {
		mod(&o)
	}
	w, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w, dir
}

// Concurrent callers of Sync share fsyncs, and none returns before an fsync that began after its own
// record was written has finished. The second half is the property: a Sync that returned early, or one
// answered by an fsync that started before the record was written, would pass the first half and fail this.
func TestSync_CallersShareFsyncsAndEachIsDurableOnReturn(t *testing.T) {
	p := &syncProbe{delay: 3 * time.Millisecond}
	w, _ := openProbed(t, p, nil)

	const goroutines, each = 24, 15
	var order sync.Mutex // makes "write, then read where it ended" one step, so end is exact
	var wg sync.WaitGroup
	var failures atomic.Int64
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				order.Lock()
				err := w.Log(rec(1, fmt.Sprintf("g%d-%d", g, i)))
				end := w.End().Offset
				order.Unlock()
				if err != nil {
					t.Error(err)
					return
				}
				if err := w.Sync(); err != nil {
					t.Error(err)
					return
				}
				if got := p.synced.Load(); got < end {
					failures.Add(1)
					t.Errorf("Sync returned with %d bytes durable; its record ends at %d", got, end)
					return
				}
			}
		}()
	}
	wg.Wait()
	total := int64(goroutines * each)
	if n := p.calls.Load(); n*2 > total {
		t.Errorf("%d fsyncs for %d syncs: they were not shared", n, total)
	}
	if failures.Load() != 0 {
		t.Fatal("a Sync returned before its record was durable")
	}
}

// A failed fsync fails every caller it was answering, and on a log that stops on failure the groups
// queued behind it are answered with the stop, not with an fsync of pages the first one failed to write.
func TestSync_AFailedFsyncFailsItsGroupAndNothingSyncsAfterIt(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	defer open() // a failing assertion must not leave an fsync blocked, or Close in the cleanup hangs
	errDisk := errors.New("disk on fire")
	p := &syncProbe{}
	p.onCall = func(n int64) error {
		if n == 1 {
			close(entered)
			<-release
			return errDisk
		}
		return nil // every later fsync would succeed: the log must not ask
	}
	w, _ := openProbed(t, p, nil)

	results := make(chan error, 3)
	logSync := func(s string) {
		if err := w.Log(rec(1, s)); err != nil {
			results <- err
			return
		}
		results <- w.Sync()
	}
	go logSync("leader")
	<-entered
	go logSync("follower-1")
	go logSync("follower-2")
	// Both followers queue behind the running fsync (one group forms; if the second is slower than the
	// wait, it meets the stopped log instead, and the assertions hold either way).
	deadline := time.Now().Add(5 * time.Second)
	for {
		w.gmu.Lock()
		formed := w.next != nil
		w.gmu.Unlock()
		if formed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the followers never queued")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	open()

	for i := 0; i < 3; i++ {
		if err := <-results; err == nil {
			t.Errorf("caller %d was told its record is durable after a failed fsync", i)
		}
	}
	if w.Broken() == nil {
		t.Error("the log kept accepting writes after a failed fsync")
	}
	if n := p.calls.Load(); n != 1 {
		t.Errorf("%d fsyncs ran; after the failed one the log must not fsync again", n)
	}
	if err := w.Log(rec(1, "later")); !errors.Is(err, ErrBroken) {
		t.Errorf("Log after the failure = %v, want ErrBroken", err)
	}
	if err := w.Sync(); !errors.Is(err, ErrBroken) {
		t.Errorf("Sync after the failure = %v, want ErrBroken", err)
	}
}

// A log that does not stop on failure (nothing ozyd opens) reports the failure to the group and carries on.
func TestSync_WithoutStopOnSyncErrorTheNextGroupRuns(t *testing.T) {
	p := &syncProbe{}
	p.onCall = func(n int64) error {
		if n == 1 {
			return errors.New("transient")
		}
		return nil
	}
	w, _ := openProbed(t, p, func(o *Options) { o.StopOnSyncError = false })
	if err := w.Log(rec(1, "a")); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); !errors.Is(err, ErrSync) {
		t.Fatalf("first Sync = %v, want ErrSync", err)
	}
	if err := w.Log(rec(1, "b")); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("a log that does not stop on failure should sync again: %v", err)
	}
}

// A rollover that began while a group fsync was failing must not fsync the file again and carry on:
// that second fsync can succeed over pages the first one failed to write, and the records in them would
// then be acknowledged by the next group.
func TestSync_ARolloverCannotHideAFailedFsync(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	defer open()
	p := &syncProbe{}
	p.onCall = func(n int64) error {
		if n == 1 {
			close(entered)
			<-release
			return errors.New("disk on fire")
		}
		return nil
	}
	w, _ := openProbed(t, p, func(o *Options) { o.SegmentSize = 64 })
	if err := w.Log(rec(1, "first")); err != nil {
		t.Fatal(err)
	}
	syncErr := make(chan error, 1)
	go func() { syncErr <- w.Sync() }()
	<-entered

	rollErr := make(chan error, 1)
	go func() { rollErr <- w.Log(rec(1, strings.Repeat("x", 80))) }() // does not fit: rolls
	select {
	case err := <-rollErr:
		t.Fatalf("the rollover did not wait for the fsync in flight: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	open()

	if err := <-syncErr; err == nil {
		t.Error("the failed fsync was reported as success")
	}
	if err := <-rollErr; err == nil {
		t.Error("the rollover went ahead over a failed fsync")
	}
	if w.Broken() == nil {
		t.Error("the log is still writable")
	}
	if n := p.calls.Load(); n != 1 {
		t.Errorf("%d fsyncs ran, want only the one that failed", n)
	}
}

// Rollovers and syncs race all day: every Sync succeeds, every record is there after, and no fsync lands
// on a segment that a rollover has closed.
func TestSync_RolloversAndSyncsRaceWithoutErrors(t *testing.T) {
	p := &syncProbe{}
	w, dir := openProbed(t, p, func(o *Options) { o.SegmentSize = 256 })
	const goroutines, each = 16, 40
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := w.Log(rec(1, fmt.Sprintf("g%02d-%03d-%s", g, i, strings.Repeat("p", 30)))); err != nil {
					t.Error(err)
					return
				}
				if err := w.Sync(); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := len(replay(t, dir)); got != goroutines*each {
		t.Fatalf("replayed %d records, want %d", got, goroutines*each)
	}
	if w.Segment() < 3 {
		t.Fatalf("only %d segments: the test did not roll", w.Segment())
	}
}

// Close waits for the fsync in flight and later callers are told the log is closed; none reports an
// fsync of a closed file.
func TestSync_CloseWhileSyncsAreInFlight(t *testing.T) {
	p := &syncProbe{delay: 2 * time.Millisecond}
	w, _ := openProbed(t, p, nil)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if err := w.Log(rec(1, "x")); err != nil {
					return
				}
				if err := w.Sync(); err != nil && !strings.Contains(err.Error(), "closed") {
					t.Errorf("unexpected: %v", err)
					return
				} else if err != nil && strings.Contains(err.Error(), "already closed") {
					t.Errorf("an fsync reached a closed file: %v", err)
					return
				}
			}
		}()
	}
	time.Sleep(15 * time.Millisecond)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
}

// Close waits for a group fsync in flight before it closes the segment under it.
func TestSync_CloseWaitsForAnFsyncInFlight(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	defer open()
	p := &syncProbe{}
	p.onCall = func(n int64) error {
		if n == 1 {
			close(entered)
			<-release
		}
		return nil
	}
	w, _ := openProbed(t, p, nil)
	if err := w.Log(rec(1, "x")); err != nil {
		t.Fatal(err)
	}
	syncErr := make(chan error, 1)
	go func() { syncErr <- w.Sync() }()
	<-entered

	closeErr := make(chan error, 1)
	go func() { closeErr <- w.Close() }()
	select {
	case err := <-closeErr:
		t.Fatalf("Close returned while an fsync of the segment was still running: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	open()
	if err := <-syncErr; err != nil {
		t.Errorf("the fsync in flight: %v", err)
	}
	if err := <-closeErr; err != nil {
		t.Errorf("Close: %v", err)
	}
}

// After a group fsync has failed on a log that stops on failure, Cut and Close do not fsync the file
// again: it could succeed over pages the failed one lost. (Between the failed fsync and the log being
// marked broken there is a window; failedSync is what closes it, so it is set directly here.)
func TestSync_CutAndCloseDoNotFsyncAfterAFailedGroupFsync(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(w *WAL) error
	}{
		{"Cut", func(w *WAL) error { _, err := w.Cut(); return err }},
		{"Close", func(w *WAL) error { return w.Close() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &syncProbe{}
			w, _ := openProbed(t, p, nil)
			if err := w.Log(rec(1, "x")); err != nil {
				t.Fatal(err)
			}
			w.failedSync.Store(true)
			before := p.calls.Load()
			err := tc.do(w)
			if err == nil || !errors.Is(err, ErrSync) {
				t.Errorf("%s after a failed group fsync = %v, want ErrSync", tc.name, err)
			}
			if n := p.calls.Load(); n != before {
				t.Errorf("%s fsynced %d more times after a failed fsync", tc.name, n-before)
			}
		})
	}
}

// An fsync that panics (a test's hook; nothing in the standard library does) must not wedge the log:
// its group is told it failed, the lock it held is released, and the next Sync runs.
func TestSync_APanickingFsyncDoesNotWedgeTheLog(t *testing.T) {
	p := &syncProbe{}
	p.onCall = func(n int64) error {
		if n == 1 {
			panic("boom")
		}
		return nil
	}
	// Opened by hand: if the log is wedged, the usual Close in a cleanup would hang the test instead of
	// failing it, so it is closed here with a deadline.
	w, err := Open(Options{Dir: t.TempDir(), SegmentSize: 64, SyncFile: p.fn})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closed := make(chan struct{})
		go func() { _ = w.Close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(2 * time.Second):
		}
	}()
	if err := w.Log(rec(1, "a")); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic was swallowed")
			}
		}()
		_ = w.Sync()
	}()
	done := make(chan error, 1)
	go func() {
		if err := w.Log(rec(1, "b")); err != nil {
			done <- err
			return
		}
		done <- w.Sync()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the Sync after the panic = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a Sync after a panicking fsync never returned: the group was left running")
	}
	// A rollover must not wait on a lock the panic left held.
	rolled := make(chan error, 1)
	go func() { rolled <- w.Log(rec(1, strings.Repeat("x", 80))) }()
	select {
	case err := <-rolled:
		if err != nil {
			t.Fatalf("rollover after the panic: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a rollover after a panicking fsync never returned: the segment lock was left held")
	}
}

// When the fsync of a group that has followers panics, the followers are told it failed, not that their
// records are durable, and the log goes on. (The leader's own caller sees the panic.)
func TestSync_FollowersOfAPanickingFsyncAreToldItFailed(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	defer open()
	p := &syncProbe{}
	p.onCall = func(n int64) error {
		switch n {
		case 1:
			close(entered)
			<-release
		case 2:
			panic("boom")
		}
		return nil
	}
	w, err := Open(Options{Dir: t.TempDir(), SyncFile: p.fn})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closed := make(chan struct{})
		go func() { _ = w.Close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(2 * time.Second):
		}
	}()

	type result struct {
		err      error
		panicked bool
	}
	results := make(chan result, 3)
	run := func(s string) {
		var r result
		defer func() {
			if recover() != nil {
				r.panicked = true
			}
			results <- r
		}()
		if err := w.Log(rec(1, s)); err != nil {
			r.err = err
			return
		}
		r.err = w.Sync()
	}
	go run("leader")
	<-entered
	go run("b")
	go run("c")
	deadline := time.Now().Add(5 * time.Second)
	for {
		w.gmu.Lock()
		formed := w.next != nil
		w.gmu.Unlock()
		if formed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the followers never queued")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	open()

	var panicked, failed, ok int
	for i := 0; i < 3; i++ {
		select {
		case r := <-results:
			switch {
			case r.panicked:
				panicked++
			case r.err != nil:
				failed++
			default:
				ok++
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a caller never returned: the group was left running")
		}
	}
	// The leader's fsync (call 1) succeeded. In the second group one caller ran the panicking fsync and
	// the other must have been told it failed; if b and c were split across groups, the later one ran
	// call 3 and succeeded, so only the first two outcomes are required.
	if panicked != 1 {
		t.Errorf("%d callers saw the panic, want exactly the one that ran the fsync", panicked)
	}
	if failed+ok != 2 || ok > 1 {
		t.Errorf("outcomes: %d panicked, %d failed, %d ok", panicked, failed, ok)
	}
}
