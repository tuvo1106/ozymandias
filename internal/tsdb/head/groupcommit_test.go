package head

import (
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/wal"
)

// gatedLog is a Logger whose Sync can be held and failed on demand.
type gatedLog struct {
	mu      sync.Mutex
	logs    int
	syncs   int
	entered chan struct{} // receives once per Sync call
	release chan struct{} // a Sync blocks until this is closed, if hold is set
	hold    bool
	failing bool
}

func newGatedLog() *gatedLog {
	return &gatedLog{entered: make(chan struct{}, 16), release: make(chan struct{})}
}

func (l *gatedLog) Log(...wal.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.logs++
	return nil
}

func (l *gatedLog) Sync() error {
	l.mu.Lock()
	l.syncs++
	hold, failing := l.hold, l.failing
	l.mu.Unlock()
	l.entered <- struct{}{}
	if hold {
		<-l.release
	}
	if failing {
		return errors.New("fsync failed")
	}
	return nil
}

// The fsync is not inside the commit lock: while one Append waits for it, another logs and applies, and
// its samples can be read. With the fsync inside the lock the second Append would wait for the first
// (the serialization group commit removes, ADR-0065).
func TestHead_AnAppendCanCommitWhileAnotherWaitsForItsFsync(t *testing.T) {
	l := newGatedLog()
	l.hold = true
	h := New(Options{WAL: l, BlockRange: 1000, SyncOnAppend: true})

	first := make(chan error, 1)
	go func() { first <- appendOne(h, ref("m", "host:a"), 10, 1) }()
	<-l.entered // the first is now in its fsync, holding nothing the second needs

	second := make(chan error, 1)
	go func() { second <- appendOne(h, ref("m", "host:b"), 10, 2) }()
	<-l.entered // the second logged, applied, and reached its own fsync

	got := mustSelect(t, h, tsdb.Selector{Metric: "m"}, 0, math.MaxInt64)
	if len(got) != 2 {
		t.Fatalf("%d series readable while both fsyncs are pending, want 2: the commit lock was held across an fsync", len(got))
	}
	select {
	case err := <-first:
		t.Fatalf("the first Append returned before its fsync finished: %v", err)
	case err := <-second:
		t.Fatalf("the second Append returned before its fsync finished: %v", err)
	default:
	}
	close(l.release)
	for i, ch := range []chan error{first, second} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("append %d: %v", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("append %d never returned", i)
		}
	}
}

// An fsync that fails fails the batch (Index -1), though its samples were applied: the caller is told
// to retry, never that the batch is stored. A batch that was refused before the log, or that logged
// nothing, owes no fsync.
func TestHead_AFailedFsyncFailsTheBatchAndAnUnloggedOneOwesNone(t *testing.T) {
	l := newGatedLog()
	h := New(Options{WAL: l, BlockRange: 1000, SyncOnAppend: true})

	l.failing = true
	stored, rejected := h.Append([]tsdb.SeriesRef{ref("m", "host:a")}, []Sample{{T: 10, V: 1}})
	if stored != nil || len(rejected) != 1 || rejected[0].Index != -1 || rejected[0].Err == nil {
		t.Fatalf("a failed fsync gave stored=%v rejected=%v, want the whole batch failed", stored, rejected)
	}
	if got := mustSelect(t, h, tsdb.Selector{Metric: "m"}, 0, math.MaxInt64); len(got) != 1 {
		t.Fatalf("the samples are applied before the fsync (visible first): got %v", got)
	}

	// A retry logs again and syncs again, and a success is a success.
	l.mu.Lock()
	l.failing = false
	logsBefore, syncsBefore := l.logs, l.syncs
	l.mu.Unlock()
	stored, rejected = h.Append([]tsdb.SeriesRef{ref("m", "host:a")}, []Sample{{T: 10, V: 1}})
	if len(rejected) != 0 || len(stored) != 1 {
		t.Fatalf("the retry: stored=%v rejected=%v", stored, rejected)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.logs != logsBefore+1 || l.syncs != syncsBefore+1 {
		t.Errorf("the retry logged %d and synced %d times, want 1 and 1", l.logs-logsBefore, l.syncs-syncsBefore)
	}
}

// Nothing logged, nothing to sync: a batch of samples that are all too old never reaches the log.
func TestHead_ABatchThatLoggedNothingDoesNotSync(t *testing.T) {
	l := newGatedLog()
	h := New(Options{WAL: l, BlockRange: 1000, SyncOnAppend: true})
	h.Truncate(10_000)
	h.Append([]tsdb.SeriesRef{ref("m")}, []Sample{{T: 5, V: 1}})
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.logs != 0 || l.syncs != 0 {
		t.Errorf("a batch refused as out of bounds logged %d and synced %d times", l.logs, l.syncs)
	}
}
