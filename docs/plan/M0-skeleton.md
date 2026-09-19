# M0 — Skeleton

**Goal:** an empty but fully wired repo: both binaries build and run, compose
brings them up, CI enforces the test and docs gates from day one (so they are
never retrofitted).

## Tasks

1. `git init` (default branch `main`), first commit on `main` is the existing
   plan + template files, everything after goes through `feat/…` branches.
   The `the project template` files are **already in place** (LICENSE,
   CONTRIBUTING, CHANGELOG, `.github/` templates, ADR-0001 + template,
   `lefthook.yml`, `.gitignore`, README, AGENTS conventions) — extend, don't
   replace: append Go / node / python / `data/` / `web/dist` entries to
   `.gitignore`; fill the placeholder in `lefthook.yml`'s `pre-commit` with
   `gofmt`/`golangci-lint`, `ruff`, `eslint` on staged files (keep the
   `conventional-commit` and `no-secrets` hooks untouched); add `.env.example`.
   The repo is **private for now** (owner decision) → skip
   `CODE_OF_CONDUCT.md` / `SECURITY.md` (template usage step 3); add them if
   it goes public. The no-secrets posture applies regardless.
2. `go mod init github.com/tuvo1106/ozymandias`. Create the package tree from PLAN.md §5 with a `doc.go` in each
   package (package comment may be a stub stating intent + milestone).
3. `internal/testutil`: `FakeClock` (manual advance, tickers fire on advance),
   `Eventually(t, timeout, fn)`, `TempDir` helpers, goroutine-leak checker.
   Define `Clock` in `internal/clock`.
4. `cmd/ozyd`: loads `ozyd.yaml` (+ env overrides `OZY_*`),
   serves `GET /healthz` → `{"status":"ok","version":…}` on :9400, graceful
   shutdown on SIGTERM/SIGINT with a 10s deadline.
5. `cmd/agent`: loads `agent.yaml` (+ `OZY_AGENT_*`), serves `/healthz` on
   :8126, same shutdown behaviour.
6. Config structs with validation and defaults; fully commented reference
   YAMLs in `deploy/`.
7. `internal/selfmetrics`: tiny in-process registry (counter, gauge) exposed at
   `/debug/vars`-style JSON. (From M1 these also flow through the pipeline.)
8. Multi-stage `Dockerfile` (one image, two entrypoints; distroless or alpine
   final). `deploy/docker-compose.yml`: services `ozyd` (volume
   `ozymandias-data:/data`, port 9400) and `agent` (ports 8125/udp, 8126; mounts
   `/var/run/docker.sock:ro`), both on external network `ozymandias`
   (`docker network create ozymandias` in `make up`).
9. `web/`: Vite + React + TS + Tailwind scaffold, dev server on :9401 proxying
   `/api` and `/v1` to :9400, a shell layout with nav (Metrics, Dashboards,
   Logs, APM, Monitors — all "coming in M<n>"). `internal/api` serves the built
   UI via `go:embed` with SPA fallback.
10. `Makefile` with the targets in AGENTS.md §7.
11. CI workflow exactly as `docs/plan/testing.md` §4 and
    `docs/plan/documentation.md` §5, including `scripts/check-coverage.sh`,
    `scripts/coverage-thresholds.txt`, `scripts/check-docs.sh`. Gates are live
    now, even though little code exists.
12. Docs: fill README's Setup/Testing blocks with the now-real commands;
    `DESIGN.md` (skeleton with the section headings it will grow into), linked
    from README and CONTRIBUTING (template usage step 4); first `[Unreleased]`
    CHANGELOG entries; ADR-0002 onward seeded from the PLAN.md §1 decisions
    using `adr-template.md` (ADR-0001 is the template's); 
    `docs/diagrams/system-overview.mmd`.
13. `scripts/check-no-app-coupling.sh` (extensibility.md §1) wired into CI;
    `deploy/agent.d/` directory loading in the agent config (`conf.d`-style:
    every `*.yaml` merged), `provisioning.paths` list in `ozyd.yaml`.

## Test plan

- Unit: config loading (defaults, env override precedence, validation errors);
  `FakeClock` ticker semantics; graceful shutdown (server stops accepting,
  in-flight request completes, returns within deadline); leak checker.
- Integration: start both binaries in-process on ephemeral ports, hit `/healthz`.
- Smoke: `scripts/smoke.sh` v0 — compose up, both `/healthz` return 200, UI
  index served.

## Acceptance criteria

- [ ] `make build test lint` green locally and in CI.
- [ ] `make up` → `curl :9400/healthz` and `curl :8126/healthz` OK; `http://localhost:9400/` shows the UI shell.
- [ ] `docker kill -s TERM` on either container exits 0 within 10s.
- [ ] CI fails if a package lacks `doc.go`, an exported symbol lacks a comment, or coverage drops below gate (demonstrate once with a throwaway commit, note it in `docs/notes/M0.md`).
- [ ] `docs/notes/M0.md` written.
