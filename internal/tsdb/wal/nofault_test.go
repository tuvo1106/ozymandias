//go:build !faultinject

package wal

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A normal build ignores the fault environment entirely. The slow-disk drill sets it for an ozyd
// built with -tags faultinject; this says that the ozyd anyone runs is not such a build, so a stray
// OZY_FAULT_FSYNC_FILE in an environment slows nothing.
func TestFaultEnvIsIgnoredWithoutTheBuildTag(t *testing.T) {
	faults := filepath.Join(t.TempDir(), "faults")
	if err := os.WriteFile(faults, []byte("all 2s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OZY_FAULT_FSYNC_FILE", faults)
	w, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if err := w.Log(Record{Type: 1, Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("a Sync took %v with the fault file set: the production build reads it", d)
	}
}
