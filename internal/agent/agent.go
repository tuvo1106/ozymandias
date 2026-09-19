package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/config"
	"github.com/tuvo1106/ozymandias/internal/buildinfo"
	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/httpserve"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
)

// Component is how the agent identifies itself in health output and on its
// self-metrics.
const Component = "agent"

// Options carries the agent's dependencies. Zero values get production
// defaults.
type Options struct {
	Logger   *slog.Logger          // default: slog.Default()
	Registry *selfmetrics.Registry // default: a new registry
	Clock    clock.Clock           // default: clock.Real()
	// Hostname resolves the OS hostname when the config leaves it empty.
	// Default: os.Hostname.
	Hostname func() (string, error)
}

// Agent is a configured agent, ready to Run.
type Agent struct {
	cfg      config.Agent
	log      *slog.Logger
	reg      *selfmetrics.Registry
	clock    clock.Clock
	hostname string
	started  time.Time
	handler  http.Handler
}

// New resolves the hostname and builds the agent's HTTP routes. It does not
// listen; see Run.
func New(cfg config.Agent, opts Options) (*Agent, error) {
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
	host := cfg.Hostname
	if host == "" {
		h, err := opts.Hostname()
		if err != nil {
			return nil, fmt.Errorf("hostname is not configured and the OS hostname is unavailable: %w", err)
		}
		host = h
	}
	a := &Agent{cfg: cfg, log: opts.Logger, reg: opts.Registry, clock: opts.Clock, hostname: host, started: opts.Clock.Now()}

	a.reg.Gauge("ozy.build.info", "component:"+Component, "version:"+buildinfo.Version).Set(1)
	a.reg.GaugeFunc("ozy.process.uptime_seconds", func() float64 {
		return a.clock.Now().Sub(a.started).Seconds()
	}, "component:"+Component)

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", httpserve.Health(Component, a.started, a.clock.Now, func() map[string]any {
		return map[string]any{"hostname": a.hostname, "intake_url": a.cfg.Intake.URL}
	}))
	mux.Handle("GET /debug/vars", a.reg.Handler())
	a.handler = httpserve.Instrument(a.reg, Component, mux)
	return a, nil
}

// Hostname returns the value of the host tag this agent applies.
func (a *Agent) Hostname() string { return a.hostname }

// Handler returns the agent's HTTP handler, for in-process tests.
func (a *Agent) Handler() http.Handler { return a.handler }

// Run serves on ln (or the configured address if ln is nil) until ctx is
// cancelled, then shuts down gracefully within http.shutdown_timeout.
func (a *Agent) Run(ctx context.Context, ln net.Listener) error {
	if ln == nil {
		var err error
		if ln, err = httpserve.Listen(a.cfg.HTTP.Addr); err != nil {
			return err
		}
	}
	srv := &http.Server{
		Handler:           a.handler,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(a.log.Handler(), slog.LevelWarn),
	}
	a.log.Info("listening", "component", Component, "addr", ln.Addr().String(),
		"version", buildinfo.Version, "hostname", a.hostname, "intake_url", a.cfg.Intake.URL)
	err := httpserve.Serve(ctx, srv, ln, a.cfg.HTTP.ShutdownTimeout)
	a.log.Info("stopped", "component", Component, "err", err)
	return err
}
