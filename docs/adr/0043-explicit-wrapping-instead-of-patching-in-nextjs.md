# ADR-0043: Explicit wrapping, not module patching, for Node and Next.js

- **Status:** Accepted
- **Date:** 2026-10-05

## Context

dd-trace instruments Node by hooking `require` (require-in-the-middle) and, for ESM,
import-in-the-middle, patching a library when it loads. Next.js bundles server code, so most
modules are never loaded through a hookable `require`; the patch never runs and the app is silently
untraced.

## Decision

The Node SDK instruments by explicit wrapping the app opts into: `withTelemetry(handler)` and
`traceRoute()` for route handlers, `instrumentSqlite(db)` for a database it is handed, an
idempotent `instrumentFetch()` wrapping whatever `globalThis.fetch` is at the time, and a winston
format. Integrations are exposed through a public `Integration` interface and use only the public
tracer API. The SDK is listed in the host's `serverExternalPackages` so one instance loads, and its
state also lives on `globalThis[Symbol.for("ozy")]` as a second line of defence.

## Alternatives considered

| Option | Why not |
|---|---|
| A require/import hook | Does not see bundled code (the very case that motivates the choice), and a hook that sometimes works is worse than none. |
| A webpack/SWC plugin | Couples the SDK to one bundler's internals and version. |
| Next's `instrumentation.ts` + OpenTelemetry | Valid, and what M8 offers; but it makes the SDK an OpenTelemetry wrapper, which hides the span model this project exists to teach. |

## Consequences

Instrumentation is a few lines per app, visible in review. Nothing is traced that was not asked for.
