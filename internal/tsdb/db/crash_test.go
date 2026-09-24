package db

import (
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
)

// crashDirEnv both selects the child mode of this test binary and tells it
// where to build its database.
const crashDirEnv = "OZY_CRASH_DIR"

const (
	crashBase  = int64(1_600_000_000_000) // a fixed epoch: the child owns no clock
	crashBatch = 50                       // samples per Append, so one fsync covers 50
)

// TestDB_CrashLoop kills a real process, fifty times, at a moment it cannot
// prepare for.
//
// Every other crash test in this package simulates: it reopens a directory
// without closing it, or corrupts a file by hand. Those cover the states we
// thought of. This covers the ones we did not, by forking this same test
// binary, letting it append and cut and compact as fast as it can, and sending
// SIGKILL somewhere in the middle — no deferred Close, no final sync, no
// unwinding. Whatever is on disk afterwards is what a power cut would leave.
//
// The child writes one strictly increasing series, so the invariant is sharp:
// what comes back must be a *prefix* of what it wrote. A missing sample in the
// middle is a hole, a repeated one is a duplicate, and an extra one is data
// the store invented — a prefix check catches all three at once, without the
// parent having to know how far the child got.
func TestDB_CrashLoop(t *testing.T) {
	if os.Getenv(crashDirEnv) != "" {
		t.Skip("child process: TestDB_CrashChild is the one to run")
	}
	if testing.Short() {
		t.Skip("spawns 50 processes")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const iterations = 50
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // reproducible kill timings
	var nonEmpty, deepest int

	for i := 0; i < iterations; i++ {
		dir := t.TempDir()
		child := exec.Command(exe, "-test.run=^TestDB_CrashChild$", "-test.timeout=60s")
		child.Env = append(os.Environ(), crashDirEnv+"="+dir)
		child.Stdout, child.Stderr = nil, nil
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		// Wait for it to be up and writing; killing during startup would make
		// the iteration prove nothing.
		if !waitForFile(filepath.Join(dir, "ready"), 30*time.Second) {
			_ = child.Process.Kill()
			_ = child.Wait()
			t.Fatalf("iteration %d: the child never started writing", i)
		}
		time.Sleep(time.Duration(rng.Intn(35)+5) * time.Millisecond)
		if err := child.Process.Kill(); err != nil {
			t.Fatalf("iteration %d: kill: %v", i, err)
		}
		_ = child.Wait() // always "signal: killed"

		n := checkPrefix(t, i, dir)
		if n > 0 {
			nonEmpty++
		}
		if n > deepest {
			deepest = n
		}
	}
	// A loop where every child died before writing anything would pass every
	// assertion above and prove nothing at all.
	if nonEmpty < iterations*3/4 {
		t.Errorf("only %d of %d iterations recovered any data; the kills are landing too early",
			nonEmpty, iterations)
	}
	t.Logf("%d crashes, %d recovered data, deepest prefix %d samples", iterations, nonEmpty, deepest)
}

// checkPrefix reopens a killed child's directory and returns how many samples
// survived, failing if they are not exactly the child's first n.
func checkPrefix(t *testing.T, iteration int, dir string) int {
	t.Helper()
	db, err := Open(Options{Dir: dir, BlockRange: crashBlockRange, Retention: -1})
	if err != nil {
		t.Fatalf("iteration %d: reopening after the crash: %v", iteration, err)
	}
	defer func() { _ = db.Close() }()

	// Nothing half-written may be visible: an interrupted block write leaves a
	// .tmp directory, and Open is what is supposed to have swept it.
	entries, err := os.ReadDir(filepath.Join(dir, blocksDirName))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("iteration %d: %v", iteration, err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("iteration %d: %s survived the reopen", iteration, e.Name())
		}
	}

	set, err := db.Select(ctx, tsdb.Selector{Metric: "m"}, math.MinInt64, math.MaxInt64)
	if err != nil {
		t.Fatalf("iteration %d: %v", iteration, err)
	}
	n := 0
	for set.Next() {
		it := set.Iterator()
		for it.Next() {
			s := it.At()
			if s.T != crashBase+int64(n) || s.V != float64(n) {
				t.Fatalf("iteration %d: sample %d is (%d, %g), want (%d, %d) — the recovered log is not a prefix",
					iteration, n, s.T, s.V, crashBase+int64(n), n)
			}
			n++
		}
		if err := it.Err(); err != nil {
			t.Fatalf("iteration %d: %v", iteration, err)
		}
	}
	if err := set.Err(); err != nil {
		t.Fatalf("iteration %d: %v", iteration, err)
	}
	if err := set.Close(); err != nil {
		t.Fatalf("iteration %d: %v", iteration, err)
	}
	if n%crashBatch != 0 {
		t.Errorf("iteration %d: %d samples recovered, not a whole number of batches — a torn record was accepted",
			iteration, n)
	}
	// And it is a database again, not a museum piece.
	res, err := db.Append(ctx, []tsdb.SeriesSamples{{
		Series:  ref("m", "host:a"),
		Samples: []tsdb.Sample{{T: crashBase + int64(n) + 1_000_000, V: -1}},
	}})
	if err != nil || res.Samples != 1 {
		t.Fatalf("iteration %d: the reopened store will not take a sample: %v %+v", iteration, err, res)
	}
	return n
}

func waitForFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(200 * time.Microsecond)
	}
	return false
}

// crashBlockRange is short enough that the child cuts and compacts constantly,
// so a kill at a random moment has a real chance of landing inside one.
const crashBlockRange = 100 * time.Millisecond

// TestDB_CrashChild is the other half of TestDB_CrashLoop and is a no-op
// unless the parent selected it. It never returns: it is killed.
func TestDB_CrashChild(t *testing.T) {
	dir := os.Getenv(crashDirEnv)
	if dir == "" {
		t.Skip("run by TestDB_CrashLoop, not on its own")
	}
	db, err := Open(Options{
		Dir:        dir,
		BlockRange: crashBlockRange,
		// Wall-clock retention against 2020 timestamps would delete every
		// block as fast as it was cut, and there would be nothing to recover.
		Retention:     -1,
		MaxBlockRange: time.Second,
		SyncOnAppend:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Cut and compact in a tight loop rather than on the maintenance ticker,
	// so the few tens of milliseconds the child lives contain many of both.
	go func() {
		for {
			_ = db.CutBlock()
			_, _ = db.Compact()
		}
	}()

	r := ref("m", "host:a")
	for k := int64(0); ; k += crashBatch {
		samples := make([]tsdb.Sample, crashBatch)
		for j := range samples {
			samples[j] = tsdb.Sample{T: crashBase + k + int64(j), V: float64(k + int64(j))}
		}
		res, err := db.Append(ctx, []tsdb.SeriesSamples{{Series: r, Samples: samples}})
		if err != nil || res.Samples != crashBatch {
			t.Fatalf("child append at %d: %v %+v", k, err, res.Rejected)
		}
		if k == 0 {
			// Only now: the parent may kill us the instant this appears, and
			// an empty database would make its prefix check vacuous.
			if err := os.WriteFile(filepath.Join(dir, "ready"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}
