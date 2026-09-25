# On-disk formats

Byte-level specifications for everything ozymandias writes to disk. Each one is
normative: a change here, to the encoder, to the decoder and to the golden
tests happens in the same commit.

| Format | Written by | Read by |
|---|---|---|
| [chunk.md](chunk.md) | `internal/tsdb/chunkenc` | the head, blocks, compaction |
| [wal.md](wal.md) | `internal/tsdb/wal` | replay at startup |
| [block.md](block.md) | `internal/tsdb/block` | queries, compaction |

Conventions used throughout:

- **uvarint** / **varint** are Go's `encoding/binary` LEB128 encodings —
  unsigned, and zig-zag signed, respectively.
- **u32**, **u64** are big-endian unless a section says otherwise. Big-endian
  for anything a human might read in a hex dump, little-endian for float bits
  where it matches the machine.
- **crc32c** is CRC-32 Castagnoli (`crc32.Castagnoli`), the polynomial with
  hardware support on every CPU ozymandias runs on.
- Every multi-byte length is bounds-checked against the bytes actually
  available before it is used to allocate. Corruption arrives as arbitrary
  bytes; a length prefix is a suggestion until it is verified.

## Golden files

Each format has a small committed example, and a test that reads it:

| File | Test |
|---|---|
| `internal/tsdb/chunkenc/testdata/golden/chunk.bin` | `TestChunk_Golden` |
| `internal/tsdb/wal/testdata/golden/00000000.wal` | `TestWAL_Golden` |
| `internal/tsdb/block/testdata/golden/block/` | `TestBlock_Golden` |

They exist because no ordinary test can catch a format change: a test encodes
and decodes with the same build, so changing both halves at once looks correct
and is catastrophic on a disk written months ago. The committed bytes are the
only thing that remembers.

The chunk and the log are compared byte for byte in both directions — the
committed file must still decode, *and* the encoder must still produce it. A
block cannot be, because it carries a ULID and a creation time, so its test
asserts that the directory still opens and still answers the same queries.

Regenerate deliberately, never to make a test pass:

```
go test ./internal/tsdb/chunkenc -run Golden -update-golden
go test ./internal/tsdb/wal      -run Golden -update-golden
go test ./internal/tsdb/block    -run Golden -update-golden
```

A regenerated golden means the on-disk format changed, so the same commit owes
a reader that can still open the old one, or a statement that it cannot.
