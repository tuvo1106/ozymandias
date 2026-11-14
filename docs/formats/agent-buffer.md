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
of deletion (see below), so it is also the granularity of dropping.

## The cursor

`head` is `<segment> <offset> <crc32>\n`: the segment number and byte offset just
past the last payload delivered, and the IEEE CRC32 of the first two fields as
text. It is replaced atomically (write a temp file, rename) and **not fsynced**.

A missing or invalid cursor, a cursor naming a segment that is gone, or an offset
that is not the end of a record, means: start at the beginning of the oldest
segment. That re-sends payloads that were already delivered, never loses any,
and is harmless because every intake path is idempotent for a repeated payload.

## Rules

- **Write:** append the record, fsync, then count it. The payload is on disk
  before the forwarder forgets it.
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
  counted as dropped, and later segments are used as normal.

## What the forwarder does with it

Payloads go to memory first. When the memory queue (`forwarder.max_queue_bytes`)
is full, its **oldest** payloads move to their endpoint's spool, so each spool
always holds older data than the memory queue and delivery stays oldest-first.
At shutdown, payloads still in memory after the final delivery attempt are moved
to the spool instead of being dropped.

Sending order is by endpoint priority, series, then sketches, then spans; within
an endpoint the spool's oldest payload is sent before anything newer, and a
payload in backoff holds back the ones behind it in its endpoint.

Format changes are breaking: a different `type`, or a changed `data` prefix, would
be read as damage and skipped, so the buffer must be emptied (or deleted) across
such a change.
