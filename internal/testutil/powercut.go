package testutil

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// SyncTracker stands in for a log's file fsync (wal.Options.SyncFile) and remembers how long each
// file was at its last successful fsync. Those lengths are what a power cut leaves: bytes written
// after them were only in the page cache.
//
// A process kill (kill -9) loses none of those bytes, since the kernel still has them, so no drill
// that kills the process can tell a missing fsync from a present one. [SyncTracker.PowerCut]
// can: it builds the directory a power cut would have left, and a test opens that.
//
// It models the file contents only. It does not model the directory entry of a file whose creation
// was never synced (the WAL syncs its directory itself when it creates a segment), and a real disk
// may keep some of the unsynced bytes: PowerCut's extra says how many.
type SyncTracker struct {
	mu     sync.Mutex
	synced map[string]int64 // by file name, which is unique within one log's directory
}

// NewSyncTracker returns a tracker with nothing synced.
func NewSyncTracker() *SyncTracker { return &SyncTracker{synced: map[string]int64{}} }

// SyncFile fsyncs f and, if that worked, records its length as durable.
func (st *SyncTracker) SyncFile(f *os.File) error {
	if err := f.Sync(); err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	st.mu.Lock()
	st.synced[filepath.Base(f.Name())] = info.Size()
	st.mu.Unlock()
	return nil
}

// Syncs is how many files have ever been fsynced, for a test that wants to know the hook is wired.
func (st *SyncTracker) Syncs() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.synced)
}

// PowerCut returns a copy of src as a power cut at this instant would leave it. Every file under
// logDir (the directory the tracked log writes to) is cut back to its length at its last fsync, or
// to nothing if it never had one, then given up to extra of the bytes written after that: 0 is the
// worst case, a large number is a disk that happened to keep everything. Everything else in src is
// copied whole, so a test must not let anything else under src matter: a block or a checkpoint
// written outside the log is the code under test's own fsyncs, which this does not see.
//
// Take the copy from a running store. Close syncs, and a store closed first has nothing to lose.
func (st *SyncTracker) PowerCut(t testing.TB, src, logDir string, extra int64) string {
	t.Helper()
	dst := t.TempDir()
	st.mu.Lock()
	defer st.mu.Unlock()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if inDir(path, logDir) {
			keep := st.synced[d.Name()] + extra // never synced: nothing durable
			if keep < int64(len(b)) {
				b = b[:keep]
			}
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func inDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
