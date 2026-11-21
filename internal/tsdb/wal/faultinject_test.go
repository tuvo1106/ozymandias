//go:build faultinject

package wal

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
		{"a matching rule that does not parse means no delay", "/queue/ soon\n", "/d/queue/a/1.wal", 0},
		{"a zero duration exempts a log from a later all", "/queue/ 0s\nall 5s\n", "/d/queue/a/1.wal", 0},
		{"an unparseable rule exempts it too", "/queue/ soon\nall 5s\n", "/d/queue/a/1.wal", 0},
		{"a negative duration is no delay", "/queue/ -1s\n", "/d/queue/a/1.wal", 0},
		{"a rule that does not match is passed over", "/logs/ 0s\nall 5s\n", "/d/queue/a/1.wal", 5 * time.Second},
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

// What a log's fsync is asked to sleep, seen through a real log: the sleeps are recorded, not waited
// for, so nothing here depends on how fast the disk is. The rules are read on every fsync, not once
// (a drill turns slowness on and off in a running ozyd).
func TestFaultSyncFile_SleepsByTheRulesAtTheTimeOfTheFsync(t *testing.T) {
	faults := filepath.Join(t.TempDir(), "faults")
	var slept []time.Duration
	w, err := Open(Options{
		Dir:      filepath.Join(t.TempDir(), "queue", "series"),
		SyncFile: faultSyncFile(faults, func(d time.Duration) { slept = append(slept, d) }),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	syncAfter := func(rules string) []time.Duration {
		t.Helper()
		if rules == "" {
			_ = os.Remove(faults)
		} else if err := os.WriteFile(faults, []byte(rules), 0o644); err != nil {
			t.Fatal(err)
		}
		slept = nil
		if err := w.Log(Record{Type: 1, Data: []byte("x")}); err != nil {
			t.Fatal(err)
		}
		if err := w.Sync(); err != nil {
			t.Fatal(err)
		}
		return slept
	}
	for _, tc := range []struct {
		name, rules string
		want        []time.Duration
	}{
		{"no fault file", "", nil},
		{"a rule for this log", "/queue/ 250ms\n", []time.Duration{250 * time.Millisecond}},
		{"a rule for another log", "/tsdb/wal/ 250ms\n", nil},
		{"the file removed again", "", nil},
		{"a changed rule is seen at the next fsync", "/queue/ 3s\n", []time.Duration{3 * time.Second}},
	} {
		if got := syncAfter(tc.rules); !slices.Equal(got, tc.want) {
			t.Fatalf("%s: slept %v, want %v", tc.name, got, tc.want)
		}
	}
}

// With the tag, the environment decides at Open which fsync a log gets; without it, nothing does.
func TestDefaultSyncFile_ReadsTheEnvironmentWhenALogIsOpened(t *testing.T) {
	t.Setenv(faultEnv, "")
	if reflect.ValueOf(defaultSyncFile()).Pointer() != reflect.ValueOf((*os.File).Sync).Pointer() {
		t.Fatal("with no fault file named, a log did not get the file's own fsync")
	}
	t.Setenv(faultEnv, filepath.Join(t.TempDir(), "faults"))
	if reflect.ValueOf(defaultSyncFile()).Pointer() == reflect.ValueOf((*os.File).Sync).Pointer() {
		t.Fatal("with a fault file named, a log still got the file's own fsync")
	}
}
