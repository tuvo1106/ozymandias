// Command agent is the ozymandias agent: the per-host collector that aggregates
// and forwards telemetry to ozyd. All behaviour lives in internal/cli;
// this file only connects it to the operating system.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/tuvo1106/ozymandias/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Agent(ctx, os.Args[1:], os.Environ(), os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
