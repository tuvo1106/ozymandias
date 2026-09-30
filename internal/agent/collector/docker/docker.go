package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

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
	Events(ctx context.Context, since time.Time, fn func(dockerapi.Event) error) error
}

// DefaultMaxConcurrency bounds stats requests in flight. With stream=false
// the daemon holds each for about a second while it takes its second CPU
// sample, so one at a time would take a minute for sixty containers; a
// hundred at once would be a hundred goroutines in the daemon.
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
	// started caches each container's start time, for container.uptime.
	// The list endpoint gives only the creation time, which for a restarted
	// container is not when it started; so a container is inspected once,
	// the first time it is seen.
	started map[string]time.Time
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
		rates: collector.NewRates(), started: map[string]time.Time{},
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

func (r *run) add(m collector.Metric) {
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
	for img, n := range perImage {
		emit(collector.Metric{Name: "docker.containers.running", Value: float64(n), Tags: []string{"image_name:" + img}})
	}

	live := make(map[string]bool, len(list))
	var (
		wg   sync.WaitGroup
		sem  = make(chan struct{}, c.conc)
		emu  sync.Mutex
		errs []error
	)
	for _, ct := range list {
		live[ct.ID] = true
		started, known := c.started[ct.ID]
		if !known {
			if st, err := c.startedAt(ctx, ct.ID); err == nil {
				c.started[ct.ID] = st
				started, known = st, true
			}
		}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := c.one(ctx, ct, started, known, now, res); err != nil {
				emu.Lock()
				errs = append(errs, err)
				emu.Unlock()
			}
		})
	}
	wg.Wait()
	for id := range c.started {
		if !live[id] {
			delete(c.started, id)
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
	rate := func(name string, v uint64) {
		res.mu.Lock()
		r, ok := c.rates.Observe(ct.ID+"\x00"+name, float64(v), now)
		res.mu.Unlock()
		if ok {
			res.add(collector.Metric{Name: name, Kind: collector.Rate, Value: r, Tags: tags})
		}
	}
	if pct, ok := dockerapi.CPUPercent(s); ok {
		gauge("container.cpu.usage", pct)
	}
	rate("container.cpu.throttled", s.CPUStats.ThrottlingData.ThrottledPeriods)
	if m, ok := s.MemoryStats.Breakdown(); ok {
		gauge("container.memory.usage", float64(m.Usage))
		gauge("container.memory.limit", float64(m.Limit))
		gauge("container.memory.rss", float64(m.RSS))
		gauge("container.memory.cache", float64(m.Cache))
	}
	rx, tx := s.NetworkBytes()
	rate("container.net.rx_bytes", rx)
	rate("container.net.tx_bytes", tx)
	rd, wr := s.BlockIO()
	rate("container.io.read_bytes", rd)
	rate("container.io.write_bytes", wr)
	gauge("container.pids", float64(s.PidsStats.Current))
	if known && !now.Before(started) {
		gauge("container.uptime", now.Sub(started).Seconds())
	}
	return nil
}
