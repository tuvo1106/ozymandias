package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent"
	agentconfig "github.com/tuvo1106/ozymandias/internal/agent/config"
	"github.com/tuvo1106/ozymandias/internal/buildinfo"
	"github.com/tuvo1106/ozymandias/internal/config"
	"github.com/tuvo1106/ozymandias/internal/httpserve"
	"github.com/tuvo1106/ozymandias/internal/server"
)

// Exit codes.
const (
	ExitOK      = 0
	ExitFailure = 1
	ExitUsage   = 2
)

// healthcheckTimeout bounds the healthcheck subcommand. Docker's own
// healthcheck timeout should be a little longer than this.
const healthcheckTimeout = 3 * time.Second

// loaded is what a command needs after its config is read: enough to log,
// to find itself for a healthcheck, and to run.
type loaded struct {
	http config.HTTP
	log  config.Log
	run  func(ctx context.Context, logger *slog.Logger) error
}

// command describes one binary.
type command struct {
	name string
	load func(path string, environ []string) (loaded, []string, error)
}

// Ozyd runs the ozyd command line. ui serves the web UI at / (nil
// for none); cmd/ozyd passes the embedded build.
func Ozyd(ctx context.Context, args, environ []string, stdout, stderr io.Writer, ui http.Handler) int {
	return command{
		name: "ozyd",
		load: func(path string, environ []string) (loaded, []string, error) {
			cfg, warnings, err := config.LoadOzyd(path, environ)
			return loaded{
				http: cfg.HTTP,
				log:  cfg.Log,
				run: func(ctx context.Context, logger *slog.Logger) error {
					s, err := server.New(cfg, server.Options{Logger: logger, UI: ui})
					if err != nil {
						return err
					}
					return s.Run(ctx, nil)
				},
			}, warnings, err
		},
	}.main(ctx, args, environ, stdout, stderr)
}

// Agent runs the agent command line.
func Agent(ctx context.Context, args, environ []string, stdout, stderr io.Writer) int {
	return command{
		name: "agent",
		load: func(path string, environ []string) (loaded, []string, error) {
			cfg, warnings, err := agentconfig.Load(path, environ)
			return loaded{
				http: cfg.HTTP,
				log:  cfg.Log,
				run: func(ctx context.Context, logger *slog.Logger) error {
					a, err := agent.New(cfg, agent.Options{Logger: logger})
					if err != nil {
						return err
					}
					return a.Run(ctx, nil)
				},
			}, warnings, err
		},
	}.main(ctx, args, environ, stdout, stderr)
}

func (c command) main(ctx context.Context, args, environ []string, stdout, stderr io.Writer) int {
	healthcheck := len(args) > 0 && args[0] == "healthcheck"
	if healthcheck {
		args = args[1:]
	}

	fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "YAML config file (default: built-in defaults)")
	version := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: %s [-config FILE] | -version | healthcheck [-config FILE]\n", c.name)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "%s: unexpected argument %q\n", c.name, fs.Arg(0))
		fs.Usage()
		return ExitUsage
	}
	if *version {
		fmt.Fprintf(stdout, "%s %s\n", c.name, buildinfo.Version)
		return ExitOK
	}

	l, warnings, err := c.load(*configPath, environ)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", c.name, err)
		return ExitUsage
	}
	if healthcheck {
		return c.healthcheck(ctx, l, stdout, stderr)
	}

	logger := newLogger(stderr, l.log)
	for _, w := range warnings {
		logger.Warn(w)
	}
	if err := l.run(ctx, logger); err != nil {
		logger.Error("exiting", "err", err)
		return ExitFailure
	}
	return ExitOK
}

func (c command) healthcheck(ctx context.Context, l loaded, stdout, stderr io.Writer) int {
	url, err := httpserve.LoopbackURL(l.http.Addr, "/healthz")
	if err != nil {
		fmt.Fprintf(stderr, "%s healthcheck: %v\n", c.name, err)
		return ExitUsage
	}
	if err := httpserve.Probe(ctx, url, healthcheckTimeout); err != nil {
		fmt.Fprintf(stderr, "%s healthcheck: unhealthy: %v\n", c.name, err)
		return ExitFailure
	}
	fmt.Fprintf(stdout, "%s healthcheck: ok\n", c.name)
	return ExitOK
}

func newLogger(w io.Writer, cfg config.Log) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.SlogLevel()}
	if cfg.Format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}
