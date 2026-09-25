package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/aggregator"
	"github.com/tuvo1106/ozymandias/internal/agent/config"
	"github.com/tuvo1106/ozymandias/internal/agent/forwarder"
	"github.com/tuvo1106/ozymandias/internal/agent/statsd"
	"github.com/tuvo1106/ozymandias/internal/buildinfo"
	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/httpserve"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/pkg/wire"
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

	statsd *statsd.Server // nil when statsd is disabled
	agg    *aggregator.Aggregator
	fwd    *forwarder.Forwarder
	self   *selfmetrics.Reporter
}

// New resolves the hostname, binds the statsd socket and builds the
// pipeline and HTTP routes. Binding here rather than in Run means a port
// conflict fails startup with a clear error. Call Run to start, or Close to
// release the socket without running.
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

	a.agg = aggregator.New(aggregator.Options{
		Clock:               a.clock,
		Registry:            a.reg,
		Hostname:            host,
		Tags:                cfg.Tags,
		FlushInterval:       cfg.Aggregator.FlushInterval,
		ContextExpiry:       cfg.Aggregator.ContextExpiry,
		HistogramMaxSamples: cfg.Aggregator.HistogramMaxSamples,
	})
	a.fwd = forwarder.New(forwarder.Options{
		URL:           strings.TrimSuffix(cfg.Intake.URL, "/"),
		Timeout:       cfg.Forwarder.Timeout,
		MaxQueueBytes: cfg.Forwarder.MaxQueueBytes,
		Hostname:      host,
		Version:       buildinfo.Version,
		Clock:         a.clock,
		Registry:      a.reg,
		Logger:        a.log,
	})
	hostTag, _ := wire.NormalizeTag("host:" + host)
	a.self = selfmetrics.NewReporter(a.reg, cfg.Aggregator.FlushInterval, hostTag)

	if cfg.Statsd.Enabled {
		s, err := statsd.Listen(statsd.Options{
			Addr:       cfg.Statsd.Addr,
			ReadBuffer: cfg.Statsd.ReadBuffer,
			Readers:    cfg.Statsd.Readers,
			Workers:    cfg.Statsd.Workers,
			QueueSize:  cfg.Statsd.QueueSize,
			Sink:       a.fromStatsd,
			Clock:      a.clock,
			Registry:   a.reg,
			Logger:     a.log,
		})
		if err != nil {
			return nil, err
		}
		a.statsd = s
	}

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", httpserve.Health(Component, a.started, a.clock.Now, func() map[string]any {
		extra := map[string]any{"hostname": a.hostname, "intake_url": a.cfg.Intake.URL}
		if a.statsd != nil {
			extra["statsd_addr"] = a.statsd.Addr().String()
		}
		return extra
	}))
	mux.Handle("GET /debug/vars", a.reg.Handler())
	a.handler = httpserve.Instrument(a.reg, Component, mux)
	return a, nil
}

// statsdKinds maps statsd types to aggregation kinds. Timing is a histogram
// of milliseconds.
var statsdKinds = [...]aggregator.Kind{
	statsd.Counter:      aggregator.Counter,
	statsd.Gauge:        aggregator.Gauge,
	statsd.Set:          aggregator.Set,
	statsd.Histogram:    aggregator.Histogram,
	statsd.Timing:       aggregator.Histogram,
	statsd.Distribution: aggregator.Distribution,
}

// fromStatsd copies a parsed line out of the statsd server's pooled buffer
// into an aggregator sample. This is where the zero-copy parse ends: the
// strings allocated here are what the aggregator may keep.
func (a *Agent) fromStatsd(m *statsd.Message, now time.Time) {
	s := aggregator.Sample{
		Name:       string(m.Name),
		Kind:       statsdKinds[m.Type],
		Value:      m.Value,
		SampleRate: m.SampleRate,
		Timestamp:  m.Timestamp,
	}
	if m.Type == statsd.Set {
		s.SetMember = string(m.SetMember)
	}
	if len(m.Tags) > 0 {
		s.Tags = make([]string, 0, 8)
		m.EachTag(func(t []byte) { s.Tags = append(s.Tags, string(t)) })
	}
	a.agg.Add(s, now)
}

// flush hands one aggregator flush, plus the agent's own metrics, to the
// forwarder.
//
// The self-metrics ride with the series rather than being reported
// separately, so an agent whose only traffic is distributions still says it
// is alive on the same cadence as one whose traffic is counters.
func (a *Agent) flush(series []wire.Series, sketches []wire.SketchSeries) {
	a.fwd.Submit(append(series, a.self.Collect(a.clock.Now())...))
	if len(sketches) > 0 {
		a.fwd.SubmitSketches(sketches)
	}
}

// Hostname returns the value of the host tag this agent applies.
func (a *Agent) Hostname() string { return a.hostname }

// Handler returns the agent's HTTP handler, for in-process tests.
func (a *Agent) Handler() http.Handler { return a.handler }

// StatsdAddr returns the bound statsd address, or nil when disabled.
func (a *Agent) StatsdAddr() net.Addr {
	if a.statsd == nil {
		return nil
	}
	return a.statsd.Addr()
}

// Close releases the statsd socket of an agent that will not Run.
func (a *Agent) Close() error {
	if a.statsd == nil {
		return nil
	}
	return a.statsd.Close()
}

// Run serves until ctx is cancelled, then shuts down in pipeline order so
// nothing received is lost on the way out:
//
//  1. stop accepting: HTTP drains within http.shutdown_timeout, and the
//     statsd socket closes after its queue is parsed;
//  2. the aggregator does a final flush of every open bucket;
//  3. the forwarder makes one last delivery attempt within
//     forwarder.shutdown_timeout.
func (a *Agent) Run(ctx context.Context, ln net.Listener) error {
	if ln == nil {
		var err error
		if ln, err = httpserve.Listen(a.cfg.HTTP.Addr); err != nil {
			_ = a.Close()
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

	a.fwd.Start()
	aggCtx, stopAgg := context.WithCancel(context.Background())
	aggDone := make(chan struct{})
	go func() { a.agg.Run(aggCtx, a.flush); close(aggDone) }()

	var wg sync.WaitGroup
	var statsdErr error
	statsdCtx, stopStatsd := context.WithCancel(context.Background())
	if a.statsd != nil {
		wg.Go(func() { statsdErr = a.statsd.Run(statsdCtx) })
	}

	err := httpserve.Serve(ctx, srv, ln, a.cfg.HTTP.ShutdownTimeout)

	stopStatsd()
	wg.Wait()
	stopAgg()
	<-aggDone
	fctx, cancel := context.WithTimeout(context.Background(), a.cfg.Forwarder.ShutdownTimeout)
	defer cancel()
	if ferr := a.fwd.Shutdown(fctx); ferr != nil {
		// Logged, not returned: data lost because ozyd is down at
		// shutdown is worth a warning, but the agent itself did its job and
		// should exit 0.
		a.log.Warn("forwarder: undelivered data at shutdown", "err", ferr)
	}
	if statsdErr != nil && !errors.Is(statsdErr, net.ErrClosed) {
		a.log.Warn("statsd: closing", "err", statsdErr)
	}
	a.log.Info("stopped", "component", Component, "err", err)
	return err
}
