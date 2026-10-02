// Package openmetrics parses the Prometheus text exposition format (0.0.4)
// and OpenMetrics 1.0 — what a `/metrics` endpoint returns — and provides the
// two pieces of histogram arithmetic the agent's `openmetrics` check needs.
// It is a library: it knows nothing about scraping, scheduling or the wire,
// so it can be tested on canned pages alone.
//
// # The format
//
// A page is lines. Metadata lines start with '#'; everything else is a
// sample:
//
//	# HELP http_requests_total Requests served.
//	# TYPE http_requests_total counter
//	http_requests_total{method="GET",code="200"} 1027 1395066363000
//	http_requests_total{method="POST",code="500"} 3
//
// A sample is `name{labels} value [timestamp]`. Labels are optional; values
// are floats including NaN and ±Inf; label values are double-quoted with
// \\, \" and \n escaped. Samples are grouped into families by the TYPE line,
// and some types own several sample names: a histogram `foo` writes
// `foo_bucket{le="…"}`, `foo_sum` and `foo_count`; a summary writes `foo`
// with a `quantile` label plus `_sum` and `_count`.
//
// The two formats differ in three places this parser cares about.
// OpenMetrics ends with a `# EOF` line, which is what makes a truncated
// response detectable; it writes timestamps in seconds where Prometheus text
// writes milliseconds; and its counters' samples are named `<family>_total`.
// Parse accepts both, telling them apart by the Content-Type when the caller
// has one ([FormatFromContentType]) and by the `# EOF` otherwise.
//
// # Parsing
//
// Hand-written, line at a time, in one pass over a body read in full. In
// full because a page is small (node_exporter writes well under 1 MiB) and
// because the parse is all-or-nothing anyway: a consumer that differences
// counters between scrapes must not see half a page. Every dimension a
// target controls — bytes, samples, labels per sample, name and value length
// — is bounded by [Limits], because a scrape target is someone else's
// process and a bug there must cost this agent an error, not its memory.
// Errors carry line and column.
//
// # Cumulative buckets
//
// A histogram's buckets are cumulative: `le="0.5"` counts every observation
// ≤ 0.5, including those already counted by `le="0.25"`, and `le="+Inf"`
// counts them all. And they are counters: they only grow, from process start.
// So the question "how were the last 15 seconds distributed?" takes two
// subtractions — this scrape minus the last one, then each bucket minus the
// one below it — which is what [BucketDeltas] does.
//
// # Why counters need reset detection
//
// A counter's value means "since the process started". When the process
// restarts, it starts again at zero, and a naive difference between the last
// scrape before the restart and the first after is negative — or positive but
// wrong, if the new process has already counted part of the way back. The
// signal is that some count went down, which a counter otherwise never does;
// then everything in the new scrape happened since the restart, and the new
// value is itself the delta. That is the rule Prometheus's rate() applies and
// the one BucketDeltas applies to buckets. (Rates of plain counters are the
// collector framework's job, not this package's.)
//
// # Buckets to a sketch, and what is lost
//
// The agent's native distributions are DDSketches ([sketch.Sketch]), which
// answer p50…p99 within a relative error α. A Prometheus histogram can be
// fed into one so the same aggregators work on it — but a bucket only says
// "n observations fell in (lower, upper]", and where inside is gone for good.
// [ToSketch] spreads them uniformly across the bucket, the same assumption
// histogram_quantile() makes, so the error is bounded by the bucket's width,
// not by α. That is the honest summary of the lossy path: as good as the
// exporter's bucket layout, and no better.
package openmetrics
