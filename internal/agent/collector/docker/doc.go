// Package docker reports the containers on the agent's Docker daemon:
// resource use per container (container.*), the number running per image
// (docker.containers.running), and exits and lifetimes from the daemon's
// event stream (docs/metrics-catalog.md).
//
// # Two sources, because polling misses things
//
// The [Collector] polls: every interval it lists the running containers and
// asks the daemon for each one's stats. That is the right shape for levels
// (memory, pids) and for rates computed from cumulative counters (network,
// block I/O, throttling), but a container that lives two seconds starts and
// dies between two polls and is never seen. The [Watcher] follows the event
// stream instead, where every start and die is delivered, and turns each die
// into container.exits and container.lifetime. Its samples go to the statsd
// aggregator, which is built for values that arrive one at a time at any
// moment; the collector's go through the scheduler, one value per series
// per run.
//
// # Tags, and keeping them few
//
// A container is tagged by name, short id, image name and tag, and its
// compose project and service. Each distinct set is a series, so containers
// that are many and short-lived by design (a sandbox per job) would bury a
// metric under one series each. A [Rewrite] folds them into one name and
// drops the id; containers that end up with the same tags are combined —
// amounts summed, uptime and the memory limit the largest — rather than
// sent as duplicate points that would overwrite each other.
//
// # Choices worth knowing
//
//   - Stats are read one-shot (ADR-0030): one sample, answered at once,
//     where the default makes the daemon sample CPU twice a second apart.
//     CPU % is therefore taken between this run's sample and the last, so
//     it first appears on a container's second run, and again two runs
//     after it restarts in place (the watcher's start event resets its
//     baselines). Calls run in parallel, bounded by MaxConcurrency, which
//     bounds the load on the daemon.
//   - A container that stops between the list and its stats call is skipped
//     silently: that race is normal, not an error.
//   - Rates (network, I/O, throttled periods) are per second, sent as gauges
//     (ADR-0026), keyed by container id so a rewritten name that covers
//     several containers still differences each one's own counter.
//   - The event stream resumes from the last event seen after a disconnect,
//     and skips the events at that time the daemon delivers again (since is
//     inclusive), so a resume does not double-count exits. It cannot recover
//     what the daemon no longer has: the daemon replays from a bounded
//     in-memory buffer (256 events), which its own restart empties, so exits
//     during a daemon restart, or beyond the buffer in a long disconnect,
//     are lost. container.exits is a floor, not an audit log.
package docker
