# ADR-0065: Direct-mode appenders share the WAL fsync; a sample is readable before it is durable

- **Status:** Accepted
- **Date:** 2026-11-21

## Context

ADR-0063 removed the per-record fsync from queued mode. Direct mode (`pipeline.mode: direct`, an explicit
choice since ADR-0061) was left as it was: every `Append` writes the WAL and fsyncs it before it returns, and
**concurrent appenders queue behind each other through that fsync**. Measured on the machine the benchmarks
use (an fsync is about 4 ms): a one-sample `Append` took 4.5 ms with 10, 80 or 640 goroutines alike
(`BenchmarkDB_Append/sync=true/batch=1`), about 220 appends a second however many senders there were. The
benchmark's own comment already said so: "a second appender does not overlap with the first".

The obvious fix, letting concurrent callers of `wal.Sync` share one fsync, does nothing on its own. Both stores
that fsync inside `Append` hold their commit lock across it:

- `Head.Append` holds `logMu` across resolve, log, **fsync** and apply. The comment on that section records
  that each of its boundaries "was a bug before it was a comment": logging and applying together is what makes
  WAL order be apply order, and resolving inside it is what makes the series pointer trustworthy against
  `Truncate`.
- `logstore.Store.Append` holds `wmu` across dedupe, log, fsync and the publish to the stream heads.

A second appender cannot reach the WAL while the first is in its fsync, so there is nothing to share. Group
commit needs the fsync **outside** those locks, and that breaks an invariant the head states: *a sample is
written to the log, and synced, before it becomes visible, so nothing can be read that would not survive a
crash.* Moving the fsync out means a sample can be read for the few milliseconds between being applied and
being synced.

The owner chose that trade ("visible first, ack after sync") over the alternative that keeps the invariant
(see below).

## Decision

**`WAL.Sync()` is a group commit, and the stores call it outside their commit locks.**

- **`wal.Sync` shares fsyncs.** The first caller runs the fsync; callers that arrive while it runs wait and are
  answered together by the next one, which starts when the first ends and is run by one of them. There is no
  background goroutine and no timer: a caller that arrives when nothing is running fsyncs at once, so a lone
  appender pays what it paid before. The fsync does not hold the log's lock, so `Log` goes on while one is in
  flight and those records join the next group. Every caller wrote its records before it called `Sync`, and the
  fsync that answers it starts after that, so each returns only once its own records are durable.
- **The contract of `Sync` is unchanged** (everything logged before the call is durable on return), so the
  consumers (ADR-0063), the periodic sync, `Close` and every other caller are unaffected.
- **`Head.Append` and `logstore.Append` log and apply under their lock as before, release it, then call
  `Sync`.** WAL order is still apply order, because both still happen in one critical section. Only the fsync
  moved.
- **A failed fsync fails every caller in its group**, never one of them quietly. On a log that stops on a failed
  fsync (every log ozyd opens), the log is stopped before the next group can run, and **nothing fsyncs the file
  again**: a second fsync of pages the first failed to write can succeed over data the kernel dropped, which is
  the failure `StopOnSyncError` exists for. This needed care in two places. A rollover (which fsyncs the old
  segment, then closes it) now waits for a group fsync in flight, and refuses to proceed over one that failed;
  `Cut` and `Close` do the same. Without that, a rollover starting while a group's fsync was failing would
  fsync the same pages "successfully" and the next group would acknowledge records that were lost.
- **A batch whose fsync failed is reported failed** (`Index -1` from the head, an error from the log store)
  although its samples were already applied. The sender was told to retry. A retry on a stopped log is refused
  at the log, so it cannot be acknowledged; on a log that is not stopped (tests) it writes the batch again and
  syncs that.
- **The log store's dedupe path owes a sync too.** With `DedupeHead` (queued mode) a retried batch whose logs
  are all in the head already has nothing to write. Because those logs may be there from an `Append` that has
  not synced yet, or whose sync failed, it must still wait for an fsync before it says "stored". Without that,
  a visible-first store would acknowledge a retry for logs a power cut could still take.

### What this guarantees and what it does not

- **Unchanged:** an `Append` that returned success is durable. A power cut after the return loses none of it
  (`TestPowerCut_EveryAcknowledgedConcurrentAppendSurvives`, and its log-store twin, cut the WAL at the length
  the last successful fsync covered while twelve appenders share fsyncs).
- **Changed:** a sample is queryable from the moment it is applied, before its fsync returns. A crash or power
  cut in that window loses a sample that a query may have shown and that **nobody was told was stored** (its
  `Append` had not returned). For a monitoring store that reads its own recent writes this is a small cost; it
  would not be for a store whose readers act on what they read.
- **On a failed fsync the head keeps serving samples that were not made durable** until the process restarts
  (the stopped log refuses every write, and the readiness check fails). Readers see data the sender was told
  failed. A restart replays what the WAL holds and the senders retry.

## Alternatives considered

| Option | Why not |
|---|---|
| Durable before visible: log under the lock, share the fsync outside it, then apply in WAL order through a ticketed applier | Keeps the head's invariant. It rewrites the most bug-prone section of the head (series resolution, the `Truncate` interaction and the WAL-order argument all lean on one critical section), for a mode that is no longer the default. The owner chose against it. |
| Leave direct mode as it is and say queued is the answer for concurrent senders | Honest, and what ADR-0061 implies, but it leaves `pipeline.mode: direct` with a throughput ceiling of one fsync per request that a client cannot work around. |
| A timer-based window (wait a millisecond for company before fsyncing) | Adds latency to a lone appender to help a crowd, and the queue's group-commit measurements (ADR-0055) found a window only added latency on this disk. Opportunistic sharing (the next group is whoever arrived during the fsync) costs a lone appender nothing. |
| A background sync goroutine that appenders wait on | One more goroutine to start, stop and leak, and a wake-up hop on the path of every append, for the same sharing. |
| `wal_sync_on_append: false` | The documented bounded-loss workaround. Not durable on return. |

## Consequences

- **Direct mode scales with concurrent senders.** `BenchmarkDB_Append/sync=true/batch=1`: 4.5 ms per append
  at every concurrency before, 0.28-0.88 ms after (about 5-16 times the appends a second). Large batches gain
  less, because each already amortized its own fsync (`batch=1000`: 4.9 ms before, 2.2-4.0 ms after).
  The figures and the method are in `docs/notes/M7.md`.
- **The new states have tests**: the fsync really runs outside the commit lock (an `Append` commits while another
  waits for its fsync); a failed fsync fails its whole group and nothing fsyncs after it; a rollover cannot hide
  a failed fsync; `Close` waits for one in flight; a retry of an unsynced batch is not acknowledged.
- **The WAL gained a lock (`fmu`) and a flag (`failedSync`).** The order is always `mu`, then `fmu`; an fsync
  outside `mu` holds `fmu` shared, and a rollover, `Cut` or `Close` holds it exclusively. Anyone adding a new
  user of the segment file takes `fmu` or does not touch the file.
- **Not covered.** The trace and sketch stores: they sync through Pebble, not through this WAL, and were not
  changed or measured here. A rolled segment's directory fsync is not part of this. Linux was not measured. No test can cut the power between an
  fsync's start and its end; the power-cut tests cut after it.
