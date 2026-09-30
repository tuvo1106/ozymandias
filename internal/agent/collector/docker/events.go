package docker

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/collector/dockerapi"
	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
)

// SampleKind says how the aggregator is to treat an event-driven sample.
type SampleKind int

const (
	// CountSample adds Value to a count for the interval.
	CountSample SampleKind = iota
	// DistributionSample adds Value to a distribution (a sketch), so
	// percentiles come out at query time.
	DistributionSample
)

// Sample is one event-driven measurement. Events arrive one at a time and
// at any moment, which is what the statsd aggregator is built for — many
// samples per interval, bucketed — so the watcher hands samples to it
// rather than to the scheduler, whose collectors produce one value per
// series per run.
type Sample struct {
	Name  string
	Kind  SampleKind
	Value float64
	Tags  []string
}

// WatcherOptions configures a [Watcher].
type WatcherOptions struct {
	API      API
	Sink     func(Sample)
	Rewrites []Rewrite
	// OnStart, if set, is called with each started container's id: the
	// collector forgets its cached start time (Collector.ContainerStarted).
	OnStart  func(id string)
	Clock    clock.Clock           // default clock.Real()
	Registry *selfmetrics.Registry // default: a new registry
	Logger   *slog.Logger          // default slog.Default()
}

// Watcher follows the daemon's event stream for what polling misses: a
// container that lives for two seconds starts and dies between two 15s
// polls and is never listed, but its start and die are in the stream. On
// each die it reports
//
//   - container.exits, a count tagged exit_code and oom_killed, and
//   - container.lifetime, a distribution of seconds from start to die,
//     when the start was seen (or can be inspected).
type Watcher struct {
	api   API
	sink  func(Sample)
	tag   tagger
	clock clock.Clock
	log   *slog.Logger

	onStart                     func(id string)
	reconnects, events, skipped *selfmetrics.Counter

	// started and oom are keyed by container id and emptied on die; a
	// container whose die was missed (the agent was down) is forgotten
	// after maxTracked.
	started map[string]time.Time
	oom     map[string]bool
	last    dockerapi.Event // the newest event seen, by time: where a resume starts
	// atLast holds every event seen at last's time (action and container):
	// since is inclusive, so a resume delivers them all again, and several
	// can share a time — a daemon that sends only whole seconds, or a burst.
	atLast map[string]bool
	// began is when the first connection was attempted: the resume point
	// until an event has been seen, so a disconnect before the first event
	// still replays what happened meanwhile.
	began time.Time
}

// maxTracked bounds the started map against dies that never arrive.
const maxTracked = 10_000

// evictOldest forgets the older half of the tracked starts: the entries a
// missed die left behind are old, and the containers still running that
// lose their entry are the long-lived ones, whose die falls back to
// inspect. Clearing everything instead would also forget the churn of
// short-lived containers running now, whose --rm dies inspect cannot
// answer.
//
// The cut is by count, not by time, so it frees half even when many starts
// share one timestamp (a daemon that sends whole seconds, a burst).
func (w *Watcher) evictOldest() {
	ids := slices.Collect(maps.Keys(w.started))
	slices.SortFunc(ids, func(a, b string) int { return w.started[a].Compare(w.started[b]) })
	for _, id := range ids[:len(ids)/2] {
		delete(w.started, id)
		delete(w.oom, id)
	}
}

// NewWatcher returns a Watcher. Call Run to start it.
func NewWatcher(opts WatcherOptions) *Watcher {
	if opts.Clock == nil {
		opts.Clock = clock.Real()
	}
	if opts.Registry == nil {
		opts.Registry = selfmetrics.NewRegistry()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Watcher{
		api: opts.API, sink: opts.Sink, tag: tagger{rewrites: opts.Rewrites},
		clock: opts.Clock, log: opts.Logger.With("component", "collector", "collector", "docker-events"),
		reconnects: opts.Registry.Counter("ozy.agent.docker.events_reconnects"),
		events:     opts.Registry.Counter("ozy.agent.docker.events"),
		skipped:    opts.Registry.Counter("ozy.agent.docker.events_skipped"),
		onStart:    opts.OnStart,
		started:    map[string]time.Time{}, oom: map[string]bool{}, atLast: map[string]bool{},
	}
}

// Backoff bounds for reconnecting to the event stream.
const (
	minBackoff = time.Second
	maxBackoff = 30 * time.Second
)

// Run follows the stream until ctx is cancelled, reconnecting with backoff
// when it ends. A reconnect asks for events since the last one seen, so a
// dropped connection loses nothing the daemon still remembers; the events
// the resume replays are recognised and skipped.
//
// The daemon being unreachable (not running, socket not mounted) is logged
// once, not on every retry, like a failing collector.
func (w *Watcher) Run(ctx context.Context) {
	backoff := minBackoff
	var lastErr string
	for {
		connected := w.clock.Now()
		// The first attempt asks for events from now (zero); every later one
		// resumes from the last event seen, or, if none was, from the first
		// attempt, so what happened while disconnected is replayed. since is
		// on the daemon's clock, so the first attempt's time is the
		// daemon's too: taken from the agent's, a skewed VM clock would
		// replay events from before the watcher began, or skip some after.
		// Until the daemon has answered, there is no such time to resume
		// from, and nothing it could have sent was missed.
		var since time.Time
		switch {
		case w.last.Action != "":
			since = w.last.At()
		case !w.began.IsZero():
			since = w.began
		default:
			if t, err := w.api.Now(ctx); err == nil {
				// The daemon's Date is whole seconds, truncated: the true
				// time is up to a second later. Resuming from the truncated
				// second could replay a die a previous agent already
				// counted; rounding up can only skip what this stream
				// would have delivered before it dropped.
				w.began = t.Add(time.Second)
			}
		}
		err := w.api.Events(ctx, since, func(ev dockerapi.Event) error { return w.handle(ctx, ev) }, w.skip)
		if ctx.Err() != nil {
			return
		}
		w.reconnects.Inc()
		if err != nil && err.Error() != lastErr && !errors.Is(err, dockerapi.ErrStreamClosed) {
			w.log.Warn("docker event stream failed; retrying", "error", err)
			lastErr = err.Error()
		}
		// A stream that stayed up a while was healthy: start the backoff
		// over rather than waiting 30s after a routine daemon restart, and
		// forget the last error, so the same failure days later is logged.
		if w.clock.Now().Sub(connected) > maxBackoff {
			backoff = minBackoff
			lastErr = ""
		}
		t := w.clock.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C():
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// replayOf reports whether ev is one a resume delivers again (since is
// inclusive, so everything at the resume time comes back), and otherwise
// records it as seen.
//
// The resume point is the newest event by time, not the last delivered:
// the daemon stamps an event before it publishes it, so concurrent events
// (compose stopping several containers) can arrive out of order. An older
// event is counted and leaves the resume point alone — a resume from the
// newest time cannot deliver it again.
func (w *Watcher) replayOf(ev dockerapi.Event) bool {
	k := ev.Action + "\x00" + ev.Actor.ID
	switch {
	case w.last.Action != "" && ev.At().Before(w.last.At()):
		return false
	case w.last.Action != "" && ev.At().Equal(w.last.At()):
		if w.atLast[k] {
			return true
		}
	default:
		clear(w.atLast)
		w.last = ev
	}
	w.atLast[k] = true
	return false
}

// skip counts an event line the client could not decode and skipped.
func (w *Watcher) skip(err error) {
	w.skipped.Inc()
	w.log.Debug("skipped an undecodable docker event", "error", err)
}

func (w *Watcher) handle(ctx context.Context, ev dockerapi.Event) error {
	if w.replayOf(ev) {
		return nil
	}
	w.events.Inc()
	id := ev.Actor.ID
	switch ev.Action {
	case "start":
		if len(w.started) >= maxTracked {
			w.evictOldest()
		}
		w.started[id] = ev.At()
		if w.onStart != nil {
			w.onStart(id)
		}
	case "oom":
		w.oom[id] = true
	case "die":
		w.died(ctx, ev)
	}
	return nil
}

// died reports one container's exit. Its tags come from the event, which
// carries the container's name, image and labels as attributes — the
// container itself may already be gone (--rm).
func (w *Watcher) died(ctx context.Context, ev dockerapi.Event) {
	id := ev.Actor.ID
	attrs := ev.Actor.Attributes
	tags := w.tag.tags(attrs["name"], id, attrs["image"], attrs)
	code := "unknown"
	if n, ok := ev.ExitCode(); ok {
		code = strconv.Itoa(n)
	}
	// The kernel's OOM kill arrives as an oom event just before the die.
	// When the start was seen on this stream, so was any oom, and the maps
	// are the whole answer — no call to the daemon, which matters when
	// short-lived containers exit several times a second and each inspect
	// holds the stream. Otherwise (connected after the start, or a die
	// replayed after a reconnect) inspect supplies the start and the flag.
	oom := w.oom[id]
	start, seen := w.started[id]
	if !seen {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		j, err := w.api.Inspect(ctx, id)
		cancel()
		if err == nil {
			oom = oom || j.State.OOMKilled
			if !j.State.StartedAt.IsZero() {
				start, seen = j.State.StartedAt, true
			}
		}
	}
	delete(w.started, id)
	delete(w.oom, id)

	w.sink(Sample{
		Name: "container.exits", Kind: CountSample, Value: 1,
		Tags: append(tags, "exit_code:"+code, "oom_killed:"+strconv.FormatBool(oom)),
	})
	if seen {
		if d := ev.At().Sub(start); d >= 0 {
			w.sink(Sample{Name: "container.lifetime", Kind: DistributionSample, Value: d.Seconds(), Tags: tags})
		}
	}
}
