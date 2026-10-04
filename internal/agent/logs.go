package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/collector/dockerapi"
	"github.com/tuvo1106/ozymandias/internal/agent/forwarder"
	"github.com/tuvo1106/ozymandias/internal/agent/logpipeline"
	"github.com/tuvo1106/ozymandias/internal/agent/tailer"
	"github.com/tuvo1106/ozymandias/internal/buildinfo"
)

// logsRuntime is the log collection half of the agent: file and container
// tailers feeding one sink.
type logsRuntime struct {
	files  *tailer.Files
	docker *tailer.Docker
	reg    *tailer.Registry
	poll   time.Duration
}

// setupLogs builds the tailers when logs.enabled. It reads nothing from disk
// and opens no stream: that starts in Run.
func (a *Agent) setupLogs(opts Options) error {
	l := a.cfg.Logs
	if !l.Enabled {
		return nil
	}
	registry, err := tailer.OpenRegistry(l.RegistryPath)
	if err != nil {
		// Positions are lost, which is wasteful or lossy at the edges, not fatal.
		a.log.Warn("logs: starting with an empty registry", "err", err)
	}
	sink := forwarder.NewLogSink(forwarder.LogSinkOptions{
		URL:      strings.TrimSuffix(a.cfg.Intake.URL, "/"),
		Timeout:  a.cfg.Forwarder.Timeout,
		Hostname: a.hostname,
		Version:  buildinfo.Version,
		Registry: a.reg,
		Logger:   a.log,
	})
	totals := &logpipeline.Totals{}
	a.registerLogMetrics(totals)

	var fileSrcs []tailer.FileSource
	var dockerSrcs []tailer.DockerSource
	for _, s := range l.Sources {
		tags := slices.Concat(a.cfg.Tags, s.Tags)
		start := s.Multiline.StartPattern
		switch s.Type {
		case "file":
			fileSrcs = append(fileSrcs, tailer.FileSource{
				Path: s.Path, Service: s.Service, Source: s.Source, Tags: tags,
				StartPosition: s.StartPosition, MultilineStart: start, Pipeline: s.Spec,
			})
		case "docker":
			dockerSrcs = append(dockerSrcs, tailer.DockerSource{
				IncludeLabels: s.IncludeLabels, ExcludeNames: s.ExcludeNames, Service: s.Service,
				Source: s.Source, Tags: tags, MultilineStart: start, Pipeline: s.Spec, StartPosition: s.StartPosition,
			})
		}
	}
	rt := &logsRuntime{reg: registry, poll: l.PollInterval}
	if len(fileSrcs) > 0 {
		rt.files, err = tailer.NewFiles(fileSrcs, tailer.Options{
			Registry: registry, Sink: sink, Host: a.hostname, Clock: a.clock, Logger: a.log,
			ScanInterval: l.ScanInterval, BatchLogs: l.BatchLogs, Totals: totals,
		})
		if err != nil {
			return fmt.Errorf("logs: %w", err)
		}
		a.reg.CounterFunc("ozy.agent.logs.file_truncations", func() float64 { return float64(rt.files.Stats().Truncations) })
		a.reg.GaugeFunc("ozy.agent.logs.files_tailed", func() float64 { return float64(rt.files.Stats().Files) })
	}
	if len(dockerSrcs) > 0 || l.ContainerCollectAll {
		api := opts.LogDockerAPI
		if api == nil {
			if a.dockerClient == nil {
				a.dockerClient = dockerapi.New(dockerapi.Options{Socket: a.cfg.Collectors.Docker.Socket})
			}
			api = a.dockerClient
		}
		rt.docker, err = tailer.NewDocker(dockerSrcs, tailer.DockerOptions{
			API: api, Registry: registry, Sink: sink, Host: a.hostname, Clock: a.clock, Logger: a.log,
			ScanInterval: l.ScanInterval, BatchLogs: l.BatchLogs, CollectAll: l.ContainerCollectAll, Totals: totals,
		})
		if err != nil {
			return fmt.Errorf("logs: %w", err)
		}
		a.reg.GaugeFunc("ozy.agent.logs.containers_tailed", func() float64 { return float64(rt.docker.Stats().Containers) })
		a.reg.CounterFunc("ozy.agent.logs.container_reconnects", func() float64 { return float64(rt.docker.Stats().Reconnects) })
	}
	a.logs = rt
	return nil
}

func (a *Agent) registerLogMetrics(t *logpipeline.Totals) {
	for name, get := range map[string]func(logpipeline.Stats) int64{
		"ozy.agent.logs.lines":        func(s logpipeline.Stats) int64 { return s.Lines },
		"ozy.agent.logs.emitted":      func(s logpipeline.Stats) int64 { return s.Emitted },
		"ozy.agent.logs.excluded":     func(s logpipeline.Stats) int64 { return s.Excluded },
		"ozy.agent.logs.rate_limited": func(s logpipeline.Stats) int64 { return s.RateLimited },
		"ozy.agent.logs.unparsed":     func(s logpipeline.Stats) int64 { return s.Unparsed },
		"ozy.agent.logs.redactions":   func(s logpipeline.Stats) int64 { return s.Redactions },
	} {
		a.reg.CounterFunc(name, func() float64 { return float64(get(t.Stats())) })
	}
}

// run starts the tailers; they stop when ctx is done, after flushing.
func (rt *logsRuntime) run(ctx context.Context, start func(func())) {
	if rt == nil {
		return
	}
	if rt.files != nil {
		start(func() { rt.files.Run(ctx, rt.poll) })
	}
	if rt.docker != nil {
		start(func() { rt.docker.Run(ctx) })
	}
}
