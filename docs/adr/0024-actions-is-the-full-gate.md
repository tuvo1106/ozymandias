# ADR-0024: GitHub Actions is the full gate; pre-push runs nothing

- **Status:** Accepted
- **Date:** 2026-09-28
- **Supersedes:** [ADR-0013](0013-actions-on-pull-requests.md)

## Context

ADR-0013 kept two gates: the git hooks as the fast one — pre-commit on the
staged files, pre-push running the full `make ci` — and GitHub Actions
re-running `make ci` on every pull request as the backstop.

In practice the backstop became a gate of its own, and the pre-push run became
a copy of it:

- `ci` is a required status check on `main`, so no pull request merges without
  it, and merging waits for it to pass on the exact head commit
  (`main` has no push-triggered run, so that check is the only one that sees
  what is merged). Every PR has been waiting on Actions anyway.
- Every push ran the whole gate twice: once on the laptop, then again on the
  runner. The laptop run is the one that costs something. Every tool in it
  sizes itself to all the cores — `go test -p`, each race binary's
  GOMAXPROCS, the fuzz workers, vitest's pool — so each push held a
  10-core laptop at full load for about 140 s, and a PR with four review
  rounds did that four times. The owner noticed the machine heating up.

What pre-push still bought was catching a failure before the push rather than
about five minutes after it. The pre-commit hook already catches most of those
at commit time: lint, gofmt, the race tests of the affected packages with
their coverage gates, docs drift, and the related web tests.

## Decision

**GitHub Actions is the full gate.** It runs `make ci`'s targets on every pull
request, and `ci` stays a required check on `main`. A PR merges when it is
green on the head commit.

**pre-commit stays the fast gate**, unchanged. **pre-push runs nothing.**

**`make ci` stays, run by hand** before opening a PR or when a change is risky,
and its output still goes in the PR description with `make smoke` when
runtime behaviour changed. On macOS it runs at background priority
(`taskpolicy -c background`, on the efficiency cores): 207 s instead of about
140 s, with no load on the performance cores. `CI_PRIORITY=full` runs it at
full speed.

## Alternatives considered

| Option | Why not |
|---|---|
| Keep `make ci` on pre-push at background priority | Measured: cooler, but slower (207 s per push), and still a full duplicate of a check every merge already waits for. |
| Keep pre-push with a subset (lint, `go test` without race) | Most of that subset is what pre-commit already ran on the same changes; the rest is what Actions runs anyway. A third tier to keep in step with the other two. |
| Cap each tool's parallelism (`-p`, `-parallel`, `--maxWorkers`) | Four flags on four tools that drift, and a Go test binary still starts one thread per core; the background clamp caps everything the gate starts with one wrapper. |
| Keep ADR-0013 as is | Heats the laptop for a check that is repeated minutes later by the one that actually gates the merge. |

## Consequences

A broken push is found by Actions, about five minutes later, instead of before
the push. That is sometimes one more push. `--no-verify` stops mattering for
pushes, since there is nothing to skip.

A PR's evidence is now the green check plus whatever was run by hand. The PR
template's request for local `make ci` output stands, so the author still runs
the whole gate at least once per PR — at a time of their choosing, not on
every push.

`lefthook.yml`, `AGENTS.md`, `README.md`, `docs/plan/testing.md` §4 and the
workflow's header describe the new split. A clone with hooks installed before
this change keeps a `pre-push` hook that calls lefthook, which now has nothing
to run for it.
