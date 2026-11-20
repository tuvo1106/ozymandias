# ADR-0063: Queued mode syncs each store once per consumer batch, before the offset commit

- **Status:** Accepted
- **Date:** 2026-11-20

## Context

ADR-0061 made queued intake the default. The benchmarks that followed (docs/benchmarks.md, #79 and #80) found
that the stores behind the queue stored 18-23% *less* than direct mode did (series 18.8k points/s against
24.4k, logs 17.8k lines/s against 21.8k), and a 30 s burst left a backlog that took 42-50 s to drain. The cause
was one fsync per `Store.Append` (about 4 ms on the machine measured): the consumer already polled up to 64
records at a time, but applied them one `Append` each, each with its own fsync.

`storage.wal_sync_on_append: false` removes the fsync (direct 1.25 M points/s, queued 89k stored) and is not
the answer. It is the documented bounded-window workaround; in queued mode it means the consumer commits the
queue offset while the points are only in the operating system's page cache. The queue has forgotten a record
it had promised was durable once acknowledged, for up to `wal_sync_interval`, on an OS crash or power cut. A
process kill loses nothing either way, so the kill -9 chaos drills would not notice.

## Decision

**In queued mode a consumer applies its whole batch, syncs the stores it wrote, then commits the offset.**
The order is apply, sync, commit, and the sync is the new step: one fsync covers up to `consumeBatch` (64)
records, where there were 64.

- Each store that fsyncs inside `Append` gets a mode that does not, and a `Sync()` that makes everything
  appended before the call durable: the metric TSDB (`SyncOnAppend` off plus the `db.Sync()` it already had),
  the log store (`DeferSync`, the WAL fsync), the trace store and the sketch store (`DeferSync`; Pebble
  commits without sync, and `Sync` writes an empty `LogData` marker with the Sync option, which makes the one
  ordered WAL durable up to that point).
- **One plan reads the decision twice.** `planSync(cfg)` says which stores are deferred; the code that opens
  each store and the code that tells each consumer what to sync both read it, so a store cannot be opened
  deferred and left unsynced. A deferred metric store that cannot `Sync` stops the server from starting. A
  test asks every store how it was opened against the plan for each shape of config.
- **Direct mode is unchanged.** `Append` still syncs: a `202` waits for it.
- **What `wal_sync_on_append` means.** On (the default): nothing leaves the queue before it is on disk, now
  by the batch sync. Off: the same bounded window as before, and nothing is synced before the commit. The
  log, trace and sketch stores have no window to choose: `logs.no_sync` and `traces.no_sync` still give up
  the fsync entirely, and the sketch store always syncs.
- **A failed sync** is retried after a backoff, applying the batch again first (every store is idempotent), and
  is counted in `ozy.consumer.sync_errors`; successful syncs in `ozy.consumer.syncs`. The WALs are fail-stop on
  a failed fsync (`StopOnSyncError`): after one the store refuses everything until restart, the topic backs
  up, and the restart redelivers the uncommitted batch from the queue. Re-applying matters only for a store
  that is not fail-stop, where a second fsync could report success over pages the kernel dropped.
- A sketch batch writes the sketch store and, for its exact aggregates, the TSDB, so that topic syncs both.

Rejected alternatives:

- **`wal_sync_on_append: false`.** Fast and gives up the queue's promise (above).
- **Sync before the commit, but per record.** The status quo; the whole cost.
- **A group commit in the WAL** (concurrent appenders share one fsync). It would help direct mode, which
  serialises on the fsync inside the commit lock, and is a larger change to a lock everything shares. It does
  not make a consumer batch cheaper: that is one fsync already. Still open for direct mode.
- **A larger batch.** Orthogonal; 64 records of up to 100 series is already past the point where the fsync is
  the cost (see the numbers below).

## Consequences

- **Sustained queued ingest is no longer fsync-bound.** 30 s runs, 8 senders, 100 per request, same machine:

  | | direct | queued, one fsync per record | queued, one per batch |
  |---|---|---|---|
  | series points stored per second | 22 100 | 18 800 | **81 500** |
  | log lines stored per second | 19 800 | 17 800 | **80 300** |
  | backlog after the run, as time to drain | none | 42 s / 50 s | 0 s / 5.5 s |

  (Direct measured 24 500 and 21 800 in the earlier run of the same code: about 10% run-to-run spread; one run
  per cell.) What limits queued now is not measured; the acknowledged rate (81.7k) and the stored rate are
  equal, so it is no longer the consumers.
- **A lone request is no faster.** A single point still waits for one queue fsync (11.7 ms to `202`, 16.8 to
  queryable).
- **A record is queryable before it is durable**, as in direct mode (the head is in memory before the WAL is
  synced). A crash in that window loses nothing: the offset was not committed, so the batch is redelivered and
  stored again. Live tail may show a log twice for the same reason (it already could).
- **A slow or failing sync stalls the whole topic**, where a per-record fsync stalled one record at a time. The
  topic backs up behind it and intake turns that into 429s as before.
- **What is tested, and what is not.** Pebble-backed stores (traces, sketches) are tested against a filesystem that
  discards unsynced writes: an unsynced append is lost, a synced one is not, and one sync covers many appends.
  The TSDB and log WALs are plain files, with no such filesystem here: their ordering is tested by counting
  fsyncs (the log store's WAL sync hook, the TSDB's `Syncs` counter) and the consumer's ordering by fakes that
  report the state at the moment of the sync (all appended, nothing committed). The kill -9 drills pass (3 of 3 at
  5 kills each) but cannot see page-cache loss. **A real power cut was not tested.**
- **A drill's oracle depended on throughput.** The kill9 drill failed once for a reason that was not data loss:
  its verify query asked for the whole time range at once and hit the API's 1500-bucket limit, because queued
  mode now acknowledged enough requests to run the range past it. The oracle now asks in 1000 s windows.
