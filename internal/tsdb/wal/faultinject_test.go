//go:build faultinject

package wal

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFaultDelay_RulesByPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "faults")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name  string
		rules string
		file  string
		want  time.Duration
	}{
		{"no rules", "", "/d/queue/series/1.wal", 0},
		{"a substring match", "/queue/ 300ms\n", "/d/queue/series/1.wal", 300 * time.Millisecond},
		{"another path is not slowed", "/queue/ 300ms\n", "/d/tsdb/wal/1.wal", 0},
		{"all matches everything", "all 2s\n", "/d/tsdb/wal/1.wal", 2 * time.Second},
		{"the first matching rule wins", "/tsdb/wal/ 1s\nall 5s\n", "/d/tsdb/wal/1.wal", time.Second},
		{"a later rule applies when the first does not match", "/queue/ 1s\nall 5s\n", "/d/logs/wal/1.wal", 5 * time.Second},
		{"a rule that does not parse is skipped", "/queue/ soon\n", "/d/queue/a/1.wal", 0},
		{"a zero or negative duration is no delay", "/queue/ -1s\n", "/d/queue/a/1.wal", 0},
		{"blank lines and extra fields are skipped", "\n/queue/ 1s extra\n  \n", "/d/queue/a/1.wal", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write(tc.rules)
			if got := faultDelay(path, tc.file); got != tc.want {
				t.Fatalf("faultDelay = %v, want %v", got, tc.want)
			}
		})
	}
	if got := faultDelay(filepath.Join(t.TempDir(), "absent"), "/d/queue/a/1.wal"); got != 0 {
		t.Fatalf("a missing fault file delayed by %v", got)
	}
}

// The rules are read on every fsync, not once: a drill turns slowness on and off in a running
// ozyd. This goes through the code init runs and a real log, not a copy of it.
func TestInstallFaults_SlowsALogAndIsReReadOnEveryFsync(t *testing.T) {
	saved := defaultSyncFile
	t.Cleanup(func() { defaultSyncFile = saved })
	faults := filepath.Join(t.TempDir(), "faults")
	installFaults(faults)

	dir := filepath.Join(t.TempDir(), "queue", "series")
	w, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	sync := func() time.Duration {
		t.Helper()
		if err := w.Log(Record{Type: 1, Data: []byte("x")}); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if err := w.Sync(); err != nil {
			t.Fatal(err)
		}
		return time.Since(start)
	}
	if d := sync(); d > 100*time.Millisecond {
		t.Fatalf("a fsync with no fault file took %v", d)
	}
	if err := os.WriteFile(faults, []byte("/queue/ 250ms\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if d := sync(); d < 250*time.Millisecond {
		t.Fatalf("a fsync after the fault file appeared took only %v", d)
	}
	if err := os.WriteFile(faults, []byte("/tsdb/wal/ 250ms\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if d := sync(); d > 100*time.Millisecond {
		t.Fatalf("a rule for another path slowed this log: %v", d)
	}
	if err := os.WriteFile(faults, []byte("/queue/ 250ms\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(faults); err != nil {
		t.Fatal(err)
	}
	if d := sync(); d > 100*time.Millisecond {
		t.Fatalf("a fsync after the fault file was removed still took %v", d)
	}
}
