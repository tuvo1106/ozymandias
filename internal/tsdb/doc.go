// Package tsdb defines the MetricStore interface — append samples, select
// series by tag matchers — that every metric store implements. The naive
// SQLite store (M1) and the real engine built in M2 (WAL, Gorilla chunks,
// inverted index, blocks, compaction) sit behind it.
//
// Status: arrives in M1 (docs/plan/M1-metrics-tracer-bullet.md); this file marks its place in
// the architecture until then.
package tsdb
