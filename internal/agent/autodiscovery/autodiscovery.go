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
	// NameCap is the Docker collector's, so a check's tags name the container
	// the way its own metrics do.
	NameCap *docker.NameCap
	// Network is the Docker network %%host%% takes a container's address
	// on: the one the agent shares with the containers it checks. Empty,
	// a container on one network uses that one, and one on several is
	// refused, since any choice might be an address the agent cannot reach.
	Network string
	// Reserved are the names of the collectors the agent runs anyway — the
	// built-ins and the configured checks. A discovered instance may not
	// take one: the two would share one set of self-metrics, and a failing
	// configured check's errors could no longer be told from the
	// container's. Discovered instances may share a name with each other;
	// that is what folding replicas means.
	Reserved []string
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
	running map[key]*member
	failed  map[key]string // the settings that failed, logged once
	count   *selfmetrics.Gauge
	errs    *selfmetrics.Counter
	lastErr string
}

// key identifies one instance: a container and a check.
type key struct{ id, check string }

// member is a running instance. fp is the settings it was built from after
// template substitution: a container keeps its id across a restart in
// place, but may come back with another address, and an instance built
// with the old one would dial it forever. A changed fp rebuilds it.
type member struct {
	remove func()
	fp     string
	folded string // check and folded name, when the name was rewritten
	slot   int    // its replica tag, when folded
}

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
		running: map[key]*member{}, failed: map[key]string{},
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
//
// Each instance's settings are resolved again every sync. The same
// settings leave it alone; different ones (the container restarted in
// place with a new address) rebuild it; settings that failed before are
// not retried or logged again until they change — a container listed
// before it had an address is tried again once it has one.
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
	type item struct {
		ct    dockerapi.Container
		check string
		raw   map[string]string
	}
	seen := map[key]item{}
	for _, ct := range list {
		for check, raw := range groups(ct.Labels) {
			seen[key{ct.ID, check}] = item{ct, check, raw}
		}
	}
	// Gone first, so a replica number a departed container held is free
	// for one that arrived in the same sync.
	for k, m := range d.running {
		if _, ok := seen[k]; !ok {
			m.remove()
			delete(d.running, k)
		}
	}
	for k := range d.failed {
		if _, ok := seen[k]; !ok {
			delete(d.failed, k)
		}
	}
	// In a stable order, so replica numbers do not depend on map order.
	for _, k := range slices.SortedFunc(maps.Keys(seen), func(a, b key) int {
		return strings.Compare(a.id+"\x00"+a.check, b.id+"\x00"+b.check)
	}) {
		it := seen[k]
		d.sync(k, it.ct, it.check, it.raw)
	}
	d.count.Set(float64(len(d.running)))
}

// sync brings one container's instance of one check up to date.
func (d *Discovery) sync(k key, ct dockerapi.Container, check string, raw map[string]string) {
	resolved, rerr := d.resolveAll(ct, raw)
	// The name is part of what the instance was built from (its name, its
	// container_name tag): docker rename keeps the id and the labels, and
	// the check must follow the container's metrics to the new name.
	fp := ct.Name() + "\x01" + fingerprint(resolved, rerr)
	old := d.running[k]
	if old != nil && old.fp == fp || old == nil && d.failed[k] == fp {
		return
	}
	stop := func() {
		if old != nil {
			old.remove()
			delete(d.running, k)
		}
	}
	fail := func(err error) {
		stop()
		d.failed[k] = fp
		d.errs.Inc()
		d.log.Warn("container's check labels do not make a valid check; ignoring them",
			"container", ct.Name(), "check", check, "error", err)
	}
	if rerr != nil {
		fail(rerr)
		return
	}
	tags := docker.Tags(ct, d.opts.Rewrites, d.opts.NameCap)
	m := &member{fp: fp}
	if name := containerName(ct, tags); !slices.ContainsFunc(tags, func(t string) bool { return strings.HasPrefix(t, "container_id:") }) {
		m.folded = check + "\x00" + name
		m.slot = d.freeSlot(m.folded, old)
		tags = append(tags, "replica:"+strconv.Itoa(m.slot))
	}
	c, err := d.instance(ct, check, resolved, tags)
	if err == nil && slices.Contains(d.opts.Reserved, c.Name()) {
		err = fmt.Errorf("%s is the name of a configured collector; give the container's instance another with an %s%s.name label", c.Name(), LabelPrefix, check)
	}
	if err != nil {
		fail(err)
		return
	}
	stop()
	delete(d.failed, k)
	m.remove = d.opts.Scheduler.Add(c)
	d.running[k] = m
	verb := "started a check for a container"
	if old != nil {
		verb = "restarted a container's check: its settings changed"
	}
	d.log.Info(verb, "container", ct.Name(), "check", c.Name())
}

// freeSlot is the lowest replica number no other running instance folded
// into the same name holds; old keeps its own. Containers that a rewrite
// folds into one name carry the same tags, so their checks would write the
// same series and overwrite each other. A replica tag keeps them apart,
// and reusing the lowest free number bounds the series by the replicas
// running at once, not by every container that ever ran.
func (d *Discovery) freeSlot(folded string, old *member) int {
	if old != nil && old.folded == folded {
		return old.slot
	}
	used := map[int]bool{}
	for _, m := range d.running {
		if m != old && m.folded == folded {
			used[m.slot] = true
		}
	}
	n := 0
	for used[n] {
		n++
	}
	return n
}

// fingerprint is a stable text for resolved settings, or for the error
// resolving them.
func fingerprint(resolved map[string]string, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(resolved)) {
		b.WriteString(k)
		b.WriteByte(0)
		b.WriteString(resolved[k])
		b.WriteByte(0)
	}
	return b.String()
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

// resolveAll substitutes the template variables in every label value.
func (d *Discovery) resolveAll(ct dockerapi.Container, labels map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(labels))
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		v, err := d.resolve(labels[k], ct)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = v
	}
	return out, nil
}

// instance builds the collector for one check on one container, from its
// resolved label values and the tags its metrics carry.
//
// Unnamed, the instance takes its container's name after
// container_name_rewrite, the name its container's metrics carry. The
// instance name is the collector tag on its self-metrics, so containers
// with a name each (job-1, job-2, …) would otherwise mint a set of
// self-metric series per container, however the rewrite folds their
// container metrics. Folded containers' instances share one name, and so
// one set of self-metrics (the scheduler allows that); each is still
// checked, and its metrics carry its replica tag.
func (d *Discovery) instance(ct dockerapi.Container, check string, resolved map[string]string, tags []string) (collector.Collector, error) {
	settings := make(map[string]any, len(resolved)+1)
	for k, v := range resolved {
		settings[k] = typed(k, v)
	}
	if _, ok := settings["name"]; !ok {
		settings["name"] = containerName(ct, tags)
	}
	return d.opts.Checks.NewDiscovered(check, settings, tags, d.opts.Clock, d.opts.Logger)
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

// lowestPort is the smallest TCP port the container exposes. A container
// usually exposes one; with several, the lowest is the stable choice (the
// list's order is not), and a label can always name the port outright.
// Every check dials TCP, so a UDP port (a DNS server's 53 beside its
// 8080 status page) would be a port nothing answers on.
func lowestPort(ct dockerapi.Container) (int, bool) {
	best := 0
	for _, p := range ct.Ports {
		if p.Type != "" && p.Type != "tcp" {
			continue
		}
		if p.PrivatePort > 0 && (best == 0 || p.PrivatePort < best) {
			best = p.PrivatePort
		}
	}
	return best, best > 0
}

// typed makes a label value a setting. A check's own settings get the text
// as a plain YAML scalar node, which the check's config struct then
// decodes: "6379" becomes a number for an int field, "true" a boolean,
// "5s" a duration, and for a string field the text exactly as written.
// The text is not parsed as YAML here. Parsed, 0123 was octal 83, a
// password "pa ss #1" lost its "comment", "!x" was a tag, "&a b" an anchor,
// "[pw]" a list, and quotes and outer spaces went: a password or database
// name reached the check changed. Whether "[200, 301]" is a list depends
// on the field it lands in, which collector.Instance.Decode knows. Empty
// text and the null spellings are marked as text, or a string setting
// would decode them as nothing at all. The common settings are read here:
// name and interval are text, tags a list.
func typed(key, v string) any {
	switch key {
	case "name", "interval":
		return v
	case "tags":
		var out []any
		if strings.HasPrefix(v, "[") && yaml.Unmarshal([]byte(v), &out) == nil {
			return out
		}
		return v
	}
	n := &yaml.Node{Kind: yaml.ScalarNode, Value: v}
	switch v {
	case "", "null", "Null", "NULL", "~":
		n.Tag = "!!str"
	}
	return n
}
