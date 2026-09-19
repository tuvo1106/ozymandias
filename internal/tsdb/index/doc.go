// Package index is the inverted index from tag pairs to series ids (posting
// lists), and the set operations — intersection, union, negation — that turn a
// query's tag matchers into the series to read.
//
// Status: arrives in M2 (docs/plan/M2-tsdb.md); this file marks its place in
// the architecture until then.
package index
