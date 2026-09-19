package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder is a fake T that records failures instead of failing, so each
// helper can be tested on both its passing and its failing path.
type recorder struct {
	t        *testing.T
	mu       sync.Mutex
	errors   []string
	fatals   []string
	cleanups []func()
}

func newRecorder(t *testing.T) *recorder { return &recorder{t: t} }

func (r *recorder) Helper() {}
func (r *recorder) Errorf(f string, a ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, fmt.Sprintf(f, a...))
}
func (r *recorder) Fatalf(f string, a ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fatals = append(r.fatals, fmt.Sprintf(f, a...))
}
func (r *recorder) Cleanup(f func()) { r.cleanups = append(r.cleanups, f) }
func (r *recorder) TempDir() string  { return r.t.TempDir() }

// runCleanups runs registered cleanups last-in first-out, like testing.T.
func (r *recorder) runCleanups() {
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i]()
	}
}

func TestEventually_ReturnsOnceConditionHolds(t *testing.T) {
	r := newRecorder(t)
	calls := 0
	Eventually(r, time.Second, func() bool { calls++; return calls == 3 }, "never")
	if len(r.fatals) != 0 || calls != 3 {
		t.Fatalf("fatals=%v calls=%d, want none and 3", r.fatals, calls)
	}
}

func TestEventually_FailsWithMessageAfterTimeout(t *testing.T) {
	r := newRecorder(t)
	Eventually(r, 20*time.Millisecond, func() bool { return false }, "waiting for %s", "godot")
	if len(r.fatals) != 1 || !strings.Contains(r.fatals[0], "waiting for godot") {
		t.Fatalf("fatals = %v, want one mentioning the message", r.fatals)
	}
}

func TestTempDirWith_WritesNestedFiles(t *testing.T) {
	dir := TempDirWith(t, map[string]string{"a.yaml": "x: 1", "conf.d/b.yaml": "y: 2"})
	for name, want := range map[string]string{"a.yaml": "x: 1", "conf.d/b.yaml": "y: 2"} {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
}

func TestWriteFiles_FailsWhenPathIsBlocked(t *testing.T) {
	r := newRecorder(t)
	dir := t.TempDir()
	// A regular file where a directory is needed makes MkdirAll fail.
	if err := os.WriteFile(filepath.Join(dir, "blocker"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	WriteFiles(r, dir, map[string]string{"blocker/child.txt": "x"})
	if len(r.fatals) != 1 {
		t.Fatalf("fatals = %v, want one", r.fatals)
	}
}

func TestWriteFiles_FailsWhenFileIsUnwritable(t *testing.T) {
	r := newRecorder(t)
	dir := t.TempDir()
	// The target name already exists as a directory, so WriteFile fails.
	if err := os.Mkdir(filepath.Join(dir, "taken"), 0o755); err != nil {
		t.Fatal(err)
	}
	WriteFiles(r, dir, map[string]string{"taken": "x"})
	if len(r.fatals) != 1 {
		t.Fatalf("fatals = %v, want one", r.fatals)
	}
}

func TestCheckGoroutines_PassesWhenNothingLeaks(t *testing.T) {
	r := newRecorder(t)
	CheckGoroutines(r)
	done := make(chan struct{})
	go func() { close(done) }()
	<-done
	r.runCleanups()
	if len(r.errors) != 0 {
		t.Fatalf("errors = %v, want none", r.errors)
	}
}

// Guards against a leak checker that never fires — the failure mode that
// makes every "no leaks" assertion in the codebase meaningless.
func TestCheckGoroutines_ReportsLeakedGoroutine(t *testing.T) {
	r := newRecorder(t)
	CheckGoroutines(r)
	stop := make(chan struct{})
	go leakyWorker(stop)
	r.runCleanups() // waits the full grace period, then reports
	close(stop)
	if len(r.errors) != 1 || !strings.Contains(r.errors[0], "leakyWorker") {
		t.Fatalf("errors = %v, want one naming leakyWorker", r.errors)
	}
}

func leakyWorker(stop <-chan struct{}) { <-stop }

func TestGoroutineID_RejectsMalformedHeaders(t *testing.T) {
	for _, s := range []string{"", "not a stack", "goroutine", "goroutine x [running]:"} {
		if _, ok := goroutineID(s); ok {
			t.Errorf("goroutineID(%q) ok = true, want false", s)
		}
	}
	if id, ok := goroutineID("goroutine 42 [chan receive]:\nmain.f()"); !ok || id != 42 {
		t.Errorf("goroutineID = %d, %v; want 42, true", id, ok)
	}
}

func TestLeakedStacks_IgnoresKnownAndFrameworkGoroutines(t *testing.T) {
	now := []string{
		"goroutine 1 [running]:\nmain.old()",
		"goroutine 2 [running]:\ntesting.tRunner(...)",
		"goroutine 3 [running]:\nmain.new()",
		"garbage",
	}
	got := leakedStacks(map[int]bool{1: true}, now)
	if len(got) != 1 || !strings.Contains(got[0], "main.new") {
		t.Fatalf("leaked = %v, want only goroutine 3", got)
	}
}
