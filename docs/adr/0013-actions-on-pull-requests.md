# ADR-0013: GitHub Actions runs on pull requests; the hooks stay the fast gate

- **Status:** Accepted
- **Date:** 2026-09-23
- **Supersedes:** [ADR-0010](0010-local-first-ci.md)

## Context

ADR-0010 turned GitHub Actions off because the repo was private on the Free
plan and its 2,000 monthly minutes were already spent by other private repos.
That reasoning was sound and is now void: the repo is public (ADR-0012), and
Actions minutes are free and unmetered for public repositories.

The rest of ADR-0010 still holds. Hooks catch a problem in seconds, before the
commit exists; remote CI catches it minutes later, after a push. Fast feedback
is worth more than remote feedback, and nothing about free minutes changes that.

What the hooks cannot do is verify that the tree is green on a machine other
than the one that wrote it — a stale build cache, a `--no-verify` push, an
uncommitted file the gates happened to read. That is the gap.

## Decision

Two gates, with different jobs.

**The git hooks are the fast gate.** Unchanged: pre-commit runs the checks the
staged file types need, pre-push runs the full `make ci`. Still never
`--no-verify` a push.

**GitHub Actions is the backstop.** The existing one-job workflow gets its
`pull_request` trigger back, with `paths-ignore` for docs-only changes. It
re-runs `make ci` from a clean checkout. It is not a thing to wait on: a PR
whose local `make ci` passed is expected to be green, and the workflow exists
to catch the cases where that expectation is wrong.

Still excluded, for the same reasons as before: `make smoke` needs the compose
stack, and the crash loop and `fuzz-long` cost minutes of wall clock. Those stay
manual, once per milestone. One job, because jobs bill rounded up to the minute
and that habit is worth keeping even when the bill is zero.

The PR description keeps carrying the local evidence. A reviewer should be able
to see that `make ci` and `make smoke` passed without opening the Actions tab.

## Alternatives considered

| Option | Why not |
|---|---|
| Leave Actions off | The premise for turning it off is gone, and "the laptop said it was fine" is not a gate a public repo should rely on alone. |
| Make Actions the primary gate and thin the hooks | Trades seconds of feedback for minutes. The hooks are what make a bad commit impossible rather than merely detected. |
| Add smoke and the crash loop to CI | Smoke needs docker compose and a healthy stack; the crash loop is 50 SIGKILLed child processes. Both belong to the milestone ritual, not to every PR. |
| Run on `push` as well as `pull_request` | Doubles the runs to re-verify what pre-push already checked. |

## Consequences

A PR now has a green check a reviewer can see, which matters more with the repo
public than it did when the only reader was the author.

Actions is a second place where the build can break for reasons unrelated to
the code — a pinned `golangci-lint` version, a runner image change. When that
happens the fix belongs in the workflow, not in a `--no-verify`.

`docs/plan/testing.md` §4 and `lefthook.yml`'s header describe two gates now.
