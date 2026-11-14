# Agent disk buffer format

The agent's disk buffer (`internal/agent/spool`, M7 PR 4) holds gzip'd intake
payloads that could not be sent, so that an ozyd outage longer than the memory
queue, or a restart of the agent, loses nothing while the disk has room.

## Layout

`forwarder.buffer_path` is a directory with one spool per intake endpoint:

```
<buffer_path>/series/    POST /v1/series
<buffer_path>/sketches/  POST /v1/sketches
<buffer_path>/spans/     POST /v1/traces
```

Logs are not buffered here. A log batch is sent synchronously and the tailer
commits its file offset only after a `2xx`, so the log file itself is the buffer.

Each spool directory holds:

| File | Content |
|---|---|
| `00000000.wal`, `00000001.wal`, … | segments of the shared WAL framing, oldest first |
| `head` | the delivery cursor, one text line |

## Segments

Segments use the framing of `internal/tsdb/wal`, unchanged
([queue-segment.md](queue-segment.md) describes the same bytes for the queue):

```
record  = length u32 BE | type u8 | crc32c u32 BE | data[length]
```

`type` is always `1`. `data` is:

```
series u32 BE | body ...
```

`series` is the number of series, sketches or spans in the payload, kept so that
dropping a payload can be counted without decompressing it. `body` is the request
body exactly as it would be POSTed: gzip'd JSON, sent with `Content-Encoding: gzip`.

A segment is rolled at `min(max_disk_bytes / 8, 8 MiB)`. The segment is the unit
of deletion (see below), so it is also the granularity of dropping. `max_disk_bytes`
is at least 16 MiB, so a segment (2 MiB at the least) holds any payload the
forwarder builds.

## The cursor

`head` is `<segment> <offset> <crc32>\n`: the segment number and byte offset just
past the last payload delivered, and the IEEE CRC32 of the first two fields as
text. It is replaced atomically (write a temp file, rename) and **not fsynced**.

The cursor is written every 8 deliveries, when a segment is finished, and on a
clean stop, so a kill repeats at most 8 payloads. A missing or invalid cursor, or
one naming a segment that is gone, means: start at the beginning of the oldest
segment. An offset that is not the end of a record resumes at the last record end
at or before it. Either way payloads already delivered may be sent again, none is
lost, and that is harmless because every intake path is idempotent for a repeated
payload.

## Rules

- **Write:** append the record, fsync, then count it. The payload is on disk
  before the forwarder forgets it. A payload larger than a segment is refused (the
  cap could never evict it). A failed fsync stops the spool for good until a
  restart (`ozy.agent.forwarder.spool_enabled` reads 0): the record may be in the
  file but is not counted, and a restart will find and deliver it, a repeat of
  something the caller was told had failed.
- **Read:** strictly oldest first from the cursor. One payload is read at a time.
- **Deliver:** when ozyd accepts a payload, or refuses it for good (a 4xx other
  than 408/429), the cursor moves past it. A segment whose last payload is
  delivered is deleted.
- **Cap:** when the spool exceeds `max_disk_bytes` the oldest segments are
  deleted, delivered or not, and the series in the undelivered ones are added to
  `ozy.agent.forwarder.dropped`. The newest segment is never deleted.
- **Open:** every segment is scanned to rebuild the counts. A torn tail on the
  newest segment is cut. A record that fails its CRC, or is malformed, ends its
  segment at the last good record; what follows in that segment is skipped and
  counted as dropped, and later segments are used as normal. If the damaged segment
  is the one being appended to, it is cut at the cursor and appending resumes after
  the cut. A read that fails for another reason (no file descriptors, an I/O error)
  is not damage: nothing is skipped and the read is tried again.

## What the forwarder does with it

Payloads go to memory first. When the memory queue (`forwarder.max_queue_bytes`)
is full, its **oldest** payloads move to their endpoint's spool, so each spool
always holds older data than the memory queue and delivery stays oldest-first.
At shutdown, payloads still in memory after the final delivery attempt are moved
to the spool instead of being dropped.

Sending order is by endpoint priority, series, then sketches, then spans. Within
an endpoint the spool's oldest payload is sent before anything newer, and while
that payload is in backoff nothing newer of the endpoint is sent. Payloads still in
memory are not strictly ordered: one waiting out its backoff does not hold back the
ones behind it. A payload being sent when the memory queue overflows stays in
memory (it would otherwise be delivered and still spooled). At shutdown, an
endpoint with older payloads on disk is not sent from memory at all: its memory
payloads go to the disk behind them.

Format changes are breaking: a different `type`, or a changed `data` prefix, would
be read as damage and skipped, so the buffer must be emptied (or deleted) across
such a change.
