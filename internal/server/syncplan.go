package server

import (
	"errors"
	"fmt"

	"github.com/tuvo1106/ozymandias/internal/config"
	"github.com/tuvo1106/ozymandias/internal/intake"
	"github.com/tuvo1106/ozymandias/internal/logstore"
	"github.com/tuvo1106/ozymandias/internal/sketchstore"
	"github.com/tuvo1106/ozymandias/internal/tracestore"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
)

// syncPlan says which stores leave their fsync out of Append because a queue consumer syncs
// them before it commits an offset (ADR-0063).
//
// It is one decision with two readers: the code that opens each store reads it to open the store
// deferred, and syncers reads it to tell the consumers what to sync. A store opened deferred and
// left off the consumers' list would acknowledge nothing durable and say nothing, so neither
// reader decides on its own.
type syncPlan struct{ tsdb, sketches, logs, traces bool }

// planSync is all false in direct mode, where Append is what a 202 waits for and must keep
// syncing; and for a store whose operator turned its durability off.
//
// wal_sync_on_append: false is the operator choosing a bounded window of loss for the metric
// WAL (see docs/benchmarks.md): in queued mode that keeps its meaning, and nothing syncs the
// TSDB before the commit. The log and trace stores have no window to choose, only no_sync, which
// gives up the fsync altogether; the sketch store always syncs.
func planSync(cfg config.Ozyd) syncPlan {
	if cfg.Pipeline.Mode != config.PipelineQueued {
		return syncPlan{}
	}
	return syncPlan{
		tsdb:     cfg.Storage.MetricStore != "naive" && cfg.Storage.WALSyncOnAppend,
		sketches: true,
		logs:     !cfg.Logs.NoSync,
		traces:   !cfg.Traces.NoSync,
	}
}

// syncers builds what each topic's consumer syncs after a batch: the stores the plan deferred
// that the topic writes. Sketches write the sketch store and, for their exact aggregates, the
// metric store, so that topic syncs both.
func (p syncPlan) syncers(metrics tsdb.MetricStore, sk *sketchstore.Store, ls *logstore.Store, ts *tracestore.Store) (map[string][]intake.Syncer, error) {
	var metric []intake.Syncer
	if p.tsdb {
		m, ok := metrics.(intake.Syncer)
		if !ok {
			// A store opened to defer its fsync that cannot be synced would be a store that
			// is never durable: refuse to start rather than discover it in a power cut.
			return nil, errors.New("the metric store was opened to defer its fsync but cannot Sync")
		}
		metric = []intake.Syncer{m}
	}
	out := map[string][]intake.Syncer{}
	add := func(topic string, s ...intake.Syncer) {
		if len(s) > 0 {
			out[topic] = append(out[topic], s...)
		}
	}
	add(intake.TopicSeries, metric...)
	if p.sketches {
		add(intake.TopicSketches, sk)
	}
	add(intake.TopicSketches, metric...)
	if p.logs {
		add(intake.TopicLogs, ls)
	}
	if p.traces {
		add(intake.TopicTraces, ts)
	}
	for topic, s := range out {
		if len(s) == 0 {
			return nil, fmt.Errorf("topic %s has an empty sync list", topic)
		}
	}
	return out, nil
}
