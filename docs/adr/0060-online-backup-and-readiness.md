# ADR-0060: Online backup by per-store snapshots; /readyz reports a stopped store

- **Status:** Accepted
- **Date:** 2026-11-14

## Context

M7 §5 asks for `/readyz` apart from `/healthz`, a consistent backup of a running ozyd,
`ozyd restore`, and the TSDB and log store stopping after a failed fsync as the queue does
(the last was deliberately deferred in PR 1, because a store that stops while `/healthz` stays
green is an outage nobody is told about). Four decisions were not obvious.

## Decision

**Readiness is a list of named checks, and a stopped store is the case it exists for.** `/readyz`
is 503 while the metadata database does not answer, a store or queue topic has stopped, the queue
consumers are not all running (queued mode), or shutdown has begun, and its body says which. `/healthz`
stays "the process is alive": a liveness failure restarts the process, and "a store stopped" is
fixed by a restart, so reporting it on liveness would be correct but would hide the difference between
"restart me" and "do not send me traffic". Both the TSDB and the log store now open their log with
`StopOnSyncError`; each exposes `Broken()`, which feeds `/readyz` and `ozy.store.broken{store}`.
The Pebble stores are not in the list: Pebble owns its own log and its own fsync policy.

**Backup is a snapshot per store, not a stop-the-world.** Each store snapshots itself under its own
locks to a state it can recover from as after a crash: the TSDB and the log store cut their logs to a
length under the log's lock and copy that prefix (a log only grows at its tail, so a prefix is a
consistent copy of a file still being written), blocks are hard-linked, the SQLite databases use
`VACUUM INTO`, Pebble uses its checkpoint, the queue cuts segments to a length and writes the consumers'
offsets *as they were at the cut*. Rejected alternative: pause intake and the consumers, then copy the
data directory. It is simpler to state and wrong in practice: it needs a second pause mechanism in
every writer (the self-reporter writes through intake directly, monitors write SQLite, retention and
compaction run on timers, Pebble compacts in the background and a plain copy of a live Pebble directory
is not consistent), and a long pause is an outage the agents then have to buffer through.

**The queue is cut first, then the stores.** A store's snapshot is then never older than the queue's
committed offsets, so a restore redelivers records both hold and the stores' idempotent consumers
absorb them. Cutting the queue last could record as committed an offset whose records the
store's earlier snapshot never saw: silent loss. The cost is the gap the queue already has: a log sealed
into a chunk between the cuts can be stored twice (ADR-0056). Said in the docs, not hidden.

**The backup is made by the running process and requested over HTTP.** `ozyd backup` is a client of
`POST /api/v1/admin/backup`, because Pebble holds its directory exclusively and a copy of live files made
from outside the process is not consistent with what the process holds in memory. Because the endpoint
writes a tree of files, it exists only when `backup.root` is set, writes only to `<root>/<name>` with a
name restricted to `[A-Za-z0-9._-]`, and `root` must be absolute and neither inside nor containing
`data_dir`. With auth on it needs the admin login like the rest of `/api`.

**A backup is a closed set vouched for by a manifest, written last.** Every file's size and SHA-256 are
listed, and `restore` verifies all of them and that no other file is present before writing anything.
It refuses a non-empty `data_dir` rather than merge two histories, and copies rather than links.

## Consequences

- Hard-linked blocks make a backup on the same filesystem cheap, and not a copy: it survives mistakes and
  ozyd's bugs, not the loss of the disk. Documented where the setting is.
- A log store backup holds that store's write lock for its duration, so log ingest waits. Chunk files are
  copied because reopening a closed chunk cuts its footer off in place, which would corrupt a link. Large
  days make this long; not measured yet.
- `metric_store: naive` has no online backup and says so.
- Restore is offline only. A restored `data_dir` starts like a crashed one; nothing checks that the
  restored ozyd's version can read the backup's data beyond the manifest's `format`.
- Not covered by a test: a crash in the middle of a backup (it leaves a directory with no manifest, which
  `restore` refuses), and the `queue:<topic>` readiness check at server level (the queue's own tests pin
  that a failed fsync stops a topic).
