// Package collector runs the agent's pull-based collectors — host metrics,
// Docker container stats, and configurable checks (OpenMetrics scrape, HTTP,
// Redis, Postgres, process) — each on its own interval, with autodiscovery
// from container labels.
//
// Status: arrives in M3 (docs/plan/M3-query-dashboards.md); this file marks its place in
// the architecture until then.
package collector
