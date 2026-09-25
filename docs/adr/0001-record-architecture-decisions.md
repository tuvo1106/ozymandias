# ADR-0001: Record architecture decisions

- **Status:** Accepted
- **Date:** 2026-08-08

## Context

A project's spec (`DESIGN.md`/`ARCHITECTURE.md`, if it has one) and its conventions doc
(`README.md`/`CONTRIBUTING.md`) don't cover everything: implementation surfaces decisions —
a library choice, a workaround, a tradeoff — that weren't part of the up-front design and
aren't a recurring convention either. Left in a commit message, the reasoning is unfindable
later; left unwritten, it's relitigated.

## Decision

Keep an ADR log in `docs/adr/`, numbered sequentially, using `adr-template.md`.

Write one when a decision is hard to reverse, non-obvious to the next reader (including future
you), or was reached by rejecting a plausible alternative. Routine choices don't need one.

ADRs are immutable once accepted. To change a decision, write a new ADR and mark the old one
`Superseded by ADR-XXXX`.

## Alternatives considered

| Option | Why not |
|---|---|
| Put it in the spec doc | A spec reads as a single coherent argument; an implementation-decision log would erode that. |
| Nothing, rely on git history | Shows *what* changed, never *what else was considered and why it lost*. |

## Consequences

A decision worth writing down gets reviewed in the PR that makes it, while it's still cheap to
revisit. Small tax on non-obvious changes; the alternative is losing the reasoning entirely.
