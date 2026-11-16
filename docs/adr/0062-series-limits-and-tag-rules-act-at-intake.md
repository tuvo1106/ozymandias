# ADR-0062: Per-key series limits and tag rules act at intake, not at the consumer

- **Status:** Accepted
- **Date:** 2026-11-15

## Context

M7 §4 asks for limits on a key's series (per metric and in all) and a per-metric tag allow/deny list.
ADR-0059 deferred both, expecting them to act at the consumer with the key carried in the queued record.
The cardinality-bomb drill (ADR-0061) then showed what that means in queued mode: intake accepted 1,000,000
series with `202` and the consumer refused 990,000 afterwards, in a counter the sender never reads.

## Decision

**Both act at intake, before the queue, in both pipeline modes.**

- *Series limits.* Each key's principal holds a tracker of the series it has sent in the last hour (a hash
  per series, counts per metric). A new series is admitted only while there is room under
  `max_series_per_metric` and `max_active_series`; otherwise the request still answers `202` and the series is
  an item-level rejection with a reason, counted under `ozy.intake.rejected{reason}`. A series already
  tracked is always admitted, an idle one is forgotten after an hour, and a key with a per-metric limit and no
  total gets a one-million cap, so the tracker's memory is bounded by the limit it enforces, never by what a
  sender names.
- *Tag rules.* `metric_tag_config(metric, mode, keys)` in the metadata database, held in memory as an
  immutable map swapped on write. A rule drops tags from a metric's series and sketches before the type check,
  the limits and the queue, so the limit counts the merged series and every record on the queue already carries
  the tags it will be stored with.

Rejected alternative: at the consumer, with the key in the queued record. It changes the queue's record
format; it tells the sender nothing in queued mode; and a tag rule applied after acknowledgement would mean a record
queued before a rule is saved and consumed after is stored under different tags depending on timing, and a
redelivered record re-filtered by whatever the rules are by then. Applied at intake the queue holds what will
be stored, and redelivery stays exactly idempotent.

## Consequences

- The tracker counts what *this key sent*, not what the store holds: two keys sending one series each count
  it, and it is process memory, so after a restart it starts empty and relearns from traffic (the limit holds from
  then on; the store's own `max_series_per_metric` is the backstop that does not forget).
- Dropping a tag merges series. At one timestamp the store keeps the first point and counts the rest in
  `ozy.tsdb.ooo_rejected`; a count split by the dropped tag is not summed back together. A rule is for the tag that is
  a bomb. It does not rewrite stored data.
- A sketch counts as one series against a key's limits though it is stored as several.
- Not built: the Metric Summary's "top growing metrics" view from M7 §4 (the existing cardinality view and
  `ozy.intake.rejected` are what an operator has), rules by metric prefix, and a per-key tag rule.
