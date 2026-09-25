// Package tsdb defines the MetricStore interface — append samples, select
// series by tag matchers, list names and tags — that every metric store
// implements, together with the reference semantics of its matchers.
//
// Everything above storage (intake, query, monitors) knows only this
// interface. M1's naive SQLite store (package naive) sits behind it now; the
// real engine built in M2 (WAL, Gorilla chunks, inverted index, blocks,
// compaction) will replace it without anything above noticing, and M2's
// differential tests hold the new engine to the naive store's answers.
//
// Identity is the metric name plus a sorted, de-duplicated tag set
// (SeriesRef.Key). Samples are (unix milliseconds, float64); a sample at an
// existing timestamp replaces the old one.
package tsdb
