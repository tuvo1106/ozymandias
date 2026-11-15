# ADR-0061: Queued intake is the default; chaos drills judge by an acks file

- **Status:** Accepted
- **Date:** 2026-11-15

## Context

ADR-0056 kept `pipeline.mode: direct` as the default "until the smoke and e2e suites run
queued", because a `202` in queued mode means "durable", not "queryable", and every test that
posts and then queries would race the consumers. M7 §6 also asks for scripted chaos drills,
each asserting its expected outcome.

## Decision

**Queued is the default.** `config.DefaultOzyd()` and `deploy/ozyd.yaml` say `queued`. The evidence
the ADR-0056 condition asked for: `make smoke` (142 checks, compose, on a clean volume) and
`make e2e` (Playwright, native) both pass with ozyd queued, as does the server's end-to-end suite
(`make test-queued`). `direct` stays selectable: it is the oracle the equivalence tests compare to
and the right mode where "queryable when answered" matters more than decoupling. The server's tests
pin the mode (`testConfig`) instead of inheriting it, so a test that means `direct` does not change
meaning with the default; flipping the default first hung the suite, because the queue's group-commit
timer runs on the server clock and most tests fake it.

**Drills decide by an acks file, not by log lines.** `loadgen -scenario ack-ingest` writes one line per
request: acknowledged (a `202` whose body rejects nothing) or unknown (anything else, with no retry, so an
unknown stays unknown). `verify` then reads the stores back and demands: every acked log stored exactly
once, nothing stored that was never sent, and acked <= stored points <= acked + unknown. Rejected
alternative: the sender retries until acknowledged, as an agent does. It is the better test of an agent
and the worse one of ozyd, because a retry that lands twice is a duplicate the sender caused and the oracle
could not tell from one ozyd caused. Logs carry a sequence number, so duplicates show one by one; series
points of a lane share a series, so only the bound can be checked.

**The drills are bash around the real binaries**, in `scripts/chaos/`, each its own temp dir and ports,
`make chaos` (quick settings, ~3 min) and `make chaos-long` (the acceptance settings). Three run on every
push (kill -9, agent down, disk full) and all run nightly at acceptance size, which is the split
docs/plan/testing.md L9 asks for.

## Consequences

- A `202` no longer implies a point is queryable, for every default install. A sender that read `202` as
  "stored" now sees a gap, and a cardinality limit is no longer visible to it: the bomb drill's intake
  accepted 200,000 series and the consumer refused 190,000 of them afterwards (`ozy.consumer.series_rejected`,
  `ozy.tsdb.series_limit_rejected`). Per-key limits (not built yet) will have to be checked at intake.
- Not drilled, and not claimed: **slow disk** (needs fault injection; the fsync-failure behaviour is covered
  by unit tests with an injected sync, and nothing at drill level proves acks follow durability), **block
  corruption** (ozyd refuses to start on a corrupt block; the plan says quarantine it, which reverses that and
  needs a startup scan of chunk data, so it is the owner's call), and the **apps' latency** with ozymandias
  down (needs the private app repos). `kill -9` cannot show a missing fsync either: the OS keeps the page cache
  of a killed process. Only a power cut or an injected failure can.
- `make smoke` assumes a clean volume ("the service is discovered ... and nothing else is" counts every service in
  the store), so it fails against a stack with real data in it. Run it on its own compose project name.
