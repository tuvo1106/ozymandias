package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"time"
)

// EnvPrefixOzyd prefixes every ozyd environment override, e.g.
// OZY_HTTP_ADDR.
const EnvPrefixOzyd = "OZY_"

// HTTP configures a binary's HTTP listener. Shared by ozyd and the agent.
type HTTP struct {
	// Addr is the listen address, host:port. An empty host means all
	// interfaces.
	Addr string `yaml:"addr"`
	// ShutdownTimeout bounds graceful shutdown: in-flight requests get this
	// long to finish after SIGTERM before connections are cut.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

// Validate checks the listener settings.
func (h HTTP) Validate() error {
	var errs []error
	if _, _, err := net.SplitHostPort(h.Addr); err != nil {
		errs = append(errs, fmt.Errorf("http.addr %q: %w", h.Addr, err))
	}
	if h.ShutdownTimeout <= 0 {
		errs = append(errs, fmt.Errorf("http.shutdown_timeout must be positive, got %v", h.ShutdownTimeout))
	}
	return errors.Join(errs...)
}

// Log configures a binary's own logging (not the logs it collects).
type Log struct {
	// Level is debug, info, warn or error.
	Level string `yaml:"level"`
	// Format is text (human-readable, for a terminal) or json (for anything
	// that will be parsed — including ozymandias's own log pipeline from M4).
	Format string `yaml:"format"`
}

// SlogLevel returns Level as a slog.Level.
func (l Log) SlogLevel() slog.Level {
	var lv slog.Level
	_ = lv.UnmarshalText([]byte(l.Level)) // validated already; info on error
	return lv
}

// Validate checks the level and format names.
func (l Log) Validate() error {
	var errs []error
	switch l.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level %q: want debug, info, warn or error", l.Level))
	}
	switch l.Format {
	case "text", "json":
	default:
		errs = append(errs, fmt.Errorf("log.format %q: want text or json", l.Format))
	}
	return errors.Join(errs...)
}

// Provisioning lists where dashboards and monitors-as-code are read from
// (M3/M6). Several directories are allowed so an app can mount its own
// alongside ozymandias's.
type Provisioning struct {
	// Paths are dashboard directories.
	Paths []string `yaml:"paths"`
	// MonitorPaths are monitor directories. Separate from Paths because a
	// provisioner reads every *.json in its directories as its own kind, and
	// a monitor read as a dashboard is one skipped file and one error line on
	// every startup.
	MonitorPaths []string `yaml:"monitor_paths"`
}

// Storage configures the metric store.
type Storage struct {
	// MetricStore selects the engine: "tsdb" (the real one) or "naive" (the
	// M1 SQLite reference implementation, kept as a differential-test oracle
	// and an escape hatch).
	MetricStore string `yaml:"metric_store"`
	// BlockRange is how much time one on-disk block covers. Larger blocks mean
	// fewer files and better compression; smaller ones mean retention can free
	// disk sooner, since a block is deleted whole or not at all.
	BlockRange time.Duration `yaml:"block_range"`
	// Retention deletes blocks whose newest sample is older than this.
	// Negative keeps everything. Zero is rejected rather than given a
	// meaning: max_bytes two fields down uses zero for "no cap", so zero here
	// reads as "off" — and it used to reach db.Options as an unset field and
	// come back as the 15-day default, quietly deleting data from a config
	// that looked like it had turned deletion off.
	Retention time.Duration `yaml:"retention"`
	// MaxBytes caps total size on disk, deleting the oldest blocks when
	// exceeded. Zero means no cap. It is a backstop against a full disk, not a
	// retention policy — it evicts by age with no regard for what the data is.
	MaxBytes int64 `yaml:"max_bytes"`
	// MaxSeriesPerMetric bounds cardinality per metric name. Negative means
	// unlimited, which is a decision to make deliberately: the index grows
	// with distinct tag values whether or not anyone queries them.
	MaxSeriesPerMetric int `yaml:"max_series_per_metric"`
	// MaxBlockRange caps how wide a compacted block may become.
	MaxBlockRange time.Duration `yaml:"max_block_range"`
	// WALSyncOnAppend fsyncs the log before an intake request is acknowledged.
	// On means a crash loses nothing that was acknowledged; off trades a
	// WALSyncInterval-wide window of exposure for throughput.
	WALSyncOnAppend bool `yaml:"wal_sync_on_append"`
	// WALSyncInterval is the group-commit period when WALSyncOnAppend is off.
	WALSyncInterval time.Duration `yaml:"wal_sync_interval"`
}

// Validate checks the storage settings.
func (s Storage) Validate() error {
	var errs []error
	switch s.MetricStore {
	case "tsdb", "naive":
	default:
		errs = append(errs, fmt.Errorf("storage.metric_store %q: want tsdb or naive", s.MetricStore))
	}
	if s.BlockRange <= 0 {
		errs = append(errs, fmt.Errorf("storage.block_range must be positive, got %v", s.BlockRange))
	}
	if s.MaxBlockRange > 0 && s.MaxBlockRange < s.BlockRange {
		errs = append(errs, fmt.Errorf(
			"storage.max_block_range (%v) is smaller than storage.block_range (%v), so no compaction could ever run",
			s.MaxBlockRange, s.BlockRange))
	}
	if s.Retention == 0 {
		errs = append(errs, errors.New(
			"storage.retention is 0, which is ambiguous: use a negative value to keep everything, "+
				"or a positive duration to delete blocks older than it"))
	}
	if s.MaxBytes < 0 {
		errs = append(errs, fmt.Errorf("storage.max_bytes must not be negative, got %d", s.MaxBytes))
	}
	if s.WALSyncInterval <= 0 {
		errs = append(errs, fmt.Errorf("storage.wal_sync_interval must be positive, got %v", s.WALSyncInterval))
	}
	return errors.Join(errs...)
}

// Logs configures the log store.
type Logs struct {
	// Retention is how long a day of logs is kept, counted from the end of that
	// day (so a log lives between Retention and Retention+24h). Negative keeps
	// everything. Zero is rejected for the reason storage.retention's is.
	Retention time.Duration `yaml:"retention"`
	// ScanBudget caps the raw bytes one log query may decompress before it
	// returns a partial answer with a cursor; zero is the store's default.
	ScanBudget int64 `yaml:"scan_budget"`
	// NoSync skips fsync: a crash can lose acknowledged logs. For benchmarks
	// and throwaway dev stacks only.
	NoSync bool `yaml:"no_sync"`
	// MaxStreams bounds distinct label combinations; past it new ones are filed
	// under "_overflow". Zero is the store's default (5000).
	MaxStreams int `yaml:"max_streams"`
}

// Validate checks the log settings.
func (l Logs) Validate() error {
	var errs []error
	if l.Retention == 0 {
		errs = append(errs, errors.New("logs.retention is 0, which is ambiguous: use a negative value to keep everything, or a positive duration"))
	}
	if l.MaxStreams < 0 {
		errs = append(errs, fmt.Errorf("logs.max_streams must not be negative, got %d", l.MaxStreams))
	}
	if l.ScanBudget < 0 {
		errs = append(errs, fmt.Errorf("logs.scan_budget must not be negative, got %d", l.ScanBudget))
	}
	return errors.Join(errs...)
}

// Traces configures the trace store.
type Traces struct {
	// Retention is how long a trace is kept, counted from the hour it was first
	// seen. Negative keeps everything; zero is rejected for the reason
	// storage.retention's is.
	Retention time.Duration `yaml:"retention"`
	// NoSync skips fsync: a crash can lose acknowledged spans. For benchmarks
	// and throwaway dev stacks only.
	NoSync bool `yaml:"no_sync"`
}

// Validate checks the trace settings.
func (t Traces) Validate() error {
	if t.Retention == 0 {
		return errors.New("traces.retention is 0, which is ambiguous: use a negative value to keep everything, or a positive duration")
	}
	return nil
}

// Monitors configures the monitor scheduler (M6).
type Monitors struct {
	// Enabled runs the scheduler. Off, monitors can still be defined and
	// edited; nothing evaluates them.
	Enabled bool `yaml:"enabled"`
	// Workers bounds concurrent evaluations.
	Workers int `yaml:"workers"`
	// EventRetention is how long monitor events (transitions, deliveries,
	// errors) are kept. Negative keeps everything; zero is rejected.
	EventRetention time.Duration `yaml:"event_retention"`
	// BaseURL is the UI's address as a person receiving a notification would
	// open it; notification links are built from it.
	BaseURL string `yaml:"base_url"`
}

// Validate checks the monitor settings.
func (m Monitors) Validate() error {
	var errs []error
	if m.Workers < 1 || m.Workers > 64 {
		errs = append(errs, fmt.Errorf("monitors.workers: want 1 to 64, got %d", m.Workers))
	}
	if m.EventRetention == 0 {
		errs = append(errs, errors.New("monitors.event_retention is 0, which is ambiguous: use a negative value to keep everything, or a positive duration"))
	}
	if m.BaseURL != "" {
		if u, err := url.Parse(m.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("monitors.base_url %q: want an http(s) URL", m.BaseURL))
		}
	}
	return errors.Join(errs...)
}

// Notifier is one entry under `notifiers:`, named by its key; a monitor's
// message selects it with @name.
//
// Secrets are never values here, only the names of environment variables
// that hold them (the *_env fields). The config file is committed and printed;
// the environment is neither.
type Notifier struct {
	// Type is webhook, discord, email or log.
	Type string `yaml:"type"`
	// URL is a webhook's address, for one that carries no secret; URLEnv
	// names a variable holding it instead. Exactly one is set.
	URL    string `yaml:"url"`
	URLEnv string `yaml:"url_env"`
	// WebhookURLEnv names the variable holding a Discord webhook URL, which
	// is itself the credential.
	WebhookURLEnv string `yaml:"webhook_url_env"`
	// Headers are sent with every webhook request; HeadersEnv maps a header
	// to the variable holding its value (an Authorization token).
	Headers    map[string]string `yaml:"headers"`
	HeadersEnv map[string]string `yaml:"headers_env"`
	// To and SMTP configure email.
	To   []string `yaml:"to"`
	SMTP SMTP     `yaml:"smtp"`
}

// SMTP is an email notifier's relay.
type SMTP struct {
	Host        string `yaml:"host"`
	HostEnv     string `yaml:"host_env"`
	Port        int    `yaml:"port"`
	UserEnv     string `yaml:"user_env"`
	PasswordEnv string `yaml:"password_env"`
	From        string `yaml:"from"`
}

var notifierName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// validateNotifiers checks each notifier's shape. It does not read the
// environment: a missing secret is reported when the notifier is built, as a
// notifier that fails every delivery with that reason, rather than as a server
// that will not start.
func validateNotifiers(ns map[string]Notifier) error {
	var errs []error
	bad := func(name, format string, a ...any) {
		errs = append(errs, fmt.Errorf("notifiers.%s: %s", name, fmt.Sprintf(format, a...)))
	}
	for name, n := range ns {
		if !notifierName.MatchString(name) {
			bad(name, "a name is letters, digits, '_' or '-' (it is written as @%s in messages)", name)
		}
		switch n.Type {
		case "webhook":
			if (n.URL == "") == (n.URLEnv == "") {
				bad(name, "a webhook needs exactly one of url and url_env")
			}
		case "discord":
			if n.WebhookURLEnv == "" {
				bad(name, "a discord notifier needs webhook_url_env: the webhook URL is a credential, so it is read from the environment")
			}
			if n.URL != "" {
				bad(name, "url is not used by discord; set webhook_url_env")
			}
		case "email":
			if len(n.To) == 0 {
				bad(name, "an email notifier needs at least one address in to")
			}
			if (n.SMTP.Host == "") == (n.SMTP.HostEnv == "") {
				bad(name, "smtp needs exactly one of host and host_env")
			}
			if n.SMTP.Port < 1 || n.SMTP.Port > 65535 {
				bad(name, "smtp.port: want 1 to 65535")
			}
			if n.SMTP.From == "" {
				bad(name, "smtp.from is required")
			}
		case "log":
		default:
			bad(name, "type %q: want webhook, discord, email or log", n.Type)
		}
	}
	return errors.Join(errs...)
}

// Ozyd is the ozyd server's configuration.
type Ozyd struct {
	HTTP    HTTP   `yaml:"http"`
	DataDir string `yaml:"data_dir"`
	// Hostname is the host tag on ozyd's own metrics. Empty means the OS
	// hostname, which is the wrong answer twice over: in a container it is
	// the container id, new on every recreate, so each restart would add a
	// host value and a copy of every self-metric series; and on macOS it is
	// "name.local" (or a DHCP name) where the agent, given
	// scripts/hostname.sh, says "name", so the two would not agree.
	// Deployments set it from the same value as the agent's hostname
	// (OZY_HOSTNAME).
	Hostname     string       `yaml:"hostname"`
	Log          Log          `yaml:"log"`
	Storage      Storage      `yaml:"storage"`
	Logs         Logs         `yaml:"logs"`
	Traces       Traces       `yaml:"traces"`
	Provisioning Provisioning `yaml:"provisioning"`
	Monitors     Monitors     `yaml:"monitors"`
	// Notifiers are where monitor notifications go, by name.
	Notifiers map[string]Notifier `yaml:"notifiers"`
}

// DefaultOzyd returns the settings a bare `ozyd` runs with.
func DefaultOzyd() Ozyd {
	return Ozyd{
		HTTP:    HTTP{Addr: ":9400", ShutdownTimeout: 10 * time.Second},
		DataDir: "./data/ozyd",
		Log:     Log{Level: "info", Format: "text"},
		Logs:    Logs{Retention: 7 * 24 * time.Hour},
		Traces:  Traces{Retention: 7 * 24 * time.Hour},
		Monitors: Monitors{
			Enabled: true, Workers: 4, EventRetention: 30 * 24 * time.Hour,
			BaseURL: "http://localhost:9400",
		},
		Storage: Storage{
			MetricStore:        "tsdb",
			BlockRange:         2 * time.Hour,
			Retention:          15 * 24 * time.Hour,
			MaxSeriesPerMetric: 10_000,
			MaxBlockRange:      54 * time.Hour,
			// On by default: agent batches are large and infrequent, so the
			// fsync is amortized over thousands of samples, and the
			// alternative is telling a client its data is safe before it is.
			WALSyncOnAppend: true,
			WALSyncInterval: 100 * time.Millisecond,
		},
	}
}

// Validate checks every section.
func (c *Ozyd) Validate() error {
	var errs []error
	errs = append(errs, c.HTTP.Validate(), c.Log.Validate(), c.Storage.Validate(), c.Logs.Validate(), c.Traces.Validate(), ValidateHostname(c.Hostname),
		c.Monitors.Validate(), validateNotifiers(c.Notifiers))
	if c.DataDir == "" {
		errs = append(errs, errors.New("data_dir must be set"))
	}
	for i, p := range c.Provisioning.Paths {
		if p == "" {
			errs = append(errs, fmt.Errorf("provisioning.paths[%d] is empty", i))
		}
	}
	for i, p := range c.Provisioning.MonitorPaths {
		if p == "" {
			errs = append(errs, fmt.Errorf("provisioning.monitor_paths[%d] is empty", i))
		}
	}
	return errors.Join(errs...)
}

// LoadOzyd loads the server config from path (empty: defaults only) and
// the OZY_* environment.
func LoadOzyd(path string, environ []string) (Ozyd, []string, error) {
	cfg := DefaultOzyd()
	warnings, err := Load(&cfg, Options{Path: path, EnvPrefix: EnvPrefixOzyd, Environ: environ})
	return cfg, warnings, err
}
