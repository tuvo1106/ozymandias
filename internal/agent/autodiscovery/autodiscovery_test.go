package autodiscovery

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/collector"
	"github.com/tuvo1106/ozymandias/internal/agent/collector/docker"
	"github.com/tuvo1106/ozymandias/internal/agent/collector/dockerapi"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

// probe is a check that records its settings.
type probeConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Statuses []int  `yaml:"statuses"`
	Verify   bool   `yaml:"verify"`
	URL      string `yaml:"url"`
	Password string `yaml:"password"`
	DB       string `yaml:"db"`
	Note     string `yaml:"note"`
}

type probe struct{ cfg probeConfig }

func (p *probe) Name() string            { return "probe" }
func (p *probe) Interval() time.Duration { return 0 }
func (p *probe) Collect(_ context.Context, emit collector.Emit) error {
	emit(collector.Metric{Name: "probe.up", Value: 1})
	return nil
}

var checks = collector.Registry{"probe": func(inst collector.Instance) (collector.Collector, error) {
	var cfg probeConfig
	if err := inst.Decode(&cfg); err != nil {
		return nil, err
	}
	return &probe{cfg: cfg}, nil
}}

type lister struct {
	mu   sync.Mutex
	list []dockerapi.Container
	err  error
}

func (l *lister) set(list []dockerapi.Container, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.list, l.err = list, err
}

func (l *lister) ListContainers(context.Context) ([]dockerapi.Container, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.list), l.err
}

// sched records what is running. Like the real scheduler, it runs
// collectors that share a name side by side.
type sched struct {
	mu      sync.Mutex
	running []*collector.Collector
}

func (s *sched) Add(c collector.Collector) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := &c
	s.running = append(s.running, p)
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if i := slices.Index(s.running, p); i >= 0 {
			s.running = slices.Delete(s.running, i, i+1)
		}
	}
}

func (s *sched) get(name string) collector.Collector {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.running {
		if (*c).Name() == name {
			return *c
		}
	}
	return nil
}

func (s *sched) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.running {
		out = append(out, (*c).Name())
	}
	slices.Sort(out)
	return out
}

func container(id, name string, labels map[string]string) dockerapi.Container {
	return dockerapi.Container{
		ID: id + strings.Repeat("0", 64-len(id)), Names: []string{"/" + name}, Image: "redis:7", Labels: labels,
		Ports: []dockerapi.Port{{PrivatePort: 16379}, {PrivatePort: 6379}},
		NetworkSettings: dockerapi.NetworkSettings{Networks: map[string]dockerapi.EndpointSettings{
			"alpha": {IPAddress: "172.18.0.4"},
		}},
	}
}

// twoNets is c on a second network too.
func twoNets(c dockerapi.Container) dockerapi.Container {
	c.NetworkSettings.Networks = map[string]dockerapi.EndpointSettings{
		"zeta": {IPAddress: "10.0.0.9"}, "alpha": {IPAddress: "172.18.0.4"}, "none": {},
	}
	return c
}

// build is what a sync does for one container and check: resolve the
// templates, then make the instance with the container's tags.
func build(d *Discovery, ct dockerapi.Container, check string, labels map[string]string) (collector.Collector, error) {
	r, err := d.resolveAll(ct, labels)
	if err != nil {
		return nil, err
	}
	return d.instance(ct, check, r, docker.Tags(ct, d.opts.Rewrites))
}

// tagsOf runs the named instance once and returns its metric's tags.
func tagsOf(t *testing.T, s *sched, name string) []string {
	t.Helper()
	c := s.get(name)
	if c == nil {
		t.Fatalf("%s is not running: %v", name, s.names())
	}
	var m collector.Metric
	_ = c.Collect(context.Background(), func(x collector.Metric) { m = x })
	return append(m.Tags, m.Keep...)
}

func setup(t *testing.T, opts Options) (*Discovery, *lister, *sched, *bytes.Buffer, *selfmetrics.Registry) {
	t.Helper()
	l, s := &lister{}, &sched{}
	var logs bytes.Buffer
	reg := selfmetrics.NewRegistry()
	opts.API, opts.Scheduler, opts.Checks, opts.Registry = l, s, checks, reg
	opts.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	opts.Clock = testutil.NewFakeClock(time.Unix(1790000000, 0))
	return New(opts), l, s, &logs, reg
}

func TestSync_StartsAndStopsWithTheContainer(t *testing.T) {
	d, l, s, _, reg := setup(t, Options{})
	l.set([]dockerapi.Container{
		container("a1", "shop-redis-1", map[string]string{
			"ozy.check.probe.host":       "%%host%%",
			"ozy.check.probe.port":       "%%port%%",
			"ozy.check.probe.statuses":   "[200, 301]",
			"ozy.check.probe.verify":     "true",
			"ozy.check.probe.url":        "http://%%host%%:%%port%%/metrics",
			"com.docker.compose.project": "shop",
		}),
		container("b2", "plain", nil),
	}, nil)
	d.Sync(context.Background())
	c := s.get("probe:shop-redis-1")
	if c == nil {
		t.Fatalf("running: %v", s.names())
	}
	var got []collector.Metric
	_ = c.Collect(context.Background(), func(m collector.Metric) { got = append(got, m) })
	if len(got) != 1 || !slices.Contains(got[0].Keep, "container_name:shop-redis-1") || !slices.Contains(got[0].Keep, "compose_project:shop") {
		t.Fatalf("not tagged like the container: %+v", got)
	}
	if n := reg.Gauge("ozy.agent.autodiscovery.instances").Value(); n != 1 {
		t.Errorf("instances = %v", n)
	}

	d.Sync(context.Background()) // unchanged: nothing restarted
	if s.get("probe:shop-redis-1") != c {
		t.Fatal("the instance was rebuilt on an unchanged sync")
	}
	l.set(nil, nil)
	d.Sync(context.Background())
	if len(s.names()) != 0 {
		t.Fatalf("still running after the container went: %v", s.names())
	}
}

// The settings the check sees: templates resolved, values typed.
func TestInstance_ResolvesAndTypes(t *testing.T) {
	d, _, _, _, _ := setup(t, Options{})
	var seen probeConfig
	d.opts.Checks = collector.Registry{"probe": func(inst collector.Instance) (collector.Collector, error) {
		if err := inst.Decode(&seen); err != nil {
			return nil, err
		}
		return &probe{}, nil
	}}
	_, err := build(d, container("a1", "r", nil), "probe", map[string]string{
		"host": "%%host%%", "port": "%%port%%", "statuses": "[200, 301]", "verify": "true",
		"url": "http://%%host%%:%%port%%/metrics",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := probeConfig{Host: "172.18.0.4", Port: 6379, Statuses: []int{200, 301}, Verify: true, URL: "http://172.18.0.4:6379/metrics"}
	if seen.Host != want.Host || seen.Port != want.Port || !slices.Equal(seen.Statuses, want.Statuses) || seen.Verify != want.Verify || seen.URL != want.URL {
		t.Fatalf("got %+v\nwant %+v", seen, want)
	}
}

// A label value reaches a string setting exactly as written. Read as YAML
// first, 0123 was octal 83, 1e3 was 1000 and a date a timestamp; parsed as
// a YAML node, "pa ss #1" lost its "comment", "!secret" was a tag and
// quotes and spaces went: a password or a database name changed on the way.
func TestInstance_StringSettingsKeepTheirText(t *testing.T) {
	d, _, _, _, _ := setup(t, Options{})
	var seen probeConfig
	var name string
	d.opts.Checks = collector.Registry{"probe": func(inst collector.Instance) (collector.Collector, error) {
		name = inst.Name
		return &probe{}, inst.Decode(&seen)
	}}
	for _, v := range []string{"0123", "007", "1e3", "0x1F", "1_000", "2024-01-01", "null", "~", "yes", "3.10", "a: b", "[x", "",
		"pa ss #1", "!secret", "&a bar", "*alias", "'quoted'", `"dq"`, " sp ", "|", ">", "- x", "%TAG", "@at", "a\nb",
		"[pw]", "[ok, yes]", "[]"} {
		_, err := build(d, container("a1", "r", nil), "probe", map[string]string{"password": v, "db": v, "name": "007"})
		if err != nil {
			t.Errorf("%q: %v", v, err)
			continue
		}
		if seen.Password != v || seen.DB != v {
			t.Errorf("label %q reached the check as password %q, db %q", v, seen.Password, seen.DB)
		}
		if name != "probe:007" {
			t.Errorf("a name label of 007 made instance %q", name)
		}
	}
	for in, want := range map[string]int{"6379": 6379, "0x1F": 31} {
		if _, err := build(d, container("a1", "r", nil), "probe", map[string]string{"port": in}); err != nil || seen.Port != want {
			t.Errorf("port %q = %d, %v; want %d", in, seen.Port, err, want)
		}
	}
	if _, err := build(d, container("a1", "r", nil), "probe", map[string]string{"port": "0123x"}); err == nil {
		t.Error("a port that is not a number was accepted")
	}
}

// %%host%% is the address on the network the agent shares: the configured
// one, or the container's only one. On several with none configured, any
// pick might be unreachable, so the labels are refused and the log says
// what to set.
func TestInstance_HostIsOnTheSharedNetwork(t *testing.T) {
	var seen probeConfig
	checks := collector.Registry{"probe": func(inst collector.Instance) (collector.Collector, error) {
		return &probe{}, inst.Decode(&seen)
	}}
	labels := map[string]string{"host": "%%host%%"}
	d, _, _, _, _ := setup(t, Options{})
	d.opts.Checks = checks
	if _, err := build(d, twoNets(container("a1", "r", nil)), "probe", labels); err == nil ||
		!strings.Contains(err.Error(), "autodiscovery_network") || !strings.Contains(err.Error(), "alpha, zeta") {
		t.Fatalf("two networks, none configured: %v", err)
	}
	d.opts.Network = "zeta"
	if _, err := build(d, twoNets(container("a1", "r", nil)), "probe", labels); err != nil || seen.Host != "10.0.0.9" {
		t.Fatalf("configured zeta: host %q, %v", seen.Host, err)
	}
	if _, err := build(d, container("a1", "r", nil), "probe", labels); err == nil || !strings.Contains(err.Error(), `"zeta"`) {
		t.Fatalf("not on the configured network: %v", err)
	}
	d.opts.Network = ""
	if _, err := build(d, container("a1", "r", nil), "probe", labels); err != nil || seen.Host != "172.18.0.4" {
		t.Fatalf("one network: host %q, %v", seen.Host, err)
	}
}

func TestSync_BadLabelsAreLoggedOnce(t *testing.T) {
	d, l, s, logs, reg := setup(t, Options{})
	noAddr := container("c3", "lonely", map[string]string{"ozy.check.probe.host": "%%host%%"})
	noAddr.NetworkSettings.Networks = nil
	noPort := container("d4", "portless", map[string]string{"ozy.check.probe.port": "%%port%%"})
	noPort.Ports = nil
	l.set([]dockerapi.Container{
		container("a1", "typo", map[string]string{"ozy.check.probe.hots": "x"}),
		container("b2", "unknown", map[string]string{"ozy.check.nope.x": "1"}),
		noAddr, noPort,
		container("e5", "malformed", map[string]string{"ozy.check.probe": "x", "ozy.check..x": "y"}),
	}, nil)
	d.Sync(context.Background())
	d.Sync(context.Background())
	if len(s.names()) != 0 {
		t.Fatalf("started %v", s.names())
	}
	if n := strings.Count(logs.String(), "do not make a valid check"); n != 4 {
		t.Fatalf("logged %d times, want once per bad container:\n%s", n, logs.String())
	}
	for _, want := range []string{"hots", "no such check", "no network address", "exposes no port"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q", want)
		}
	}
	if n := reg.Counter("ozy.agent.autodiscovery.errors").Value(); n != 4 {
		t.Errorf("errors = %d", n)
	}
	// A container that goes and comes back (a new id) is tried afresh; the
	// forgotten failure does not leak.
	l.set(nil, nil)
	d.Sync(context.Background())
	if len(d.failed) != 0 {
		t.Fatalf("remembering %d failures of containers that are gone", len(d.failed))
	}
}

// A daemon that fails to list keeps what is running, and says so once.
func TestSync_AFailedListChangesNothing(t *testing.T) {
	d, l, s, logs, _ := setup(t, Options{})
	l.set([]dockerapi.Container{container("a1", "r", map[string]string{"ozy.check.probe.port": "1"})}, nil)
	d.Sync(context.Background())
	l.set(nil, errors.New("daemon away"))
	d.Sync(context.Background())
	d.Sync(context.Background())
	if len(s.names()) != 1 {
		t.Fatalf("running %v after a failed list", s.names())
	}
	if n := strings.Count(logs.String(), "listing containers failed"); n != 1 {
		t.Fatalf("logged %d times", n)
	}
}

// A name rewrite applies to the instance name as to the tags: containers
// with a name each would otherwise mint self-metric series (tagged with the
// instance name) per container, however the rewrite folds their metrics.
// Folded, each container is still checked, and a replica tag keeps their
// metrics apart — the same tags would overwrite each other in the store.
// A replica that leaves frees its number for the next, so the series are
// as many as the replicas running at once.
func TestSync_FoldedContainersGetReplicaTags(t *testing.T) {
	d, l, s, _, _ := setup(t, Options{Rewrites: []docker.Rewrite{{Match: regexp.MustCompile(`^job-.*`), Replace: "job"}}})
	job := func(id, name string) dockerapi.Container {
		return container(id, name, map[string]string{"ozy.check.probe.port": "1"})
	}
	l.set([]dockerapi.Container{job("a1", "job-1"), job("b2", "job-2")}, nil)
	d.Sync(context.Background())
	if got := s.names(); !slices.Equal(got, []string{"probe:job", "probe:job"}) {
		t.Fatalf("running %v", got)
	}
	replicas := func() []string {
		var out []string
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, c := range s.running {
			var m collector.Metric
			_ = (*c).Collect(context.Background(), func(x collector.Metric) { m = x })
			m.Tags = append(m.Tags, m.Keep...)
			if !slices.Contains(m.Tags, "container_name:job") || slices.ContainsFunc(m.Tags, func(t string) bool { return strings.HasPrefix(t, "container_id:") }) {
				t.Errorf("folded tags %v", m.Tags)
			}
			for _, tg := range m.Tags {
				if strings.HasPrefix(tg, "replica:") {
					out = append(out, tg)
				}
			}
		}
		slices.Sort(out)
		return out
	}
	if got := replicas(); !slices.Equal(got, []string{"replica:0", "replica:1"}) {
		t.Fatalf("replicas %v", got)
	}
	// job-1 goes, job-3 comes: it takes the number job-1 freed.
	l.set([]dockerapi.Container{job("b2", "job-2"), job("c3", "job-3")}, nil)
	d.Sync(context.Background())
	if got := replicas(); !slices.Equal(got, []string{"replica:0", "replica:1"}) {
		t.Fatalf("after a replica was replaced: %v", got)
	}
	// An unfolded container has its id, and no replica tag.
	d2, l2, s2, _, _ := setup(t, Options{})
	l2.set([]dockerapi.Container{job("a1", "web")}, nil)
	d2.Sync(context.Background())
	if tg := tagsOf(t, s2, "probe:web"); slices.ContainsFunc(tg, func(t string) bool { return strings.HasPrefix(t, "replica:") }) {
		t.Fatalf("an unfolded container got a replica tag: %v", tg)
	}
}

// A container restarted in place keeps its id but may get a new address:
// the instance is rebuilt with it. One listed before it had an address
// fails, and is tried again once it has one — and only then, not every
// sync, and it logs once per distinct failure.
func TestSync_SettingsThatChangeRebuildTheInstance(t *testing.T) {
	d, l, s, logs, reg := setup(t, Options{})
	var hosts []string
	d.opts.Checks = collector.Registry{"probe": func(inst collector.Instance) (collector.Collector, error) {
		var cfg probeConfig
		err := inst.Decode(&cfg)
		hosts = append(hosts, cfg.Host)
		return &probe{cfg: cfg}, err
	}}
	ct := container("a1", "cache", map[string]string{"ozy.check.probe.host": "%%host%%"})
	ct.NetworkSettings.Networks = nil
	l.set([]dockerapi.Container{ct}, nil)
	d.Sync(context.Background())
	d.Sync(context.Background())
	if len(s.names()) != 0 || strings.Count(logs.String(), "no network address") != 1 || reg.Counter("ozy.agent.autodiscovery.errors").Value() != 1 {
		t.Fatalf("no address yet: running %v, logs:\n%s", s.names(), logs.String())
	}
	ct.NetworkSettings.Networks = map[string]dockerapi.EndpointSettings{"alpha": {IPAddress: "172.18.0.4"}}
	l.set([]dockerapi.Container{ct}, nil)
	d.Sync(context.Background())
	d.Sync(context.Background())
	ct.NetworkSettings.Networks = map[string]dockerapi.EndpointSettings{"alpha": {IPAddress: "172.18.0.7"}}
	l.set([]dockerapi.Container{ct}, nil)
	d.Sync(context.Background())
	if got := s.names(); !slices.Equal(got, []string{"probe:cache"}) || !slices.Equal(hosts, []string{"172.18.0.4", "172.18.0.7"}) {
		t.Fatalf("running %v, built with hosts %v", got, hosts)
	}
	if !strings.Contains(logs.String(), "settings changed") {
		t.Errorf("the rebuild was not logged:\n%s", logs.String())
	}
}

// A container recreated under its name (compose up) is a new id: its check
// is started for the new container and the old one's stopped, in one sync.
func TestSync_ARecreatedContainerKeepsItsCheck(t *testing.T) {
	d, l, s, _, _ := setup(t, Options{})
	l.set([]dockerapi.Container{container("a1", "redis", map[string]string{"ozy.check.probe.port": "1"})}, nil)
	d.Sync(context.Background())
	old := s.get("probe:redis")
	l.set([]dockerapi.Container{container("b2", "redis", map[string]string{"ozy.check.probe.port": "2"})}, nil)
	d.Sync(context.Background())
	if got := s.names(); !slices.Equal(got, []string{"probe:redis"}) || s.get("probe:redis") == old {
		t.Fatalf("after the recreate: running %v (the old instance: %v)", got, s.get("probe:redis") == old)
	}
}

func TestRun_SyncsOnItsInterval(t *testing.T) {
	testutil.CheckGoroutines(t)
	d, l, s, _, _ := setup(t, Options{Interval: 5 * time.Second})
	fc := d.opts.Clock.(*testutil.FakeClock)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	testutil.Eventually(t, time.Second, func() bool { return fc.Waiters() == 1 }, "first sync")
	l.set([]dockerapi.Container{container("a1", "r", map[string]string{"ozy.check.probe.port": "1"})}, nil)
	fc.Advance(5 * time.Second)
	testutil.Eventually(t, time.Second, func() bool { return len(s.names()) == 1 }, "second sync")
	cancel()
	<-done
}

// Review finding: a container named like a configured instance (redis:cache
// configured, a container cache with redis labels) shared its self-metrics
// silently. The discovered one is refused, once, and told what to set.
func TestSync_AConfiguredNameIsNotTaken(t *testing.T) {
	d, l, s, logs, _ := setup(t, Options{Reserved: []string{"probe:cache", "host"}})
	l.set([]dockerapi.Container{
		container("a1", "cache", map[string]string{"ozy.check.probe.port": "1"}),
		container("b2", "cache2", map[string]string{"ozy.check.probe.port": "1", "ozy.check.probe.name": "other"}),
	}, nil)
	d.Sync(context.Background())
	d.Sync(context.Background())
	if got := s.names(); !slices.Equal(got, []string{"probe:other"}) {
		t.Fatalf("running %v", got)
	}
	if n := strings.Count(logs.String(), "name of a configured collector"); n != 1 || !strings.Contains(logs.String(), "ozy.check.probe.name") {
		t.Fatalf("logged %d times:\n%s", n, logs.String())
	}
}

// Discovered instances say so, so a check can leave out the tags that
// carry its container's (changing) address.
func TestInstance_IsMarkedDiscovered(t *testing.T) {
	d, _, _, _, _ := setup(t, Options{})
	var discovered bool
	d.opts.Checks = collector.Registry{"probe": func(inst collector.Instance) (collector.Collector, error) {
		discovered = inst.Discovered
		return &probe{}, nil
	}}
	if _, err := build(d, container("a1", "r", nil), "probe", map[string]string{"port": "1"}); err != nil || !discovered {
		t.Fatalf("discovered = %v, %v", discovered, err)
	}
}
