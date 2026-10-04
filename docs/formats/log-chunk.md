# Log chunk format (v1)

Written by `internal/logstore`. A chunk holds one log stream's blocks for one day.
Normative: a change here, to `chunk.go` and to its golden happens in one commit, with
a version bump.

All integers are big-endian. crc32c is CRC-32 Castagnoli. Every length read from disk is
checked against the bytes that are actually there before it is used to allocate (see
[README](README.md)).

```
chunk  = header block* [footer]
header = "OZYC" | version u16 (=1) | reserved u16 (=0)                         8 bytes
block  = blockHeader zstd(entries)
footer = indexEntry{count} | count u32 | indexOffset u64 | crc32c u32 | "OZYF"
```

## Block

```
blockHeader (40 bytes)
  minTs    i64    smallest entry timestamp, unix ms
  maxTs    i64    largest entry timestamp, unix ms
  n        u32    entries in the block (>= 1)
  rawLen   u32    bytes after decompression
  compLen  u32    bytes of zstd data that follow
  crc32c   u32    of the compressed bytes only
  lastSeq  u64    highest WAL sequence number of any entry (see Recovery)
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
indexEntry (44 bytes, one per block, in file order)
  offset u64 | minTs i64 | maxTs i64 | n u32 | rawLen u32 | compLen u32 | lastSeq u64
trailer (20 bytes)
  count u32 | indexOffset u64 | crc32c u32 (of the index entries) | "OZYF"
```

It is written when a chunk is closed cleanly, so a reader can learn every block's time range
with one read from the end instead of walking the file. A reader trusts it only if all of these
hold, and otherwise ignores it:

1. the trailer's magic is present and `count` is at most 2^22;
2. `indexOffset + count * 44` ends exactly where the trailer begins;
3. the index's crc32c matches;
4. the entries tile the file: the first starts at byte 8, each starts where the previous
   ended, and the last ends at `indexOffset`; and each is a plausible block (`n >= 1`,
   `minTs <= maxTs`, lengths within the caps below, fits in the file).

## Reading without a footer

Walk from byte 8: read a 40-byte header, check it is plausible and fits, read `compLen` bytes,
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

## Compatibility

`testdata/golden/v1.chunk` is a committed v1 chunk and `TestGolden_V1ChunkStillReads` reads it.
Its compressed bytes depend on the zstd library's version and are **not** compared; the header,
index and trailer bytes are (`TestGolden_LayoutBytes`). Regenerate with `-update-golden` only
for a deliberate format change.

v2 (per-block bloom filters, M4's second half) will add a section beside each block and bump
`version`; v1 chunks stay readable.
