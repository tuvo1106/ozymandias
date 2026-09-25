package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
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
	Paths []string `yaml:"paths"`
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

// Ozyd is the ozyd server's configuration.
type Ozyd struct {
	HTTP         HTTP         `yaml:"http"`
	DataDir      string       `yaml:"data_dir"`
	Log          Log          `yaml:"log"`
	Storage      Storage      `yaml:"storage"`
	Provisioning Provisioning `yaml:"provisioning"`
}

// DefaultOzyd returns the settings a bare `ozyd` runs with.
func DefaultOzyd() Ozyd {
	return Ozyd{
		HTTP:    HTTP{Addr: ":9400", ShutdownTimeout: 10 * time.Second},
		DataDir: "./data/ozyd",
		Log:     Log{Level: "info", Format: "text"},
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
	errs = append(errs, c.HTTP.Validate(), c.Log.Validate(), c.Storage.Validate())
	if c.DataDir == "" {
		errs = append(errs, errors.New("data_dir must be set"))
	}
	for i, p := range c.Provisioning.Paths {
		if p == "" {
			errs = append(errs, fmt.Errorf("provisioning.paths[%d] is empty", i))
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
