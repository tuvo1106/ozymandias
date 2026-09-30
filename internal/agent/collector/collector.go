package collector

import (
	"context"
	"time"
)

// Kind says how a collected value is to be read.
type Kind int

const (
	// Gauge is a level at the moment of collection: memory used, load
	// average, a percentage.
	Gauge Kind = iota
	// Rate is a per-second rate over the collector's interval, computed by
	// the collector from two readings of a cumulative counter (see [Rates]).
	// Collectors report rates rather than raw counter values because a raw
	// cumulative value means nothing on its own — it depends on when the
	// kernel, container or process started counting.
	//
	// On the wire a Rate is a gauge, not the wire's rate type (ADR-0026).
	// The query engine rolls a rate series up the way it rolls up a count —
	// by summing the bucket's points — so a per-second value sent as a rate
	// would read as n× itself for a bucket holding n points, and a chart
	// would change with the zoom. A per-second value is a level: averaged
	// over a bucket it is the bucket's rate, at any width.
	Rate
	// Count is a number of events that happened during the interval, for
	// things observed as events rather than read from a counter (a container
	// exiting, in M3's docker collector).
	Count
)

func (k Kind) String() string {
	switch k {
	case Gauge:
		return "gauge"
	case Rate:
		return "rate"
	case Count:
		return "count"
	}
	return "unknown"
}

// Metric is one value a collector reports. The scheduler adds the host tag,
// the agent's global tags and the timestamp, so a collector says only what it
// measured.
type Metric struct {
	Name  string
	Kind  Kind
	Value float64
	// Tags are "key:value" strings. The scheduler normalizes them as the
	// intake would; a tag that cannot be normalized is dropped and counted,
	// not the whole metric.
	Tags []string
}

// Emit hands one metric to the scheduler. It is safe for concurrent use (a
// collector may fan out, as the docker one does per container), but only
// until Collect returns: a late call is dropped and counted, because the
// batch it belonged to has already been sent.
type Emit func(Metric)

// Collector is one source of pull-based metrics: the host, the Docker daemon,
// one configured check instance.
//
// The contract, all of which the scheduler relies on:
//
//   - Collect reads its source once and emits what it found. It may emit
//     nothing (a rate needs two readings; the first run has one).
//   - Collect honours ctx. The scheduler cancels it at the collector's timeout
//     and at shutdown, and cannot stop a goroutine that ignores it: a collector
//     that blocks past its deadline delays only itself, but it does delay the
//     agent's shutdown.
//   - An error means "this run failed". What was emitted before it is still
//     sent — a disk that cannot be read should not hide the CPU numbers read
//     a moment earlier. The error is logged and counted, never fatal.
//   - Collect is never called concurrently with itself, so a collector may
//     keep state between runs (previous counter readings) without locking.
type Collector interface {
	// Name identifies the collector in logs and on its self-metrics
	// (collector:<name>). Stable, lower-case, short.
	Name() string
	// Interval is how often to run. Zero means the scheduler's default.
	Interval() time.Duration
	// Collect gathers one reading. See the contract above.
	Collect(ctx context.Context, emit Emit) error
}
