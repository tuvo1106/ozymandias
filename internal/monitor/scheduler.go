package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/monitor/eval"
	"github.com/tuvo1106/ozymandias/internal/monitor/notify"
	"github.com/tuvo1106/ozymandias/internal/monitor/spec"
	"github.com/tuvo1106/ozymandias/internal/monitor/state"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
)

// Event kinds, as stored in the events table and served by the API.
const (
	EventTransition   = "transition"
	EventRenotify     = "renotify"
	EventNotifyOK     = "notify_ok"
	EventNotifyFailed = "notify_failed"
	EventEvalError    = "eval_error"
	EventEvalOK       = "eval_recovered"
	EventGroupExpired = "group_expired"
	// EventReset and EventReleased are written by internal/meta, in the
	// transaction that changes the query or releases a provisioned monitor.
	EventReset    = "reset"
	EventReleased = "released"
)

// Store is the part of internal/meta the scheduler needs.
type Store interface {
	Monitors(ctx context.Context) ([]meta.MonitorRow, error)
	States(ctx context.Context, monitorID int64) ([]meta.StateRow, error)
	Commit(ctx context.Context, c meta.Commit) ([]meta.OutboxRow, error)
	AddEvent(ctx context.Context, e meta.EventRow) error
	PruneEvents(ctx context.Context, before time.Time) (int64, error)
}

// Evaluator evaluates one monitor. Implemented by *eval.Evaluator.
type Evaluator interface {
	Evaluate(ctx context.Context, c *spec.Compiled, now time.Time) ([]eval.Value, eval.Data, error)
	Fetch(ctx context.Context, c *spec.Compiled, from, to time.Time) (eval.Data, error)
}

// Enqueuer receives committed notifications. Implemented by
// *notify.Dispatcher.
type Enqueuer interface {
	Enqueue(ctx context.Context, rows []meta.OutboxRow)
}

// Options configures a Scheduler.
type Options struct {
	Store     Store
	Evaluator Evaluator
	Outbox    Enqueuer
	// Notifiers is used directly only by test notifications, which are sent
	// synchronously so the person pressing the button sees the result.
	Notifiers      map[string]notify.Notifier
	Clock          clock.Clock
	Logger         *slog.Logger
	Registry       *selfmetrics.Registry
	Workers        int           // default 4
	BaseURL        string        // for notification links
	EventRetention time.Duration // negative keeps everything; default 30 days
}

// evalTimeout bounds one evaluation's query, and so does the interval: an
// evaluation still running when the next is due has stopped being useful.
const evalTimeout = 30 * time.Second

// entry is one scheduled monitor.
type entry struct {
	row      meta.MonitorRow
	compiled *spec.Compiled // nil when the stored definition no longer compiles
	offset   time.Duration
	next     time.Time

	// Owned by the evaluation in progress, which the running flag makes
	// exclusive; read by nothing else.
	running   bool
	states    map[string]tracked
	statesFor string // the query the states were built by
	lastErr   string
}

// Scheduler runs monitors.
//
// One goroutine (Run) owns the schedule: it sleeps until the earliest monitor
// is due, hands due monitors to a pool of Workers goroutines, and wakes early
// when Reload is called. An evaluation still running when its monitor is due
// again is not queued behind itself: that tick is skipped and counted
// (ozy.monitor.eval_skipped), because a backlog of stale evaluations is worse
// than one missing one.
type Scheduler struct {
	opt Options
	log *slog.Logger

	mu      sync.Mutex
	entries map[int64]*entry
	jobs    chan job
	wake    chan struct{}
	lastOK  time.Time // completion of the last successful evaluation
	started time.Time
	loading bool // the startup load is failing and being retried

	evaluations, errorsTotal, skipped, commitErrors *selfmetrics.Counter
}

type job struct {
	e  *entry
	at time.Time
}

// New builds a scheduler. Call Run to start it.
func New(opt Options) *Scheduler {
	if opt.Clock == nil {
		opt.Clock = clock.Real()
	}
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.DiscardHandler)
	}
	if opt.Registry == nil {
		opt.Registry = selfmetrics.NewRegistry()
	}
	if opt.Workers <= 0 {
		opt.Workers = 4
	}
	if opt.EventRetention == 0 {
		opt.EventRetention = 30 * 24 * time.Hour
	}
	s := &Scheduler{
		opt: opt, log: opt.Logger,
		entries: map[int64]*entry{},
		jobs:    make(chan job, 1024),
		wake:    make(chan struct{}, 1),
	}
	r := opt.Registry
	s.evaluations = r.Counter("ozy.monitor.evaluations")
	s.errorsTotal = r.Counter("ozy.monitor.eval_errors")
	s.skipped = r.Counter("ozy.monitor.eval_skipped")
	s.commitErrors = r.Counter("ozy.monitor.commit_errors")
	r.GaugeFunc("ozy.monitor.last_eval_age_seconds", func() float64 { return s.Health().LastEvalAge.Seconds() })
	r.GaugeFunc("ozy.monitor.monitors", func() float64 { return float64(s.Health().Monitors) })
	return s
}

// Offset is a monitor's start offset within its interval: a hash of its uid
// (or id), so two hundred monitors on a thirty-second interval spread across
// the thirty seconds instead of all querying at :00 and :30. Stable across
// restarts, so a monitor evaluates at the same phase every time.
func Offset(uid string, id int64, interval time.Duration) time.Duration {
	h := fnv.New64a()
	if uid == "" {
		uid = strconv.FormatInt(id, 10)
	}
	_, _ = h.Write([]byte(uid))
	return time.Duration(h.Sum64() % uint64(interval/time.Second) * uint64(time.Second))
}

// nextAfter is the first evaluation time strictly after t for this phase.
func nextAfter(t time.Time, interval, offset time.Duration) time.Time {
	base := t.Add(-offset).Truncate(interval).Add(offset)
	if !base.After(t) {
		base = base.Add(interval)
	}
	return base
}

// Run loads the monitors and their states, then schedules until ctx ends. It
// returns once every worker has stopped.
func (s *Scheduler) Run(ctx context.Context) error {
	s.mu.Lock()
	s.started = s.opt.Clock.Now()
	s.mu.Unlock()
	// The first load is retried rather than fatal: a database that is busy for
	// a moment at startup must not leave a server up with no monitors running
	// and nothing saying so. While it fails, Health reports a stall.
	stalled := false
	for {
		err := s.load(ctx, true)
		s.mu.Lock()
		s.loading = err != nil
		s.mu.Unlock()
		if err == nil {
			break
		}
		s.log.Error("monitors: loading; retrying in 10s", "err", err)
		stalled = s.watchdog(s.opt.Clock.Now(), stalled)
		t := s.opt.Clock.NewTimer(10 * time.Second)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C():
		}
	}
	var wg sync.WaitGroup
	for range s.opt.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.worker(ctx)
		}()
	}
	defer wg.Wait()

	timer := s.opt.Clock.NewTimer(time.Hour)
	defer timer.Stop()
	prune := s.opt.Clock.NewTicker(time.Hour)
	defer prune.Stop()
	for {
		now := s.opt.Clock.Now()
		next := s.dispatchDue(now)
		stalled = s.watchdog(now, stalled)
		wait := time.Hour
		if !next.IsZero() {
			// From the clock now, not from the now read before dispatching:
			// time spent in dispatchDue and the watchdog would otherwise
			// make every tick late by that much.
			wait = max(next.Sub(s.opt.Clock.Now()), 0)
		}
		// The watchdog needs a look at least every minute even when nothing
		// is due — which is exactly when evaluations have stopped.
		wait = min(wait, time.Minute)
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C():
		case <-s.wake:
			if err := s.load(ctx, false); err != nil && ctx.Err() == nil {
				s.log.Error("monitors: reloading", "err", err)
			}
		case <-prune.C():
			s.prune(ctx)
		}
	}
}

// Reload asks the scheduler to re-read monitor definitions. The API calls it
// after every write; it never blocks.
func (s *Scheduler) Reload() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// load reads the monitor table and reconciles the schedule with it. With
// states, it also reads every persisted state (startup).
func (s *Scheduler) load(ctx context.Context, withStates bool) error {
	rows, err := s.opt.Store.Monitors(ctx)
	if err != nil {
		return err
	}
	// States are read for every monitor new to the schedule — all of them at
	// startup, and later one that was created, or disabled and enabled again.
	// Starting a re-enabled monitor from empty would forget an ALERT that was
	// already announced, and announce it a second time.
	persisted := map[int64][]meta.StateRow{}
	if withStates {
		all, err := s.opt.Store.States(ctx, 0)
		if err != nil {
			return err
		}
		for _, r := range all {
			persisted[r.MonitorID] = append(persisted[r.MonitorID], r)
		}
	} else {
		s.mu.Lock()
		var fresh []int64
		for _, r := range rows {
			if _, ok := s.entries[r.ID]; !ok && r.Enabled {
				fresh = append(fresh, r.ID)
			}
		}
		s.mu.Unlock()
		for _, id := range fresh {
			st, err := s.opt.Store.States(ctx, id)
			if err != nil {
				return err
			}
			persisted[id] = st
		}
	}
	now := s.opt.Clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := map[int64]bool{}
	for _, r := range rows {
		if !r.Enabled {
			continue
		}
		keep[r.ID] = true
		c, cerr := CompileRow(r)
		if cerr != nil {
			// A row that no longer compiles (edited by hand, or written by a
			// newer build) is kept out of the schedule and said once.
			s.log.Error("monitors: skipping a monitor whose definition does not compile", "id", r.ID, "name", r.Name, "err", cerr)
		}
		e := s.entries[r.ID]
		if e == nil {
			e = &entry{states: map[string]tracked{}, statesFor: r.Query}
			for _, st := range persisted[r.ID] {
				e.states[st.GroupKey] = fromRow(st)
			}
			s.entries[r.ID] = e
		}
		// Swapping the definition under a running evaluation is safe: the
		// worker took its own pointer to the compiled monitor when it started.
		e.row, e.compiled = r, c
		if c != nil {
			off := Offset(r.UID, r.ID, c.Interval)
			if e.next.IsZero() || off != e.offset || e.next.Sub(now) > c.Interval {
				e.offset = off
				e.next = nextAfter(now, c.Interval, off)
			}
		}
	}
	for id := range s.entries {
		if !keep[id] {
			delete(s.entries, id)
		}
	}
	return nil
}

// dispatchDue hands every due monitor to the pool and returns the earliest
// next due time.
func (s *Scheduler) dispatchDue(now time.Time) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	var earliest time.Time
	for _, e := range s.entries {
		if e.compiled == nil {
			continue
		}
		if !e.next.After(now) {
			at := e.next
			e.next = nextAfter(now, e.compiled.Interval, e.offset)
			switch {
			case e.running:
				s.skipped.Inc()
				s.log.Warn("monitors: evaluation overran its interval; skipping a tick", "id", e.row.ID, "name", e.row.Name)
			default:
				select {
				case s.jobs <- job{e: e, at: at}:
					e.running = true
				default:
					s.skipped.Inc()
				}
			}
		}
		if earliest.IsZero() || e.next.Before(earliest) {
			earliest = e.next
		}
	}
	return earliest
}

func (s *Scheduler) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-s.jobs:
			s.mu.Lock()
			c, row := j.e.compiled, j.e.row
			s.mu.Unlock()
			s.evaluate(ctx, j.e, c, row, j.at)
			s.mu.Lock()
			j.e.running = false
			s.mu.Unlock()
		}
	}
}

// evaluate runs one evaluation of one monitor and commits what it decided.
//
// The order is the reliability argument, so it is worth spelling out:
//  1. query and step every group, in memory, without touching e.states;
//  2. commit states, events and owed notifications in one transaction;
//  3. only then adopt the new states and hand the notifications to delivery.
//
// A failed commit therefore changes nothing: the in-memory state is still the
// persisted one, the next evaluation recomputes the same transition, and it is
// announced then. A notification can never be sent for a transition the
// database does not know about.
//
// now is the tick the evaluation was scheduled for, not the moment a worker
// got to it. A job that waited in the queue for two seconds still evaluates the
// window of its own tick, so the result does not depend on how busy the pool
// was — and a backtest, which replays ticks, computes the same thing.
func (s *Scheduler) evaluate(ctx context.Context, e *entry, c *spec.Compiled, row meta.MonitorRow, now time.Time) {
	if c == nil {
		return
	}
	timeout := min(c.Interval, evalTimeout)
	ectx, cancel := context.WithTimeout(ctx, timeout)
	vals, data, err := s.opt.Evaluator.Evaluate(ectx, c, now)
	cancel()
	s.evaluations.Inc()
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.errorsTotal.Inc()
		s.evalError(ctx, e, row, now, err)
		return
	}
	_, windowEnd := eval.Window(c, now)
	muted := !row.MutedUntil.IsZero() && row.MutedUntil.After(now)

	commit := meta.Commit{MonitorID: row.ID, Query: row.Query}
	states := e.states
	if e.statesFor != row.Query {
		// The query changed: the old groups are answers to a different
		// question, and stepping them as absent would announce NO_DATA for
		// groups that simply no longer exist. Start over, and say so.
		for k := range states {
			commit.Delete = append(commit.Delete, k)
		}
		// (The database already deleted them, and recorded the reset, in the
		// transaction that changed the query; these deletes are no-ops there.)
		states = map[string]tracked{}
	}
	if e.lastErr != "" {
		commit.Events = append(commit.Events, meta.EventRow{Ts: now, Kind: EventEvalOK, MonitorID: row.ID,
			Value: math.NaN(), Text: "evaluating again after: " + e.lastErr})
	}
	steps := advance(c, states, vals, data.AbsentIsZero, now, windowEnd, muted)
	next := make(map[string]tracked, len(states))
	for k, v := range states {
		next[k] = v
	}
	for _, st := range steps {
		if st.Deleted {
			delete(next, st.Key)
			commit.Delete = append(commit.Delete, st.Key)
			commit.Events = append(commit.Events, meta.EventRow{Ts: now, Kind: EventGroupExpired, MonitorID: row.ID,
				GroupKey: st.Key, From: string(st.Prev.Status), Value: math.NaN(),
				Text: fmt.Sprintf("no data for %v; group forgotten", GroupTTL)})
			continue
		}
		next[st.Key] = tracked{State: st.Next, Group: st.Group}
		commit.Upsert = append(commit.Upsert, toRow(row.ID, st.Key, st.Group, st.Next))
		if st.Res.Changed() || st.Res.Notify == state.NotifyRenotify {
			kind := EventTransition
			if !st.Res.Changed() {
				kind = EventRenotify
			}
			commit.Events = append(commit.Events, meta.EventRow{Ts: now, Kind: kind, MonitorID: row.ID,
				GroupKey: st.Key, From: string(st.Res.From), To: string(st.Res.To), Value: st.Value})
		}
		if st.Res.Notify != state.NotifyNone {
			commit.Outbox = append(commit.Outbox, s.notifications(c, row, st, now, windowEnd)...)
		}
	}
	out, err := s.opt.Store.Commit(ctx, commit)
	if errors.Is(err, meta.ErrNotRunnable) {
		// Deleted or disabled while this evaluation ran: nothing to record,
		// and the next reload drops the entry.
		return
	}
	if err != nil {
		s.commitErrors.Inc()
		s.log.Error("monitors: committing an evaluation; it will be recomputed next time", "id", row.ID, "err", err)
		return
	}
	e.states, e.statesFor, e.lastErr = next, row.Query, ""
	s.mu.Lock()
	s.lastOK = s.opt.Clock.Now()
	s.mu.Unlock()
	if s.opt.Outbox != nil && len(out) > 0 {
		s.opt.Outbox.Enqueue(ctx, out)
	}
}

// evalError records a failed evaluation. The state is left exactly as it was:
// a query that timed out says nothing about the value. An event is written
// when the error is new, not on every failing evaluation, so a monitor broken
// for a day leaves one line, not three thousand.
func (s *Scheduler) evalError(ctx context.Context, e *entry, row meta.MonitorRow, now time.Time, err error) {
	msg := err.Error()
	if errors.Is(err, context.DeadlineExceeded) {
		msg = "the query did not answer within the evaluation timeout"
	}
	if msg == e.lastErr {
		return
	}
	s.log.Warn("monitors: evaluation failed", "id", row.ID, "name", row.Name, "err", err)
	// Through Commit, so an error from an evaluation whose monitor was
	// deleted or changed under it is not recorded against it.
	_, aerr := s.opt.Store.Commit(ctx, meta.Commit{MonitorID: row.ID, Query: row.Query, Events: []meta.EventRow{{
		Ts: now, Kind: EventEvalError, MonitorID: row.ID, Value: math.NaN(), Text: msg}}})
	if errors.Is(aerr, meta.ErrNotRunnable) {
		return
	}
	if aerr != nil {
		s.log.Error("monitors: recording an evaluation error", "id", row.ID, "err", aerr)
		return
	}
	e.lastErr = msg
}

// notifications renders the message for one step and makes one outbox row per
// notifier it mentions.
func (s *Scheduler) notifications(c *spec.Compiled, row meta.MonitorRow, st groupStep, now, windowEnd time.Time) []meta.OutboxRow {
	n, to := s.render(c, row, st.Group, st.Key, st.Res.From, st.Res.To, st.Value, st.Next.Since, now, windowEnd)
	n.Reason = string(st.Res.Notify)
	payload, _ := json.Marshal(n)
	var out []meta.OutboxRow
	for _, name := range to {
		out = append(out, meta.OutboxRow{Created: now, MonitorID: row.ID, GroupKey: st.Key, Notifier: name, Payload: payload})
	}
	return out
}

// render builds a notification and says who it goes to
// (spec.Template.Recipients). A template that fails at runtime (it compiled
// at save, so this is rare: a value that prints past the size bound) still
// produces a notification, with the failure in it — a page that says "your
// template broke" beats no page.
func (s *Scheduler) render(c *spec.Compiled, row meta.MonitorRow, g spec.Group, key string, from, to state.Status,
	value float64, since, now, windowEnd time.Time) (notify.Notification, []string) {
	if g == nil {
		g = spec.Group{}
	}
	threshold := c.Levels.Critical
	if to == state.Warn {
		threshold = c.Levels.Warning
	}
	link := s.link(row.ID, windowEnd.Add(-c.Parsed.Window-time.Hour), windowEnd.Add(time.Hour))
	d := spec.Data{
		Monitor: spec.Ref{ID: row.ID, UID: row.UID, Name: row.Name}, Group: g,
		Status: string(to), PrevStatus: string(from), Value: value, Threshold: threshold,
		IsAlert: to == state.Alert, IsWarning: to == state.Warn, IsNoData: to == state.NoData,
		IsRecovery: to == state.OK && from != state.OK, Since: since, Link: link,
	}
	text, err := c.Template.Render(d)
	recipients := c.Template.Recipients(text)
	if err != nil {
		// The mentions are kept, so the failure still reaches whoever the
		// message was for: everyone the template names.
		text = fmt.Sprintf("(the message template failed: %v)", err)
		for _, m := range c.Template.Named() {
			text += " @" + m
		}
		recipients = c.Template.Named()
	}
	return notify.Notification{
		MonitorID: row.ID, MonitorUID: row.UID, MonitorName: row.Name, Tags: row.Tags,
		GroupKey: key, Group: g, Status: string(to), PrevStatus: string(from),
		Value: notify.ValuePtr(value), Threshold: threshold, Message: text, Link: link, Ts: now,
	}, recipients
}

func (s *Scheduler) link(id int64, from, to time.Time) string {
	if s.opt.BaseURL == "" {
		return ""
	}
	q := url.Values{}
	q.Set("from", strconv.FormatInt(from.Unix(), 10))
	q.Set("to", strconv.FormatInt(to.Unix(), 10))
	return fmt.Sprintf("%s/monitors/%d?%s", s.opt.BaseURL, id, q.Encode())
}

// Health is the self-watchdog's view.
type Health struct {
	Monitors    int           `json:"monitors"`
	LastEvalAge time.Duration `json:"-"`
	// Stalled means there are monitors to run (or the scheduler cannot even
	// load them) and none has completed an evaluation for longer than
	// stallAfter: the alerting is down, and every monitor's silence is now
	// meaningless.
	Stalled bool `json:"stalled"`
	// Loading means the startup load is failing and being retried.
	Loading bool `json:"loading"`
}

// StallAfter is how long without a successful evaluation counts as stalled,
// when monitors exist. Three of the longest default interval, and never less
// than two minutes, so one slow query is not an outage.
func (s *Scheduler) stallAfter() time.Duration {
	longest := time.Duration(0)
	for _, e := range s.entries {
		if e.compiled != nil {
			longest = max(longest, e.compiled.Interval)
		}
	}
	return max(3*longest, 2*time.Minute)
}

// Health reports whether evaluations are happening.
//
// Monitors counts the runnable ones: an entry whose stored definition no
// longer compiles is never evaluated, and counting it would make an idle
// scheduler look stalled forever. While the startup load keeps failing there
// are no entries at all, so that case is reported separately: Loading, and
// stalled once it has failed for as long as a stall takes.
func (s *Scheduler) Health() Health {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.opt.Clock.Now()
	h := Health{Loading: s.loading}
	for _, e := range s.entries {
		if e.compiled != nil {
			h.Monitors++
		}
	}
	ref := s.lastOK
	if ref.IsZero() {
		ref = s.started
	}
	if !ref.IsZero() {
		h.LastEvalAge = max(now.Sub(ref), 0)
	}
	after := s.stallAfter()
	h.Stalled = !ref.IsZero() && h.LastEvalAge > after && (h.Monitors > 0 || s.loading)
	return h
}

// watchdog logs loudly on the edge into and out of a stall.
func (s *Scheduler) watchdog(now time.Time, was bool) bool {
	h := s.Health()
	switch {
	case h.Stalled && !was:
		s.log.Error("MONITORS STALLED: no monitor has completed an evaluation; alerting is not working",
			"last_eval_age", h.LastEvalAge.Round(time.Second), "monitors", h.Monitors)
	case !h.Stalled && was:
		s.log.Info("monitors: evaluations resumed")
	}
	return h.Stalled
}

func (s *Scheduler) prune(ctx context.Context) {
	if s.opt.EventRetention < 0 {
		return
	}
	n, err := s.opt.Store.PruneEvents(ctx, s.opt.Clock.Now().Add(-s.opt.EventRetention))
	if err != nil {
		s.log.Warn("monitors: pruning events", "err", err)
	} else if n > 0 {
		s.log.Info("monitors: pruned events", "deleted", n)
	}
}

// CompileRow compiles a stored monitor.
func CompileRow(r meta.MonitorRow) (*spec.Compiled, error) {
	m, err := RowToSpec(r)
	if err != nil {
		return nil, err
	}
	return spec.Compile(m)
}

// RowToSpec is the stored row as a definition.
func RowToSpec(r meta.MonitorRow) (spec.Monitor, error) {
	m := spec.Monitor{UID: r.UID, Name: r.Name, Type: spec.Type(r.Type), Query: r.Query, Message: r.Message, Tags: r.Tags}
	if len(r.Options) > 0 {
		if err := json.Unmarshal(r.Options, &m.Options); err != nil {
			return m, fmt.Errorf("options: %w", err)
		}
	}
	return m, nil
}

func fromRow(r meta.StateRow) tracked {
	return tracked{Group: spec.Group(r.Group), State: state.Group{
		Status: state.Status(r.Status), Since: r.Since, Breaches: r.Breaches, LastValue: r.LastValue,
		LastEval: r.LastEval, LastData: r.LastData, LastNotified: r.LastNotified,
	}}
}

func toRow(id int64, key string, g spec.Group, st state.Group) meta.StateRow {
	return meta.StateRow{MonitorID: id, GroupKey: key, Group: g, Status: string(st.Status), Since: st.Since,
		Breaches: st.Breaches, LastValue: st.LastValue, LastEval: st.LastEval, LastData: st.LastData,
		LastNotified: st.LastNotified}
}
