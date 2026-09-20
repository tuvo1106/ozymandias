package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	base "github.com/tuvo1106/ozymandias/internal/config"
)

// EnvPrefix prefixes every agent environment override, e.g.
// OZY_AGENT_INTAKE_URL.
const EnvPrefix = "OZY_AGENT_"

// Intake says where the agent forwards what it collects.
type Intake struct {
	// URL is ozyd's base URL; the agent POSTs to /v1/* under it.
	URL string `yaml:"url"`
}

// Statsd configures the extended StatsD UDP server (docs/wire-protocol.md §A).
type Statsd struct {
	// Enabled turns the UDP listener on.
	Enabled bool `yaml:"enabled"`
	// Addr is the UDP listen address.
	Addr string `yaml:"addr"`
	// ReadBuffer is the socket receive buffer (SO_RCVBUF) in bytes: what
	// absorbs a burst while the readers are busy.
	ReadBuffer int `yaml:"read_buffer"`
	// Readers and Workers are the goroutines reading the socket and parsing
	// datagrams.
	Readers int `yaml:"readers"`
	Workers int `yaml:"workers"`
	// QueueSize bounds datagrams waiting between readers and workers; beyond
	// it they are dropped and counted.
	QueueSize int `yaml:"queue_size"`
}

// Aggregator configures bucketing (docs/plan/M1-metrics-tracer-bullet.md §2.2).
type Aggregator struct {
	// FlushInterval is the bucket width, in whole seconds.
	FlushInterval time.Duration `yaml:"flush_interval"`
	// ContextExpiry is how long an idle series is remembered (and a counter
	// zero-filled).
	ContextExpiry time.Duration `yaml:"context_expiry"`
	// HistogramMaxSamples caps the values kept per histogram per bucket.
	HistogramMaxSamples int `yaml:"histogram_max_samples"`
}

// Forwarder configures delivery to ozyd.
type Forwarder struct {
	// Timeout bounds one request.
	Timeout time.Duration `yaml:"timeout"`
	// MaxQueueBytes bounds the compressed payloads held for retry while
	// ozyd is unreachable; beyond it the oldest are dropped.
	MaxQueueBytes int `yaml:"max_queue_bytes"`
	// ShutdownTimeout bounds the final delivery attempt on shutdown.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

// Agent is the agent's configuration.
type Agent struct {
	HTTP       base.HTTP  `yaml:"http"`
	Intake     Intake     `yaml:"intake"`
	Statsd     Statsd     `yaml:"statsd"`
	Aggregator Aggregator `yaml:"aggregator"`
	Forwarder  Forwarder  `yaml:"forwarder"`
	// Hostname is the value of the host tag on everything this agent sends.
	// Empty means the OS hostname. Set it explicitly in containers, where the
	// OS hostname is a meaningless container id.
	Hostname string `yaml:"hostname"`
	// Tags are added to everything this agent sends, e.g. env:dev. Fragments
	// append to this list rather than replacing it.
	Tags []string `yaml:"tags"`
	// ConfdPath is the conf.d directory of per-app fragments. Relative paths
	// are resolved against the working directory.
	ConfdPath string   `yaml:"confd_path"`
	Log       base.Log `yaml:"log"`
}

// Default returns the settings a bare `agent` runs with.
func Default() Agent {
	return Agent{
		HTTP:   base.HTTP{Addr: ":8126", ShutdownTimeout: 10 * time.Second},
		Intake: Intake{URL: "http://localhost:9400"},
		Statsd: Statsd{
			Enabled: true, Addr: ":8125", ReadBuffer: 4 << 20,
			Readers: 2, Workers: 2, QueueSize: 1024,
		},
		Aggregator: Aggregator{FlushInterval: 10 * time.Second, ContextExpiry: 5 * time.Minute, HistogramMaxSamples: 10000},
		Forwarder:  Forwarder{Timeout: 10 * time.Second, MaxQueueBytes: 64 << 20, ShutdownTimeout: 5 * time.Second},
		ConfdPath:  "./deploy/agent.d",
		Log:        base.Log{Level: "info", Format: "text"},
	}
}

// Validate checks every section.
func (a *Agent) Validate() error {
	errs := []error{a.HTTP.Validate(), a.Log.Validate()}
	if u, err := url.Parse(a.Intake.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, fmt.Errorf("intake.url %q: want an absolute http(s) URL", a.Intake.URL))
	}
	if a.Statsd.Enabled {
		if _, _, err := net.SplitHostPort(a.Statsd.Addr); err != nil {
			errs = append(errs, fmt.Errorf("statsd.addr %q: %w", a.Statsd.Addr, err))
		}
		if a.Statsd.ReadBuffer < 0 || a.Statsd.Readers < 1 || a.Statsd.Workers < 1 || a.Statsd.QueueSize < 1 {
			errs = append(errs, errors.New("statsd: read_buffer must be ≥ 0 and readers, workers, queue_size ≥ 1"))
		}
	}
	if iv := a.Aggregator.FlushInterval; iv < time.Second || iv%time.Second != 0 {
		errs = append(errs, fmt.Errorf("aggregator.flush_interval %v: want a whole number of seconds, at least 1s", iv))
	}
	if a.Aggregator.ContextExpiry < a.Aggregator.FlushInterval {
		errs = append(errs, fmt.Errorf("aggregator.context_expiry %v must be at least flush_interval", a.Aggregator.ContextExpiry))
	}
	if a.Aggregator.HistogramMaxSamples < 1 {
		errs = append(errs, errors.New("aggregator.histogram_max_samples must be at least 1"))
	}
	if a.Forwarder.Timeout <= 0 || a.Forwarder.ShutdownTimeout <= 0 || a.Forwarder.MaxQueueBytes < 1 {
		errs = append(errs, errors.New("forwarder: timeout, shutdown_timeout and max_queue_bytes must be positive"))
	}
	for _, t := range a.Tags {
		if strings.TrimSpace(t) == "" || strings.ContainsAny(t, ",|\n") {
			errs = append(errs, fmt.Errorf("tags: %q is empty or contains ',', '|' or a newline", t))
		}
	}
	return errors.Join(errs...)
}

// Load reads the agent config from path (empty: defaults only), the conf.d
// directory it names, and the OZY_AGENT_* environment.
func Load(path string, environ []string) (Agent, []string, error) {
	// Phase 1: where is conf.d? Main file + env only; validation waits for
	// phase 2, when every layer is in.
	probe := Default()
	if _, err := base.Load(&probe, base.Options{
		Path: path, EnvPrefix: EnvPrefix, Environ: environ, SkipValidate: true,
	}); err != nil {
		return Agent{}, nil, err
	}
	// A fragment cannot move the directory it was loaded from (the directory
	// was chosen before any fragment was read). Checked on the fragments
	// alone, since env would otherwise mask it.
	var fragOnly Agent
	if _, err := base.Load(&fragOnly, base.Options{FragmentDir: probe.ConfdPath, SkipValidate: true}); err != nil {
		return Agent{}, nil, err
	}
	if fragOnly.ConfdPath != "" {
		return Agent{}, nil, fmt.Errorf("config: confd_path cannot be set inside a conf.d fragment (found %q)", fragOnly.ConfdPath)
	}
	// Phase 2: everything, in precedence order.
	cfg := Default()
	warnings, err := base.Load(&cfg, base.Options{
		Path:        path,
		FragmentDir: probe.ConfdPath,
		EnvPrefix:   EnvPrefix,
		Environ:     environ,
	})
	if err != nil {
		return Agent{}, warnings, err
	}
	return cfg, warnings, nil
}
