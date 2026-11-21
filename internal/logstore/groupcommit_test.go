package logstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/internal/tsdb/wal"
)

// Concurrent Appends share WAL fsyncs (ADR-0065), and what each was told is stored survives a power cut:
// every Append that returned nil is in what the cut leaves, however the fsyncs were shared. A Sync
// that was shared with the wrong group, or one that returned early, loses a log here.
func TestGroupCommit_EveryAcknowledgedAppendSurvivesAPowerCut(t *testing.T) {
	st := testutil.NewSyncTracker()
	var fsyncs atomic.Int64
	dir := t.TempDir()
	s, _ := openStore(t, dir, func(o *Options) {
		o.NoSync = false
		o.BlockBytes = 8 << 20
		o.walSyncFile = func(f *os.File) error {
			fsyncs.Add(1)
			time.Sleep(3 * time.Millisecond)
			return st.SyncFile(f)
		}
	})
	var armed atomic.Bool
	armed.Store(true)
	s.afterBlock = func() {
		if armed.Load() {
			t.Error("a block was sealed: this test models only the WAL")
		}
	}
	t.Cleanup(func() { armed.Store(false); _ = s.Close() })

	const goroutines, each = 12, 10
	var mu sync.Mutex
	var acked []string
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				prefix := fmt.Sprintf("g%02d-%02d-", g, i)
				if err := s.Append(context.Background(), batchOf(prefix, 0, 2, t0.UnixMilli())); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				acked = append(acked, prefix+"000", prefix+"001")
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if n := fsyncs.Load(); n*2 > goroutines*each {
		t.Errorf("%d WAL fsyncs for %d Appends: they were not shared", n, goroutines*each)
	}
	cut := st.PowerCut(t, dir, filepath.Join(dir, "wal"), 0)
	pcWant(t, pcSurvivors(t, cut), acked)
}

// The fsync is outside the write lock: while one Append waits for its fsync, another can write and
// have its logs read. (With the fsync inside the lock the second would wait for the first.)
func TestGroupCommit_AnAppendCanWriteWhileAnotherWaitsForItsFsync(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var n atomic.Int64
	s, _ := openStore(t, t.TempDir(), func(o *Options) {
		o.NoSync = false
		o.walSyncFile = func(f *os.File) error {
			if n.Add(1) == 1 {
				close(entered)
				<-release
			}
			return f.Sync()
		}
	})
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	first := make(chan error, 1)
	go func() { first <- s.Append(ctx, batchOf("one-", 0, 1, t0.UnixMilli())) }()
	<-entered

	second := make(chan error, 1)
	go func() { second <- s.Append(ctx, batchOf("two-", 0, 1, t0.UnixMilli())) }()
	// The second has written and its log is readable, though neither Append has returned.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := everything(t, s); len(got) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the second Append could not write while the first waited for its fsync: %v", everything(t, s))
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-first:
		t.Fatalf("the first Append returned before its fsync finished: %v", err)
	case err := <-second:
		t.Fatalf("the second Append returned before any fsync covered it: %v", err)
	default:
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
}

// A retry of a batch whose fsync failed finds every log already in the head. It must still be refused:
// answering nil because there was nothing new to write would tell the sender "stored" for logs that
// were never synced.
func TestGroupCommit_ARetryAfterAFailedFsyncIsNotAcknowledged(t *testing.T) {
	var failing atomic.Bool
	s, _ := openStore(t, t.TempDir(), func(o *Options) {
		o.NoSync = false
		o.DedupeHead = true // queued mode's setting: a redelivered batch finds its logs already in the head
		o.walSyncFile = func(f *os.File) error {
			if failing.Load() {
				return errors.New("injected EIO")
			}
			return f.Sync()
		}
	})
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	batch := batchOf("a", 0, 3, t0.UnixMilli())

	failing.Store(true)
	if err := s.Append(ctx, batch); !errors.Is(err, wal.ErrSync) {
		t.Fatalf("Append over a failing fsync = %v, want wal.ErrSync", err)
	}
	failing.Store(false)
	// The logs are in the head, unsynced: the retry sees nothing new.
	if got := everything(t, s); len(got) != 3 {
		t.Fatalf("expected the three logs to be readable after the failed Append (visible first), got %v", got)
	}
	if err := s.Append(ctx, batch); err == nil {
		t.Fatal("the retry of an unsynced batch was acknowledged")
	} else if !errors.Is(err, wal.ErrBroken) {
		t.Fatalf("the retry = %v, want the stopped log's refusal", err)
	}
}

// On a store that is not stopped (the fsync was merely slow), a retry that finds everything already
// there still waits for an fsync before it says stored.
func TestGroupCommit_ADuplicateBatchStillWaitsForAnFsync(t *testing.T) {
	var n atomic.Int64
	s, _ := openStore(t, t.TempDir(), func(o *Options) {
		o.NoSync = false
		o.DedupeHead = true
		o.walSyncFile = walSyncs(&n, nil)
	})
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	batch := batchOf("a", 0, 3, t0.UnixMilli())
	if err := s.Append(ctx, batch); err != nil {
		t.Fatal(err)
	}
	before := n.Load()
	if err := s.Append(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if n.Load() == before {
		t.Error("an Append of logs already held returned without an fsync")
	}
	if got := everything(t, s); len(got) != 3 {
		t.Errorf("a duplicate batch changed what is stored: %v", everything(t, s))
	}
}
