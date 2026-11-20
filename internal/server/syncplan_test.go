package server

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/config"
	"github.com/tuvo1106/ozymandias/internal/intake"
	"github.com/tuvo1106/ozymandias/internal/logstore"
	"github.com/tuvo1106/ozymandias/internal/sketchstore"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/internal/tracestore"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/db"
)

func queuedWith(mod func(*config.Ozyd)) config.Ozyd {
	cfg := config.DefaultOzyd()
	cfg.Pipeline.Mode = config.PipelineQueued
	if mod != nil {
		mod(&cfg)
	}
	return cfg
}

func TestPlanSync_OnlyQueuedModeDefersAndOnlyWhatTheOperatorLeftDurable(t *testing.T) {
	direct := queuedWith(func(c *config.Ozyd) { c.Pipeline.Mode = config.PipelineDirect })
	for name, tc := range map[string]struct {
		cfg  config.Ozyd
		want syncPlan
	}{
		"direct mode: Append is what a 202 waits for, nothing defers": {direct, syncPlan{}},
		"queued, defaults: every store defers and is synced":          {queuedWith(nil), syncPlan{tsdb: true, sketches: true, logs: true, traces: true}},
		"queued, wal_sync_on_append false: the bounded window stays": {queuedWith(func(c *config.Ozyd) { c.Storage.WALSyncOnAppend = false }),
			syncPlan{sketches: true, logs: true, traces: true}},
		"queued, logs.no_sync: the log store gives up its fsync entirely": {queuedWith(func(c *config.Ozyd) { c.Logs.NoSync = true }),
			syncPlan{tsdb: true, sketches: true, traces: true}},
		"queued, traces.no_sync": {queuedWith(func(c *config.Ozyd) { c.Traces.NoSync = true }),
			syncPlan{tsdb: true, sketches: true, logs: true}},
		"queued, naive metric store cannot defer": {queuedWith(func(c *config.Ozyd) { c.Storage.MetricStore = "naive" }),
			syncPlan{sketches: true, logs: true, traces: true}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := planSync(tc.cfg); got != tc.want {
				t.Fatalf("planSync = %+v, want %+v", got, tc.want)
			}
		})
	}
}

type syncingStore struct{ tsdb.MetricStore }

func (syncingStore) Sync() error { return nil }

// Each topic syncs the stores it writes, and only those; sketches write two.
func TestSyncPlan_EachTopicListsTheStoresItWrites(t *testing.T) {
	m := syncingStore{}
	sk, ls, ts := &sketchstore.Store{}, &logstore.Store{}, &tracestore.Store{}

	got, err := syncPlan{tsdb: true, sketches: true, logs: true, traces: true}.syncers(m, sk, ls, ts)
	if err != nil {
		t.Fatal(err)
	}
	same := func(topic string, want ...intake.Syncer) {
		t.Helper()
		if len(got[topic]) != len(want) {
			t.Fatalf("topic %s syncs %d stores, want %d: %v", topic, len(got[topic]), len(want), got[topic])
		}
		for i := range want {
			if got[topic][i] != want[i] {
				t.Fatalf("topic %s store %d = %v, want %v", topic, i, got[topic][i], want[i])
			}
		}
	}
	same(intake.TopicSeries, m)
	same(intake.TopicSketches, sk, m)
	same(intake.TopicLogs, ls)
	same(intake.TopicTraces, ts)

	got, err = syncPlan{sketches: true}.syncers(m, sk, ls, ts)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[intake.TopicSeries]; ok {
		t.Fatal("the series topic syncs a store the plan did not defer")
	}
	same(intake.TopicSketches, sk)

	if got, err := (syncPlan{}).syncers(m, sk, ls, ts); err != nil || len(got) != 0 {
		t.Fatalf("an empty plan synced %v (%v), want nothing", got, err)
	}
}

// A store opened to defer its fsync that cannot be synced would never be durable: the server
// refuses to start instead.
func TestSyncPlan_ADeferredStoreThatCannotSyncIsRefused(t *testing.T) {
	if _, err := (syncPlan{tsdb: true}).syncers(struct{ tsdb.MetricStore }{}, nil, nil, nil); err == nil {
		t.Fatal("a metric store that cannot Sync was accepted as deferred")
	}
}

func ingestSeriesAndLogs(t *testing.T, s *Server) {
	t.Helper()
	body := `{"series":[{"metric":"s.count","type":"count","interval":10,"tags":["env:t"],"points":[[1790000000,3]]}]}`
	if rec := post(t, s.Handler(), "/v1/series", body); rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	logBody := `{"logs":[{"ts":1790000000000,"message":"synced line","status":"info","service":"svc"}]}`
	if rec := post(t, s.Handler(), "/v1/logs", logBody); rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	testutil.Eventually(t, 10*time.Second, func() bool { return seriesCount(t, s.Handler(), "s.count") == 1 }, "the series was never stored")
	testutil.Eventually(t, 10*time.Second, func() bool {
		return bytes.Contains(get(t, s.Handler(), "/api/v1/logs?q=service:svc").Body.Bytes(), []byte("synced line"))
	}, "the log was never stored")
}

// The wiring is live: by default the consumers sync the metric WAL and the log WAL, and Append
// did not (the TSDB's own periodic sync is on the server's fake clock here, so it cannot be what
// moved the count).
func TestQueued_ConsumersSyncTheStoresBeforeTheyCommit(t *testing.T) {
	s, err := New(queuedConfig(t), Options{Logger: quiet, Clock: testutil.NewFakeClock(time.Unix(1790000000, 0))})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	feedStores(t, s)
	ingestSeriesAndLogs(t, s)
	testutil.Eventually(t, 10*time.Second, func() bool {
		return s.reg.Counter("ozy.consumer.syncs", "topic:series").Value() >= 1 &&
			s.reg.Counter("ozy.consumer.syncs", "topic:logs").Value() >= 1
	}, "the consumers committed without ever syncing their stores")
	real, ok := s.store.(*db.DB)
	if !ok {
		t.Fatalf("metric store is %T", s.store)
	}
	if real.Syncs() == 0 {
		t.Fatal("the TSDB's WAL was never synced: records were committed off the queue with their points in the page cache")
	}
	if real.SyncOnAppend() {
		t.Fatal("the TSDB still fsyncs inside every Append in queued mode: one fsync per record, which is what the consumer's batch sync replaces")
	}
}

// Direct mode keeps what a 202 waits for: Append syncs, as the config says.
func TestDirect_AppendStillSyncsTheWAL(t *testing.T) {
	cfg := testConfig(t)
	cfg.Pipeline.Mode = config.PipelineDirect
	s, err := New(cfg, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if real, ok := s.store.(*db.DB); !ok || !real.SyncOnAppend() {
		t.Fatalf("direct mode with the default wal_sync_on_append opened a TSDB that does not sync on Append (%v)", ok)
	}
}

// wal_sync_on_append: false keeps its documented meaning in queued mode, a bounded window: the
// consumers do not sync the metric WAL before they commit (the logs, which have no such window,
// still sync).
func TestQueued_WalSyncOnAppendFalseStillMeansABoundedWindow(t *testing.T) {
	cfg := queuedConfig(t)
	cfg.Storage.WALSyncOnAppend = false
	s, err := New(cfg, Options{Logger: quiet, Clock: testutil.NewFakeClock(time.Unix(1790000000, 0))})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	feedStores(t, s)
	ingestSeriesAndLogs(t, s)
	testutil.Eventually(t, 10*time.Second, func() bool {
		return s.reg.Counter("ozy.consumer.syncs", "topic:logs").Value() >= 1
	}, "the log consumer never synced its store")
	if n := s.reg.Counter("ozy.consumer.syncs", "topic:series").Value(); n != 0 {
		t.Fatalf("the series consumer synced %v times with wal_sync_on_append off", n)
	}
	if real, ok := s.store.(*db.DB); !ok || real.Syncs() != 0 {
		t.Fatalf("the metric WAL was synced (%v) with wal_sync_on_append off", ok)
	}
}

// Each store is opened as the plan says, for every shape of config. A store opened deferred that the
// consumers do not sync is never durable; a store the consumers sync that was opened synchronous
// still works but pays one fsync per record, which is the cost this removes. Neither shows up in the
// stored data, so the test asks the stores how they were opened.
func TestNew_OpensEachStoreAsThePlanSays(t *testing.T) {
	for name, mod := range map[string]func(*config.Ozyd){
		"direct":                 func(c *config.Ozyd) { c.Pipeline.Mode = config.PipelineDirect },
		"queued":                 func(c *config.Ozyd) {},
		"queued, wal off":        func(c *config.Ozyd) { c.Storage.WALSyncOnAppend = false },
		"queued, logs no_sync":   func(c *config.Ozyd) { c.Logs.NoSync = true },
		"queued, traces no_sync": func(c *config.Ozyd) { c.Traces.NoSync = true },
		"direct, wal off":        func(c *config.Ozyd) { c.Pipeline.Mode = config.PipelineDirect; c.Storage.WALSyncOnAppend = false },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := queuedConfig(t)
			mod(&cfg)
			s, err := New(cfg, Options{Logger: quiet, Clock: testutil.NewFakeClock(time.Unix(1790000000, 0))})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			plan := planSync(cfg)
			real, ok := s.store.(*db.DB)
			if !ok {
				t.Fatalf("metric store is %T", s.store)
			}
			if want := cfg.Storage.WALSyncOnAppend && !plan.tsdb; real.SyncOnAppend() != want {
				t.Errorf("metric store SyncOnAppend = %v, want %v", real.SyncOnAppend(), want)
			}
			if s.sketches.DeferSync() != plan.sketches {
				t.Errorf("sketch store DeferSync = %v, plan says %v", s.sketches.DeferSync(), plan.sketches)
			}
			if s.logs.DeferSync() != plan.logs {
				t.Errorf("log store DeferSync = %v, plan says %v", s.logs.DeferSync(), plan.logs)
			}
			if s.traces.DeferSync() != plan.traces {
				t.Errorf("trace store DeferSync = %v, plan says %v", s.traces.DeferSync(), plan.traces)
			}
		})
	}
}
