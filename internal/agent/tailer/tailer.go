package tailer

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/logpipeline"
	"github.com/tuvo1106/ozymandias/internal/clock"
)

// Defaults from the plan: a polling tailer, because file-system events do not
// cross the bind mounts the agent reads through reliably.
const (
	DefaultScanInterval = 10 * time.Second
	DefaultPollInterval = time.Second
	DefaultIdleClose    = 60 * time.Second
)

// FileSource is one `type: file` source.
type FileSource struct {
	// Path is a glob.
	Path    string
	Service string
	Source  string
	Tags    []string
	// StartPosition is where a file seen for the first time starts: "end"
	// (the default: do not replay history) or "beginning". A file that appears
	// after the agent started always starts at the beginning: it is a
	// rotation product, and everything in it is new.
	StartPosition string
	// MultilineStart is a regular expression that begins an event.
	MultilineStart string
	Pipeline       logpipeline.Spec
}

// Options are the tailer's dependencies.
type Options struct {
	Registry *Registry
	Sink     Sink
	Host     string
	Clock    clock.Clock
	Logger   *slog.Logger

	ScanInterval, IdleClose time.Duration
	BatchLogs               int
}

// Stats counts what the file tailer did.
type Stats struct {
	Files, Truncations, SendErrors, TruncatedLines int64
}

// Files tails every file matching its sources' globs.
//
// The mechanism, in the order a line travels: a poll reads new bytes from each
// open file at its own offset; complete lines are joined into events by the
// multiline rule; each event goes through the source's pipeline; the resulting
// logs go to the sink; and only when the sink says they are accepted does the
// offset move into the registry. A crash or a failed send therefore costs
// duplicates (the lines are read again), never gaps.
type Files struct {
	opts    Options
	sources []*fileSource

	mu      sync.Mutex
	stats   Stats
	started bool
	scanned time.Time
}

type fileSource struct {
	cfg   FileSource
	start *regexp.Regexp
	tail  map[fileID]*tracked
}

type tracked struct {
	id        fileID
	path      string
	r         lineReader
	st        *stream
	committed int64
	// gone is when the path stopped resolving to this file (rotated away or
	// deleted); the file is closed once it has been idle for IdleClose after that.
	gone time.Time
	idle time.Time // last time a read returned data
}

// NewFiles builds the tailer. It does not touch the disk until Poll.
func NewFiles(sources []FileSource, opts Options) (*Files, error) {
	if opts.Clock == nil {
		opts.Clock = clock.Real()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.ScanInterval <= 0 {
		opts.ScanInterval = DefaultScanInterval
	}
	if opts.IdleClose <= 0 {
		opts.IdleClose = DefaultIdleClose
	}
	if opts.Registry == nil {
		opts.Registry, _ = OpenRegistry("")
	}
	f := &Files{opts: opts}
	for i, s := range sources {
		fs := &fileSource{cfg: s, tail: map[fileID]*tracked{}}
		if _, err := filepath.Match(s.Path, ""); err != nil {
			return nil, fmt.Errorf("file source %d: bad glob %q: %w", i, s.Path, err)
		}
		if s.MultilineStart != "" {
			re, err := regexp.Compile(s.MultilineStart)
			if err != nil {
				return nil, fmt.Errorf("file source %d: multiline start: %w", i, err)
			}
			fs.start = re
		}
		switch s.StartPosition {
		case "", "end", "beginning":
		default:
			return nil, fmt.Errorf("file source %d: start_position %q must be end or beginning", i, s.StartPosition)
		}
		f.sources = append(f.sources, fs)
	}
	return f, nil
}

// Stats returns the counters so far.
func (f *Files) Stats() Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.stats
	for _, src := range f.sources {
		s.Files += int64(len(src.tail))
	}
	return s
}

// Run polls until ctx is done, then flushes pending events and the registry.
func (f *Files) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = DefaultPollInterval
	}
	t := f.opts.Clock.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Shutdown: deliver what is pending, with a fresh context, because
			// the one we were given is already cancelled.
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			f.Shutdown(sctx)
			cancel()
			return
		case <-t.C():
			f.Poll(ctx)
		}
	}
}

// Shutdown delivers pending multiline events and Rails requests and writes the registry.
func (f *Files) Shutdown(ctx context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.opts.Clock.Now()
	for _, src := range f.sources {
		for _, t := range src.tail {
			evs := t.st.ml.flush(now, true)
			if end, err := t.st.deliver(ctx, evs, t.committed, now); err == nil {
				t.committed = end
			}
			// Open Rails requests have no file offset to commit against; they are
			// delivered best-effort.
			if logs := t.st.pl.FlushAll(); len(logs) > 0 {
				_ = t.st.sink.Send(ctx, logs)
			}
			f.commit(t, now)
		}
	}
	if err := f.opts.Registry.Flush(); err != nil {
		f.opts.Logger.Warn("tailer: writing registry on shutdown", "err", err)
	}
}

// Poll does one round: rescan globs if due, read every open file, deliver, commit.
func (f *Files) Poll(ctx context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.opts.Clock.Now()
	if !f.started || now.Sub(f.scanned) >= f.opts.ScanInterval {
		f.scan(now)
		f.scanned = now
	}
	for _, src := range f.sources {
		ids := make([]fileID, 0, len(src.tail))
		for id := range src.tail {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i].less(ids[j]) })
		for _, id := range ids {
			f.pollOne(ctx, src, src.tail[id], now)
		}
	}
	f.started = true
	if err := f.opts.Registry.Flush(); err != nil {
		f.opts.Logger.Warn("tailer: writing registry", "err", err)
	}
}

// scan matches the globs, starts tailing files it has not seen, and notes
// which tracked files no longer sit at their path.
func (f *Files) scan(now time.Time) {
	for _, src := range f.sources {
		paths, _ := filepath.Glob(src.cfg.Path)
		sort.Strings(paths)
		present := map[fileID]string{}
		for _, p := range paths {
			fi, err := os.Stat(p)
			if err != nil || !fi.Mode().IsRegular() {
				continue
			}
			id, ok := idOf(fi)
			if !ok {
				continue
			}
			present[id] = p
			if t := src.tail[id]; t != nil {
				if t.path != p {
					t.path = p // renamed within the glob: the inode follows the file
				}
				t.gone = time.Time{}
				continue
			}
			f.open(src, id, p, fi.Size(), now)
		}
		for id, t := range src.tail {
			if _, ok := present[id]; !ok && t.gone.IsZero() {
				t.gone = now
			}
		}
	}
}

func (f *Files) open(src *fileSource, id fileID, path string, size int64, now time.Time) {
	fh, err := os.Open(path)
	if err != nil {
		f.opts.Logger.Warn("tailer: opening", "path", path, "err", err)
		return
	}
	key := "file:" + id.String()
	var off int64
	switch e, known := f.opts.Registry.Get(key); {
	case known:
		off = e.Offset
		if off > size {
			off = 0 // truncated while we were away
		}
	case f.started:
		off = 0 // appeared after the agent started: a rotation product
	case src.cfg.StartPosition == "beginning":
		off = 0
	default:
		off = size
	}
	pl, err := logpipeline.New(pipelineSpec(src.cfg), logpipeline.Options{Clock: f.opts.Clock})
	if err != nil {
		_ = fh.Close()
		f.opts.Logger.Error("tailer: building pipeline", "source", src.cfg.Source, "err", err)
		return
	}
	meta := logpipeline.Meta{Service: src.cfg.Service, Source: src.cfg.Source, Host: f.opts.Host, Tags: src.cfg.Tags}
	t := &tracked{id: id, path: path, committed: off, idle: now,
		st: newStream(pl, meta, src.start, f.opts.Sink, f.opts.BatchLogs)}
	t.r = lineReader{f: fh}
	t.r.seek(off)
	src.tail[id] = t
	f.commit(t, now)
}

func pipelineSpec(c FileSource) logpipeline.Spec {
	s := c.Pipeline
	if s.Source == "" {
		s.Source = c.Source
	}
	return s
}

func (f *Files) commit(t *tracked, now time.Time) {
	f.opts.Registry.Set("file:"+t.id.String(), Entry{Path: t.path, Offset: t.committed, LastSeen: now.Unix()})
}

func (f *Files) pollOne(ctx context.Context, src *fileSource, t *tracked, now time.Time) {
	lines, truncated, err := t.r.read(now)
	if err != nil {
		f.opts.Logger.Warn("tailer: reading", "path", t.path, "err", err)
		f.close(src, t)
		return
	}
	if truncated {
		f.stats.Truncations++
		t.committed = 0
		t.st.ml.reset()
	}
	var events []event
	for _, l := range lines {
		if l.truncated {
			f.stats.TruncatedLines++
		}
		events = append(events, t.st.ml.add(l.text, l.end, now, false)...)
	}
	events = append(events, t.st.ml.flush(now, false)...)
	if len(lines) > 0 {
		t.idle = now
	}
	end, err := t.st.deliver(ctx, events, t.committed, now)
	t.committed = end
	if err != nil {
		f.stats.SendErrors++
		f.opts.Logger.Warn("tailer: delivery failed, will retry", "path", t.path, "err", err)
		// Rewind to the last acknowledged byte: the lines are read again next poll.
		t.r.seek(t.committed)
		t.st.ml.reset()
		return
	}
	// Requests the Rails grouper has held too long come out here, with no offset.
	if logs := t.st.pl.Flush(now); len(logs) > 0 {
		if err := t.st.sink.Send(ctx, logs); err != nil {
			f.stats.SendErrors++
		}
	}
	f.commit(t, now)
	// A rotated-away file is closed once it has been quiet for IdleClose.
	if !t.gone.IsZero() && now.Sub(t.idle) >= f.opts.IdleClose && now.Sub(t.gone) >= f.opts.IdleClose {
		f.close(src, t)
	}
}

func (f *Files) close(src *fileSource, t *tracked) {
	_ = t.r.f.Close()
	delete(src.tail, t.id)
	if !t.gone.IsZero() {
		f.opts.Registry.Delete("file:" + t.id.String())
	}
}
