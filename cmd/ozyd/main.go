// Command ozyd is the ozymandias server: intake, storage, query API and web
// UI in one process. All behaviour lives in internal/cli; this file only
// connects it to the operating system (arguments, environment, signals).
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/tuvo1106/ozymandias/internal/api"
	"github.com/tuvo1106/ozymandias/internal/api/ui"
	"github.com/tuvo1106/ozymandias/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Ozyd(ctx, os.Args[1:], os.Environ(), os.Stdout, os.Stderr, api.UI(ui.FS()))
	stop()
	os.Exit(code)
}
