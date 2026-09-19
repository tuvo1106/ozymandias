// Package forwarder ships batches from the agent to ozyd: split, gzip,
// POST, retry with jittered exponential backoff on retryable failures, bounded
// memory with drop-oldest, and (M7) spill to disk across outages.
//
// Status: arrives in M1 (docs/plan/M1-metrics-tracer-bullet.md); this file marks its place in
// the architecture until then.
package forwarder
