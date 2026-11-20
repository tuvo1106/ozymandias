//go:build faultinject

package wal

import (
	"os"
	"strings"
	"time"
)

// This file is compiled only with -tags faultinject, which scripts/chaos/slow-disk.sh builds ozyd
// with. A normal build has syncdefault.go instead: no code in it looks at the file named below.
//
// It makes the disk slow, which nothing outside the process can do here (dm-delay needs Linux and
// root, and there is no FUSE on a Mac). Every log fsync (the queue's, the TSDB's, the log store's,
// the agent's spool) first sleeps for whatever the fault file says for that log's path. The file is
// read on every fsync, so a drill turns slowness on and off while ozyd runs, and one log can be
// slow while another is not: that is how the drill slows the stores and not the queue.
//
// The fault file is named by OZY_FAULT_FSYNC_FILE, read when a log is opened. It holds one rule per
// line, "<path-substring> <duration>" ("all" matches every log). The first rule whose substring
// matches decides, and a duration that does not parse or is not positive means no delay: that is how
// one log is exempted from a later "all". A missing file, a blank line or a line that is not two
// fields costs nothing. Pebble's fsyncs (traces, sketches) and files written outside a log are not
// slowed.
const faultEnv = "OZY_FAULT_FSYNC_FILE"

// defaultSyncFile is the fsync a log uses when its Options name none: the file's own, slowed by the
// fault file if the environment names one.
func defaultSyncFile() func(*os.File) error {
	path := os.Getenv(faultEnv)
	if path == "" {
		return (*os.File).Sync
	}
	return faultSyncFile(path, time.Sleep)
}

// faultSyncFile is a file fsync that first calls sleep with the delay the rules at path give for the
// file. sleep is a parameter so that a test can see what was asked for without waiting for it.
func faultSyncFile(path string, sleep func(time.Duration)) func(*os.File) error {
	return func(f *os.File) error {
		if d := faultDelay(path, f.Name()); d > 0 {
			sleep(d)
		}
		return f.Sync()
	}
}

// faultDelay is how long an fsync of the log file name sleeps, by the rules in the file at path.
func faultDelay(path, name string) time.Duration {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || (fields[0] != "all" && !strings.Contains(name, fields[0])) {
			continue
		}
		d, err := time.ParseDuration(fields[1])
		if err != nil || d <= 0 {
			return 0 // the first matching rule decides, and this one says no delay
		}
		return d
	}
	return 0
}
