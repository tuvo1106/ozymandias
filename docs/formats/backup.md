# Backup directory

Written by `internal/backup` and the stores' `Backup` methods; read by
`ozyd restore`. A backup is a directory laid out as `data_dir` is, plus a manifest.

```
<backup>/
  MANIFEST.json
  meta.db                 SQLite, one file (VACUUM INTO)
  tsdb/blocks/…           the metric store's blocks (hard links, or copies)
  tsdb/wal/…              its write-ahead log, each file cut to its length at the snapshot
  logs/catalog.db         the log store's stream catalog (VACUUM INTO)
  logs/wal/…              its write-ahead log, cut
  logs/<day>/<id>.chunk   chunk files, copied (an open one up to its last whole block)
  sketches/…              a Pebble checkpoint
  traces/…                a Pebble checkpoint
  queue/<topic>/…         segments cut to their length at the snapshot, and
                          consumers/<name>.offset as they were at the cut (queued mode only)
```

## MANIFEST.json

```json
{
  "format": 1,
  "created_at": "2026-11-14T09:30:00Z",
  "version": "v0.1.0",
  "files": [
    {"path": "meta.db", "size": 94208, "sha256": "…hex…"}
  ]
}
```

- `format` is the manifest's own version. A reader refuses a version it does not know.
- `files` lists every file in the backup except the manifest, by slash-separated
  path relative to the root, sorted by path. `sha256` is lower-case hex.
- It is written last, after every store has finished, so it describes exactly
  what is there. A backup without one is a backup that did not finish.
- A path that is absolute or contains `..` is refused (it would write outside `data_dir`).

## What a reader must check

`ozyd restore` verifies before it writes anything: every listed file exists with the
recorded size and hash, and **no file exists that the manifest does not list**. A backup
is a closed set; one that has acquired a file would put into `data_dir` something nothing
vouched for. Restoring into a `data_dir` that is not empty is refused for the same
reason, and restored files are copies, never links.

## What the consistency is

Each store's part is a state that store can recover from, as if ozyd had crashed at the
moment of its snapshot. The stores are snapshotted one after another, queue first. See
`internal/backup/doc.go` for why that order is safe and what the one gap is.
