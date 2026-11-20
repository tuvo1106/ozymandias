package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, f *os.File, s string) {
	t.Helper()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The tracker is what every power-cut test stands on, so it is tested first: a PowerCut that kept
// bytes it should have dropped would make each of those tests pass for no reason.
func TestPowerCut_KeepsOnlyWhatWasSynced(t *testing.T) {
	src := t.TempDir()
	logDir := filepath.Join(src, "wal")
	if err := os.Mkdir(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	st := NewSyncTracker()
	seg, err := os.Create(filepath.Join(logDir, "00000001.wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = seg.Close() }()
	never, err := os.Create(filepath.Join(logDir, "00000002.wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = never.Close() }()
	if err := os.WriteFile(filepath.Join(src, "meta.json"), []byte("outside the log"), 0o644); err != nil {
		t.Fatal(err)
	}

	write(t, seg, "synced|")
	if err := st.SyncFile(seg); err != nil {
		t.Fatal(err)
	}
	write(t, seg, "unsynced")
	write(t, never, "never synced")

	for _, tc := range []struct {
		extra    int64
		seg, nev string
	}{
		{0, "synced|", ""},
		{3, "synced|uns", "nev"},
		{1000, "synced|unsynced", "never synced"},
	} {
		cut := st.PowerCut(t, src, logDir, tc.extra)
		if got := read(t, filepath.Join(cut, "wal", "00000001.wal")); got != tc.seg {
			t.Errorf("extra %d: synced file = %q, want %q", tc.extra, got, tc.seg)
		}
		if got := read(t, filepath.Join(cut, "wal", "00000002.wal")); got != tc.nev {
			t.Errorf("extra %d: never-synced file = %q, want %q", tc.extra, got, tc.nev)
		}
		if got := read(t, filepath.Join(cut, "meta.json")); got != "outside the log" {
			t.Errorf("extra %d: a file outside the log was cut: %q", tc.extra, got)
		}
	}
	if st.Syncs() != 1 {
		t.Errorf("Syncs = %d, want 1 file synced", st.Syncs())
	}
	// A later fsync moves the line.
	if err := st.SyncFile(seg); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(st.PowerCut(t, src, logDir, 0), "wal", "00000001.wal")); got != "synced|unsynced" {
		t.Errorf("after a second fsync = %q", got)
	}
}

// A write that lands while the fsync runs is not claimed as durable: the length is read first.
func TestSyncTracker_ABytesWrittenDuringTheFsyncIsNotClaimed(t *testing.T) {
	src := t.TempDir()
	seg, err := os.Create(filepath.Join(src, "1.wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = seg.Close() }()
	st := NewSyncTracker()
	st.betweenStatAndSync = func() { write(t, seg, "-late") }
	write(t, seg, "early")
	if err := st.SyncFile(seg); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(st.PowerCut(t, src, src, 0), "1.wal")); got != "early" {
		t.Fatalf("a power cut kept %q, want only what was written before the fsync began", got)
	}
}

// Two files with one name in different directories are tracked apart.
func TestSyncTracker_KeysByPathNotName(t *testing.T) {
	src := t.TempDir()
	if err := os.Mkdir(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	a, _ := os.Create(filepath.Join(src, "1.wal"))
	b, _ := os.Create(filepath.Join(src, "sub", "1.wal"))
	defer func() { _ = a.Close(); _ = b.Close() }()
	st := NewSyncTracker()
	write(t, a, "aaaa")
	write(t, b, "bb")
	if err := st.SyncFile(a); err != nil { // only the outer one is synced
		t.Fatal(err)
	}
	cut := st.PowerCut(t, src, src, 0)
	if got := read(t, filepath.Join(cut, "1.wal")); got != "aaaa" {
		t.Errorf("the synced file = %q", got)
	}
	if got := read(t, filepath.Join(cut, "sub", "1.wal")); got != "" {
		t.Errorf("a same-named file that was never synced kept %q", got)
	}
}

// fatalRecorder is a testing.TB that records Fatalf and panics out of it, as FailNow would leave the caller.
type fatalRecorder struct {
	testing.TB
	fatal bool
}

func (f *fatalRecorder) Helper() {}
func (f *fatalRecorder) Fatalf(string, ...any) {
	f.fatal = true
	panic("fatal")
}

func TestPowerCut_RefusesANegativeExtra(t *testing.T) {
	ft := &fatalRecorder{}
	func() {
		defer func() { _ = recover() }()
		NewSyncTracker().PowerCut(ft, t.TempDir(), t.TempDir(), -1)
	}()
	if !ft.fatal {
		t.Fatal("a negative extra was accepted")
	}
}

func TestInDir(t *testing.T) {
	for _, tc := range []struct {
		path, dir string
		want      bool
	}{
		{"/a/wal/1.wal", "/a/wal", true},
		{"/a/wal/sub/1.wal", "/a/wal", true},
		{"/a/meta.json", "/a/wal", false},
		{"/a/wal2/1.wal", "/a/wal", false},
		{"/a/walfile", "/a/wal", false},
	} {
		if got := inDir(tc.path, tc.dir); got != tc.want {
			t.Errorf("inDir(%q, %q) = %v", tc.path, tc.dir, got)
		}
	}
}
