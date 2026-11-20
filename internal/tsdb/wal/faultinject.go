//go:build faultinject

package wal

import (
	"os"
	"strings"
	"time"
)

// This file is compiled only with -tags faultinject, which scripts/chaos/slow-disk.sh builds ozyd
// with. A normal build has none of it: the variable below is not read, and nothing in the binary
// looks at the file it names.
//
// It makes the disk slow, which nothing outside the process can do here (dm-delay needs Linux and
// root, and there is no FUSE on a Mac). Every log fsync (the queue's, the TSDB's, the log store's,
// the agent's spool) first sleeps for whatever the fault file says for that log's path. The file is
// read on every fsync, so a drill turns slowness on and off while ozyd runs, and one log can be
// slow while another is not: that is how the drill slows the stores and not the queue.
//
// The fault file is named by OZY_FAULT_FSYNC_FILE, one rule per line, "<path-substring> <duration>"
// ("all" matches every log), the first rule that matches wins; a missing file, a blank line or a
// rule that does not parse costs nothing. Pebble's fsyncs (traces, sketches) and the files written
// outside a log are not slowed.
const faultEnv = "OZY_FAULT_FSYNC_FILE"

func init() {
	if path := os.Getenv(faultEnv); path != "" {
		installFaults(path)
	}
}

// installFaults makes every log's default fsync sleep first, by the rules in the file at path.
func installFaults(path string) {
	real := defaultSyncFile
	defaultSyncFile = func(f *os.File) error {
		time.Sleep(faultDelay(path, f.Name()))
		return real(f)
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
		if d, err := time.ParseDuration(fields[1]); err == nil && d > 0 {
			return d
		}
	}
	return 0
}
