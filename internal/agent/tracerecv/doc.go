// Package tracerecv receives trace chunks from SDKs on POST /v1/traces,
// normalises every span (truncation, defaults, clock clamping) and hands them
// to the stats concentrator and the samplers.
//
// Status: arrives in M5 (docs/plan/M5-tracing.md); this file marks its place in
// the architecture until then.
package tracerecv
