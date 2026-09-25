package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// T is the subset of testing.TB the helpers use. *testing.T and *testing.B
// satisfy it; so does a recording fake in this package's own tests.
type T interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Cleanup(func())
	TempDir() string
}

// pollInterval is how often Eventually re-checks. Short enough that a passing
// condition costs little wall time, long enough not to spin a core.
const pollInterval = 5 * time.Millisecond

// Eventually polls cond until it returns true, failing the test with msg (a
// fmt format plus args) if timeout passes first. It is for conditions that
// depend on real concurrency outside the test's control — a server goroutine,
// a socket. For anything driven by time, use [FakeClock] instead.
func Eventually(t T, timeout time.Duration, cond func() bool, msg string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %v: %s", timeout, fmt.Sprintf(msg, args...))
			return
		}
		time.Sleep(pollInterval)
	}
}

// WriteFiles creates each file under dir (making parent directories as
// needed) and fails the test on any error. Keys are slash-separated relative
// paths; values are file contents.
func WriteFiles(t T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
			return
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
			return
		}
	}
}

// TempDirWith returns a fresh temporary directory (removed after the test)
// populated with files, as for [WriteFiles].
func TempDirWith(t T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	WriteFiles(t, dir, files)
	return dir
}
