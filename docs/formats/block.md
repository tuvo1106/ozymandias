# Block format

Written by `internal/tsdb/block`. A block is a closed time range of one
store's data, written once and never modified. Immutability is what pays for
everything else: no locks, any number of concurrent readers, checksums computed
at write time and trusted forever, and a compaction that can never corrupt its
own input because it only ever writes a new file.

## Directory

```
blocks/<ULID>/
  meta.json     what this block is
  chunks.dat    the samples
  index.dat     symbols, series, postings, offset table
```

`<ULID>` sorts lexicographically by creation time, so `ls blocks/` is
chronological and compaction can find adjacent blocks without opening
anything. Entropy is drawn from a shared monotonic source, never from the
timestamp — two blocks written in the same millisecond must not collide.

**The rename is the commit.** Everything is written into `<ULID>.tmp/`,
`meta.json` last of all, and the whole directory is fsynced and renamed to
`<ULID>/`; rename is atomic, so a block either appears complete or does not
appear. A directory ending in `.tmp` is therefore never a block *whatever it
contains* — a crash in the window between the meta write and the rename leaves
one that is complete in every other respect — and neither is one without a
`meta.json`. Both are the wreckage of an interrupted write and are deleted at
startup.

Deletion runs the argument backwards: a `tombstone` file is written and fsynced
*before* anything is unlinked, so an interrupted delete is finished at startup
rather than leaving a block missing half its files.

## meta.json

```json
{
  "ulid": "01M2ZAZZG0K14A6HBWYH72ZHTQ",
  "version": 1,
  "minTime": 1000,
  "maxTime": 2000,
  "stats": { "series": 2, "samples": 3, "chunks": 2 },
  "compaction": { "level": 0 },
  "resolution_s": 0
}
```

JSON rather than a binary header because it is small, read once per block at
startup, and the first thing anyone looks at when something is wrong — being
able to `cat` it is worth more than its bytes. `minTime`/`maxTime` are
inclusive unix milliseconds, and the querier prunes on them without opening the
block. `compaction.sources` lists the merged blocks' ULIDs when level > 0.
`resolution_s` is 0 for raw samples and the rollup interval otherwise.

A reader refuses any `version` it does not know rather than guessing.

## chunks.dat

```
u32     magic     0x4f5a4348 "OZCH"
u8      version   1
repeated:
  uvarint len     1 + len(data): the encoding byte plus the chunk
  u8      enc     1 = gorilla (see chunk.md)
  bytes   data
  u32     crc32c  over enc and data
```

A chunk reference in the index is the byte offset of the length prefix plus the
total record length, so reading one chunk is a single `pread` at a known
offset. The length prefix is still written, because it is what makes the file
walkable on its own for repair and inspection.

## index.dat

```
u32     magic     0x4f5a4958 "OZIX"
u8      version   1
symbols   uvarint n | (uvarint len | bytes)…            sorted, unique
series    uvarint n | per series, in key order:
            uvarint metricSym
            uvarint ntags | (uvarint keySym | uvarint valSym)…
            uvarint nchunks | (varint minT | uvarint maxT-minT |
                               uvarint offset | uvarint recLen)…
postings  per (key,value): uvarint n | uvarint firstID | (uvarint delta)…
table     uvarint n | (uvarint keySym | uvarint valSym | uvarint offset)…
TOC       u64 symbols | u64 series | u64 postings | u64 table
u32     crc32c    over everything above
```

Four ideas:

- **Symbols.** Tag strings repeat across series — one `env`, one `prod`, a
  handful of route names — so each distinct string is stored once and
  referenced by ordinal. Measured on 200 series sharing a long metric name and
  environment: 666 bytes of symbols against 12,200 stored inline.
- **Symbols are sorted**, so a symbol's ordinal orders the same way the string
  does, and the offset table can be binary-searched on ordinals without
  dereferencing a single string.
- **Series are stored in key order**, so a block's series ids are its
  ordinals — a deterministic function of its contents. Two compactions of the
  same input produce byte-identical blocks.
- **Postings are delta-encoded.** Ids are dense and sorted, so almost every
  delta is one byte where the id would be two or three.

Per-chunk `minT`/`maxT` are what let a five-minute query against a two-hour
block read a handful of chunks rather than all of them.

The checksum covers the whole file. That is affordable because the index is
read whole at open, and it means the parser's own bounds checks are nearly
unreachable from a bit flip — they exist anyway, because a checksum proves the
bytes are the ones that were written, not that they mean anything. A fuzz
target feeds the parser bodies under a *valid* checksum for exactly that
reason.

## Worked example

A block with two series — `cpu{env:prod}` at t=1000,2000 and `cpu{env:dev}` at
t=1000.

### chunks.dat (46 bytes)

```
0000  44 44 43 48 01                     "OZCH", version 1
      12                                 record: len = 18
      01                                 encoding = gorilla
      00 02 d0 0f 3f f0 00 00 00 00
      00 00 d0 0f c2 5f ff               17 bytes of chunk: 2 samples
      de 57 a6 3a                        crc32c
001c  0d                                 record: len = 13
      01                                 encoding = gorilla
      00 01 d0 0f 40 14 00 00 00 00
      00 00                              12 bytes of chunk: 1 sample
      8d f5 aa 22                        crc32c
```

The first record starts at offset 5 and is 23 bytes; the second at 28 and is
18. Those are the `(offset, recLen)` pairs the index stores.

### index.dat (107 bytes)

```
0000  44 44 49 58 01                     "OZIX", version 1
0005  05                                 5 symbols
      08 "__name__"  03 "cpu"  03 "dev"  03 "env"  04 "prod"
0020  02                                 2 series
      01  01  03 02  01                  series 0: cpu, {env:dev}, 1 chunk
      d0 0f  00  1c  12                    minT 1000, +0, off 28, len 18
      01  01  03 04  01                  series 1: cpu, {env:prod}, 1 chunk
      d0 0f  e8 07  05  17                 minT 1000, +1000, off 5, len 23
0036  02 00 01                           __name__=cpu → ids 0, 0+1
      01 00                              env=dev → id 0
      01 01                              env=prod → id 1
003d  03                                 3 table entries
      00 01 36                             (__name__, cpu) → 54
      03 02 39                             (env, dev)      → 57
      03 04 3b                             (env, prod)     → 59
0047  …05  …20  …36  …3d                 TOC: 5, 32, 54, 61
0067  0e e9 55 f6                        crc32c of everything above
```

Note the ordering: series 0 is `cpu|env:dev` and series 1 is `cpu|env:prod`,
because `dev` sorts before `prod`. The ids in the postings follow from that,
not from the order the series were written.

## Invariants

- A series' chunks are in ascending time order and do not overlap.
- Every series in the index has at least one chunk.
- `minTime`/`maxTime` in `meta.json` bound every sample in the block.
- Series keys are unique within a block, and sorted.
- A block is complete or absent; there is no third state a reader can observe.
