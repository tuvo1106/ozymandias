// Package aggregator turns a stream of individual statsd messages into one
// point per series per 10-second bucket: counters sum, gauges keep the last
// value, sets count distinct members, histograms and distributions summarise.
// Aggregating at the edge is what lets thousands of events per second become a
// handful of points on the wire.
//
// Status: arrives in M1 (docs/plan/M1-metrics-tracer-bullet.md); this file marks its place in
// the architecture until then.
package aggregator
