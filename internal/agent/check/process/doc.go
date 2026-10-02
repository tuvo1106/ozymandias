// Package process is the process check: it finds the processes an instance
// describes — by exact name, by a regular expression over the command line,
// or by a pid file — and reports how many there are and what they use
// together (system.processes.*, docs/metrics-catalog.md).
//
// # Summing over matches
//
// A service is often several processes (a pre-forking server, workers), and
// which pids they have changes on every restart, so the check reports the
// set, not each pid: the count, and the sums of CPU, resident memory, open
// file descriptors and threads, all tagged process_name:<label>.
//
// CPU is a rate, and a sum of rates is taken per pid: each process's CPU
// seconds are differenced against its own previous reading (collector.Rates,
// keyed by pid), then the per-second values are added. Differencing the sum
// instead would be wrong whenever the set changes — a worker that exits
// takes its CPU seconds with it and the sum drops, which is not negative
// usage, and a new one arrives with all the seconds it has used so far. A
// process seen for the first time contributes no CPU until its second
// reading, so the first run after start reports no system.processes.cpu.pct.
//
// # What the agent can see
//
// Only the processes in its own pid namespace. In compose that is the
// agent's container, so the check is useful there only with `pid: host`
// (which lets it see, and read the command lines of, every process on the
// Docker VM). Natively (`make dev`) it sees the Mac's processes, but reading
// another user's file descriptors needs privileges: when any matched
// process's count cannot be read, open_file_descriptors is left out rather
// than reported as a partial sum.
package process
