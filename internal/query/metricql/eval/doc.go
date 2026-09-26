// Package eval runs a parsed metricql query against the stores.
//
// The pipeline is five stages, and the order is the whole point:
//
//  1. Plan. Resolve the dashboard's template variables, pick the bucket grid
//     every node will share, and look up each metric's type — which decides
//     how its samples reduce over time.
//  2. Select. Ask the store for the series of each query node's metric that
//     pass its matchers.
//  3. Time-aggregate. Reduce each series to one value per bucket, by the
//     method the metric's type implies or the one `.rollup()` names.
//  4. Space-aggregate. Group the series by the `by` keys and combine each
//     group bucket by bucket with the aggregator before the ':'.
//  5. Combine. Fill gaps, apply functions, then arithmetic between nodes.
//
// # Why time before space
//
// A dashboard reader expects "requests per second across the fleet" to be the
// sum of each host's rate, and "p95 latency" to describe the fleet rather than
// the average of per-host p95s. Reducing each series over time *first* and
// only then across series is what makes both come out that way. Doing it the
// other way round — combine the hosts at each sample, then bucket — gives a
// number that is not wrong so much as not about anything: it weights hosts by
// how often they happened to report.
//
// The percentile aggregators are the case that makes this concrete, and they
// cannot be done in the other order at all. Merging sketches is exact and a
// quantile of a merge is the fleet's quantile; a mean of quantiles is a
// different number with no error bound. See [Evaluator.Eval] and
// docs/adr/0015-sketch-storage-and-identity.md.
//
// # One grid for the whole expression
//
// Every node in one request is evaluated onto the same buckets, because
// arithmetic between two lines is pointwise and pointwise needs a shared
// x-axis. A `.rollup(method, seconds)` therefore sets the grid for the
// request, and two rollups asking for different widths is an error rather
// than a resampling — see docs/adr/0016-one-grid-per-query.md.
//
// # Nulls
//
// An empty bucket is NaN inside this package and null in JSON, never zero. A
// gap in a chart and a measured zero are different facts, and the difference
// is usually the one the reader cares about. Aggregators skip nulls; a bucket
// where every input was null stays null. `.fill()` is how a caller says it
// wants something else.
package eval
