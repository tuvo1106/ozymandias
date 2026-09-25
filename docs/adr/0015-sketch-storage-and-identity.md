# ADR-0015: Sketches live beside the TSDB, and are keyed by a hash of the series

- Status: accepted
- Date: 2026-09-25
- Deciders: repo owner

## Context

M2 §6 says sketches are stored in Pebble under
`s | seriesID u64 BE | ts u32 BE`, and that "series identity and tag index are
*shared with the TSDB* (a sketch series registers in the same head/index with
metric type `distribution`), so selection works unchanged". Building it
surfaced two things the plan could not have known.

**The TSDB's series ids are not stable identity.** The head assigns an id when
it first sees a series and remembers it through the WAL, but `Truncate` drops a
series once its samples have moved into a block, and a series that later
reappears is given a *new* id. A sketch key built from a head id would point
somewhere else after the first block cut. Whatever keys the sketch store, it
cannot be that number.

**Registering a series in the head needs samples.** `head.Append` takes refs
*and* samples; there is no way to say "this series exists" without giving it a
value. Adding one would mean a new method on `tsdb.MetricStore`, which the
naive store implements too — a change to the interface every future store must
honour, in order to record a series whose actual data lives in a different
store entirely.

## Decision

**The sketch store keys by `FNV-1a` over the series' canonical key**
(`metric|k1:v1,k2:v2`), not by a number anyone hands out.

**Selection goes through the TSDB, by way of `<metric>.count`.** The intake
already writes `.count`, `.sum`, `.min` and `.max` as ordinary series beside
every sketch, because M2 §6 asks for them. A percentile query resolves its
series set by selecting `<metric>.count` with the caller's matchers, then
reads each resulting series' sketches. That is the same index, the same
matchers and the same code path as any other query — "selection works
unchanged" in the most literal sense available.

**The TSDB rules on ordering; the sketch store follows.** The intake writes the
scalars first and stores sketches only for the series the TSDB accepted, so
there is one decision about what is too old or out of order, not two that can
disagree. Within the sketch store a point *replaces* the point in its bucket,
which is what makes the agent's at-least-once delivery safe.

## Consequences

- Given a `tsdb.SeriesRef`, any process can compute where its sketches live.
  No coordination, no second source of truth for identity to keep in step
  across restarts, compactions and head truncation.
- Two series can hash alike — about one chance in 37 million at a hundred
  thousand series. The `'x'` keyspace holds each id's canonical key, so the
  store can tell: a collision is a rejection naming both series and a
  `ozy.sketchstore.id_collisions` counter, not two metrics quietly sharing a
  percentile. This is the cost of the decision and it is deliberate.
- `<metric>` itself is not a series in the TSDB, so it does not appear in
  `GET /api/v1/metrics`, which lists what the *store* holds. The metadata
  registry knows it — that is where its `distribution` type is recorded — so
  the M3 metric picker should list from there. Noted as the follow-up it is.
- A bucket whose `.count` sample the TSDB rejected as out of order is not
  selectable, so its sketch is not queryable either, even though the blob
  would have been written. Consistent rather than lossy: the two stores agree
  because only one of them decides.
- Retention is a `DeleteRange` per series rather than one sweep of everything,
  because the key orders by series first. A store with a hundred thousand
  series issues a hundred thousand small tombstones instead of reading every
  point.

## Alternatives rejected

**Assign ids and persist the mapping.** Exact, compact keys, no collisions.
Rejected because it is a second identity authority: every restart has to
reload it, every series has to be looked up before it can be written, and
nothing makes it agree with the TSDB's view of what exists. The collision
risk it removes is smaller than the drift risk it adds.

**Add `Register(refs)` to `tsdb.MetricStore`.** The literal reading of the
plan. Rejected because it widens an interface every store must implement in
order to hold a series with no samples in it, for the benefit of one caller —
and `.count` already exists, already carries the same tags, and is already
written in the same request.

**Store the base metric's samples too** (the observation count under
`<metric>` as well as `<metric>.count`). Rejected: it doubles the scalar cost
of every distribution and makes `avg:<metric>` return a count, which is a more
confusing answer than an error.

**One sketch series per store, in the TSDB, encoded as a chunk.** Rejected in
the plan already; recorded here because it is the obvious question. A chunk
encoder that assumes small deltas between consecutive values has nothing to
offer a structure whose "value" is two thousand counts.
