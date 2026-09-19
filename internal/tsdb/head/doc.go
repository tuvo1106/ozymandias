// Package head is the TSDB's mutable in-memory block: the most recent samples
// of every active series, appended in time order and cut into immutable on-
// disk blocks as they age.
//
// Status: arrives in M2 (docs/plan/M2-tsdb.md); this file marks its place in
// the architecture until then.
package head
