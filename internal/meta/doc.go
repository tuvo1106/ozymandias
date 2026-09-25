// Package meta is ozyd's metadata database (SQLite): everything that
// isn't telemetry. In M1 that is one table, each metric's type; dashboards,
// monitors and their state, events and API keys join it in later milestones.
//
// A metric's type is fixed when it is first seen, because the query layer
// can only aggregate a series over time correctly if it knows what the
// values mean: counts sum, gauges average. Reads are served from memory,
// since every intake request asks about every metric it carries.
package meta
