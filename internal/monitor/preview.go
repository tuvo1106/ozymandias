package monitor

import (
	"context"
	"errors"
	"fmt"
	"math"
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
	for now := first; !now.After(to); now = now.Add(c.Interval) {
		if err := ctx.Err(); err != nil {
			return Backtest{}, err
		}
		wf, wt := eval.Window(c, now)
		vals := eval.Reduce(c, data, wf, wt)
		for _, st := range advance(c, states, vals, data.AbsentIsZero, now, wt, false) {
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
		res.Evaluations++
	}
	for k, t := range states {
		res.Final[k] = string(t.State.Status)
	}
	return res, nil
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
