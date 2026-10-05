# ADR-0038: M4 is built against synthetic fixtures for app-node and app-ruby

- **Status:** Accepted
- **Date:** 2026-10-04

## Context

M4's plan names three apps as the source of its log formats and its acceptance. Only app-python
(shikomi) is integrated; ADR-0037 closed M3 without app-ruby, and app-node was never integrated.
Three acceptance boxes and part of the test plan depend on the other two:

- `service:app-node @tag:api @ms:>200` returns the slow requests;
- midnight rotation of app-node's winston file loses nothing, "one real overnight run noted";
- app-ruby's plain-text Rails logs are searchable with no app change, and a placed order's phone
  number is findable nowhere in ozymandias;
- L1's grok patterns are tested "against real captured lines from all three apps".

## Decision

**M4 is built and accepted against synthetic fixtures for app-node's winston lines and app-ruby's
Rails and Sidekiq lines, and against real, scrubbed lines for app-python.** The fixtures are written
from the formats the plan documents (`docs/plan/M4-logs.md` §2) and committed as testdata. The
three boxes become:

- the app-node query box is met on synthetic winston JSON lines; its real-app half stays open;
- midnight rotation is proven by the fake-clock integration test; the overnight run is dropped;
- the app-ruby box is met on a synthetic Rails log, with the phone-number scan test run against a
  synthetic order; the claim "with no app change" is **unverified** until app-ruby is onboarded.

Each plan box keeps a line saying what was and was not checked.

## Alternatives considered

| Option | Why not |
|---|---|
| Onboard app-ruby and app-node first | It is the work the owner deferred in M3, and it blocks the tailer, pipeline, store and query language, none of which need a real app to be built. |
| Drop the boxes | The formats and the redaction scan are the milestone's point. A synthetic fixture exercises the same code paths. |

## Consequences

- A synthetic line encodes **my reading of the format**, so it cannot find a format surprise (a
  field order, an escape, a line the real app emits that the docs do not mention). That is the
  "claim nothing tests" bug class from `AGENTS.md`, accepted knowingly; the notes must say which
  patterns were never seen on a real app. app-python's patterns are the exception, tested on real lines.
- The redaction scan proves the rules catch the phone numbers the fixture contains, not that
  app-ruby never logs one in some other shape.
- When app-ruby or app-node is onboarded, capture real lines, scrub them, add them next to the
  synthetic ones, and tick the open halves. This ADR is where that follow-up is tracked.
