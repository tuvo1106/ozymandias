# Sketch format

Written by `internal/sketchstore`. One DDSketch per series per time bucket,
held in Pebble. The sketch itself is `internal/sketch`; this document is the
bytes.

A sketch is a kilobyte of bucket counts, not a float64, so none of the TSDB's
chunk encoding applies to it — Gorilla exists to spend a handful of bits on a
value that barely moved, and consecutive sketches have nothing in common at
that level. What they do share is *identity*, and that comes from the TSDB
(see [ADR-0015](../adr/0015-sketch-storage-and-identity.md)).

## Keys

```
's' | seriesID u64 BE | ts u32 BE      the encoded sketch
'x' | seriesID u64 BE                  the series' canonical key, as text
```

Big-endian because byte order is then numeric order, which is what makes one
series' time window a single contiguous scan. Series before time, because
every read is "one series over a window" and none is "every series at one
instant".

`ts` is the bucket start in unix **seconds**, so the key format runs out in
2106. Everything above this layer works in milliseconds; the conversion is at
the boundary.

The `'x'` keyspace is not an optimisation. Retention needs it — "older than
t" is not one range but one range per series — and it is what turns a
`seriesID` collision into a named rejection instead of two metrics silently
sharing a percentile.

## Values

```
u8      codec     0 = raw, 1 = zstd
bytes   body      the payload, or its zstd frame
```

The codec byte sits outside the payload so reading it costs nothing. A payload
under 128 bytes is stored raw: zstd's frame header is 9–18 bytes and a
single-bucket sketch does not recover it. So is one that compresses larger
than it started, which would otherwise cost space *and* a decompression on
every read.

A value may not expand past 1 MiB. A full sketch — 2048 buckets each side with
float counts — is about 41 KiB, so that is roomy by a factor of 25 and still
refuses a frame claiming to expand to a gigabyte.

### Payload

```
u8      version   1
f64     gamma     little-endian IEEE-754 bits
f64     count
f64     sum
f64     min
f64     max
f64     zero_count
<bucket run>      positive buckets
<bucket run>      negative buckets, indexed by absolute value
```

The six aggregates are full float64s because they are *exact*: count, sum, min
and max are accumulated, not estimated, and the query layer reports them
as-is. Squeezing them would make them estimates. The buckets are where the
bytes actually are — a few hundred of them, with a dense ascending run of
indices — so that is where the varints go.

`gamma` travels with every sketch for the same reason it travels on the wire:
γ is what the bucket indices *mean*, and a reader that assumed its own γ would
merge two agents' sketches into a confident wrong answer.

### Bucket run

```
u8        flags     bit 0: every count in this run is a whole number
uvarint   n         buckets, ≤ 2048
repeated n times:
  varint  Δindex    zigzag, from the previous index (the first from 0)
  count             uvarint if bit 0 is set, else f64 little-endian bits
```

Counts are float64 in the sketch because a sampled metric contributes `1/rate`
per observation — but the overwhelming majority of sketches never see a sample
rate and hold whole numbers. A per-bucket discriminator would spend a byte on
every bucket to say so; the flag spends one byte for the whole run.

Indices are deltas because a real distribution occupies a narrow contiguous
band: latencies from one endpoint land in a few dozen adjacent buckets, so
almost every delta is a single byte.

## Reading a value

Every length in a value comes off a disk this process does not own. `n` is
checked against 2048 *before* anything is allocated for it, the index is
checked against ±2^31 (the range `Add` itself clamps to), and the assembled
sketch goes through the same `sketch.FromWire` that a payload off the network
does. A corrupt value is an error. It is never an allocation and never a
panic; `FuzzDecodeValue` is the standing proof.

## Compatibility

`testdata/golden/sketch-v1.bin` is a sketch written by the code that
introduced this format: positive values, negative values, a zero and a
fractional weight. `TestSketchValue_Golden` checks both that it still decodes
and that this build re-encodes it byte for byte. These bytes are what a Pebble
value holds across a restart, an upgrade or a backup restore — all moments
when finding out the layout moved is far too late, and none of which a
round-trip test can see, because it encodes and decodes with the same build.
