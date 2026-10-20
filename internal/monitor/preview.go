package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
	res := Backtest{From: from, To: to, Events: []BacktestEvent{}, Final: map[string]string{}, Series: []BacktestSeries{}}
	if !fetchTo.After(fetchFrom) {
		return res, nil
	}
	data, err := s.opt.Evaluator.Fetch(ctx, c, fetchFrom, fetchTo)
	if err != nil {
		return Backtest{}, err
	}
	// Two passes over the same replay. The first finds the events and every
	// group; the second records lines for the MaxBacktestSeries groups shown.
	// One pass recording every group's line before choosing was groups x
	// ticks of memory per request (a thousand groups over twenty thousand
	// ticks is hundreds of megabytes); this is twice the reduce work instead,
	// bounded by the same tick limit.
	tags := map[string]spec.Group{}
	states, ticks, err := replay(ctx, c, data, first, to, func(_ int, now time.Time, st groupStep) {
		if _, ok := tags[st.Key]; !ok {
			tags[st.Key] = st.Group
		}
		if st.Res.Changed() || st.Res.Notify != state.NotifyNone {
			res.Events = append(res.Events, BacktestEvent{Ts: now, Group: st.Key, From: string(st.Res.From),
				To: string(st.Res.To), Value: notify.ValuePtr(st.Value), Notify: st.Res.Notify != state.NotifyNone,
				Reason: string(st.Res.Notify)})
		}
	})
	if err != nil {
		return Backtest{}, err
	}
	res.Evaluations = ticks
	for k, t := range states {
		res.Final[k] = string(t.State.Status)
	}
	shown, omitted := pickSeries(slices.Collect(maps.Keys(tags)), res.Events)
	res.SeriesOmitted = omitted
	// Every tick a group was not stepped (before its first value, or skipped
	// by require_full_window) stays NaN: a null, one point per evaluation.
	values := make(map[string][]float64, len(shown))
	for _, k := range shown {
		v := make([]float64, res.Evaluations)
		for i := range v {
			v[i] = math.NaN()
		}
		values[k] = v
	}
	if len(shown) > 0 {
		if _, _, err := replay(ctx, c, data, first, to, func(i int, _ time.Time, st groupStep) {
			if v, ok := values[st.Key]; ok {
				v[i] = st.Value
			}
		}); err != nil {
			return Backtest{}, err
		}
	}
	res.Series = make([]BacktestSeries, 0, len(shown))
	for _, k := range shown {
		line := BacktestSeries{Group: k, Tags: tags[k], Points: make([]TickValue, res.Evaluations)}
		for i, v := range values[k] {
			line.Points[i] = TickValue{Ts: first.Add(time.Duration(i) * c.Interval), Value: v}
		}
		res.Series = append(res.Series, line)
	}
	return res, nil
}

// replay steps c through every tick from first to to, as the scheduler would,
// calling each with the tick's index and every group step, and returns the
// final states and how many ticks there were. Deterministic: two replays over the same data step the same
// groups the same way, which is what lets Backtest run it twice.
func replay(ctx context.Context, c *spec.Compiled, data eval.Data, first, to time.Time,
	each func(i int, now time.Time, st groupStep)) (map[string]tracked, int, error) {
	states := map[string]tracked{}
	i := 0
	for now := first; !now.After(to); now = now.Add(c.Interval) {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		wf, wt := eval.Window(c, now)
		vals := eval.Reduce(c, data, wf, wt)
		for _, st := range advance(c, states, vals, data.AbsentIsZero, now, wt, false) {
			each(i, now, st)
			if st.Deleted {
				delete(states, st.Key)
				continue
			}
			states[st.Key] = tracked{State: st.Next, Group: st.Group}
		}
		i++
	}
	return states, i, nil
}

// pickSeries chooses the MaxBacktestSeries groups whose lines are returned:
// groups with events first (in the order of their first event, which is the
// order a person reads the markers in), then the rest by key. It returns them
// and how many were left out.
func pickSeries(keys []string, events []BacktestEvent) ([]string, int) {
	known := make(map[string]bool, len(keys))
	for _, k := range keys {
		known[k] = true
	}
	order := make([]string, 0, len(keys))
	seen := map[string]bool{}
	for _, e := range events {
		if !seen[e.Group] && known[e.Group] {
			seen[e.Group] = true
			order = append(order, e.Group)
		}
	}
	rest := make([]string, 0, len(keys))
	for _, k := range keys {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	slices.Sort(rest)
	order = append(order, rest...)
	n := min(len(order), MaxBacktestSeries)
	return order[:n], len(order) - n
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
