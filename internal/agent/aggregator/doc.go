// Package aggregator turns a stream of individual measurements into one
// point per series per bucket (10 seconds by default). Aggregating at the
// edge is what lets thousands of events per second become a handful of
// points on the wire: the agent sends "42 requests in these 10 seconds", not
// 42 messages.
//
// Each series ("context") is a metric name plus a sorted, de-duplicated tag
// set — the agent adds its host and configured tags, and normalizes
// everything, so `route:/A` and `route:/a` in any order are one series. How a
// bucket combines its samples depends on the type:
//
//	counter    Σ value/sample_rate            → count
//	gauge      the last value                 → gauge
//	set        the number of distinct members → gauge
//	histogram  avg, min, max, median, p95 as gauges, and .count as a count
//
// Three rules carry most of the subtlety:
//
//   - Buckets close on wall-clock boundaries, and a bucket is emitted once.
//     A sample that arrives after its bucket was flushed is counted in the
//     oldest open bucket instead — shifting it slightly in time rather than
//     re-emitting a timestamp the store would treat as an overwrite.
//   - A counter that stops receiving data keeps emitting 0 until its context
//     expires, because "no errors" and "no data" are different facts. A
//     gauge is never invented.
//   - A histogram keeps at most histogram_max_samples values per bucket (a
//     uniform reservoir beyond that); min, max, sum and count stay exact.
package aggregator
