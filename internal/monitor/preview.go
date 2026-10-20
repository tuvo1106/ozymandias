package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/monitor/eval"
	"github.com/tuvo1106/ozymandias/internal/monitor/notify"
	"github.com/tuvo1106/ozymandias/internal/monitor/spec"
	"github.com/tuvo1106/ozymandias/internal/monitor/state"
)

// Bounds on a backtest: what one request to the editor may cost.
const (
	MaxBacktestRange = 7 * 24 * time.Hour
	MaxBacktestTicks = 20_000
	// MaxBacktestSeries is how many groups' values a backtest returns. The
	// events cover every group; the lines are for a person to look at, and
	// past a few dozen they are not looked at, only downloaded.
	MaxBacktestSeries = 20
)

// GroupValue is one group's value in a dry run.
type GroupValue struct {
	Key   string            `json:"group"`
	Group map[string]string `json:"tags"`
	// Value is nil when the group had no point in the window.
	Value   *float64 `json:"value"`
	Skipped bool     `json:"skipped,omitempty"`
	// Status is what a fresh monitor would say about this value: the status
	// one step from OK. It ignores min_consecutive_failures' history, which a
	// dry run does not have.
	Status string `json:"status"`
}

// DryRun evaluates c once at now, as the scheduler would, without reading or
// writing any state. It is what "validate" shows next to the editor.
func (s *Scheduler) DryRun(ctx context.Context, c *spec.Compiled, now time.Time) ([]GroupValue, error) {
	ctx, cancel := context.WithTimeout(ctx, evalTimeout)
	defer cancel()
	vals, _, err := s.opt.Evaluator.Evaluate(ctx, c, now)
	if err != nil {
		return nil, err
	}
	cfg := state.ConfigOf(c, true)
	cfg.MinFailures = 1
	out := make([]GroupValue, 0, len(vals))
	for _, v := range vals {
		gv := GroupValue{Key: v.Key, Group: v.Group, Skipped: v.Skipped, Status: string(state.OK)}
		if v.Present {
			gv.Value = notify.ValuePtr(v.Value)
			next, _ := state.Step(state.New(now), state.Observation{Now: now, WindowEnd: now, Present: true,
				Value: v.Value, DataTs: v.DataTs}, cfg)
			gv.Status = string(next.Status)
		}
		out = append(out, gv)
	}
	return out, nil
}

// BacktestEvent is one transition a backtest found.
type BacktestEvent struct {
	Ts     time.Time `json:"ts"`
	Group  string    `json:"group"`
	From   string    `json:"from"`
	To     string    `json:"to"`
	Value  *float64  `json:"value"`
	Notify bool      `json:"notify"`
	Reason string    `json:"reason,omitempty"`
}

// Backtest is a backtest's answer.
type Backtest struct {
	From        time.Time       `json:"from"`
	To          time.Time       `json:"to"`
	Evaluations int             `json:"evaluations"`
	Events      []BacktestEvent `json:"events"`
	// Final is each group's status at the end of the range.
	Final map[string]string `json:"final"`
	// Series is the value each evaluation compared with the thresholds, per
	// group: the editor's line. Not the raw query: what the evaluator saw
	// after the window's reduction, so the line crosses a band exactly where
	// a marker is. At most MaxBacktestSeries groups, those with events first;
	// SeriesOmitted counts the rest.
	Series        []BacktestSeries `json:"series"`
	SeriesOmitted int              `json:"series_omitted"`
}

// BacktestSeries is one group's evaluated values.
type BacktestSeries struct {
	Group  string            `json:"group"`
	Tags   map[string]string `json:"tags"`
	Points []TickValue       `json:"points"`
}

// TickValue is one evaluation's value, encoded as [unix seconds, value],
// with null for a tick that had no value for the group (absent, skipped by
// require_full_window, or before the group first had data). The pair is the
// shape the query API's points already have.
type TickValue struct {
	Ts    time.Time
	Value float64 // NaN: no value
}

// MarshalJSON encodes the pair.
func (v TickValue) MarshalJSON() ([]byte, error) {
	return json.Marshal([2]any{v.Ts.Unix(), notify.ValuePtr(v.Value)})
}

// Backtest replays c over [from, to] on a simulated clock: every tick the
// scheduler would have run (same interval, same phase), the same window, the
// same reduction, the same state machine, starting from no state. The events
// are the markers the editor draws.
//
// It fetches the whole range once and slides the window over it, which is
// what [eval.Reduce] exists for; the alternative, one query per tick, is two
// thousand eight hundred queries for one day of a thirty-second monitor.
// Muting is ignored: the question is what would have alerted, not who would
// have been told.
func (s *Scheduler) Backtest(ctx context.Context, c *spec.Compiled, id int64, from, to time.Time) (Backtest, error) {
	if !to.After(from) {
		return Backtest{}, fmt.Errorf("backtest: to must be after from")
	}
	if to.Sub(from) > MaxBacktestRange {
		return Backtest{}, fmt.Errorf("backtest: at most %v", MaxBacktestRange)
	}
	off := Offset(c.UID, id, c.Interval)
	first := nextAfter(from.Add(-time.Nanosecond), c.Interval, off)
	if n := int(to.Sub(first)/c.Interval) + 1; n > MaxBacktestTicks {
		return Backtest{}, fmt.Errorf("backtest: %d evaluations; the limit is %d — use a shorter range", n, MaxBacktestTicks)
	}
	fetchFrom, _ := eval.Window(c, first)
	_, fetchTo := eval.Window(c, to)
	res := Backtest{From: from, To: to, Events: []BacktestEvent{}, Final: map[string]string{}}
	if !fetchTo.After(fetchFrom) {
		return res, nil
	}
	data, err := s.opt.Evaluator.Fetch(ctx, c, fetchFrom, fetchTo)
	if err != nil {
		return Backtest{}, err
	}
	states := map[string]tracked{}
	lines := map[string]*BacktestSeries{}
	for now := first; !now.After(to); now = now.Add(c.Interval) {
		if err := ctx.Err(); err != nil {
			return Backtest{}, err
		}
		wf, wt := eval.Window(c, now)
		vals := eval.Reduce(c, data, wf, wt)
		for _, st := range advance(c, states, vals, data.AbsentIsZero, now, wt, false) {
			line := lines[st.Key]
			if line == nil {
				line = &BacktestSeries{Group: st.Key, Tags: st.Group}
				// Ticks before the group's first value, as nulls, so every
				// line has one point per evaluation.
				for t := first; t.Before(now); t = t.Add(c.Interval) {
					line.Points = append(line.Points, TickValue{Ts: t, Value: math.NaN()})
				}
				lines[st.Key] = line
			}
			line.Points = append(line.Points, TickValue{Ts: now, Value: st.Value})
			if st.Deleted {
				delete(states, st.Key)
				continue
			}
			states[st.Key] = tracked{State: st.Next, Group: st.Group}
			if st.Res.Changed() || st.Res.Notify != state.NotifyNone {
				res.Events = append(res.Events, BacktestEvent{Ts: now, Group: st.Key, From: string(st.Res.From),
					To: string(st.Res.To), Value: notify.ValuePtr(st.Value), Notify: st.Res.Notify != state.NotifyNone,
					Reason: string(st.Res.Notify)})
			}
		}
		// A known group a tick did not step (skipped by require_full_window)
		// gets a null, so its points stay one per evaluation.
		for _, line := range lines {
			if n := len(line.Points); n == 0 || !line.Points[n-1].Ts.Equal(now) {
				line.Points = append(line.Points, TickValue{Ts: now, Value: math.NaN()})
			}
		}
		res.Evaluations++
	}
	for k, t := range states {
		res.Final[k] = string(t.State.Status)
	}
	res.Series, res.SeriesOmitted = pickSeries(lines, res.Events)
	return res, nil
}

// pickSeries keeps MaxBacktestSeries lines: groups with events first (in the
// order of their first event, which is the order a person reads the markers
// in), then the rest by key.
func pickSeries(lines map[string]*BacktestSeries, events []BacktestEvent) ([]BacktestSeries, int) {
	order := make([]string, 0, len(lines))
	seen := map[string]bool{}
	for _, e := range events {
		if !seen[e.Group] && lines[e.Group] != nil {
			seen[e.Group] = true
			order = append(order, e.Group)
		}
	}
	rest := make([]string, 0, len(lines))
	for k := range lines {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	slices.Sort(rest)
	order = append(order, rest...)
	out := make([]BacktestSeries, 0, min(len(order), MaxBacktestSeries))
	for _, k := range order[:min(len(order), MaxBacktestSeries)] {
		out = append(out, *lines[k])
	}
	return out, len(order) - len(out)
}

// ErrNoMentions is returned by a test notification for a message that names
// no notifier: there is nobody to send it to.
var ErrNoMentions = errors.New("the message mentions no notifier (write @name, where name is one under notifiers: in ozyd.yaml)")

// TestNotification renders the monitor's message as an ALERT and sends it,
// now, to every notifier it mentions, returning each one's outcome. It bypasses
// the outbox on purpose: the person pressing the button is waiting for the
// answer, and a test that goes into a queue answers "queued".
func (s *Scheduler) TestNotification(ctx context.Context, row meta.MonitorRow) (map[string]error, error) {
	c, err := CompileRow(row)
	if err != nil {
		return nil, err
	}
	now := s.opt.Clock.Now()
	// The same render and the same routing as a real alert, so the button
	// tests what will actually happen.
	n := s.render(c, row, spec.Group{}, "*", state.OK, state.Alert, c.Levels.Critical, now, now, now)
	n.Reason = "test"
	names := c.Template.Recipients()
	if len(names) == 0 {
		return nil, ErrNoMentions
	}
	out := map[string]error{}
	for _, name := range names {
		nt, ok := s.opt.Notifiers[name]
		if !ok {
			out[name] = fmt.Errorf("no notifier named %q is configured", name)
			continue
		}
		actx, cancel := context.WithTimeout(ctx, notify.DefaultTimeout)
		out[name] = nt.Notify(actx, n)
		cancel()
	}
	_ = s.opt.Store.AddEvent(ctx, meta.EventRow{Ts: now, Kind: "test_notification", MonitorID: row.ID,
		Value: math.NaN(), Text: fmt.Sprintf("test notification to %v", names)})
	return out, nil
}
