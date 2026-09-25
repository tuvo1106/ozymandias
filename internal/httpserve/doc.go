// Package httpserve holds the HTTP plumbing both binaries share: running a
// server until its context is cancelled and then shutting it down gracefully,
// counting requests into the self-metrics registry, and probing another
// process's /healthz.
//
// Graceful shutdown matters more for an observability pipeline than for most
// services: the request being cut off by a restart is often an agent's batch
// of metrics. [Serve] stops accepting new connections the moment the context
// ends, gives in-flight requests the configured grace period to finish, and
// only then closes what remains — so a routine redeploy never turns into data
// loss, and a stuck handler can't hold shutdown open forever.
package httpserve
