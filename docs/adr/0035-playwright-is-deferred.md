# ADR-0035: The Playwright end-to-end test is deferred

- **Status:** Accepted
- **Date:** 2026-10-03

## Context

The M3 test plan (§5, L11) asks for a Playwright test: build a widget, save,
reload, see data. It was not built. The UI's logic has unit tests (query
context and completions, dashboard state, save, layout, chart data), and the
editor and explorer were each driven by hand in a real browser over CDP during
their review, which found bugs no unit test had (a blank widget that said "No
data", a page kept mounted when only `:id` changed).

## Decision

**Defer the Playwright test.** It is removed from M3's acceptance until a UI
change ships a regression that the unit tests and a by-hand browser check could
not see. The by-hand browser check stays the rule for UI changes that alter
what a viewer sees, and its result goes in the PR.

## Alternatives considered

| Option | Why not |
|---|---|
| Build it now | It adds a browser binary and a dev dependency to CI, and a test that needs the full stack running, to cover a flow already checked by hand. The cost is paid on every run; the evidence so far is that the by-hand checks find the bugs. |
| Drop it without an ADR | The plan names it; dropping it silently is the divergence `AGENTS.md` forbids. |

## Consequences

A UI regression in a flow that spans the editor, the API and a reload is found
by a person, not CI. That is a known gap, not a hidden one, and it is listed in
the M3 notes.
