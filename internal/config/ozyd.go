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

// Ozyd is the ozyd server's configuration.
type Ozyd struct {
	HTTP         HTTP         `yaml:"http"`
	DataDir      string       `yaml:"data_dir"`
	Log          Log          `yaml:"log"`
	Provisioning Provisioning `yaml:"provisioning"`
}

// DefaultOzyd returns the settings a bare `ozyd` runs with.
func DefaultOzyd() Ozyd {
	return Ozyd{
		HTTP:    HTTP{Addr: ":9400", ShutdownTimeout: 10 * time.Second},
		DataDir: "./data/ozyd",
		Log:     Log{Level: "info", Format: "text"},
	}
}

// Validate checks every section.
func (c *Ozyd) Validate() error {
	var errs []error
	errs = append(errs, c.HTTP.Validate(), c.Log.Validate())
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
