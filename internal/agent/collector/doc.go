// Package collector runs the agent's pull-based collectors: things the agent
// goes and reads on a timer, as opposed to the statsd metrics applications
// push to it.
//
// # The model
//
// A [Collector] reads one source (the host's kernel counters, the Docker
// daemon, one configured check instance) and emits [Metric] values. The
// [Scheduler] runs each collector on its own goroutine and interval, adds
// the host tag, the agent's global tags and a timestamp, and hands each
// run's batch to a sink — the forwarder, directly. Collector output does not
// go through the statsd aggregator: the aggregator exists to combine many
// samples of the same series arriving within a bucket, and a collector
// produces exactly one value per series per run already.
//
// # Gauges, rates and counters
//
// Most of what a machine exposes is cumulative: bytes received since boot,
// CPU seconds since the container started. A cumulative value on its own
// answers no useful question, so collectors turn two readings into a
// per-second [Rate] with [Rates], which also handles the three awkward cases
// — the first reading, a counter reset, and a source that disappears. This
// is the same job Prometheus's rate() does at query time; here it happens at
// collection, so what is stored is already the rate. The trade: the raw
// counter is gone, so a different window cannot be recomputed later, but
// every query is cheap and the store never sees a reset.
//
// # Failure
//
// A collector failing is expected (a check pointed at a service that is
// down), so a failed run is counted and logged once — on the transition into
// failure and on recovery — never fatal, and never holds up another
// collector. A run that exceeds its timeout has its context cancelled; what
// it emitted before then is still sent. The agent's own metrics under
// ozy.agent.collector.* (runs, errors, timeouts, points, dropped, duration)
// say which collector is struggling.
//
// # What lives where
//
//   - host: CPU, load, memory, swap, disks, disk I/O, network, uptime
//     (gopsutil). In compose these describe the Docker VM, not the Mac.
//   - docker, checks and autodiscovery: M3 §3, later PRs.
//
// The agent's own runtime and pipeline metrics are not a collector: they are
// instruments in the selfmetrics registry, reported with every flush.
package collector
