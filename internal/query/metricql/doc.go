// Package metricql parses and evaluates the tag-oriented metric query
// language (avg:http.request.duration{service:x} by {route}): select series,
// aggregate in time, then across series, then apply functions and arithmetic.
//
// Status: arrives in M3 (docs/plan/M3-query-dashboards.md); this file marks its place in
// the architecture until then.
package metricql
