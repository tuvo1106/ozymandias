# ADR-0011: A sample at or before a series' newest timestamp is rejected

- **Status:** Accepted
- **Date:** 2026-09-20

## Context

`tsdb.MetricStore` has documented since M1 that "a sample at an existing
(series, T) replaces the old value". The naive SQLite store implements exactly
that, with `ON CONFLICT(series_id, t) DO UPDATE SET v = excluded.v`.

The M2 TSDB cannot. A series' samples live in a Gorilla chunk: a bitstream of
delta-of-delta timestamps and XOR'd values, where each sample is encoded
relative to the one before it and the encoding is not byte-aligned. There is no
addressable slot for "the sample at T". Replacing one means decoding the chunk,
re-encoding it, and rewriting it — and if the chunk is already inside a block,
rewriting an immutable file that other readers hold open.

So the two stores behind one interface would disagree on a documented
behaviour. That matters more than usual here, because M2's acceptance rests on
a **differential test** that runs the same operations against both stores and
compares the results. A contract the two implementations cannot both honour
makes that test meaningless: it would have to avoid the very inputs where the
disagreement lives.

This needs deciding now, at the start of the `db` package, because the
differential test and the intake path's error handling both depend on the
answer.

## Decision

The contract becomes append-only, and every store implements it:

- A sample whose timestamp is **greater** than the series' newest stored
  timestamp is appended.
- A sample with the **same timestamp and the same value** is accepted as a
  no-op. Agent delivery is at-least-once, so a retry re-sends what it already
  sent; treating that as a conflict would turn a successful write into a
  reported failure.
- Anything else at or before the newest timestamp — an older timestamp, or the
  same timestamp with a different value — is **rejected and counted**, never
  silently applied.

The naive SQLite store is changed to match: it tracks each series' newest
timestamp and refuses rather than overwriting. It stays the reference
implementation, and a reference that behaves differently from the thing it is a
reference for is worse than no reference.

Rejections surface as `AppendResult.Rejected` entries, not as an error for the
whole batch, and are counted in `ozy.tsdb.ooo_rejected`.

## Alternatives considered

| Option | Why not |
|---|---|
| Keep "last write wins"; have the TSDB decode and re-encode the chunk | Turns a cheap append into a read-modify-write of up to 120 samples, and is impossible once the chunk is in an immutable block. The cost lands on the hot path to serve a case that does not occur in normal ingest. |
| Keep the contract, let each store differ, and document it as undefined | Makes the differential test — M2's main correctness argument — unable to cover the interesting inputs. An interface whose semantics are "ask the implementation" is not a contract. |
| Reject exact duplicates too, for a simpler rule | Breaks at-least-once delivery: an agent retry after a timeout would be reported as a rejected series, and the operator would chase a data-loss alarm caused by the retry that prevented data loss. |
| Accept out-of-order samples into a separate out-of-order chunk range, as Prometheus does since 2.39 | The right answer eventually, and much more machinery: a second chunk range per series, merge-on-read, and its own compaction. Deferred; this ADR does not preclude it, since going from "rejected" to "accepted" is a compatible change. |

## Consequences

- One semantic for every metric store, so the differential test can feed both
  stores arbitrary input — including out-of-order samples — and demand
  identical answers. That is a much stronger test than the one the old contract
  allowed.
- Duplicate suppression is a store-level guarantee, so the intake path does not
  need its own de-duplication for retried batches.
- The naive store pays for a `lastT` map, loaded with one `GROUP BY` query at
  open. It is the slow reference store; this is an acceptable cost and keeps it
  honest.
- Out-of-order data from a client is now visible as a rejection count instead
  of quietly winning. That is the intended behaviour — a monitoring system that
  silently changes what it was told is the worst kind of wrong — but it means
  `ozy.tsdb.ooo_rejected` needs to be on the self-metrics dashboard, and
  M7's hardening should revisit whether a bounded out-of-order window is worth
  the machinery.
- Anything relying on rewriting history — backfill, late-arriving batches from
  a long agent outage — is out of scope until an out-of-order chunk range
  exists. `docs/plan/M7-hardening.md` is where that belongs.
