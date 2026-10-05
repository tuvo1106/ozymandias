# Log chunk format (v2; v1 still readable)

Written by `internal/logstore`. A chunk holds one log stream's blocks for one day.
Normative: a change here, to `chunk.go` and to its golden happens in one commit, with
a version bump.

All integers are big-endian. crc32c is CRC-32 Castagnoli. Every length read from disk is
checked against the bytes that are actually there before it is used to allocate (see
[README](README.md)).

```
chunk  = header block* [footer]
header = "OZYC" | version u16 (=1 or 2) | reserved u16 (=0)                    8 bytes
block  = blockHeader zstd(entries) [bloom]          (the bloom is v2 only)
footer = indexEntry{count} | count u32 | indexOffset u64 | crc32c u32 | "OZYF"
```

**Versions.** v1 is the original layout. v2 adds a per-block bloom filter: a 4-byte `bloomLen`
at the end of the block header, the filter after the compressed data, and a `bloomLen` at the
end of each index entry. Everything not named in this section is identical in both. A file keeps
the version in its header for life: a v2 binary that reopens a v1 chunk to append appends v1
blocks (no filter), so one file never mixes layouts, and the first v2 chunk is the next day's.
Every test of the chunk layer runs once per version.

## Block

```
blockHeader (40 bytes; v2: 44)
  minTs    i64    smallest entry timestamp, unix ms
  maxTs    i64    largest entry timestamp, unix ms
  n        u32    entries in the block (>= 1)
  rawLen   u32    bytes after decompression
  compLen  u32    bytes of zstd data that follow
  crc32c   u32    of the compressed bytes only
  lastSeq  u64    highest WAL sequence number of any entry (see Recovery)
  bloomLen u32    (v2 only) bytes of bloom filter after the compressed data; 0 for none
```

The decompressed `entries` are `n` records back to back:

```
tsDelta   varint   (zig-zag) from the previous entry's ts; the first from minTs
seqDelta  varint   (zig-zag) from the previous entry's seq; the first from 0
len       uvarint  bytes of body
body      bytes    opaque to this layer; the store puts the log's JSON here
```

`seq` is the WAL sequence number the store gave the entry. It is unique and survives sealing
and restarts, so `(ts, seq)` is a total order over every log ever stored: it is what a
pagination cursor names, and what recovery compares with `lastSeq`. The block's `lastSeq` is
the highest `seq` among its entries.

Both deltas are signed so a block need not be sorted, but the store sorts by `(ts, seq)` before
sealing, which is what keeps them small: nearby timestamps and consecutive sequence numbers cost
a byte or two each.

A block is **self-describing**: its header says how long it is and whether its payload is
intact. Nothing else is needed to find the next block.

## Footer

The footer is an optimization and never the only copy of anything.

```
indexEntry (44 bytes; v2: 48; one per block, in file order)
  offset u64 | minTs i64 | maxTs i64 | n u32 | rawLen u32 | compLen u32 | lastSeq u64
  | bloomLen u32 (v2 only)
trailer (20 bytes)
  count u32 | indexOffset u64 | crc32c u32 (of the index entries) | "OZYF"
```

It is written when a chunk is closed cleanly, so a reader can learn every block's time range
with one read from the end instead of walking the file. A reader trusts it only if all of these
hold, and otherwise ignores it:

1. the trailer's magic is present and `count` is at most 2^22;
2. `indexOffset + count * entrySize` ends exactly where the trailer begins (entrySize 44, or 48 in v2);
3. the index's crc32c matches;
4. the entries tile the file: the first starts at byte 8, each starts where the previous
   ended, and the last ends at `indexOffset`; and each is a plausible block (`n >= 1`,
   `minTs <= maxTs`, lengths within the caps below, fits in the file, with its filter).

## Reading without a footer

Walk from byte 8: read a block header (40 bytes; 44 in v2), check it is plausible and fits, read `compLen` bytes,
check the crc32c, and repeat. The first block that fails any check ends the **valid prefix**; the
rest of the file is not served. This is what a crash mid-write leaves, and the result is every
block that was whole.

A writer reopening a chunk finds the valid prefix, **truncates the file to its end** (cutting
off a footer or a torn tail alike) and appends there; the footer is written again on close.

## Caps

| Field | Limit | Why |
|---|---|---|
| `rawLen` | 16 MiB | a block is sealed at 256 KiB; this only bounds a corrupt value |
| `compLen` | 16 MiB | same |
| `n` | 2^22 | bounds the entry slice a decode allocates |
| `bloomLen` | 1 MiB | a filter is at most 64 KiB; this only bounds a corrupt value |
| decoder memory | 16 MiB | a block claiming a small `rawLen` cannot inflate past this |

A header that lies about its size is refused before anything is allocated for it
(`TestBlock_ALyingHeaderAllocatesNothing`).

## Recovery and `lastSeq`

Each entry the store accepts is given a WAL sequence number. When a block is sealed, its
`lastSeq` is the highest number among its entries, and the block is fsynced **before** the
WAL segments that held those entries may be deleted. After a crash the store replays the WAL
and skips every entry whose number is at or below the largest `lastSeq` already in its
stream's chunks, so a crash between "block durable" and "WAL truncated" does not duplicate
logs.

## Bloom filter (v2)

A block's filter answers "can this word occur in any log of this block?" without decompressing
it: a "no" is certain, a "yes" is not. The search reads the few KiB of the filter, and skips the
block on a "no".

**What is in it.** Every three-byte window of the lowercased text of every log in the block: the
message, and every string, number and boolean attribute value (arrays and nested objects
flattened). Attribute names and the stream labels are not in it (the index answers labels).
"Lowercased" is `strings.ToLower`, which is also what the matcher does to the text it searches.
A free-text term is a case-insensitive *substring*, and a substring of three or more bytes occurs
in a text only if all its windows do, so windows are exact about what substring search means;
whole words would not be.

**What a query asks it.** `logql.Needs` derives, from the part of a query the index did not
answer, a disjunction of conjunctions of literals ("some alternative has every literal"). A
literal is a run of at least three bytes between wildcards of a free-text term or a non-numeric
`@attr:value`. A label, a numeric comparison, a numeric-looking pattern (`@ms:1000` also finds
`1e3`), a `NOT` and a literal shorter than three bytes constrain nothing; an `OR` with any
unconstrained arm is unconstrained. A block is skipped only if, for every alternative, some
literal has an absent window. The invariant is soundness: a log that matches the query is never
in a block the filter rules out, and `TestBloom_NeverRulesOutABlockHoldingAMatch` checks it
against the real matcher.

```
filter = k u8 (=4) | bits (a multiple of 8, 8..65536 bytes) | crc32c u32 (of k and bits)
```

The bits are sized at 8 per distinct window in the block (about a 2.4% false-positive rate per
window, and a term of n bytes needs n-2 windows to be wrong at once). Probes are double hashing:
a 24-bit window `t` is mixed (`x = t * 0x9E3779B97F4A7C15; x ^= x>>29; x *= 0xBF58476D1CE4E5B9;
x ^= x>>32`), `h1 = uint32(x)`, `h2 = uint32(x>>32) | 1`, and bit `i` is `(h1 + i*h2) mod m`
for i in 0..k-1, with `m` the filter's bits and bit `b` stored as `bits[b/8] & (1 << (b%8))`.

**The checksum is the safety property.** A bit flipped from 1 to 0 would make the filter deny a
log that is there, and a skipped block is a silently missing log. A filter that fails its
checksum, has an unknown `k`, is the wrong shape, or cannot be read is ignored and the block is
read in full: slower and always correct. A block with no filter (a v1 block, or one whose bodies
could not be decoded when it was sealed) is always read.

## Compatibility

`testdata/golden/v1.chunk` is a committed v1 chunk and `TestGolden_V1ChunkStillReads` reads it;
`testdata/golden/v2.chunk` is a committed v2 chunk with filters. Compressed bytes depend on the
zstd library's version and are **not** compared; the header, index and trailer bytes are
(`TestGolden_LayoutBytes`), and so are the **filter bytes**, which depend on no library, only on
the tokenizer and hash above (`TestGolden_V2ChunkStillReadsAndItsFiltersAreByteStable`). Changing
what goes in a filter changes what every existing v2 chunk answers, so it is a format change: a
version bump, a note here, and new goldens. Regenerate with `-update-golden` only for that.
`TestStore_V1ChunksStayReadableBesideV2` reads a v1 day and a v2 day in one query.
