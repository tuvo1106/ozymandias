// Package openmetrics is the `openmetrics` check: it scrapes a Prometheus or
// OpenMetrics `/metrics` endpoint on every run and turns the page into
// ozymandias metrics. One check makes anything that already exposes
// Prometheus metrics — most infrastructure, many apps — observable without a
// line of code in it.
//
// # From a page to metrics
//
// The page is parsed by internal/agent/collector/openmetrics, all or nothing
// (a half-read page would look like every missing counter restarting). Each
// family then becomes metrics by its type:
//
//   - gauge, unknown (untyped), info, stateset: the sample value as a gauge,
//     named as the sample.
//   - counter: a per-second rate from this scrape and the previous one
//     (collector.Rates), sent as a gauge (ADR-0026) and named as the sample
//     (`http_requests_total`). The first scrape has no rate; a counter that
//     went down — the target restarted — is skipped for one scrape rather
//     than reported as a huge negative rate.
//   - histogram: `<family>.bucket`, one count per bucket tagged
//     `upper_bound`, holding the observations that landed in that bucket
//     since the previous scrape (not cumulative: the page's buckets are, and
//     BucketDeltas subtracts both ways); plus `<family>.sum` and
//     `<family>.count` as counts of the same interval. A target restart
//     makes the delta everything counted since it restarted, as
//     Prometheus's rate() assumes.
//   - summary: the quantiles as a gauge `<family>` tagged `quantile`, with
//     `.sum` and `.count` as counts, like a histogram's.
//   - gaugehistogram: its buckets are levels, not counters: `<family>.bucket`
//     as gauges of the cumulative counts, `.gsum`/`.gcount` as gauges.
//
// Every label becomes a tag, `label:value`, unless listed in exclude_labels.
// A value the wire cannot carry (a comma in it) costs that tag, not the
// metric; the scheduler counts it.
//
// # Choosing what to keep
//
// The name the rules see is the one above before any suffix: the sample name
// for counters and gauges, the family name for histograms and summaries.
// `metrics` (regexes, empty = everything) and `exclude` (regexes) choose;
// `rename` maps a name to another; `namespace` is prefixed with a dot. A
// target is someone else's process, so everything is bounded: the body by
// max_body, the samples by the parser's limits, and what one scrape may emit
// by max_series — beyond it the rest are dropped and the run reports an
// error saying how many.
//
// # Health
//
// Every run emits `openmetrics.up` (1, or 0 when the scrape failed) and
// `openmetrics.scrape_duration` in seconds, so a target that is down is a
// value on a chart rather than an absence.
//
// # Histograms as distributions
//
// With histogram_buckets_as_distributions, each scrape's bucket deltas are
// spread into a DDSketch (openmetrics.ToSketch) and sent as one
// distribution named after the family, instead of `.bucket` counts, so
// `p90:` works on a scraped histogram as on a statsd distribution. It is
// lossy — an observation is known only to lie within its bucket, so a
// percentile is only as precise as the bucket widths — and `.sum` and
// `.count` are still sent as counts.
package openmetrics
