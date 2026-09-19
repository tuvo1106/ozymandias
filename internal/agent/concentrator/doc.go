// Package concentrator computes request, error and latency (RED) metrics from
// 100% of spans, before any sampling — so a service's rates stay exact even
// when only a fraction of its traces are kept.
//
// Status: arrives in M5 (docs/plan/M5-tracing.md); this file marks its place in
// the architecture until then.
package concentrator
