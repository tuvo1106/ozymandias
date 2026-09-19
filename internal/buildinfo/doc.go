// Package buildinfo carries the version string stamped into the binaries at
// link time:
//
//	go build -ldflags "-X github.com/tuvo1106/ozymandias/internal/buildinfo.Version=v0.1.0"
//
// The Makefile and Dockerfile set it from `git describe`. It is reported by
// /healthz, the -version flag and the `ozy.build.info` self-metric, so
// "which build is running?" is always one request away.
package buildinfo

// Version is the build's version; "dev" when not set by the linker.
var Version = "dev"
