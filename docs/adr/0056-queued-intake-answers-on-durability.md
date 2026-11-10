# ADR-0056: Queued intake answers on durability; stores reject out loud, not in the response

- **Status:** Accepted
- **Date:** 2026-11-10

## Context

PR 2 of M7 puts the queue of [ADR-0055](0055-own-queue-not-kafka-or-nats.md) between
intake and the stores. Until now a `202` meant "stored": the response listed what the
TSDB, sketch store and log store had refused. With a queue the `202` is sent when the
request is durable, seconds before any store has seen it, so the answer cannot carry
what a store later refuses (a point older than the TSDB accepts, a sketch whose
`.count` was refused). Three things had to be decided.

## Decision

**1. What intake still answers, and what moves to counters.** Intake does the work
that needs no store: decode, validate, and the metadata-registry type check. Those
rejections stay in the response, in both modes, because they are the sender's own
mistake and the sender is still listening. Anything a *store* refuses is counted by
the consumer (`ozy.consumer.series_rejected`, `…logs_rejected`, …) and logged, not
answered. In queued mode `accepted` means "will be applied". `docs/wire-protocol.md`
§0 says so. Rejected alternative: also run the registry check in the consumer, so
intake only validates; simpler, but a sender then never hears of a type conflict.

**2. Both modes stay.** `pipeline.mode: direct | queued`, default `direct`. The
server's end-to-end suite runs in both (`make test-queued`, a CI step), which is the
evidence that the two answer queries alike. The default flips when the smoke and e2e
suites run queued (PR 7): a `202` no longer implies "queryable", and flipping it
earlier would make every test that posts and then queries race the consumers.
ozyd's own self-metrics keep the direct path (`Intake.Ingest`): ozyd's view of
itself should not stall behind a backlog it is reporting on.

**3. Idempotency is each store's job; logs needed one.** At-least-once delivery
needs every store to apply a record twice as it applies it once. The TSDB already
did (a repeated `(t, v)` is a no-op; an older point is counted, not an error), the
sketch store is last-write-wins by bucket, the trace store overwrites a span by key.
The log store had no such property, so `Append` now drops an entry the stream's
head already holds, keyed by `(timestamp, 64-bit hash of the encoded log)`. Two
choices inside that:

- The check is against the head *as it was before the batch*, so identical lines
  inside one batch survive (a burst of the same message in one millisecond is real)
  while the same batch twice does not. A different batch carrying an identical
  `(ts, line)` for the same stream is collapsed, as Loki does.
- The memory is the head, not the store. A batch whose first delivery has been
  sealed into a block is not recognized on redelivery. Closing that window needs
  either a per-stream memory that survives a seal and a restart, or an idempotency
  token from the queue; neither is built, a test pins the gap
  (`TestAppend_RedeliveryAfterSealIsNotRecognized`), and the M7 acceptance
  criterion "no visible duplicates after kill -9" is therefore **not yet shown for
  logs** (see `docs/notes/M7.md`). Rejected alternative: a queue-offset high-water
  mark inside the log store, exact but coupling the store to the queue.

**4. A consumer retries a store failure and skips only a record that cannot decode.**
A store error is not the record's fault: the consumer retries it with exponential
backoff (100 ms to 5 s) and the topic backs up behind it, which intake turns into
`429` as the queue fills. A record that does not decode is counted
(`ozy.consumer.poison_records`), logged and committed past, since retrying it would
stall the topic for ever.

**5. The group-commit window is 1 ms, not the spec's 5 ms.** Measured on Linux (a
Colima VM, `go test -bench Produce`), 1 ms beat 5 ms at every concurrency above one
producer (128 producers: 19 µs vs 55 µs per record; 16: 161 vs 430) and beat no
window at all by an order of magnitude (355 µs at 128). A lone producer pays about
2.4 ms instead of 0.6 ms, which does not matter for an agent that posts every ten
seconds. ADR-0055's "5 ms" is superseded by this value.

## Consequences

- A sender cannot learn from the response that a store later refused its data; an
  operator learns from the consumer counters, which belong on the self dashboard (PR 6).
- Live tail may show a line twice after a redelivery: the hub is told after the store
  and tracks no offsets.
- Queued mode adds a durable hop: intake latency is one group-commit, and a query
  sees the data one consumer step later.
- `Intake.Apply` is exported so the redelivery property test (`TestIdempotent_…`) can
  drive the consumer step with a chosen delivery order.
