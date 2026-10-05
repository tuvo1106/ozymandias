package tailer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/collector/dockerapi"
	"github.com/tuvo1106/ozymandias/internal/agent/logpipeline"
	"github.com/tuvo1106/ozymandias/internal/clock"
)

// Labels a container sets to configure its own logs, so an app the agent has
// never heard of gets its logs in without anyone editing the agent's config.
const (
	LabelLogsEnabled   = "ozy.logs.enabled"
	LabelLogsSource    = "ozy.logs.source"
	LabelLogsService   = "ozy.logs.service"
	LabelLogsMultiline = "ozy.logs.multiline_start"
	LabelLogsTags      = "ozy.logs.tags"
	labelService       = "ozy.service" // the metrics label, reused as the service name
	labelComposeProj   = "com.docker.compose.project"
	labelComposeSvc    = "com.docker.compose.service"
)

// DockerAPI is what the container tailer needs of the daemon.
type DockerAPI interface {
	ListContainers(ctx context.Context) ([]dockerapi.Container, error)
	Inspect(ctx context.Context, id string) (dockerapi.ContainerJSON, error)
	Logs(ctx context.Context, id string, since time.Time, follow bool) (io.ReadCloser, error)
}

// DockerSource is one `type: docker` source.
type DockerSource struct {
	// IncludeLabels selects containers: each entry is "key" (label present) or
	// "key=value"; a container matches if any entry does. Empty selects none
	// unless CollectAll or the container opts in by label.
	IncludeLabels []string
	// ExcludeNames are regular expressions on the container name; a match is
	// never tailed (judge sandboxes, whose stdout is a user's program output).
	ExcludeNames []string
	Service      string
	Source       string
	Tags         []string
	// MultilineStart begins an event.
	MultilineStart string
	Pipeline       logpipeline.Spec
	// StartPosition is where a container found at agent start with no registry
	// entry begins: "end" (default) or "beginning". A container that starts
	// after the agent always begins at its first line.
	StartPosition string
}

// DockerOptions are the container tailer's dependencies and global settings.
type DockerOptions struct {
	API      DockerAPI
	Registry *Registry
	Sink     Sink
	Host     string
	Clock    clock.Clock
	Logger   *slog.Logger

	ScanInterval time.Duration
	BatchLogs    int
	// CollectAll tails every container not excluded, choosing the pipeline by
	// looking at the line (JSON or plain). Containers can still opt out with
	// ozy.logs.enabled=false.
	CollectAll bool
	// Totals receives every container pipeline's counters.
	Totals *logpipeline.Totals
	// Backoff bounds for reopening a broken log stream.
	BackoffMin, BackoffMax time.Duration
}

// DockerStats counts the container tailer's work.
type DockerStats struct {
	Containers, Reconnects, SendErrors, TruncatedLines int64
}

// Docker follows the log streams of running containers.
//
// One goroutine per container owns that container's stream end to end (read,
// reassemble, join, process, send, commit), so a slow or failing container
// cannot hold up another. The registry keeps, per container, the timestamp of
// the last log line the intake acknowledged; reconnecting asks the daemon for
// everything since then and drops lines at or before it, so a reconnect costs
// no duplicates and a restart of the agent costs at most the lines in flight.
type Docker struct {
	opts    DockerOptions
	sources []*dockerSource

	// listFailing is set while the daemon cannot be listed, so an absent Docker
	// is one warning, not one per scan.
	listFailing atomic.Bool

	mu      sync.Mutex
	conts   map[string]*cont
	started bool
	wg      sync.WaitGroup
	ctx     context.Context

	nReconnects, nSendErrors, nTruncated atomic.Int64
}

type dockerSource struct {
	cfg     DockerSource
	exclude []*regexp.Regexp
	start   *regexp.Regexp
}

type cont struct {
	id, name string
	cancel   context.CancelFunc
	done     chan struct{}
}

// NewDocker builds the container tailer.
func NewDocker(sources []DockerSource, opts DockerOptions) (*Docker, error) {
	if opts.Clock == nil {
		opts.Clock = clock.Real()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.ScanInterval <= 0 {
		opts.ScanInterval = DefaultScanInterval
	}
	if opts.BackoffMin <= 0 {
		opts.BackoffMin = time.Second
	}
	if opts.BackoffMax <= 0 {
		opts.BackoffMax = 30 * time.Second
	}
	if opts.Registry == nil {
		opts.Registry, _ = OpenRegistry("")
	}
	d := &Docker{opts: opts, conts: map[string]*cont{}}
	for i, s := range sources {
		ds := &dockerSource{cfg: s}
		for _, ex := range s.ExcludeNames {
			re, err := regexp.Compile(ex)
			if err != nil {
				return nil, fmt.Errorf("docker source %d: exclude_names %q: %w", i, ex, err)
			}
			ds.exclude = append(ds.exclude, re)
		}
		if s.MultilineStart != "" {
			re, err := regexp.Compile(s.MultilineStart)
			if err != nil {
				return nil, fmt.Errorf("docker source %d: multiline start: %w", i, err)
			}
			ds.start = re
		}
		d.sources = append(d.sources, ds)
	}
	return d, nil
}

// Stats returns the counters so far.
func (d *Docker) Stats() DockerStats {
	d.mu.Lock()
	n := int64(len(d.conts))
	d.mu.Unlock()
	return DockerStats{Containers: n, Reconnects: d.nReconnects.Load(), SendErrors: d.nSendErrors.Load(), TruncatedLines: d.nTruncated.Load()}
}

// Run scans for containers until ctx is done, then waits for the followers to
// flush and stop.
func (d *Docker) Run(ctx context.Context) {
	d.mu.Lock()
	d.ctx = ctx
	d.mu.Unlock()
	t := d.opts.Clock.NewTicker(d.opts.ScanInterval)
	defer t.Stop()
	d.Scan(ctx)
	for {
		select {
		case <-ctx.Done():
			d.wg.Wait()
			if err := d.opts.Registry.Flush(); err != nil {
				d.opts.Logger.Warn("tailer: writing registry on shutdown", "err", err)
			}
			return
		case <-t.C():
			d.Scan(ctx)
		}
	}
}

// Scan lists containers and starts or stops followers to match.
func (d *Docker) Scan(ctx context.Context) {
	list, err := d.opts.API.ListContainers(ctx)
	if err != nil {
		if ctx.Err() == nil && !d.listFailing.Swap(true) {
			d.opts.Logger.Warn("tailer: listing containers (reported once until it works again)", "err", err)
		}
		return
	}
	d.listFailing.Store(false)
	d.opts.Registry.Prune(d.opts.Clock.Now().Add(-RegistryTTL))
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctx == nil {
		d.ctx = ctx
	}
	running := map[string]bool{}
	for _, c := range list {
		running[c.ID] = true
		if cur := d.conts[c.ID]; cur != nil {
			select {
			case <-cur.done: // ended (the container stopped, or the stream broke for good): follow again
				delete(d.conts, c.ID)
			default:
				continue
			}
		}
		cfg, ok := d.configFor(c)
		if !ok {
			continue
		}
		d.startLocked(c, cfg)
	}
	for id, cur := range d.conts {
		if !running[id] {
			select {
			case <-cur.done:
				delete(d.conts, id)
			default:
				// Not listed any more but still draining its last lines: let it finish.
			}
		}
	}
	d.started = true
	if err := d.opts.Registry.Flush(); err != nil {
		d.opts.Logger.Warn("tailer: writing registry", "err", err)
	}
}

// containerConfig is what a follower needs, resolved from a source plus labels.
type containerConfig struct {
	service, source string
	tags            []string
	start           *regexp.Regexp
	spec            logpipeline.Spec
	startPosition   string
}

// configFor decides whether to tail c and how. A container's own labels win
// over the source that selected it; ozy.logs.enabled=false wins over everything.
func (d *Docker) configFor(c dockerapi.Container) (containerConfig, bool) {
	name := c.Name()
	labels := c.Labels
	if v, ok := labels[LabelLogsEnabled]; ok && isFalse(v) {
		return containerConfig{}, false
	}
	optIn := false
	if v, ok := labels[LabelLogsEnabled]; ok && !isFalse(v) {
		optIn = true
	}
	var src *dockerSource
	for _, s := range d.sources {
		if excluded(s, name) {
			continue
		}
		if matchesLabels(s.cfg.IncludeLabels, labels) {
			src = s
			break
		}
	}
	if src == nil && !optIn && !d.opts.CollectAll {
		return containerConfig{}, false
	}
	cc := containerConfig{}
	if src != nil {
		cc.service, cc.source, cc.start, cc.spec = src.cfg.Service, src.cfg.Source, src.start, src.cfg.Pipeline
		cc.tags = append(cc.tags, src.cfg.Tags...)
		cc.startPosition = src.cfg.StartPosition
	} else {
		for _, s := range d.sources {
			if excluded(s, name) {
				return containerConfig{}, false // an exclusion applies to opted-in containers too
			}
		}
	}
	if v := labels[LabelLogsSource]; v != "" {
		cc.source = v
	}
	if v := labels[LabelLogsService]; v != "" {
		cc.service = v
	}
	if v := labels[LabelLogsMultiline]; v != "" {
		if re, err := regexp.Compile(v); err == nil {
			cc.start = re
		} else {
			d.opts.Logger.Warn("tailer: bad "+LabelLogsMultiline+" label", "container", name, "err", err)
		}
	}
	if v := labels[LabelLogsTags]; v != "" {
		for _, t := range strings.Split(v, ",") {
			if t = strings.TrimSpace(t); t != "" {
				cc.tags = append(cc.tags, t)
			}
		}
	}
	if cc.service == "" {
		switch {
		case labels[labelService] != "":
			cc.service = labels[labelService]
		case labels[labelComposeProj] != "" && labels[labelComposeSvc] != "":
			cc.service = labels[labelComposeProj] + "-" + labels[labelComposeSvc]
		default:
			cc.service = name
		}
	}
	if cc.source == "" {
		cc.source = "json"
	}
	cc.spec.Source = firstNonEmpty(cc.spec.Source, cc.source)
	return cc, true
}

func excluded(s *dockerSource, name string) bool {
	for _, re := range s.exclude {
		if re.MatchString(name) {
			return true
		}
	}
	return false
}

func matchesLabels(want []string, have map[string]string) bool {
	for _, w := range want {
		k, v, hasV := strings.Cut(w, "=")
		got, ok := have[k]
		if ok && (!hasV || got == v) {
			return true
		}
	}
	return false
}

func isFalse(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "false", "0", "no", "off":
		return true
	}
	return false
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (d *Docker) startLocked(c dockerapi.Container, cc containerConfig) {
	ctx, cancel := context.WithCancel(d.ctx)
	ct := &cont{id: c.ID, name: c.Name(), cancel: cancel, done: make(chan struct{})}
	d.conts[c.ID] = ct
	first := !d.started
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer close(ct.done)
		d.follow(ctx, ct, cc, first)
	}()
}

// follow is one container's goroutine.
func (d *Docker) follow(ctx context.Context, c *cont, cc containerConfig, atAgentStart bool) {
	key := "docker:" + c.id
	pl, err := logpipeline.New(cc.spec, logpipeline.Options{Clock: d.opts.Clock, Totals: d.opts.Totals})
	if err != nil {
		d.opts.Logger.Error("tailer: building pipeline", "container", c.name, "err", err)
		return
	}
	meta := logpipeline.Meta{Service: cc.service, Source: cc.source, Host: d.opts.Host, Tags: cc.tags}
	st := newStream(pl, meta, cc.start, d.opts.Sink, d.opts.BatchLogs)

	var committed int64
	if e, ok := d.opts.Registry.Get(key); ok {
		committed = e.TS
	} else if atAgentStart && cc.startPosition != "beginning" {
		committed = d.opts.Clock.Now().UnixNano() // do not replay history that predates the agent
	}
	tty := false
	if ins, err := d.opts.API.Inspect(ctx, c.id); err == nil {
		tty = ins.Config.Tty
	} else if errors.Is(err, dockerapi.ErrNotFound) {
		return
	}

	backoff := d.opts.BackoffMin
	for ctx.Err() == nil {
		var since time.Time
		if committed > 0 {
			since = time.Unix(0, committed)
		}
		rc, err := d.opts.API.Logs(ctx, c.id, since, true)
		if err != nil {
			if errors.Is(err, dockerapi.ErrNotFound) || ctx.Err() != nil {
				return
			}
			d.opts.Logger.Warn("tailer: opening container logs", "container", c.name, "err", err)
			if !d.sleep(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, d.opts.BackoffMax)
			continue
		}
		var ended bool
		before := committed
		committed, ended, err = d.pump(ctx, rc, tty, st, committed, c, key)
		if committed != before {
			backoff = d.opts.BackoffMin // it worked: the next break starts from the short wait
		}
		_ = rc.Close()
		if ended {
			return // the container stopped; a restart is found by the next scan
		}
		if ctx.Err() != nil {
			return
		}
		d.nReconnects.Add(1)
		d.opts.Logger.Warn("tailer: container log stream broke, reconnecting", "container", c.name, "err", err)
		if !d.sleep(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, d.opts.BackoffMax)
	}
}

func (d *Docker) sleep(ctx context.Context, dur time.Duration) bool {
	t := d.opts.Clock.NewTimer(dur)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C():
		return true
	}
}

// pump reads one open stream until it ends or breaks. It returns the committed
// timestamp, whether the stream ended cleanly (the container stopped), and the
// error if it broke.
func (d *Docker) pump(ctx context.Context, rc io.Reader, tty bool, st *stream, committed int64, c *cont, key string) (int64, bool, error) {
	type batch struct {
		lines []dockerLine
		err   error
	}
	ch := make(chan batch, 64)
	stop := make(chan struct{})
	defer close(stop) // frees the reader if we return while it is blocked sending
	dm := newDemuxer(rc, tty)
	go func() {
		defer close(ch)
		for {
			lines, err := dm.next()
			select {
			case ch <- batch{lines, err}:
			case <-ctx.Done():
				return
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	tick := d.opts.Clock.NewTicker(MultilineFlushAfter / 4)
	defer tick.Stop()

	commit := func(events []event, now time.Time) error {
		end, err := st.deliver(ctx, events, committed, now, func(end int64) {
			committed = end
			d.opts.Registry.Set(key, Entry{Path: c.name, TS: committed, LastSeen: now.Unix()})
			if err := d.opts.Registry.Flush(); err != nil {
				d.opts.Logger.Warn("tailer: writing registry", "err", err)
			}
		})
		committed = end
		d.opts.Registry.Set(key, Entry{Path: c.name, TS: committed, LastSeen: now.Unix()})
		if err != nil {
			d.nSendErrors.Add(1)
			st.ml.reset()
		}
		return err
	}
	for {
		select {
		case <-ctx.Done():
			// Shutdown: deliver what is pending (with a context that is not cancelled).
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			end, err := st.deliver(sctx, st.ml.flush(d.opts.Clock.Now(), true), committed, d.opts.Clock.Now(), nil)
			cancel()
			if err == nil {
				committed = end
			}
			d.opts.Registry.Set(key, Entry{Path: c.name, TS: committed, LastSeen: d.opts.Clock.Now().Unix()})
			return committed, false, ctx.Err()
		case <-tick.C():
			now := d.opts.Clock.Now()
			if err := commit(st.ml.flush(now, false), now); err != nil {
				return committed, false, err
			}
		case b, ok := <-ch:
			if !ok {
				return committed, true, nil
			}
			now := d.opts.Clock.Now()
			var events []event
			for _, l := range b.lines {
				if l.ts != 0 && l.ts <= committed {
					continue // replayed by the daemon after a reconnect
				}
				if l.truncated {
					d.nTruncated.Add(1)
				}
				events = append(events, st.ml.add(l.text, l.ts, now, l.stderr)...)
			}
			if err := commit(events, now); err != nil {
				return committed, false, err
			}
			if b.err != nil {
				if errors.Is(b.err, io.EOF) {
					now = d.opts.Clock.Now()
					if err := commit(st.ml.flush(now, true), now); err != nil {
						return committed, false, err
					}
					return committed, true, nil
				}
				return committed, false, b.err
			}
		}
	}
}
