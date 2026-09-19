package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/tuvo1106/ozymandias/internal/buildinfo"
	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/config"
	"github.com/tuvo1106/ozymandias/internal/httpserve"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
)

// Component is how ozyd identifies itself in health output and on its
// self-metrics.
const Component = "ozyd"

// Options carries the server's dependencies. Zero values get sensible
// production defaults, so tests set only what they care about.
type Options struct {
	Logger   *slog.Logger          // default: slog.Default()
	Registry *selfmetrics.Registry // default: a new registry
	Clock    clock.Clock           // default: clock.Real()
	// UI serves the web UI at /. Nil means no UI (API-only).
	UI http.Handler
}

// Server is a configured ozyd, ready to Run.
type Server struct {
	cfg     config.Ozyd
	log     *slog.Logger
	reg     *selfmetrics.Registry
	clock   clock.Clock
	started time.Time
	handler http.Handler
}

// New validates the environment (the data directory must be creatable) and
// builds the HTTP routes. It does not listen; see Run.
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
	// Fail at startup, not at the first write, if the data dir is unusable.
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("data_dir: %w", err)
	}
	s := &Server{cfg: cfg, log: opts.Logger, reg: opts.Registry, clock: opts.Clock, started: opts.Clock.Now()}

	s.reg.Gauge("ozy.build.info", "component:"+Component, "version:"+buildinfo.Version).Set(1)
	s.reg.GaugeFunc("ozy.process.uptime_seconds", func() float64 {
		return s.clock.Now().Sub(s.started).Seconds()
	}, "component:"+Component)

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", httpserve.Health(Component, s.started, s.clock.Now, nil))
	mux.Handle("GET /debug/vars", s.reg.Handler())
	if opts.UI != nil {
		mux.Handle("GET /", opts.UI)
	}
	s.handler = httpserve.Instrument(s.reg, Component, mux)
	return s, nil
}

// Handler returns the server's full HTTP handler, for in-process tests.
func (s *Server) Handler() http.Handler { return s.handler }

// Run serves on ln (or on the configured address if ln is nil) until ctx is
// cancelled, then shuts down gracefully within http.shutdown_timeout.
func (s *Server) Run(ctx context.Context, ln net.Listener) error {
	if ln == nil {
		var err error
		if ln, err = httpserve.Listen(s.cfg.HTTP.Addr); err != nil {
			return err
		}
	}
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	s.log.Info("listening", "component", Component, "addr", ln.Addr().String(),
		"version", buildinfo.Version, "data_dir", s.cfg.DataDir)
	err := httpserve.Serve(ctx, srv, ln, s.cfg.HTTP.ShutdownTimeout)
	s.log.Info("stopped", "component", Component, "err", err)
	return err
}
