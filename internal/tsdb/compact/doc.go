// Package compact merges small adjacent blocks into larger ones, writes
// downsampled rollups, and enforces retention by time and size.
//
// Status: arrives in M2 (docs/plan/M2-tsdb.md); this file marks its place in
// the architecture until then.
package compact
