// Package cli is the command-line layer of both binaries. cmd/ozyd and
// cmd/agent are three-line mains that hand os.Args, the environment and a
// signal-aware context to this package, so everything that can go wrong at
// startup — flag parsing, config errors, the healthcheck subcommand, exit
// codes — is testable in-process.
//
// Usage (both binaries):
//
//	ozyd [-config FILE]              run
//	ozyd -version                    print the version and exit
//	ozyd healthcheck [-config FILE]  probe the running instance's /healthz
//
// healthcheck exists because the container images are distroless — no curl,
// no shell — so the binary checks itself: it reads the same config to find
// its own listen address and GETs /healthz over loopback.
//
// Exit codes: 0 success, 1 runtime failure (including an unhealthy
// healthcheck), 2 usage or configuration error.
package cli
