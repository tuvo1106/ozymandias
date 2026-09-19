# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Go module `github.com/tuvo1106/ozymandias`, Makefile entry points, golangci-lint v2 config,
  pre-commit hooks for Go formatting and lint, and CI with the coverage, docs-drift,
  no-app-coupling and fuzz gates from `docs/plan/testing.md` and `documentation.md`.
- `ozyd` and `agent` binaries: `/healthz`, `/debug/vars` self-metrics, graceful shutdown
  on SIGTERM, and a `healthcheck` subcommand for distroless images.
- Layered config (defaults < file < `conf.d` fragments < env) with strict unknown-key
  checking; commented reference files `deploy/ozyd.yaml` and `deploy/agent.yaml`
  (ADR-0009).
- Test support: fake clock, `Eventually`, goroutine-leak checker.
- Web UI shell (Vite, React, TypeScript, Tailwind) embedded in `ozyd`: sidebar with every
  planned section and its milestone, server status on the home page, SPA routing with
  immutable asset caching.
- Docker image (distroless, non-root, ~28 MB, UI embedded) and compose stack; `make up` waits
  until both services are healthy.
- `make smoke`: 14 end-to-end checks against the running stack, including clean exit on SIGTERM.
- `make dev`: both binaries natively plus the Vite dev server, stopped together.
- Docs: DESIGN.md (as built), docs/operations.md, ADRs 0002–0008 and 0010, M0 learning notes.

### Changed

- The git hooks are the CI (ADR-0010). pre-commit runs lint, race tests with coverage gates,
  docs and coupling checks, and the web typecheck, lint and related tests for whatever is
  staged; pre-push runs the full `make ci`. The GitHub Actions workflow is manual-only.

### Fixed

- Config: a YAML file holding only comments loads as empty instead of failing with `EOF`.
- Config: a null value (e.g. `tags:` with every item commented out) merges as absent, so a
  later fragment's list still appends.
- `testutil.FakeClock`: `Stop` and `Reset` discard an unread fire, matching Go 1.23+ timers.
