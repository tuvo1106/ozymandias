# Contributing

ozymandias is a learning project: it exists so one person can understand how
observability systems work by building one. That shapes what a good
contribution is. Questions, bug reports and "this is wrong and here is why" are
always welcome. A patch that makes something work without explaining the
mechanism is less useful here than the same patch with a paragraph on why the
old code was wrong — and a pull request that replaces a hand-written component
with a library defeats the purpose, however good the library is
([AGENTS.md §4](AGENTS.md#dependency-allowlist-ask-before-adding-anything-else)
has the list of things that are deliberately not dependencies).

If you are thinking of anything large, open an issue first.

## Branches

Never commit to `main`. Branch prefixes: `feat/<slug>`, `fix/<slug>`,
`chore/<slug>`, `docs/<slug>`.

## Commit messages

[Conventional Commits](https://www.conventionalcommits.org/): `type(scope): subject`, imperative, ≤72 chars.

Types: `feat` `fix` `docs` `refactor` `perf` `test` `build` `ci` `chore` `revert`.

No `Co-Authored-By` trailer.

Enforced by a `commit-msg` hook — run `lefthook install` once after cloning.

## Before opening a PR

- Fill out the [PR template](.github/PULL_REQUEST_TEMPLATE.md).
- `CHANGELOG.md` updated under `[Unreleased]` for anything user-visible.
- Tests and docs ship in the same PR as the code they cover; CI enforces the coverage and doc
  gates in `docs/plan/testing.md` and `docs/plan/documentation.md`.
- A decision that's hard to reverse, non-obvious to the next reader, or reached by rejecting a
  plausible alternative gets an ADR in [docs/adr/](docs/adr/) — see ADR-0001.

## Where things live

- `README.md` — getting started, nothing else.
- `AGENTS.md` — how to work in this repo: conventions, branch/PR policy, security posture.
- `PLAN.md` + `docs/plan/` — the up-front specification: milestones, plus the binding testing,
  documentation and extensibility standards. Amended (with an ADR) when reality disagrees.
- `DESIGN.md` — how the system works *as built*; grows with each milestone.
- `docs/wire-protocol.md`, `docs/formats/`, `docs/api.md`, `docs/query-language.md` — reference
  specs for the public surfaces and on-disk formats.
- `docs/notes/M<n>.md` — learning notes per milestone: mechanism, trade-offs, measurements.
- `docs/adr/` — decisions made during implementation that the above don't cover.
- `CHANGELOG.md` — what shipped and why, once it has.
