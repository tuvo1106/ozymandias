// Package logstore is a Loki-style log store: logs grouped into low-
// cardinality streams, stored as compressed chunks, found by label index plus
// a scan — then accelerated with per-block bloom filters.
//
// Status: arrives in M4 (docs/plan/M4-logs.md); this file marks its place in
// the architecture until then.
package logstore
