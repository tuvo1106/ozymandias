// Package selfmetrics is ozymandias's view of its own health: a tiny in-process
// registry of counters and gauges, named `ozymandias.*`, that every component
// updates as it works (packets received, points dropped, fsync latency, …).
//
// Principle 6 of the plan is "dogfood": the system should be observable with
// itself. In M0 the registry is only readable as JSON at GET /debug/vars on
// both binaries. From M1 the agent also flushes it through its own pipeline,
// so the ozymandias-self dashboard is built from exactly the same machinery as
// any app's.
//
// Why not expvar or a Prometheus client? expvar has no tags, and a Prometheus
// client library would be the one piece of metrics plumbing we didn't build —
// in a project whose point is building metrics plumbing. The registry is
// deliberately minimal: get-or-create by (name, tags), atomic updates, a
// sorted snapshot.
//
// Concurrency: all methods are safe for concurrent use. Counter.Add and
// Gauge.Set are lock-free (atomics); only registration takes the lock, so
// callers should look an instrument up once and keep the pointer.
package selfmetrics
