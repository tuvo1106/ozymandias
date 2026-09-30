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

// sched records what is running.
type sched struct {
	mu      sync.Mutex
	running map[string]collector.Collector
}

func (s *sched) Add(c collector.Collector) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running[c.Name()] = c
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.running, c.Name())
	}
}

func (s *sched) get(name string) collector.Collector {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[name]
}

func (s *sched) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for n := range s.running {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

func container(id, name string, labels map[string]string) dockerapi.Container {
	return dockerapi.Container{
		ID: id + strings.Repeat("0", 64-len(id)), Names: []string{"/" + name}, Image: "redis:7", Labels: labels,
		Ports: []dockerapi.Port{{PrivatePort: 16379}, {PrivatePort: 6379}},
		NetworkSettings: dockerapi.NetworkSettings{Networks: map[string]dockerapi.EndpointSettings{
			"zeta": {IPAddress: "10.0.0.9"}, "alpha": {IPAddress: "172.18.0.4"},
		}},
	}
}

func setup(t *testing.T, opts Options) (*Discovery, *lister, *sched, *bytes.Buffer, *selfmetrics.Registry) {
	t.Helper()
	l, s := &lister{}, &sched{running: map[string]collector.Collector{}}
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
	if len(got) != 1 || !slices.Contains(got[0].Tags, "container_name:shop-redis-1") || !slices.Contains(got[0].Tags, "compose_project:shop") {
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
	_, err := d.instance(container("a1", "r", nil), "probe", map[string]string{
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

func TestTyped(t *testing.T) {
	for in, want := range map[string]any{
		"6379": 6379, "true": true, "text": "text", "a: b": "a: b", "": "", "[x": "[x",
	} {
		if got := typed(in); got != want {
			t.Errorf("typed(%q) = %#v, want %#v", in, got, want)
		}
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

// A name rewrite applies to the tags, not the instance name, which must
// stay unique per container.
func TestSync_RewritesTagsNotNames(t *testing.T) {
	d, l, s, _, _ := setup(t, Options{Rewrites: []docker.Rewrite{{Match: regexp.MustCompile(`^job-.*`), Replace: "job"}}})
	l.set([]dockerapi.Container{
		container("a1", "job-1", map[string]string{"ozy.check.probe.port": "1"}),
		container("b2", "job-2", map[string]string{"ozy.check.probe.port": "1"}),
	}, nil)
	d.Sync(context.Background())
	if got := s.names(); !slices.Equal(got, []string{"probe:job-1", "probe:job-2"}) {
		t.Fatalf("running %v", got)
	}
	var m collector.Metric
	_ = s.get("probe:job-1").Collect(context.Background(), func(x collector.Metric) { m = x })
	if !slices.Contains(m.Tags, "container_name:job") {
		t.Fatalf("tags %v", m.Tags)
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
