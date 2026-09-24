# ADR-0010: Git hooks are the CI; GitHub Actions is manual-only

- **Status:** Superseded by ADR-0013
- **Date:** 2026-09-19

## Context

The repo is private on the GitHub Free plan: 2,000 Actions minutes a month,
shared with the owner's other private repos. They were used up on
2026-09-19, and the owner decided to keep the repo private and not pay for
minutes. The first CI design ran four jobs on every PR and again on every
merge to `main`. Each job is billed rounded up to a whole minute, so each PR
cost about 5–6 minutes, twice.

Without CI, the testing standard (docs/plan/testing.md) still needs something
that refuses code that fails a gate.

## Decision

The git hooks are the gate (`lefthook.yml`):

- **pre-commit**, on every commit, runs only what the staged file types need:
  gofmt (auto-fixed), golangci-lint, race tests with the coverage gates
  (`scripts/check-coverage.sh`), docs-drift and no-app-coupling checks, and
  the web typecheck, ESLint on staged files, and Vitest `related` tests. Go's
  test cache, which also covers `-coverprofile` runs, re-runs only packages
  the change affects, so a typical commit adds about 1–2 seconds.
- **pre-push** runs the complete `make ci`: everything above over the whole
  tree, web coverage thresholds, and a short fuzz pass.
- **GitHub Actions** keeps the one-job workflow but with a `workflow_dispatch`
  trigger only. It is the remote runbook, not a gate.
- Smoke, the crash loop and long fuzzing are run by hand (`make smoke`,
  `make fuzz-long`), and their output is recorded in each milestone's notes
  and PR description as evidence.

## Alternatives considered

| Option | Why not |
|---|---|
| Keep GitHub CI as the primary gate | There are no minutes. It would fail on every PR, for reasons unrelated to the code. |
| Only pre-push, nothing at commit time | Broken commits reach history, and failures are found far from the change that caused them. |
| The full `make ci` on every commit | It re-runs web coverage and fuzz for a one-line Go change. The staged-type split keeps commits fast. |
| A self-hosted runner on the owner's Mac | It's free, and may come later (owner's call), but it only runs while the Mac is on. |
| Make the repo public (free minutes) | The owner decided to stay private for now. |

## Consequences

- Nothing verifies a PR remotely. The PR description must carry the local
  evidence: `make ci`, plus `make smoke` when runtime behaviour changed.
- The hooks check the working tree, not only the staged snapshot, so
  unrelated uncommitted edits can make a commit pass or fail. Commit or stash
  them first.
- `git commit --no-verify` bypasses everything. Use it only for a
  work-in-progress commit that will be fixed before pushing, and never on a
  push.
- To re-enable remote CI, restore the `pull_request:` trigger in
  `.github/workflows/ci.yml`. docs/plan/testing.md §4 lists what to move back.
