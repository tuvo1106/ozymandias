// Package sketchstore stores DDSketches per series per time bucket in Pebble,
// sharing series selection with the TSDB so percentile queries pick series
// the same way every other query does.
//
// # Why it is not the TSDB
//
// The TSDB's chunk encoding exists to spend a handful of bits on a sample
// whose value barely moved since the last one. A sketch is a kilobyte of
// bucket counts, not a float64, and none of that machinery applies to it. So
// sketches live in a key/value store, keyed so that one series' time window
// is one contiguous scan:
//
//	's' | seriesID u64 BE | ts u32 BE  →  codec byte | zstd(payload)
//	'x' | seriesID u64 BE              →  the series' canonical key
//
// Big-endian because byte order then *is* numeric order. Series before time,
// because every read is "one series over a window" and no read is "every
// series at one instant".
//
// # Identity
//
// The seriesID is FNV-1a over the series' canonical key, not a number this
// store hands out. An assigned id would be a second source of truth for
// identity that has to stay in step with the TSDB across restarts,
// compactions and a head that forgets a series when it truncates. A hash
// needs no coordination at all. It can collide — about one chance in 37
// million at a hundred thousand series — so the 'x' keyspace holds the
// canonical key beside the id, making a collision a named rejection rather
// than two metrics quietly sharing a percentile. That keyspace is also what
// retention sweeps: "older than t" is not one range, it is one range per
// series, so the sweep needs the list of series.
//
// # Ordering
//
// A point replaces the point already in its bucket, which is what makes the
// agent's at-least-once delivery safe. The append-only rule the TSDB enforces
// is not duplicated here; instead the intake writes the `.count` scalar to
// the TSDB first and stores sketches only for series it accepted, so one
// decision about ordering governs both stores. See docs/adr/ADR-0015.
package sketchstore
