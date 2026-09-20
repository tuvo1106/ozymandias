// Package simple evaluates M1's structured metric query: one metric, tag
// filters, a group-by, and one aggregator. It is the tracer bullet's query
// engine; M3's query language (internal/query/metricql) replaces it and the
// /api/v1/query endpoint is re-implemented on top of that.
//
// A query runs in the same four steps every metrics system uses, and the
// order matters:
//
//  1. Select the series of the metric that pass the filters.
//  2. Aggregate each series over time into fixed buckets of `interval`
//     seconds. How depends on the metric's type: a count's buckets sum (ten
//     10-second counts make one 100-second count), a gauge's average (the
//     level over the bucket).
//  3. Group the series by the `by` tag keys.
//  4. Aggregate across the series of each group, bucket by bucket, with the
//     query's aggregator (avg, sum, min or max).
//
// Doing time before space is what makes "sum of rates" and "average of
// latencies" mean what a dashboard reader expects.
package simple
