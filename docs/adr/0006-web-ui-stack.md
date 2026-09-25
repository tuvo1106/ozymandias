# ADR-0006: React UI embedded in the server binary

- **Status:** Accepted
- **Date:** 2026-09-19

## Context

The product needs a UI: explorer, dashboards, logs, APM, monitors. The instrumented apps already use React and TypeScript.

## Decision

Use Vite, React, TypeScript, Tailwind, React Router and uPlot (charts, from M1), with hand-rolled canvas for the flame graph and service map. The UI is built into `internal/api/ui/dist` and embedded with `go:embed`, so `ozyd` is one file. A tracked placeholder keeps `go build` independent of Node.

## Alternatives considered

| Option | Why not |
|---|---|
| Server-rendered Go templates | This is the wrong tool for a chart-heavy, interaction-heavy SPA. |
| A component or chart framework (MUI, Recharts) | uPlot handles the point counts a metrics UI needs. The in-repo `ui/` kit stays small. |
| Serve the UI separately | A second deployable for no benefit. |

## Consequences

A Node toolchain for UI work only. Asset caching is safe across rebuilds, since hashed assets are immutable and `index.html` is `no-cache`.
