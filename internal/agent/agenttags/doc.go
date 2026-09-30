// Package agenttags is the agent's one rule for decorating a series' tags on
// the way out: the agent's global tags, then its host tag unless the series
// already names a host, canonical order, and a cap on the count.
//
// It is its own package because two paths need it and must not drift apart:
// the statsd aggregator and the collector scheduler. When they each had a
// copy, the copies disagreed about whether global tags count as "already
// naming a host" and about the cap, so the same config gave a statsd series
// one host tag and a host-metrics series two.
package agenttags
