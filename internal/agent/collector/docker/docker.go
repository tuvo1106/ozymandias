package docker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/agenttags"
	"github.com/tuvo1106/ozymandias/internal/agent/collector"
	"github.com/tuvo1106/ozymandias/internal/agent/collector/dockerapi"
	"github.com/tuvo1106/ozymandias/internal/clock"
)

// API is the part of the Docker Engine API the collector uses.
// *dockerapi.Client implements it; tests use a fake.
type API interface {
	ListContainers(ctx context.Context) ([]dockerapi.Container, error)
	Stats(ctx context.Context, id string) (dockerapi.Stats, error)
	Inspect(ctx context.Context, id string) (dockerapi.ContainerJSON, error)
	Events(ctx context.Context, since time.Time, fn func(dockerapi.Event) error, skipped func(error)) error
}

// DefaultMaxConcurrency bounds stats (and first-sight inspect) requests in
// flight. One-shot stats answer at once, but each is still a cgroup read in
// the daemon; a hundred at once would be a hundred goroutines there.
const DefaultMaxConcurrency = 8

// Options configures the collector.
type Options struct {
	API API
	// Interval is how often to run; zero means the scheduler's default.
	Interval       time.Duration
	MaxConcurrency int // default DefaultMaxConcurrency
	Rewrites       []Rewrite
	Clock          clock.Clock // default clock.Real()
}

// Collector reports every running container's resource use (container.*)
// and the number running per image (docker.containers.running).
type Collector struct {
	api   API
	iv    time.Duration
	conc  int
	tag   tagger
	clock clock.Clock
	rates *collector.Rates
	// prev is each container's previous CPU sample: one-shot stats carry
	// one sample, and CPU % needs two. Only CPUStats is kept; the rest of a
	// sample (memory breakdown, networks, block io) is not needed again.
	prev map[string]dockerapi.Stats
	// images are the images counted last run, so one whose last container
	// stopped reads 0 once rather than just stopping.
	images map[string]bool

	// startedMu guards started, which the event watcher also touches.
	startedMu sync.Mutex
	// started caches each container's start time, for container.uptime.
	// The list endpoint gives only the creation time, which for a restarted
	// container is not when it started; so a container is inspected the
	// first time it is seen, and again after the watcher reports it started
	// (a restart in place keeps the id).
	started map[string]time.Time
	// restarts counts start events per container, so an inspect already in
	// flight when one arrives does not cache the start time it replaced.
	restarts map[string]uint64
}

// ContainerStarted forgets a container's cached start time, so the next
// run inspects it again. The event watcher calls it on every start event:
// a container restarted in place (restart: always, docker restart) keeps
// its id, and its uptime would otherwise keep counting from the first
// start — hiding exactly the crash loop uptime is read to find.
func (c *Collector) ContainerStarted(id string) {
	c.startedMu.Lock()
	defer c.startedMu.Unlock()
	delete(c.started, id)
	c.restarts[id]++
}

var _ collector.Collector = (*Collector)(nil)

// New returns a Docker collector.
func New(opts Options) *Collector {
	if opts.MaxConcurrency <= 0 {
		opts.MaxConcurrency = DefaultMaxConcurrency
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real()
	}
	return &Collector{
		api: opts.API, iv: opts.Interval, conc: opts.MaxConcurrency,
		tag: tagger{rewrites: opts.Rewrites}, clock: opts.Clock,
		rates: collector.NewRates(), started: map[string]time.Time{}, restarts: map[string]uint64{},
		prev: map[string]dockerapi.Stats{}, images: map[string]bool{},
	}
}

// Name implements [collector.Collector].
func (c *Collector) Name() string { return "docker" }

// Interval implements [collector.Collector].
func (c *Collector) Interval() time.Duration { return c.iv }

// additive says how two containers that end up with the same tags combine
// within one run — which happens exactly when a Rewrite folds several into
// one name. Most container metrics are amounts, and the folded series is
// their total (all judge sandboxes together used this much memory). The two
// that are not amounts take the largest: uptime (the oldest), and the
// memory limit (a limit does not add up across containers).
func additive(name string) bool {
	return name != "container.uptime" && name != "container.memory.limit"
}

// run accumulates one run's metrics, combining same-tagged containers.
type run struct {
	mu   sync.Mutex
	keys []string
	out  map[string]*collector.Metric
}

// add keys on the tags as the store will see them — normalized, sorted and
// without duplicates — so two containers that differ only in, say, the case
// of a name are combined here rather than sent as two points for one series,
// of which the store would keep only the last.
func (r *run) add(m collector.Metric) {
	m.Tags = agenttags.Normalize(m.Tags)
	slices.Sort(m.Tags)
	m.Tags = slices.Compact(m.Tags)
	key := m.Name + "\x00" + strings.Join(m.Tags, ",")
	r.mu.Lock()
	defer r.mu.Unlock()
	prev, ok := r.out[key]
	switch {
	case !ok:
		r.out[key] = &m
		r.keys = append(r.keys, key)
	case additive(m.Name):
		prev.Value += m.Value
	default:
		prev.Value = max(prev.Value, m.Value)
	}
}

// Collect lists the running containers, reads each one's stats with bounded
// concurrency, and emits them. A container that stops between the list and
// its stats call (404, or an empty sample) is skipped silently: that is the
// normal race, not a failure. Any other per-container error is returned
// after the rest are emitted.
func (c *Collector) Collect(ctx context.Context, emit collector.Emit) error {
	list, err := c.api.ListContainers(ctx)
	if err != nil {
		return err
	}
	now := c.clock.Now()
	defer c.rates.Sweep(now)

	res := &run{out: map[string]*collector.Metric{}}
	perImage := map[string]int{}
	for _, ct := range list {
		img, _ := dockerapi.ParseImage(ct.Image)
		perImage[img]++
	}
	for img := range c.images {
		if perImage[img] == 0 {
			// Its last container stopped: say 0 once, rather than leave the
			// last count standing until the series goes stale.
			emit(collector.Metric{Name: "docker.containers.running", Value: 0, Tags: []string{"image_name:" + img}})
		}
	}
	c.images = map[string]bool{}
	for img, n := range perImage {
		c.images[img] = true
		emit(collector.Metric{Name: "docker.containers.running", Value: float64(n), Tags: []string{"image_name:" + img}})
	}

	// Every listed container is live, including any a timed-out run did
	// not reach: their previous samples must survive to the next run.
	live := make(map[string]bool, len(list))
	for _, ct := range list {
		live[ct.ID] = true
	}
	var (
		wg   sync.WaitGroup
		sem  = make(chan struct{}, c.conc)
		emu  sync.Mutex
		errs []error
	)
	for _, ct := range list {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break // out of time: what was read is still sent
		}
		wg.Go(func() {
			defer func() { <-sem }()
			started, known := c.startTime(ctx, ct.ID)
			if err := c.one(ctx, ct, started, known, now, res); err != nil {
				emu.Lock()
				errs = append(errs, err)
				emu.Unlock()
			}
		})
	}
	wg.Wait()
	c.startedMu.Lock()
	for id := range c.started {
		if !live[id] {
			delete(c.started, id)
		}
	}
	for id := range c.restarts {
		if !live[id] {
			delete(c.restarts, id)
		}
	}
	c.startedMu.Unlock()
	for id := range c.prev {
		if !live[id] {
			delete(c.prev, id)
		}
	}
	for _, k := range res.keys {
		emit(*res.out[k])
	}
	return summarize(errs)
}

// summarize keeps an error message bounded when every container fails the
// same way (the daemon is overloaded): the first error and a count.
func summarize(errs []error) error {
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	}
	return fmt.Errorf("%w (and %d more containers)", errs[0], len(errs)-1)
}

// startTime is a container's start time, from the cache or, the first time
// (and after a restart), from inspect — inside the caller's concurrency
// bound, so a run that meets fifty new containers does not inspect them one
// at a time first.
func (c *Collector) startTime(ctx context.Context, id string) (time.Time, bool) {
	c.startedMu.Lock()
	st, ok := c.started[id]
	gen := c.restarts[id]
	c.startedMu.Unlock()
	if ok {
		return st, true
	}
	st, err := c.startedAt(ctx, id)
	if err != nil {
		return time.Time{}, false
	}
	c.startedMu.Lock()
	defer c.startedMu.Unlock()
	if c.restarts[id] != gen {
		// It restarted while being inspected: st may be the old start.
		// Leave the cache empty so the next run inspects again.
		return time.Time{}, false
	}
	c.started[id] = st
	return st, true
}

func (c *Collector) startedAt(ctx context.Context, id string) (time.Time, error) {
	j, err := c.api.Inspect(ctx, id)
	if err != nil {
		return time.Time{}, err
	}
	if j.State.StartedAt.IsZero() {
		return time.Time{}, errors.New("no start time")
	}
	return j.State.StartedAt, nil
}

// one reads a container's stats and adds its metrics to res. The rates
// tracker is not safe for concurrent use, so rates are computed under res's
// lock.
func (c *Collector) one(ctx context.Context, ct dockerapi.Container, started time.Time, known bool, now time.Time, res *run) error {
	s, err := c.api.Stats(ctx, ct.ID)
	if errors.Is(err, dockerapi.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", ct.Name(), err)
	}
	if !s.Sampled() {
		return nil // stopped between the list and the stats call
	}
	tags := c.tag.tags(ct.Name(), ct.ID, ct.Image, ct.Labels)
	gauge := func(name string, v float64) {
		res.add(collector.Metric{Name: name, Kind: collector.Gauge, Value: v, Tags: tags})
	}
	rate := func(name string, v uint64, ok bool) {
		if !ok {
			return // the daemon has no such counter for it: unknown, not 0
		}
		res.mu.Lock()
		r, ok := c.rates.Observe(ct.ID+"\x00"+name, float64(v), now)
		res.mu.Unlock()
		if ok {
			res.add(collector.Metric{Name: name, Kind: collector.Rate, Value: r, Tags: tags})
		}
	}
	res.mu.Lock()
	prev, hasPrev := c.prev[ct.ID]
	c.prev[ct.ID] = dockerapi.Stats{CPUStats: s.CPUStats}
	res.mu.Unlock()
	if hasPrev {
		if pct, ok := dockerapi.CPUPercentBetween(prev, s); ok {
			gauge("container.cpu.usage", pct)
		}
	}
	rate("container.cpu.throttled", s.CPUStats.ThrottlingData.ThrottledPeriods, true)
	if m, ok := s.MemoryStats.Breakdown(); ok {
		gauge("container.memory.usage", float64(m.Usage))
		gauge("container.memory.limit", float64(m.Limit))
		gauge("container.memory.rss", float64(m.RSS))
		gauge("container.memory.cache", float64(m.Cache))
	}
	rx, tx, netOK := s.NetworkBytes()
	rate("container.net.rx_bytes", rx, netOK)
	rate("container.net.tx_bytes", tx, netOK)
	rd, wr, ioOK := s.BlockIO()
	rate("container.io.read_bytes", rd, ioOK)
	rate("container.io.write_bytes", wr, ioOK)
	if s.PidsStats.Current > 0 {
		// A running container has at least one process; 0 means the daemon
		// sent no pids_stats (no pids cgroup controller): unknown, not none.
		gauge("container.pids", float64(s.PidsStats.Current))
	}
	if known && !now.Before(started) {
		gauge("container.uptime", now.Sub(started).Seconds())
	}
	return nil
}
