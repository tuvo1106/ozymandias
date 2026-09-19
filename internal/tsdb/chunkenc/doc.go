// Package chunkenc implements Gorilla-style compression for time series:
// delta-of-delta encoded timestamps and XOR-encoded float values, packing a
// realistic sample into one or two bytes.
//
// Status: arrives in M2 (docs/plan/M2-tsdb.md); this file marks its place in
// the architecture until then.
package chunkenc
