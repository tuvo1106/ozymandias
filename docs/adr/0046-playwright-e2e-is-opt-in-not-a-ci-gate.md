# ADR-0046: The Playwright end-to-end test is opt-in, not a CI gate

- **Status:** Accepted
- **Date:** 2026-10-06

## Context

ADR-0035 deferred the browser test until a regression got past the unit tests and the by-hand
browser check. M5 added the largest UI so far (services, trace search, flame graph, waterfall,
service map) and the first time it was driven in a real browser it found two bugs the 644
unit tests had not: a list rendering the same trace twice (duplicate React key) and waterfall
rows whose bars shifted right when a row had children.

## Decision

Add `web/e2e/*.e2e.ts` and `make e2e`: it starts the native dev stack, seeds synthetic traces
(`scripts/seed-traces.py`, no app data), runs Playwright and stops the stack. It is **not** in
`make ci`, `make web-check` or the Actions gate. Playwright's browser is installed on demand
(`npx playwright install chromium`). It supersedes ADR-0035 for the APM pages; M3's
editor flow is still covered by hand.

## Alternatives considered

| Option | Why not |
|---|---|
| Run it in Actions on every PR | Needs a browser binary, a built `ozyd` + agent and a seeded stack on every run, for flows the unit tests mostly cover. Revisit if a regression gets past `make e2e`. |
| Keep it deferred | The browser check just found two bugs; leaving it as a one-off means the next UI change repeats the by-hand work. |

## Consequences

A UI regression across the API and the page is found by whoever runs `make e2e`, not by CI. Run
it for changes to `web/src/pages/apm`. The logs tab is only checked for its panel and its link,
since the seed posts no logs; log → trace stays checked by the unit tests.
