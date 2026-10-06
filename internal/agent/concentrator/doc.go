// Package concentrator computes request, error and latency (RED) metrics from
// 100% of spans, before any sampling — so a service's rates stay exact even
// when only a fraction of its traces are kept.
//
// The argument for doing it here: a sampler keeps a trace with probability p, so
// counting stored traces estimates traffic with an error that grows as p shrinks,
// and a rare-event or error sampler biases the count in a direction nobody can
// correct afterwards. Counting every span first and sampling second makes the
// statistics exact and the sample merely a set of examples. The price is that the
// agent must see every span, which is why the SDKs send unsampled traces too.
//
// The mechanism is deliberately thin. Each span that is a service entry point
// (`_top_level`) or was marked `_measured` becomes one counter increment and one
// distribution sample on the agent's ordinary [aggregator.Aggregator], stamped
// with the span's own end time, so bucketing, zero-fill, DDSketch and the hop C/D
// wire formats are the ones every other metric already uses. Series are named
// `trace.<span name>.hits`, `.errors` and `.duration` (seconds) and tagged
// service, resource, env and status_class.
//
// Every tag value is bounded in code: resources per service, services, and span
// names are capped and overflow folds into `_other_`, because a tag is a series
// and a SQL statement with an inlined literal would otherwise mint one per row.
package concentrator
