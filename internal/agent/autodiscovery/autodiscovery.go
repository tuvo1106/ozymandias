package autodiscovery

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tuvo1106/ozymandias/internal/agent/collector"
	"github.com/tuvo1106/ozymandias/internal/agent/collector/docker"
	"github.com/tuvo1106/ozymandias/internal/agent/collector/dockerapi"
	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
)

// LabelPrefix starts every label autodiscovery reads:
// ozy.check.<check>.<setting>.
const LabelPrefix = "ozy.check."

// DefaultInterval is how often the container list is read. Short enough
// that a check starts within seconds of its container; one list call is
// cheap for the daemon.
const DefaultInterval = 10 * time.Second

// Lister is the part of the Docker API discovery uses.
type Lister interface {
	ListContainers(ctx context.Context) ([]dockerapi.Container, error)
}

// Scheduler is where discovered instances run; *collector.Scheduler is one.
type Scheduler interface {
	Add(collector.Collector) (remove func())
}

// Options configures a [Discovery].
type Options struct {
	API       Lister
	Checks    collector.Registry
	Scheduler Scheduler
	// Rewrites are the Docker collector's name rewrites, so an instance is
	// tagged like its container's metrics.
	Rewrites []docker.Rewrite
	Interval time.Duration         // default DefaultInterval
	Clock    clock.Clock           // default clock.Real()
	Logger   *slog.Logger          // default slog.Default()
	Registry *selfmetrics.Registry // default: a new registry
}

// Discovery keeps the scheduler's discovered instances in step with the
// running containers' labels.
type Discovery struct {
	opts    Options
	log     *slog.Logger
	running map[key]func() // the instance's remove function
	failed  map[key]bool   // logged once; labels cannot change
	count   *selfmetrics.Gauge
	errs    *selfmetrics.Counter
	lastErr string
}

// key identifies one instance: a container and a check. A container's
// labels are fixed for its life, so the id is enough to know its settings.
type key struct{ id, check string }

// New returns a Discovery. Call Run to start it.
func New(opts Options) *Discovery {
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Registry == nil {
		opts.Registry = selfmetrics.NewRegistry()
	}
	return &Discovery{
		opts: opts, log: opts.Logger.With("component", "autodiscovery"),
		running: map[key]func(){}, failed: map[key]bool{},
		count: opts.Registry.Gauge("ozy.agent.autodiscovery.instances"),
		errs:  opts.Registry.Counter("ozy.agent.autodiscovery.errors"),
	}
}

// Run syncs at once and then every interval until ctx is cancelled. The
// instances it started are left to the scheduler, which is stopping too.
func (d *Discovery) Run(ctx context.Context) {
	tick := d.opts.Clock.NewTicker(d.opts.Interval)
	defer tick.Stop()
	for {
		d.Sync(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C():
		}
	}
}

// Sync reads the running containers once and starts and stops instances
// to match. A failed list changes nothing: a daemon that is briefly away
// should not stop every discovered check.
func (d *Discovery) Sync(ctx context.Context) {
	list, err := d.opts.API.ListContainers(ctx)
	if err != nil {
		if ctx.Err() == nil && err.Error() != d.lastErr {
			d.log.Warn("listing containers failed; keeping the current checks", "error", err)
			d.lastErr = err.Error()
		}
		return
	}
	d.lastErr = ""
	seen := map[key]bool{}
	for _, ct := range list {
		for check, raw := range groups(ct.Labels) {
			k := key{ct.ID, check}
			seen[k] = true
			if d.running[k] != nil || d.failed[k] {
				continue
			}
			c, err := d.instance(ct, check, raw)
			if err != nil {
				d.failed[k] = true
				d.errs.Inc()
				d.log.Warn("container's check labels do not make a valid check; ignoring them",
					"container", ct.Name(), "check", check, "error", err)
				continue
			}
			d.running[k] = d.opts.Scheduler.Add(c)
			d.log.Info("started a check for a container", "container", ct.Name(), "check", c.Name())
		}
	}
	for k, remove := range d.running {
		if !seen[k] {
			remove()
			delete(d.running, k)
		}
	}
	for k := range d.failed {
		if !seen[k] {
			delete(d.failed, k)
		}
	}
	d.count.Set(float64(len(d.running)))
}

// groups splits a container's ozy.check.* labels by check: label
// ozy.check.redis.port=6379 is setting port of check redis.
func groups(labels map[string]string) map[string]map[string]string {
	var out map[string]map[string]string
	for l, v := range labels {
		rest, ok := strings.CutPrefix(l, LabelPrefix)
		if !ok {
			continue
		}
		check, setting, ok := strings.Cut(rest, ".")
		if !ok || check == "" || setting == "" {
			continue
		}
		if out == nil {
			out = map[string]map[string]string{}
		}
		if out[check] == nil {
			out[check] = map[string]string{}
		}
		out[check][setting] = v
	}
	return out
}

// instance builds the collector for one check on one container.
func (d *Discovery) instance(ct dockerapi.Container, check string, labels map[string]string) (collector.Collector, error) {
	settings := make(map[string]any, len(labels)+1)
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		v, err := resolve(labels[k], ct)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		settings[k] = typed(v)
	}
	if _, ok := settings["name"]; !ok {
		settings["name"] = ct.Name()
	}
	return d.opts.Checks.NewInstance(check, 0, 1, settings, docker.Tags(ct, d.opts.Rewrites), d.opts.Clock, d.opts.Logger)
}

// resolve substitutes the template variables in one label value.
func resolve(v string, ct dockerapi.Container) (string, error) {
	if strings.Contains(v, "%%host%%") {
		host, ok := address(ct)
		if !ok {
			return "", fmt.Errorf("%%%%host%%%%: the container has no network address")
		}
		v = strings.ReplaceAll(v, "%%host%%", host)
	}
	if strings.Contains(v, "%%port%%") {
		port, ok := lowestPort(ct)
		if !ok {
			return "", fmt.Errorf("%%%%port%%%%: the container exposes no port")
		}
		v = strings.ReplaceAll(v, "%%port%%", strconv.Itoa(port))
	}
	return v, nil
}

// address is the container's IP on the first of its networks, by name.
func address(ct dockerapi.Container) (string, bool) {
	nets := ct.NetworkSettings.Networks
	for _, n := range slices.Sorted(maps.Keys(nets)) {
		if ip := nets[n].IPAddress; ip != "" {
			return ip, true
		}
	}
	return "", false
}

// lowestPort is the smallest port the container exposes. A container
// usually exposes one; with several, the lowest is the stable choice (the
// list's order is not), and a label can always name the port outright.
func lowestPort(ct dockerapi.Container) (int, bool) {
	best := 0
	for _, p := range ct.Ports {
		if p.PrivatePort > 0 && (best == 0 || p.PrivatePort < best) {
			best = p.PrivatePort
		}
	}
	return best, best > 0
}

// typed reads a label value as YAML, so "6379" is a number and
// "[200, 301]" a list; text that is not valid YAML, or is a YAML mapping
// (settings do not nest in a label), stays text.
func typed(v string) any {
	var out any
	if err := yaml.Unmarshal([]byte(v), &out); err != nil || out == nil {
		return v
	}
	if _, isMap := out.(map[string]any); isMap {
		return v
	}
	return out
}
