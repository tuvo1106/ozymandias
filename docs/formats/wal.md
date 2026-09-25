# Write-ahead log format

Written by `internal/tsdb/wal`. The log is what makes an acknowledged write
survive a crash: the record is on disk and fsynced *before* the store says yes.
A store that acknowledges first is fast and lying.

The package is deliberately generic over record types — it frames opaque bytes
— because the same log carries the TSDB's records now and will carry M4's logs
and M7's queue later.

## Directory

```
wal/
  00000000.wal        segments, %08d, rolled at 32 MiB (DefaultSegmentSize)
  00000001.wal
  checkpoint.00000001 records carried forward past a truncation
```

Segments exist so deletion is possible. A single growing file could only be
truncated from the front, which no filesystem does well; a whole file can be
unlinked once its contents are durable elsewhere.

## Record framing

```
u32     len       length of payload, big-endian, ≤ MaxRecordSize (16 MiB)
u8      type      the caller's record type; 0 is reserved
u32     crc32c    Castagnoli checksum of payload only
bytes   payload   len bytes
```

Nine bytes of header per record. Three details earn their place:

- **Type 0 is reserved.** A region of a file that was allocated but never
  written reads as zeros, and zeros must not look like a valid record. A zero
  length with a zero type is rejected before the checksum is even consulted.
- **The checksum covers the payload, not the header.** A corrupt length is
  caught by the bounds check against the file size; a corrupt payload is caught
  here.
- **A batch never straddles a rollover.** `Log(recs...)` rolls the segment
  *before* writing if the whole batch would not fit, so a reader never sees
  half a batch in one file and half in the next.

## Worked example

Two records — type 1 with payload `01 02 6d`, then type 2 with payload `hi`:

```
00 00 00 03     len = 3
01              type = 1
3c 8e de 5b     crc32c(01 02 6d)
01 02 6d        payload
00 00 00 02     len = 2
02              type = 2
f5 9d d9 c2     crc32c("hi")
68 69           payload "hi"
```

23 bytes for two records.

## Reading, and the torn tail

Replay walks the checkpoint (if any) and then the segments in numeric order.

The hard case is a process killed mid-write. The last record of the last
segment can be short, or have a payload the checksum rejects — that is a **torn
tail**, and it is expected, not corruption: those bytes were never
acknowledged. The reader stops there, quietly, and replay continues with what
it has.

Stopping quietly is only safe if the tail is then *removed*. A process that
reads past a torn record and starts appending behind it has put every
subsequent write somewhere no reader will ever reach: the writes are
acknowledged, served from memory, and gone at the next restart. So `Repair` —
replay to the end, then `TruncateTail` — runs before the log is opened for
writing, and a store that skips it converts one lost record into unbounded
silent loss.

Damage anywhere *else* is a different matter. A bad record in an earlier
segment, or before the final record of the last one, means the disk lied or
something overwrote the file. The reader returns `ErrCorrupt` naming the file
and offset, because silently discarding the rest of the log would turn a disk
fault into invisible data loss.

> The distinction is carried by a `strict` flag on the reader, and it once had
> a bug worth remembering: the checkpoint writer replays a single segment, so
> *that* segment is the last one by construction, and real corruption in it was
> being laundered as a torn tail. Checkpointing now reads in strict mode.

## Truncation and checkpoints

`Truncate(dir, before, keep)` deletes segments numbered below `before`. Doing
that naively would orphan information: a sample record names its series by a
short id defined by a *series record* that may live in a segment about to go.

So truncation first replays the doomed segments, writes every record for which
`keep` returns true into `checkpoint.%08d`, fsyncs it, and only then unlinks.

The TSDB's policy (`head.KeepForCheckpoint(minValid)`) keeps every series
record, because a series definition may still be needed by a sample record that
survives. It keeps a **sample** record if any of its samples is at or after
`minValid` — the timestamp the head now refuses writes before.

That last part is the whole of it, and this document used to state it wrongly:
"by the time a segment is truncated, its samples are in a block". They are not.
Truncation follows a block cut, so everything *below the cut* is in a block —
but the head goes on holding up to 1.5 block ranges above it, and those samples
exist only in memory and in this log. Segments roll at 32 MiB, so at any real
write rate the head's oldest samples are several segments back. Dropping those
segments drops the only durable copy: measured at 1 987 440 of 3 232 730
acknowledged samples lost across a clean restart.

A crash during truncation leaves either the old segments or the checkpoint plus
the survivors. Never neither.

## Invariants

- A record is never partially visible: `Log` writes whole records, and the
  reader validates length and checksum before yielding one.
- `Sync` is what makes records durable. `Log` alone only reaches the page
  cache — see `storage.wal_sync_on_append` in `docs/operations.md`.
- Type 0 is never written and always rejected.
