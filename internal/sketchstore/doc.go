// Package sketchstore stores DDSketches per series per time bucket in Pebble,
// sharing series identity and the tag index with the TSDB so percentile
// queries select series the same way as any other.
//
// Status: arrives in M2 (docs/plan/M2-tsdb.md); this file marks its place in
// the architecture until then.
package sketchstore
