# ADR-0007: Distribute SDKs as vendored build artifacts for now

- **Status:** Superseded by ADR-0014
- **Date:** 2026-09-19

## Context

The owner's apps need the SDKs. The project is private, so nothing is published to PyPI or npm. app-python's Docker build context is `./backend`, and both apps' CI has no access to a sibling checkout. npm also cannot install from a git subdirectory.

## Decision

`make sdk-release` (from M1) builds a wheel and a tarball and copies them into each app's `vendor/` directory, which the app's dependency manifest points at. The target directories come from a gitignored `.env`, so the script names no app. The SDKs are still built as publishable packages.

## Alternatives considered

| Option | Why not |
|---|---|
| Publish to PyPI and npm | The owner decided the project stays private for now. |
| Git dependencies | This works for Python (`#subdirectory=`) but not npm, and the apps' CI would need access to a private repo. |
| Editable/linked installs | These don't work inside app-python's Docker build, and linked packages misbehave with Next.js's bundler. |

## Consequences

Artifacts are committed in the apps' repos (small, zero-dependency). Upgrading an app is an explicit act. Publishing later requires only a pipeline, not package changes.
