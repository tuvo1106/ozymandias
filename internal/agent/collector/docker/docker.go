package docker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/agenttags"
	"github.com/tuvo1106/ozymandias/internal/agent/collector"
	"github.com/tuvo1106/ozymandias/internal/agent/collector/dockerapi"
	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// API is the part of the Docker Engine API the collector uses.
// *dockerapi.Client implements it; tests use a fake.
type API interface {
	ListContainers(ctx context.Context) ([]dockerapi.Container, error)
	Stats(ctx context.Context, id string) (dockerapi.Stats, error)
	Inspect(ctx context.Context, id string) (dockerapi.ContainerJSON, error)
	Events(ctx context.Context, since time.Time, fn func(dockerapi.Event) error, skipped func(error)) error
	Now(ctx context.Context) (time.Time, error)
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
	// images are the images counted last run, so one whose last container
	// stopped reads 0 once rather than just stopping.
	images map[string]bool
	// seen is what runs keep about each listed container: its tags and the
	// baselines rates and CPU % are measured from. Guarded by the run's
	// lock; forgotten when the container leaves the list.
	seen map[string]*seen

	// livesMu guards lives, which the event watcher also touches.
	livesMu sync.Mutex
	lives   map[string]*life
}

// life is a container's lifecycle as the collector knows it.
//
// The list endpoint gives only a container's creation time, which for a
// restarted container is not when it started; so a container is inspected
// the first time it is seen, and its start cached. A container restarted in
// place (restart: always, docker restart) keeps its id, so the event
// watcher reports every start (ContainerStarted), which forgets the cached
// time and moves gen on: a sample taken in one generation is not compared
// with one from another, whose counters began again.
type life struct {
	started time.Time // zero until inspected
	gen     uint64    // start events seen
}

// seen is what runs keep about one container between runs.
type seen struct {
	// tags and tagKey are computed once: a container's name, image and
	// labels are fixed for its life. tagKey is the tags joined, as
	// run.add combines on.
	tags   []string
	tagKey string
	// cpu is the previous CPU sample (one-shot stats carry one, and CPU %
	// needs two), taken in restart generation gen, like the container's
	// rate readings.
	cpu    dockerapi.CPUStats
	hasCPU bool
	gen    uint64
}

// ContainerStarted forgets a container's cached start time and moves its
// generation on, so the next run inspects it again and measures from new
// baselines. The event watcher calls it on every start event; without it,
// uptime would keep counting from the first start — hiding exactly the
// crash loop uptime is read to find.
func (c *Collector) ContainerStarted(id string) {
	c.livesMu.Lock()
	defer c.livesMu.Unlock()
	l := c.life(id)
	l.started = time.Time{}
	l.gen++
}

// life returns id's entry, adding it. The caller holds livesMu.
func (c *Collector) life(id string) *life {
	l := c.lives[id]
	if l == nil {
		l = &life{}
		c.lives[id] = l
	}
	return l
}

// generation is id's restart generation now.
func (c *Collector) generation(id string) uint64 {
	c.livesMu.Lock()
	defer c.livesMu.Unlock()
	if l := c.lives[id]; l != nil {
		return l.gen
	}
	return 0
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
		rates: collector.NewRates(), images: map[string]bool{},
		seen: map[string]*seen{}, lives: map[string]*life{},
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

// add combines m with any metric of the same name and tags. tagKey is m's
// tags joined, in the form the store will see them (canonicalTags), so two
// containers that differ only in, say, the case of a name are combined here
// rather than sent as two points for one series, of which the store would
// keep only the last. The caller computes it once per container.
func (r *run) add(m collector.Metric, tagKey string) {
	key := m.Name + "\x00" + tagKey
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
			started, known, gen := c.startTime(ctx, ct.ID)
			if err := c.one(ctx, ct, started, known, gen, now, res); err != nil {
				emu.Lock()
				errs = append(errs, err)
				emu.Unlock()
			}
		})
	}
	wg.Wait()
	// What is kept about a container is forgotten when it leaves the list,
	// not by age: rate readings carry the daemon's clock, which need not
	// agree with the agent's (a VM's clock after the host sleeps).
	c.livesMu.Lock()
	maps.DeleteFunc(c.lives, func(id string, _ *life) bool { return !live[id] })
	c.livesMu.Unlock()
	maps.DeleteFunc(c.seen, func(id string, _ *seen) bool { return !live[id] })
	c.rates.Forget(func(key string) bool {
		id, _, _ := strings.Cut(key, "\x00")
		return !live[id]
	})
	for _, k := range res.keys {
		emit(*res.out[k])
	}
	return summarize(errs)
}

// summarize keeps an error message bounded when every container fails the
// same way (the daemon is overloaded): one error and a count. The one is
// the first in sorted order, not the first to finish, so the same failures
// make the same message run after run — the scheduler logs a collector's
// error only when its text changes.
func summarize(errs []error) error {
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	}
	slices.SortFunc(errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
	return fmt.Errorf("%w (and %d more containers)", errs[0], len(errs)-1)
}

// startTime is a container's start time, from the cache or, the first time
// (and after a restart), from inspect — inside the caller's concurrency
// bound, so a run that meets fifty new containers does not inspect them one
// at a time first. gen is the restart generation the answer belongs to,
// read with it: the caller measures in that generation.
func (c *Collector) startTime(ctx context.Context, id string) (started time.Time, known bool, gen uint64) {
	c.livesMu.Lock()
	l := c.life(id)
	started, gen = l.started, l.gen
	c.livesMu.Unlock()
	if !started.IsZero() {
		return started, true, gen
	}
	st, err := c.startedAt(ctx, id)
	if err != nil {
		return time.Time{}, false, gen
	}
	c.livesMu.Lock()
	defer c.livesMu.Unlock()
	if l := c.lives[id]; l == nil || l.gen != gen {
		// It restarted while being inspected: st may be the old start.
		// Leave the cache empty so the next run inspects again.
		return time.Time{}, false, gen
	}
	c.lives[id].started = st
	return st, true, gen
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
func (c *Collector) one(ctx context.Context, ct dockerapi.Container, started time.Time, known bool, gen uint64, now time.Time, res *run) error {
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
	if c.generation(ct.ID) != gen {
		// A start event arrived while this was read: the sample may be from
		// either life. Skip it; the next run starts from new baselines.
		return nil
	}
	res.mu.Lock()
	sn := c.seen[ct.ID]
	if sn == nil {
		// Normalized and canonical once, for all of this container's
		// metrics and every run: the key run.add combines on, and a slice
		// they can share (the scheduler copies tags before it decorates
		// them).
		tags := wire.CanonicalTags(agenttags.Normalize(c.tag.tags(ct.Name(), ct.ID, ct.Image, ct.Labels)))
		sn = &seen{tags: tags, tagKey: strings.Join(tags, ","), gen: gen}
		c.seen[ct.ID] = sn
	}
	if sn.gen != gen {
		// Restarted in place since the last sample: its counters began
		// again, so nothing is measured from the old life's readings.
		sn.hasCPU, sn.gen = false, gen
		prefix := ct.ID + "\x00"
		c.rates.Forget(func(key string) bool { return strings.HasPrefix(key, prefix) })
	}
	prev, hasPrev := sn.cpu, sn.hasCPU
	sn.cpu, sn.hasCPU = s.CPUStats, true
	res.mu.Unlock()
	tags, tagKey := sn.tags, sn.tagKey
	gauge := func(name string, v float64) {
		res.add(collector.Metric{Name: name, Kind: collector.Gauge, Value: v, Tags: tags}, tagKey)
	}
	// Rates and uptime are timed by when the daemon took the sample, not by
	// when the run began: calls queue behind MaxConcurrency, so on a busy
	// daemon a container late in the list is sampled seconds after the
	// run's start, by a different amount each run. The daemon's clock is
	// also the one its start times are in, so uptime does not carry the
	// skew between the daemon and the agent.
	at := now
	if !s.Read.IsZero() {
		at = s.Read
	}
	rate := func(name string, v uint64, ok bool) {
		if !ok {
			return // the daemon has no such counter for it: unknown, not 0
		}
		res.mu.Lock()
		r, ok := c.rates.Observe(ct.ID+"\x00"+name, float64(v), at)
		res.mu.Unlock()
		if ok {
			res.add(collector.Metric{Name: name, Kind: collector.Rate, Value: r, Tags: tags}, tagKey)
		}
	}
	if hasPrev {
		if pct, ok := dockerapi.CPUPercentBetween(dockerapi.Stats{CPUStats: prev}, s); ok {
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
	if known && !at.Before(started) {
		gauge("container.uptime", at.Sub(started).Seconds())
	}
	return nil
}
