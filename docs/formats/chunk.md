# Chunk format (Gorilla)

Written by `internal/tsdb/chunkenc`. One chunk holds up to
`MaxSamplesPerChunk` = **120** samples of one series.

This is the encoding from Facebook's Gorilla paper, and the reason it works is
worth stating before the layout: a monitoring system's samples arrive at a
fixed interval and change slowly. So the *second* difference of the timestamp
is almost always zero, and consecutive values usually share most of their bits.
The format spends one bit on each of those cases and pays only for what is
actually surprising.

## Layout

```
u16            sample count, big-endian
bitstream      sample 0, then samples 1, 2, … n-1
```

The bitstream is **not byte-aligned**: a sample can start and end mid-byte, and
bits are written most-significant-first. Trailing bits of the final byte are
zero padding. This is why a chunk is append-only, and why resuming an append
must restore the *bit* cursor and not just the byte cursor — getting that wrong
writes padding into the middle of the stream and everything after it decodes as
garbage. (It did, once. The test that catches it is
`TestChunk_AppenderResumesAnExistingChunk`.)

### Sample 0

```
varint         t0    (zig-zag, unix milliseconds)
64 bits        math.Float64bits(v0), most significant bit first
```

### Sample 1

```
varint         t1 - t0
value          (see below)
```

The first delta is written in full rather than bucketed: there is no previous
delta to difference it against, and a varint is the cheaper way to say so.

### Sample i (i ≥ 2) — timestamp

Let `dod = (t[i] - t[i-1]) - (t[i-1] - t[i-2])`, the delta of the delta.

| `dod` fits in | Control bits | Payload bits | Total |
|---|---|---|---|
| `0` | `0` | — | **1** |
| 14 bits signed | `10` | 14 | 16 |
| 17 bits signed | `110` | 17 | 20 |
| 20 bits signed | `1110` | 20 | 24 |
| anything | `1111` | 64 | 68 |

Payloads are two's complement, written in the stated width. A series at a
steady interval costs **one bit per timestamp**.

### Value (every sample after the first)

Let `x = math.Float64bits(v[i]) XOR math.Float64bits(v[i-1])`.

| Case | Control bits | Payload |
|---|---|---|
| `x == 0` — value unchanged | `0` | — |
| meaningful bits fall inside the previous window | `10` | the window's bits |
| otherwise | `11` | 5 bits leading-zero count, 6 bits window width (`0` means 64), then the window |

The "window" is the run of bits between the leading and trailing zeros of `x`.
Reusing the previous one costs two bits instead of thirteen, which is the
common case for a gauge wobbling in its low bits. The leading-zero count is
clamped to 31 because five bits cannot hold more; the cost is a few wasted
payload bits on values with very long runs of leading zeros.

A value that never changes costs **one bit**.

## Worked examples

Generated from the encoder itself.

### A counter reporting every 10s, value unchanged

Samples `(1000, 1.0)`, `(11000, 1.0)`, `(21000, 1.0)`:

```
00 03                       count = 3
d0 0f                       varint 1000                      (zig-zag 2000)
3f f0 00 00 00 00 00 00     float64 1.0
a0 9c 01                    varint 10000                     (zig-zag 20000)
 └ then, in the same bitstream:
   0                        sample 1 value: XOR is 0
   0                        sample 2 dod:   0
   0                        sample 2 value: XOR is 0
00                          the three bits above, plus padding
```

**16 bytes** for three samples, against 50 stored as `(int64, float64)` pairs.
Samples 1 and 2 cost three bits between them.

### A gauge changing in its low bits

Samples `(1000, 0.5)`, `(11000, 0.5001)`, `(21000, 0.5002)`:

```
00 03 d0 0f 3f e0 00 00 00 00 00 00 a0 9c 01 f1
3e 8d b8 ba c7 17 7a 2e 5b 27 3d 24 c0
```

**29 bytes**. The first changed value pays the full `11` + 5 + 6 + window
price; the second reuses the window with `10` and pays only for the bits
inside it.

To regenerate either dump, append the samples to a `chunkenc.Chunk` and print
`c.Bytes()`.

## Invariants

- `Append` rejects `t <= last t`. The encoding cannot express it, and ADR-0011
  makes that the store-wide rule rather than a chunk quirk.
- A decoder stops at the sample count in the header and never reads past it:
  the padding bits at the end are indistinguishable from data.
- `FromBytes` followed by `Appender()` resumes at the exact bit position after
  the last sample.
- A chunk carries **no checksum of its own**. Integrity is the container's job
  — `chunks.dat` checksums each record ([block.md](block.md)), and the WAL
  checksums each record ([wal.md](wal.md)).
