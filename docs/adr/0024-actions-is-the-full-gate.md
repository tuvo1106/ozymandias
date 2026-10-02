# ADR-0024: GitHub Actions is the full gate; pre-push runs nothing

- **Status:** Superseded by [ADR-0033](0033-make-ci-is-opt-in.md)
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

**GitHub Actions is the full gate.** It runs the checks `make ci` runs — lint
through golangci-lint-action rather than `make lint`, and a web production
build on top — on every pull request, and `ci` stays a required check on
`main`. A PR merges when it is green on the head commit.

**It also runs on every push to `main`.** ADR-0013 rejected that as
re-verifying what pre-push had checked; without pre-push the reason is gone,
and something has to see `main` itself. The ruleset does not require a branch
to be up to date before merging, so two pull requests each green on its own
can merge into a `main` neither was tested against.

**pre-commit stays the fast gate.** Its globs widen to cover what only
pre-push used to catch: `.golangci.yml` runs the linter, and testdata and the
coverage thresholds run the Go tests. Never `--no-verify` a commit, except a
WIP commit fixed before pushing. **pre-push runs nothing.**

**`make ci` stays, run by hand** before opening a PR or when a change is risky,
and its output still goes in the PR description with `make smoke` when
runtime behaviour changed. On macOS it runs at background priority
(`taskpolicy -c background`, on the efficiency cores): 207 s instead of about
140 s, with no load on the performance cores. `CI_PRIORITY=full` runs it at
full speed, and is the first thing to try if a timing-sensitive test (an
`Eventually` with a 2 s deadline) fails only at background priority — after
which the deadline is what to fix.

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
template gains a checkbox for a hand-run `make ci` and its result, so the
author still runs the whole gate at least once per PR — at a time of their
choosing, not on every push.

Each merge costs one more Actions run, on `main`. A red one there means two
pull requests conflicted in meaning, and is fixed forward in a new PR.

`lefthook.yml`, `AGENTS.md`, `README.md`, `DESIGN.md` §8,
`docs/plan/testing.md` §4, the PR template and the workflow's header describe
the new split. A clone with hooks installed before
this change keeps a `pre-push` hook that calls lefthook, which now has nothing
to run for it.
