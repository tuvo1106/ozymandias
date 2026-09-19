// Package statsd is the agent's extended StatsD server: a UDP listener on :8125 and
// a parser for the datagram format in docs/wire-protocol.md §A. UDP because an
// app must never block or fail because its metrics can't be delivered; the
// price is that packets can be dropped, which the agent counts.
//
// Status: arrives in M1 (docs/plan/M1-metrics-tracer-bullet.md); this file marks its place in
// the architecture until then.
package statsd
