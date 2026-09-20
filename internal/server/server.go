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
	"time"

	"github.com/tuvo1106/ozymandias/internal/api"
	"github.com/tuvo1106/ozymandias/internal/buildinfo"
	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/config"
	"github.com/tuvo1106/ozymandias/internal/httpserve"
	"github.com/tuvo1106/ozymandias/internal/intake"
	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/naive"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Component is how ozyd identifies itself in health output and on its
// self-metrics.
const Component = "ozyd"

// selfReportInterval is how often ozyd stores its own metrics. It
// matches the agent's flush interval so both sides' self-metrics line up.
const selfReportInterval = 10 * time.Second

// Files under data_dir.
const (
	metaFile    = "meta.db"
	metricsFile = "metrics-naive.db" // replaced by the M2 TSDB directory
)

// Options carries the server's dependencies. Zero values get sensible
// production defaults, so tests set only what they care about.
type Options struct {
	Logger   *slog.Logger          // default: slog.Default()
	Registry *selfmetrics.Registry // default: a new registry
	Clock    clock.Clock           // default: clock.Real()
	// UI serves the web UI at /. Nil means no UI (API-only).
	UI http.Handler
	// Hostname tags ozyd's own metrics. Default: os.Hostname, falling
	// back to "ozyd".
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

	store  tsdb.MetricStore
	meta   *meta.DB
	intake *intake.Intake
	self   *selfmetrics.Reporter
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
	// Fail at startup, not at the first write, if the data dir is unusable.
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("data_dir: %w", err)
	}
	s := &Server{cfg: cfg, log: opts.Logger, reg: opts.Registry, clock: opts.Clock, started: opts.Clock.Now()}

	md, err := meta.Open(filepath.Join(cfg.DataDir, metaFile))
	if err != nil {
		return nil, fmt.Errorf("data_dir: %w", err)
	}
	store, err := naive.Open(filepath.Join(cfg.DataDir, metricsFile))
	if err != nil {
		_ = md.Close()
		return nil, fmt.Errorf("data_dir: %w", err)
	}
	s.meta, s.store = md, store
	s.intake = intake.New(intake.Options{Store: store, Registry: md, Clock: s.clock, Metrics: s.reg, Logger: s.log})

	host, err := opts.Hostname()
	if err != nil || host == "" {
		host = Component
	}
	hostTag, _ := wire.NormalizeTag("host:" + host)
	s.self = selfmetrics.NewReporter(s.reg, selfReportInterval, hostTag)

	s.reg.Gauge("ozy.build.info", "component:"+Component, "version:"+buildinfo.Version).Set(1)
	s.reg.GaugeFunc("ozy.process.uptime_seconds", func() float64 {
		return s.clock.Now().Sub(s.started).Seconds()
	}, "component:"+Component)
	s.reg.GaugeFunc("ozy.store.series", func() float64 { return float64(store.Stats().Series) }, "store:naive")
	s.reg.GaugeFunc("ozy.store.samples", func() float64 { return float64(store.Stats().Samples) }, "store:naive")

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", httpserve.Health(Component, s.started, s.clock.Now, nil))
	mux.Handle("GET /debug/vars", s.reg.Handler())
	s.intake.Register(mux)
	(&api.Metrics{Store: store, Types: md, Clock: s.clock}).Register(mux)
	if opts.UI != nil {
		mux.Handle("GET /", opts.UI)
	}
	s.handler = httpserve.Instrument(s.reg, Component, mux)
	return s, nil
}

// Handler returns the server's full HTTP handler, for in-process tests.
func (s *Server) Handler() http.Handler { return s.handler }

// Close releases the stores. Run calls it on the way out.
func (s *Server) Close() error {
	return errors.Join(s.store.Close(), s.meta.Close())
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

	selfCtx, stopSelf := context.WithCancel(context.Background())
	selfDone := make(chan struct{})
	go func() { s.reportSelf(selfCtx); close(selfDone) }()

	err := httpserve.Serve(ctx, srv, ln, s.cfg.HTTP.ShutdownTimeout)
	stopSelf()
	<-selfDone
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
