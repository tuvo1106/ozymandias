# ADR-0014: SDKs stay vendored, now because they are pre-1.0

- **Status:** Accepted
- **Date:** 2026-09-23
- **Supersedes:** [ADR-0007](0007-sdk-distribution-vendored.md)

## Context

ADR-0007 chose vendored build artifacts — a packed tarball and a built wheel
copied into each app's `vendor/` — and gave three reasons: the project was
private so nothing could be published; one app's Docker build context excludes
its parent directory; and npm cannot install from a subdirectory of a git repo.

The first reason is gone (ADR-0012). The other two are unaffected by
visibility and still hold. So the decision needs re-deriving rather than
reversing, and its premise needs to be on the record correctly.

Publishing would also now hit a naming problem worth recording while it is
cheap to know. `ozy` is unclaimed on PyPI. On npm it is taken, as is
`ozymandias`, so a first npm publish needs the scoped name `@tuvo1106/ozy`.

## Decision

The SDKs stay vendored, for a different reason than before: **the API is
pre-1.0 and still moving.** M5 adds tracing to both SDKs, which will change
their public surface; publishing 0.x releases that nobody outside this repo
consumes would buy nothing and commit to names and semantics early.

What being public does change:

- Git installation now works for Python — `pip install
  "git+https://github.com/tuvo1106/ozymandias#subdirectory=sdk/python"` — and
  is documented in `docs/sdk/python.md` as a second supported method. npm still
  cannot do this, so Node keeps the tarball.
- The M8 publishing pipeline (tag → build → publish) stays a dry-run, but the
  reason in `docs/plan/extensibility.md` §7 is now "pre-1.0", not "private".
- Names to claim at the first publish: PyPI `ozy`, npm `@tuvo1106/ozy`.

`scripts/release-sdk.sh` is unchanged. It still takes target directories as
arguments and still names no app.

## Alternatives considered

| Option | Why not |
|---|---|
| Publish 0.1.0 to PyPI and npm now | Commits to a public API that M5 will change, and claims two registry names to hold something nobody depends on. |
| Publish to GitHub Packages instead | Same commitment, plus an authenticated registry for consumers, which is strictly worse than a vendored file. |
| Switch Python to a git dependency and keep Node vendored | Two mechanisms to document and keep working, for one dependency line saved. Git install is documented as an option without being the supported path. |
| Rename the packages to something unclaimed on npm | The scoped name is free, standard, and does not make the project answer to a second name. |

## Consequences

`make sdk-release` stays the way SDK changes reach the apps, and it still has
to be re-run by hand when the SDK changes — the same footgun ADR-0007 recorded.

Nothing external can depend on these packages yet, which is the point. The
first publish is a deliberate act with its own ADR, and it should not happen
before the M5 tracing API lands.
