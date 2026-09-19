// Package naive is the deliberately simple SQLite MetricStore: the M1 tracer
// bullet's storage, and afterwards the permanent oracle the real TSDB is
// differentially tested against.
//
// Status: arrives in M1 (docs/plan/M1-metrics-tracer-bullet.md); this file marks its place in
// the architecture until then.
package naive
