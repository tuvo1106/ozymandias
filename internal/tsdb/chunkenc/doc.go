// Package chunkenc implements Gorilla-style compression for time series: a
// chunk of up to 120 samples of one series, stored as delta-of-delta
// timestamps and XOR-encoded float values.
//
// # The mental model
//
// A metric is boring, and compression is the art of charging nothing for
// boring. Two observations from Facebook's Gorilla paper drive everything
// here:
//
//   - Samples arrive on a schedule. A series scraped every 10s has timestamps
//     that differ by exactly 10000 ms, over and over. The *delta* is dull, so
//     store the delta of the delta instead: a perfectly regular series emits a
//     single 0 bit per sample.
//   - Consecutive values are similar. Floats that differ slightly share their
//     sign, exponent and leading mantissa bits, so a XOR b is mostly zeros.
//     Store only the window of bits that actually changed.
//
// Neither trick is free-form: both encode a *variable* number of bits, so the
// chunk is a bitstream, not a byte slice. Reading it back requires replaying
// it from the start, one sample at a time, which is exactly why a chunk is
// capped at 120 samples — an iterator should never have to decode more than a
// bounded amount of work to reach the end, and a partial chunk is still cheap
// to scan.
//
// # What a chunk is not
//
// A chunk is append-only and strictly ordered: Append rejects a timestamp
// that is not greater than the last one. There is no update, no delete, and
// no reordering. Every layer above (head, block, compaction) is built on that
// guarantee, and the crash-safety argument in DESIGN.md depends on it: a
// chunk's bytes, once written, are never rewritten in place.
//
// # Layout
//
// [docs/formats/chunk.md] specifies the bitstream byte for byte, with a worked
// hex example. In outline:
//
//	uint16       sample count (big endian)
//	varint       t0
//	varint       delta1 = t1 - t0
//	per sample   delta-of-delta, in one of five buckets (0, 14, 17, 20, 64 bits)
//	uint64       v0, raw
//	per sample   XOR with the previous value: a 0 bit if identical, else the
//	             changed window, reusing the previous window when it fits
//
// The timestamp buckets are wider than the paper's, which assumed second
// resolution; milliseconds need the extra room (Prometheus made the same
// adjustment).
//
// [docs/formats/chunk.md]: https://github.com/tuvo1106/ozymandias/blob/main/docs/formats/chunk.md
package chunkenc
