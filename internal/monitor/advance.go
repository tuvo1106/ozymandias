package monitor

import (
	"math"
	"slices"
	"time"

	"github.com/tuvo1106/ozymandias/internal/monitor/eval"
	"github.com/tuvo1106/ozymandias/internal/monitor/spec"
	"github.com/tuvo1106/ozymandias/internal/monitor/state"
)

// GroupTTL is how long a group may go without data before it is forgotten.
// A multi-alert over `by {container}` meets a new group every deploy; without
// a bound its state table, and its NO_DATA notifications, would grow forever.
const GroupTTL = 24 * time.Hour

// tracked is one group as the scheduler remembers it: its state and its tags.
type tracked struct {
	State state.Group
	Group spec.Group
}

// groupStep is what one evaluation did to one group.
type groupStep struct {
	Key     string
	Group   spec.Group
	Prev    state.Group
	Next    state.Group
	Res     state.Result
	Value   float64 // NaN when absent
	Deleted bool    // forgotten after GroupTTL without data
}

// advance applies one evaluation's values to a monitor's groups.
//
// It is the one place that decides which groups are stepped and how, and both
// the live scheduler and the backtest call it. Pure, like [state.Step]: it
// returns the new states rather than changing the map, so the scheduler can
// apply them only after they are durably committed.
//
// The rules, beyond what Step itself decides:
//   - A group seen for the first time starts at OK. Only a group with a value
//     is ever created: a group that never had data is not a group.
//   - A known group missing from the values is stepped as absent — or, for a
//     log monitor, as a count of zero (absent is zero for counts).
//   - A group skipped by require_full_window is not stepped at all: its window
//     had a gap, which says nothing about its value.
//   - A group absent, with no data for GroupTTL, is deleted.
func advance(c *spec.Compiled, states map[string]tracked, vals []eval.Value, absentIsZero bool,
	now, windowEnd time.Time, muted bool) []groupStep {
	cfg := state.ConfigOf(c, muted)
	seen := make(map[string]bool, len(vals))
	var out []groupStep
	for _, v := range vals {
		seen[v.Key] = true
		if v.Skipped {
			continue
		}
		t, known := states[v.Key]
		prev := t.State
		if !v.Present {
			if !known {
				continue
			}
			out = append(out, absent(v.Key, v.Group, prev, cfg, absentIsZero, now, windowEnd))
			continue
		}
		if !known {
			prev = state.New(now)
		}
		next, res := state.Step(prev, state.Observation{Now: now, WindowEnd: windowEnd, Present: true,
			Value: v.Value, DataTs: v.DataTs}, cfg)
		out = append(out, groupStep{Key: v.Key, Group: v.Group, Prev: prev, Next: next, Res: res, Value: v.Value})
	}
	keys := make([]string, 0, len(states))
	for k := range states {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		out = append(out, absent(k, states[k].Group, states[k].State, cfg, absentIsZero, now, windowEnd))
	}
	return out
}

func absent(key string, g spec.Group, prev state.Group, cfg state.Config, absentIsZero bool,
	now, windowEnd time.Time) groupStep {
	if !prev.LastData.IsZero() && windowEnd.Sub(prev.LastData) >= GroupTTL {
		return groupStep{Key: key, Group: g, Prev: prev, Next: prev, Res: state.Result{From: prev.Status, To: prev.Status},
			Value: math.NaN(), Deleted: true}
	}
	obs := state.Observation{Now: now, WindowEnd: windowEnd}
	v := math.NaN()
	if absentIsZero {
		// Zero, without moving LastData: no line matched, which is a value,
		// but not new data — so the group still ages out once it has been
		// quiet for GroupTTL.
		obs.Present, obs.Value, v = true, 0, 0
	}
	next, res := state.Step(prev, obs, cfg)
	return groupStep{Key: key, Group: g, Prev: prev, Next: next, Res: res, Value: v}
}
