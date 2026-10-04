package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tuvo1106/ozymandias/internal/api"
	"github.com/tuvo1106/ozymandias/internal/buildinfo"
	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/config"
	"github.com/tuvo1106/ozymandias/internal/dashboard"
	"github.com/tuvo1106/ozymandias/internal/httpserve"
	"github.com/tuvo1106/ozymandias/internal/intake"
	"github.com/tuvo1106/ozymandias/internal/loghub"
	"github.com/tuvo1106/ozymandias/internal/logstore"
	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/internal/sketchstore"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/db"
	"github.com/tuvo1106/ozymandias/internal/tsdb/head"
	"github.com/tuvo1106/ozymandias/internal/tsdb/naive"
)

// Component is how ozyd identifies itself in health output and on its
// self-metrics.
const Component = "ozyd"

// selfReportInterval is how often ozyd stores its own metrics. It
// matches the agent's flush interval so both sides' self-metrics line up.
const selfReportInterval = 10 * time.Second

// Files under data_dir.
const (
	metaFile = "meta.db"
	// naiveFile is the M1 SQLite store, still selectable with
	// storage.metric_store: naive.
	naiveFile = "metrics-naive.db"
	// tsdbDir holds the real store's wal/ and blocks/.
	tsdbDir = "tsdb"
	// sketchDir holds the Pebble database of DDSketches.
	sketchDir = "sketches"
	// logDir holds the log store: its WAL, catalog and per-day chunks.
	logDir = "logs"
)

// sketchSweepInterval is how often retention runs over the sketch store.
//
// Hourly rather than on the TSDB's maintenance cadence: a sketch store sweep
// issues one DeleteRange per series and frees at most an hour of data, so
// running it more often costs tombstones without freeing anything sooner.
const sketchSweepInterval = time.Hour

// Options carries the server's dependencies. Zero values get sensible
// production defaults, so tests set only what they care about.
type Options struct {
	Logger   *slog.Logger          // default: slog.Default()
	Registry *selfmetrics.Registry // default: a new registry
	Clock    clock.Clock           // default: clock.Real()
	// UI serves the web UI at /. Nil means no UI (API-only).
	UI http.Handler
	// Hostname tags ozyd's own metrics when the config's hostname is
	// empty. Default: os.Hostname, falling back to "ozyd".
	Hostname func() (string, error)
}

// Server is a configured ozyd, ready to Run.
type Server struct {
	cfg     config.Ozyd
	log     *slog.Logger
	reg     *selfmetrics.Registry
	clock   clock.Clock
	started time.Time
	handler http.Handler

	store    tsdb.MetricStore
	sketches *sketchstore.Store
	logs     *logstore.Store
	logHub   *loghub.Hub
	meta     *meta.DB
	intake   *intake.Intake
	self     *selfmetrics.Reporter
}

// New opens the stores under data_dir and builds the HTTP routes. It does
// not listen; see Run. Call Close to release the stores of a server that
// will not Run.
func New(cfg config.Ozyd, opts Options) (*Server, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Registry == nil {
		opts.Registry = selfmetrics.NewRegistry()
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real()
	}
	if opts.Hostname == nil {
		opts.Hostname = os.Hostname
	}
	// Before anything is opened, so a refusal has nothing to close.
	//
	// Unlike the agent, ozyd does not refuse to start when the OS has no
	// usable name: only its own metrics carry the tag, and "ozyd" still says
	// whose they are. A *configured* name is different: falling back would
	// hide the operator's typo, so it is an error even for a caller that
	// skipped config.Load's validation.
	_, hostTag, err := config.ResolveHostname(cfg.Hostname, opts.Hostname)
	if err != nil {
		if cfg.Hostname != "" {
			return nil, err
		}
		opts.Logger.Warn("no usable OS hostname; tagging self-metrics host:"+Component, "error", err)
		hostTag = "host:" + Component
	}
	// Fail at startup, not at the first write, if the data dir is unusable.
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("data_dir: %w", err)
	}
	s := &Server{cfg: cfg, log: opts.Logger, reg: opts.Registry, clock: opts.Clock, started: opts.Clock.Now()}

	md, err := meta.Open(filepath.Join(cfg.DataDir, metaFile))
	if err != nil {
		return nil, fmt.Errorf("data_dir: %w", err)
	}
	store, engine, err := openStore(cfg, opts)
	if err != nil {
		_ = md.Close()
		return nil, fmt.Errorf("data_dir: %w", err)
	}
	// Sketches are stored whichever metric engine is configured: the naive
	// store is a reference implementation for series, not a reason to lose
	// percentiles.
	sk, err := sketchstore.Open(sketchstore.Options{
		Dir:       filepath.Join(cfg.DataDir, sketchDir),
		Retention: cfg.Storage.Retention,
		// The TSDB drops blocks, not samples, so a count can outlive the
		// cutoff by a block range. Let the sketches lag by the same amount
		// rather than expiring first — see sketchstore.Options.Grace.
		Grace:    cfg.Storage.BlockRange,
		Clock:    s.clock,
		Logger:   s.log.With("component", "sketchstore"),
		Registry: s.reg,
	})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("data_dir: %w", err), md.Close(), store.Close())
	}
	ls, err := logstore.Open(logstore.Options{
		Dir:       filepath.Join(cfg.DataDir, logDir),
		Clock:     s.clock,
		Retention: cfg.Logs.Retention,
		NoSync:    cfg.Logs.NoSync,
		Logger:    s.log.With("component", "logstore"),
	})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("data_dir: %w", err), md.Close(), store.Close(), sk.Close())
	}
	s.meta, s.store, s.sketches, s.logs, s.logHub = md, store, sk, ls, loghub.New(0)
	s.intake = intake.New(intake.Options{
		Store: store, Sketches: sk, Registry: md, Logs: ls, LogHub: s.logHub,
		Clock: s.clock, Metrics: s.reg, Logger: s.log,
	})

	s.self = selfmetrics.NewReporter(s.reg, selfReportInterval, hostTag)

	s.reg.Gauge("ozy.build.info", "component:"+Component, "version:"+buildinfo.Version).Set(1)
	s.reg.GaugeFunc("ozy.process.uptime_seconds", func() float64 {
		return s.clock.Now().Sub(s.started).Seconds()
	}, "component:"+Component)
	s.reg.GaugeFunc("ozy.store.series", func() float64 { return float64(store.Stats().Series) }, "store:"+engine)
	s.reg.GaugeFunc("ozy.store.samples", func() float64 { return float64(store.Stats().Samples) }, "store:"+engine)
	s.registerStoreMetrics(engine)
	s.reg.GaugeFunc("ozy.loghub.subscribers", func() float64 { return float64(s.logHub.Stats().Subscribers) })
	s.reg.CounterFunc("ozy.loghub.dropped", func() float64 { return float64(s.logHub.Stats().Dropped) })

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", httpserve.Health(Component, s.started, s.clock.Now, nil))
	mux.Handle("GET /debug/vars", s.reg.Handler())
	s.intake.Register(mux)
	(&api.Metrics{
		Store: store, Types: md, Sketches: sk, Clock: s.clock,
		Logger: s.log.With("component", "query"),
	}).Register(mux)
	(&api.Logs{
		Store: ls, Hub: s.logHub, Clock: s.clock, ScanBudget: cfg.Logs.ScanBudget,
		Logger: s.log.With("component", "logs-api"),
	}).Register(mux)
	(&api.Dashboards{
		Store: md, Values: store, Types: md, Clock: s.clock,
		Logger: s.log.With("component", "dashboards"),
	}).Register(mux)
	// Provisioned before serving, so the first request cannot arrive at a
	// deployment whose dashboards-as-code have not loaded yet. It never fails
	// startup: see the comment on dashboard.Provision — one app's typo in a
	// mounted directory must not be an outage for everybody's monitoring.
	dashboard.Provision(context.Background(), api.ProvisionStore{DB: md},
		cfg.Provisioning.Paths, s.clock.Now(), s.log.With("component", "dashboards"))
	if opts.UI != nil {
		mux.Handle("GET /", opts.UI)
	}
	s.handler = httpserve.Instrument(s.reg, Component, mux)
	return s, nil
}

// openStore builds the metric store the config asks for.
//
// Both engines are kept because they check each other: the naive store is the
// oracle M2's differential tests hold the TSDB to, and leaving it reachable
// from config means an operator hitting a storage bug has somewhere to stand
// while it is fixed.
func openStore(cfg config.Ozyd, opts Options) (tsdb.MetricStore, string, error) {
	switch cfg.Storage.MetricStore {
	case "naive":
		store, err := naive.Open(filepath.Join(cfg.DataDir, naiveFile))
		return store, "naive", err
	default:
		store, err := db.Open(db.Options{
			Dir:                filepath.Join(cfg.DataDir, tsdbDir),
			BlockRange:         cfg.Storage.BlockRange,
			Retention:          cfg.Storage.Retention,
			MaxBytes:           cfg.Storage.MaxBytes,
			MaxBlockRange:      cfg.Storage.MaxBlockRange,
			MaxSeriesPerMetric: cfg.Storage.MaxSeriesPerMetric,
			SyncOnAppend:       cfg.Storage.WALSyncOnAppend,
			SyncInterval:       cfg.Storage.WALSyncInterval,
			Clock:              opts.Clock,
			Logger:             opts.Logger.With("component", "tsdb"),
		})
		return store, "tsdb", err
	}
}

// registerStoreMetrics publishes the counters only the real TSDB has: what it
// refused, and what it is costing on disk. A rejection that is not visible is
// indistinguishable from data that never arrived.
func (s *Server) registerStoreMetrics(engine string) {
	real, ok := s.store.(*db.DB)
	if !ok {
		return
	}
	tag := "store:" + engine
	// HeadStats walks every series and every chunk and allocates a slice the
	// size of the id map, so one call per gauge is one walk per gauge: six per
	// scrape once store.Stats() is counted, on a structure with a hundred
	// thousand series in it. They are all read at the same instant anyway, so
	// they share one.
	head := cachedHeadStats(real, s.clock)
	s.reg.GaugeFunc("ozy.tsdb.head.series", func() float64 {
		return float64(head().Series)
	}, tag)
	s.reg.GaugeFunc("ozy.tsdb.head.chunks", func() float64 {
		return float64(head().Chunks)
	}, tag)
	s.reg.GaugeFunc("ozy.tsdb.ooo_rejected", func() float64 {
		return float64(head().OOORejected)
	}, tag)
	s.reg.GaugeFunc("ozy.tsdb.series_limit_rejected", func() float64 {
		return float64(head().LimitRejects)
	}, tag)
	s.reg.GaugeFunc("ozy.tsdb.blocks", func() float64 {
		return float64(len(real.Blocks()))
	}, tag)
	// Monotonic, so what matters is its rate: it should climb at
	// 1/wal_sync_interval whenever anything is being written. A flat stretch
	// means acknowledged samples are sitting in the page cache for longer
	// than the configured window.
	s.reg.GaugeFunc("ozy.tsdb.wal_syncs", func() float64 {
		return float64(real.Syncs())
	}, tag)
	s.reg.GaugeFunc("ozy.tsdb.disk_bytes", func() float64 {
		n, err := real.DiskUsage()
		if err != nil {
			return 0
		}
		return float64(n)
	}, tag)
}

// Handler returns the server's full HTTP handler, for in-process tests.
func (s *Server) Handler() http.Handler { return s.handler }

// Close releases the stores. Run calls it on the way out.
func (s *Server) Close() error {
	return errors.Join(s.logs.Close(), s.store.Close(), s.sketches.Close(), s.meta.Close())
}

// Run serves on ln (or on the configured address if ln is nil) until ctx is
// cancelled, then shuts down gracefully within http.shutdown_timeout and
// closes the stores — after the last in-flight intake request has finished.
func (s *Server) Run(ctx context.Context, ln net.Listener) error {
	if ln == nil {
		var err error
		if ln, err = httpserve.Listen(s.cfg.HTTP.Addr); err != nil {
			return errors.Join(err, s.Close())
		}
	}
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	s.log.Info("listening", "component", Component, "addr", ln.Addr().String(),
		"version", buildinfo.Version, "data_dir", s.cfg.DataDir)

	// Both background loops take a context of their own rather than ctx:
	// they must outlive the cancellation that begins the shutdown, so an
	// in-flight request is not racing a store that is already closing.
	bgCtx, stopBG := context.WithCancel(context.Background())
	bgDone := make(chan struct{}, 2)
	go func() { s.reportSelf(bgCtx); bgDone <- struct{}{} }()
	go func() { s.sweepSketches(bgCtx); bgDone <- struct{}{} }()

	err := httpserve.Serve(ctx, srv, ln, s.cfg.HTTP.ShutdownTimeout)
	stopBG()
	<-bgDone
	<-bgDone
	err = errors.Join(err, s.Close())
	s.log.Info("stopped", "component", Component, "err", err)
	return err
}

// reportSelf stores ozyd's own metrics every interval, through the
// same ingest path an agent's series take — dogfooding without a network
// hop. (Routing them through the local agent would make ozyd's health
// depend on the agent being up.)
func (s *Server) reportSelf(ctx context.Context) {
	t := s.clock.NewTicker(selfReportInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C():
			resp, err := s.intake.Ingest(ctx, s.self.Collect(s.clock.Now()))
			if err != nil {
				s.log.Warn("self-metrics: storing", "err", err)
			} else if resp.Rejected > 0 {
				s.log.Warn("self-metrics: rejected series", "rejected", resp.Rejected, "errors", resp.Errors)
			}
		}
	}
}

// sweepSketches applies retention to the sketch store.
//
// It runs once at startup as well as on the tick: a process that restarts
// more often than the interval would otherwise never sweep at all, and a
// crash loop is exactly when the disk is filling.
func (s *Server) sweepSketches(ctx context.Context) {
	sweep := func() {
		dropped, err := s.sketches.Sweep(ctx)
		switch {
		case ctx.Err() != nil:
		case err != nil:
			s.log.Warn("sketchstore: retention sweep", "err", err)
		case dropped > 0:
			s.log.Info("sketchstore: retention sweep", "series_dropped", dropped)
		}
	}
	sweep()
	t := s.clock.NewTicker(sketchSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C():
			sweep()
		}
	}
}

// cachedHeadStats returns a function that walks the head at most once per
// headStatsTTL, so the gauges built on it cost one walk between them rather
// than one each. The staleness is bounded by a fraction of the scrape
// interval, and these are gauges describing a structure that changes
// continuously — reading them a moment apart was never going to be atomic.
func cachedHeadStats(real *db.DB, c clock.Clock) func() head.Stats {
	var (
		mu   sync.Mutex
		at   time.Time
		last head.Stats
	)
	return func() head.Stats {
		mu.Lock()
		defer mu.Unlock()
		if now := c.Now(); now.Sub(at) > headStatsTTL {
			last, at = real.HeadStats(), now
		}
		return last
	}
}

// headStatsTTL is short enough that a scrape never sees a previous scrape's
// numbers, and long enough that one scrape's gauges share a walk.
const headStatsTTL = 250 * time.Millisecond
