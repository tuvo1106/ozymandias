package agent

import (
	"github.com/tuvo1106/ozymandias/internal/agent/check/httpcheck"
	"github.com/tuvo1106/ozymandias/internal/agent/check/openmetrics"
	"github.com/tuvo1106/ozymandias/internal/agent/check/postgres"
	"github.com/tuvo1106/ozymandias/internal/agent/check/process"
	"github.com/tuvo1106/ozymandias/internal/agent/check/redis"
	"github.com/tuvo1106/ozymandias/internal/agent/collector"
)

// DefaultChecks is the registry of the checks this agent ships: every name
// that may appear under collectors.checks or in an ozy.check.<name>.* label.
// Adding a check is a package with a New(collector.Instance) function and
// one line here.
func DefaultChecks() collector.Registry {
	return collector.Registry{
		"openmetrics":  openmetrics.New,
		"redis":        redis.New,
		"postgres":     postgres.New,
		httpcheck.Name: httpcheck.New,
		process.Name:   process.New,
	}
}
