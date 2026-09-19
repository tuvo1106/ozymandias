// Package sketch implements DDSketch, a mergeable quantile sketch with a
// relative-error guarantee. Unlike stored percentiles, sketches from many
// hosts and time buckets can be merged and still answer p95 correctly.
//
// Status: arrives in M2 (docs/plan/M2-tsdb.md); this file marks its place in
// the architecture until then.
package sketch
