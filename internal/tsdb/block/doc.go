// Package block writes and reads immutable on-disk TSDB blocks: a time range
// of compressed chunks plus the index that locates them, written atomically so
// a crash can never expose a half-written block.
//
// Status: arrives in M2 (docs/plan/M2-tsdb.md); this file marks its place in
// the architecture until then.
package block
