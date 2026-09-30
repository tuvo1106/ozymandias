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
	// Network is the Docker network %%host%% takes a container's address
	// on: the one the agent shares with the containers it checks. Empty,
	// a container on one network uses that one, and one on several is
	// refused, since any choice might be an address the agent cannot reach.
	Network  string
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
//
// Unnamed, the instance takes its container's name after
// container_name_rewrite, the name its container's metrics carry. The
// instance name is the collector tag on its self-metrics, so containers
// with a name each (job-1, job-2, …) would otherwise mint a set of
// self-metric series per container, however the rewrite folds their
// container metrics. Folded containers' instances share one name, and so
// one set of self-metrics (the scheduler allows that); each is still
// checked.
func (d *Discovery) instance(ct dockerapi.Container, check string, labels map[string]string) (collector.Collector, error) {
	tags := docker.Tags(ct, d.opts.Rewrites)
	settings := make(map[string]any, len(labels)+1)
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		v, err := d.resolve(labels[k], ct)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		settings[k] = typed(k, v)
	}
	if _, ok := settings["name"]; !ok {
		settings["name"] = containerName(ct, tags)
	}
	return d.opts.Checks.NewInstance(check, 0, 1, settings, tags, d.opts.Clock, d.opts.Logger)
}

// containerName is the container_name tag Tags gave ct: its name after
// the rewrites.
func containerName(ct dockerapi.Container, tags []string) string {
	for _, t := range tags {
		if n, ok := strings.CutPrefix(t, "container_name:"); ok {
			return n
		}
	}
	return ct.Name()
}

// resolve substitutes the template variables in one label value.
func (d *Discovery) resolve(v string, ct dockerapi.Container) (string, error) {
	if strings.Contains(v, "%%host%%") {
		host, err := address(ct, d.opts.Network)
		if err != nil {
			return "", fmt.Errorf("%%%%host%%%%: %w", err)
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

// address is the container's IP on network, or, with no network given, on
// its only network. A container on several networks has an address on
// each, and only the ones the agent shares reach it; picking one by any
// rule the agent cannot check (the first by name, say) would dial an
// address with no route, and the check would report the container down
// forever.
func address(ct dockerapi.Container, network string) (string, error) {
	nets := ct.NetworkSettings.Networks
	if network != "" {
		if ip := nets[network].IPAddress; ip != "" {
			return ip, nil
		}
		return "", fmt.Errorf("the container has no address on network %q, the one autodiscovery_network names", network)
	}
	var names []string
	for _, n := range slices.Sorted(maps.Keys(nets)) {
		if nets[n].IPAddress != "" {
			names = append(names, n)
		}
	}
	switch len(names) {
	case 0:
		return "", fmt.Errorf("the container has no network address")
	case 1:
		return nets[names[0]].IPAddress, nil
	}
	return "", fmt.Errorf("the container is on several networks (%s); set collectors.docker.autodiscovery_network to the one the agent shares with it",
		strings.Join(names, ", "))
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

// typed makes a label value a setting. A check's own settings get the value
// as a YAML node, which the check's config struct then decodes: "6379"
// becomes a number for an int field, "[200, 301]" a list, and for a string
// field the text exactly as written. Decoding the text to a Go value here
// instead would lose that: YAML reads 0123 as octal 83, 1e3 as 1000,
// 2024-01-01 as a timestamp, and a password or database name would reach
// the check changed. The common settings are read here: name and interval
// are text, tags a list. Text that is not valid YAML, or is a mapping
// (settings do not nest in a label), stays text.
func typed(key, v string) any {
	switch key {
	case "name", "interval":
		return v
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(v), &doc); err != nil || len(doc.Content) == 0 {
		return v
	}
	n := doc.Content[0]
	switch {
	case n.Kind == yaml.MappingNode, n.Tag == "!!null": // "null", "~"
		return v
	case key == "tags":
		var out any
		if err := n.Decode(&out); err != nil {
			return v
		}
		return out
	}
	return n
}
