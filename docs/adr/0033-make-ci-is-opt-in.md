# ADR-0033: `make ci` is opt-in, not a per-PR ritual

- **Status:** Accepted
- **Date:** 2026-10-02
- **Supersedes:** [ADR-0024](0024-actions-is-the-full-gate.md)

## Context

ADR-0024 made GitHub Actions the full gate and removed pre-push, but kept a
hand-run `make ci` once per PR, with its result in the PR description. That
run is a third copy of checks the PR already gets: pre-commit runs the checks
for what was staged, and the required `ci` check runs the whole gate on the
exact head commit. For a docs-only or otherwise low-risk PR it adds about
three and a half minutes of background load and no information.

## Decision

**GitHub Actions stays the full gate; pre-commit stays the fast gate;
pre-push still runs nothing.** Nothing about those changes.

**`make ci` is run by hand only when it is worth it:** a change that touches
concurrency, storage, the wire format or anything timing-sensitive, a change
whose failure would be slow to find on the runner, or when the author simply
does not trust the staged-file checks. Everything else relies on pre-commit
and the `ci` check. `make smoke` is unchanged: run it when runtime behaviour
changed. The PR template's checkbox says which of these applied, or "not run".

## Alternatives considered

| Option | Why not |
|---|---|
| Keep ADR-0024 as is | A mandatory run that, for most PRs, repeats what pre-commit and Actions already do. |
| Run it per PR but only for non-docs changes | A rule with a carve-out to remember; "when it is worth it" covers the same cases without one. |

## Consequences

A failure pre-commit does not catch is found by Actions about five minutes
after the push instead of before it, sometimes costing one more push.
A PR's evidence is the green `ci` check, plus whatever the author chose to
run by hand and said so.
