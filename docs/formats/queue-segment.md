# Queue topic format

Written by `internal/queue` (M7). A topic holds records that intake has
already acknowledged with a `202` and that a store has not yet committed, so
for that window it is the *only* copy of the data. The format is chosen so
that a crash at any instant leaves a directory the next start can recover
from without losing an acknowledged record and without inventing one.

## Directory

```
<topic>/
  00000000.wal          segments: WAL framing (wal.md), rolled at 32 MiB
  00000001.wal
  consumers/
    tsdb.offset         one file per consumer: the next offset it will read
```

The segments are plain [WAL](wal.md) segments, reused byte for byte: same
framing, same CRC, same torn-tail rule, same `Repair` at startup. The queue
adds nothing to the framing; it defines one record type and what goes inside
it.

Segment numbers are consecutive. Retention deletes from the front, so the
oldest segment number grows over time, but a gap in the middle is corruption.
Only the newest segment may be empty (a crash between rolling and the first
append leaves one).

## Record

Every WAL record in a topic has WAL type **1**. Its payload is:

```
u64     offset     the record's position in the topic, from 0, no gaps
i64     time       produce time, unix nanoseconds, by ozyd's clock
bytes   payload    the producer's bytes, to the end of the WAL record
```

Sixteen bytes of header, big-endian, and no second checksum: the WAL frame
already covers the whole payload with a CRC-32C. The largest payload is the
WAL's 16 MiB record limit less these 16 bytes.

**Offsets are stored, not implied by position.** A record's offset could be
computed by counting from the first record of the first segment, but then a
deleted or truncated segment would silently renumber everything after it, and
a consumer's committed offset would point at a different record than the one
it committed. Storing the offset in the record makes any such change visible:
on open, offsets must run consecutively across every segment, and a gap or a
step back fails the open with `ErrCorrupt`.

**The time is not the data's timestamp.** It is when ozyd accepted the
record, and exists so consumer lag can be reported in seconds as well as in
records.

The payload's own encoding belongs to the topic's producer (intake) and is
specified with it.

### Worked example

Offset 0, time `1700000000.123456789`, payload `first` (from the golden file):

```
00 00 00 15             WAL len = 21 (16 + 5)
01                      WAL type = 1, a queue record
74 54 5c 18             crc32c of the 21 bytes below
00 00 00 00 00 00 00 00 offset = 0
17 97 9c fe 3d 85 cd 15 time = 1700000000123456789 ns
66 69 72 73 74          "first"
```

## Offset file

```
u64     next       the next offset this consumer will read
u32     crc32c     of the 8 bytes above
```

Twelve bytes, big-endian. The file stores the *next* offset rather than the
last one processed, so a consumer that has committed nothing is simply `0`.
It is replaced atomically: write `<name>.offset.tmp`, fsync it, rename it over
`<name>.offset`, fsync the directory. A crash leaves the old offset or the new
one; a leftover `.tmp` is deleted at the next open. The checksum is there
because this file decides where reading resumes: an undetected flipped bit
would skip a range of acknowledged records or replay one.

Consumer names match `[a-z0-9][a-z0-9-]*`, so a name can never be a path.

## Recovery rules

On open, in order:

1. Every record is read once, in order, decoding only the 16-byte header.
   Records must have type 1, be at least 16 bytes, and carry consecutive
   offsets; anything else is `ErrCorrupt`. The same pass finds a torn record
   at the end of the newest segment and cuts it off (`wal.TruncateTail`, what
   `wal.Repair` does, without Repair's second full read). It was never
   fsynced, so its producer was never acknowledged, and its offset is reused.
2. Every offset file is loaded. A consumer whose next offset is below the
   oldest retained record would have lost data, so that is `ErrCorrupt` too.
3. The next offset to assign is one past the last record, **or the highest
   committed offset if that is greater**. Retention can delete every record
   (it keeps the newest *segment*, which may be empty); numbering from 0 again
   would give new records offsets a consumer has already committed past.
4. The newest segment is fsynced before any consumer may read it. A process
   crash leaves written-but-unsynced records in the page cache, where they
   read back as if durable; a consumer could apply and commit them, and a
   power cut afterwards would lose them under a committed offset. (Older
   segments were synced by the rollover that closed them.)

## Durability rules (not visible in the bytes)

- `Produce` returns only after the fsync that covers its record.
- Consumers read only up to the last fsynced record, never the last written.
- A failed fsync stops the topic until restart — the group commit's, and the
  one a rollover does before closing a segment (`wal.ErrSync`): after one, the
  OS may have dropped the dirty pages and a retry can report success for data
  that is not on disk. A group sync that was in flight when a rollover's fsync
  failed acknowledges nobody.
- A segment is deleted only when every offset in it is below every consumer's
  committed offset. A topic at its byte cap refuses new records rather than
  deleting unconsumed ones.
