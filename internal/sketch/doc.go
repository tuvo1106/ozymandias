// Package sketch implements DDSketch, a mergeable quantile sketch with a
// relative-error guarantee.
//
// # The mental model
//
// The naive way to serve p95 is to keep every value and sort. That is exact
// and it does not survive contact with a fleet: a thousand hosts reporting a
// latency every ten seconds is millions of float64s an hour, and the numbers
// have to be kept, not just counted, because a percentile is a property of the
// whole population.
//
// The usual escape is to compute p95 per host and store the answer. That is
// wrong, and wrong in a way that looks fine on a dashboard: **percentiles do
// not average**. The p95 of ten hosts' p95s is not the fleet's p95, and no
// arithmetic on those ten numbers recovers it — the information needed was
// thrown away at each host.
//
// A sketch keeps the shape of the distribution instead of the values or the
// answer. DDSketch's particular shape is a histogram whose buckets grow
// geometrically:
//
//	bucket k covers (γ^(k-1), γ^k],  γ = (1+α)/(1−α)
//
// A value lands in bucket k = ceil(log_γ v), and the bucket answers with
// 2γ^k/(γ+1). Because the buckets are multiplicative rather than evenly
// spaced, the error is *relative*: an answer is always within α of the truth,
// whether the truth is 2ms or 2 minutes. That is the property a latency SLO
// needs, and the one a fixed-width histogram cannot give — it would need
// millisecond buckets out to a minute to keep the same guarantee at both ends.
//
// Two sketches with the same γ merge by adding their buckets. That is the
// whole point: every host can sketch locally, ship a few hundred bytes per
// interval, and a query can merge an arbitrary set of them — across hosts,
// across time buckets, in any order — and still answer within α. Merging is
// associative and commutative because addition is.
//
// # What it costs, and where the error is not
//
// The guarantee is on the *value*, not the rank. Quantile(0.95) returns a
// value within α of the true p95; it does not promise that exactly 95% of the
// population is below it. For latency that is the right trade, because the
// question is "how slow is slow" and not "which sample is the 95th".
//
// Count, sum, min and max are tracked exactly alongside the buckets. They cost
// four float64s and they mean Quantile(0) and Quantile(1) are exact rather
// than bucket estimates, which matters because the extremes are what people
// check a sketch against when they distrust it.
//
// # Bounded size
//
// A sketch is capped at [MaxBins] buckets. On overflow the *lowest* buckets
// are collapsed together, which is a deliberate asymmetry: the tail is what
// anyone asks a latency sketch about, so p95 and p99 keep their guarantee
// while p1 degrades. A value below the collapsed floor is still counted — it
// contributes to count, sum and min — it just stops being separable from its
// neighbours.
//
// Collapsing is the only operation here that loses the α guarantee, and it
// loses it only for quantiles that fall inside the collapsed region. With
// γ ≈ 1.02 and 2048 bins an uncollapsed sketch spans a ratio of about 10^17
// between its smallest and largest value, so in practice nothing collapses.
//
// # What this package is not
//
// It is not a general histogram: buckets are not configurable per instance
// beyond γ, and there is no way to ask which values are in a bucket. It does
// not store timestamps — a sketch describes one interval, and the caller says
// which. And it is not safe for concurrent use; the aggregator owns one per
// context per bucket and never shares it.
package sketch
