// Package httpcheck is the http_check check: it requests one URL per run and
// reports whether the endpoint answered, how fast, with what status, and —
// for https — how many days its certificate has left (network.http.*,
// docs/metrics-catalog.md).
//
// # What "up" means
//
// Three questions, three metrics, because they fail differently:
//
//   - network.http.can_connect: did a response arrive at all? 0 for a
//     refused connection, a DNS failure, a timeout or a certificate the
//     agent does not trust.
//   - network.http.status_code: which status it was (a gauge, so a chart
//     shows the 200 turning into a 503).
//   - network.http.up: 1 only if the response also had an expected status
//     (by default 200–399) and, when content_match is set, a body matching
//     it. This is the one a monitor alerts on.
//
// A run that is not up also returns an error saying why, so the scheduler
// logs the transition once and counts it in ozy.agent.collector.errors; the
// 0s are still emitted, because "down" is a value, not an absence.
//
// # The url tag
//
// Every metric is tagged url:<scheme>://<host><path>. The query string and
// any user:password@ are left out: both are where credentials and tokens
// live, and a tag is stored, indexed and shown in every picker. Like every
// tag the value is lower-cased; a URL that cannot be a tag at all (a comma
// in the path) is sent without one, and the instance's name and tags
// identify it instead.
//
// # Timing
//
// Response time is measured on the injected clock from just before the
// request to the end of reading the body (at most 64 KiB, the same prefix
// content_match sees), so it includes DNS, connect, TLS and transfer — what
// a user waits for. The request's own timeout is necessarily real time: it
// is a deadline on network I/O.
package httpcheck
