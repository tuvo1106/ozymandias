package state

import (
	"math"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/tuvo1106/ozymandias/internal/monitor/spec"
)

var t0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// above is the spec's example: alert above 20, warn above 10, recover from
// critical at 15 and from warning at 8.
var above = spec.Levels{Op: spec.OpGT, Critical: 20, CriticalRecovery: 15, HasWarning: true, Warning: 10, WarningRecovery: 8}

// below mirrors it: alert below 3, warn below 5, recover at 4 and 7.
var below = spec.Levels{Op: spec.OpLT, Critical: 3, CriticalRecovery: 4, HasWarning: true, Warning: 5, WarningRecovery: 7}

// run feeds values (NaN = no data) at 30s steps and returns the status after
// each and the notifications in order.
func run(t *testing.T, cfg Config, values ...float64) ([]Status, []Result) {
	t.Helper()
	g := New(t0)
	var statuses []Status
	var notes []Result
	for i, v := range values {
		now := t0.Add(time.Duration(i+1) * 30 * time.Second)
		obs := Observation{Now: now, WindowEnd: now.Add(-30 * time.Second), Present: !math.IsNaN(v), Value: v,
			DataTs: now.Add(-30 * time.Second)}
		var res Result
		g, res = Step(g, obs, cfg)
		statuses = append(statuses, g.Status)
		if res.Notify != NotifyNone {
			notes = append(notes, res)
		}
	}
	return statuses, notes
}

func eq(t *testing.T, name string, got, want []Status) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", name, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v, want %v (first difference at %d)", name, got, want, i)
		}
	}
}

// TestTransitionTable walks every row of the spec's table, for both
// directions.
func TestTransitionTable(t *testing.T) {
	nan := math.NaN()
	cases := []struct {
		name   string
		lv     spec.Levels
		values []float64
		want   []Status
	}{
		{"OK->WARN on warning", above, []float64{5, 11}, []Status{OK, Warn}},
		{"OK->ALERT on critical", above, []float64{5, 25}, []Status{OK, Alert}},
		{"WARN->ALERT", above, []float64{11, 25}, []Status{Warn, Alert}},
		{"ALERT holds in the critical band", above, []float64{25, 19, 16, 15.5}, []Status{Alert, Alert, Alert, Alert}},
		{"ALERT->WARN past critical_recovery, still warning", above, []float64{25, 12}, []Status{Alert, Warn}},
		{"ALERT->WARN past critical_recovery, in warning band", above, []float64{25, 9}, []Status{Alert, Warn}},
		{"ALERT->OK past both recoveries", above, []float64{25, 8}, []Status{Alert, OK}},
		{"WARN holds in the warning band", above, []float64{11, 9, 8.5}, []Status{Warn, Warn, Warn}},
		{"WARN->OK past warning_recovery", above, []float64{11, 7}, []Status{Warn, OK}},
		{"OK stays OK in the warning band", above, []float64{9, 9.9}, []Status{OK, OK}},
		{"recovery is exact at the level", above, []float64{25, 15}, []Status{Alert, Warn}},

		{"below: OK->WARN", below, []float64{10, 4.5}, []Status{OK, Warn}},
		{"below: OK->ALERT", below, []float64{10, 2}, []Status{OK, Alert}},
		{"below: ALERT holds above critical, below recovery", below, []float64{2, 3.5, 3.9}, []Status{Alert, Alert, Alert}},
		{"below: ALERT->WARN", below, []float64{2, 4.5}, []Status{Alert, Warn}},
		{"below: ALERT->OK", below, []float64{2, 8}, []Status{Alert, OK}},
		{"below: WARN holds in band", below, []float64{4.5, 6, 6.9}, []Status{Warn, Warn, Warn}},
		{"below: WARN->OK", below, []float64{4.5, 7}, []Status{Warn, OK}},

		{"no data keeps status inside the timeframe", above, []float64{25, nan, nan}, []Status{Alert, Alert, Alert}},
		{"no data past the timeframe", above, []float64{25, nan, nan, nan, nan}, []Status{Alert, Alert, Alert, Alert, NoData}},
		{"NO_DATA exits on data, evaluated fresh", above, []float64{5, nan, nan, nan, nan, 25}, []Status{OK, OK, OK, OK, NoData, Alert}},
		{"NO_DATA exits to OK", above, []float64{5, nan, nan, nan, nan, 1}, []Status{OK, OK, OK, OK, NoData, OK}},
		{"never-seen group is never NO_DATA", above, []float64{nan, nan, nan, nan, nan, nan}, []Status{OK, OK, OK, OK, OK, OK}},
	}
	for _, c := range cases {
		cfg := Config{Levels: c.lv, MinFailures: 1, NoDataTimeframe: 2 * time.Minute}
		got, _ := run(t, cfg, c.values...)
		eq(t, c.name, got, c.want)
	}
}

func TestNoDataOff(t *testing.T) {
	nan := math.NaN()
	got, notes := run(t, Config{Levels: above, MinFailures: 1}, 25, nan, nan, nan, nan, nan, nan)
	eq(t, "no-data off", got, []Status{Alert, Alert, Alert, Alert, Alert, Alert, Alert})
	if len(notes) != 1 {
		t.Fatalf("notifications %v", notes)
	}
}

func TestMinConsecutiveFailures(t *testing.T) {
	noWarn := spec.Levels{Op: spec.OpGT, Critical: 20, CriticalRecovery: 20}
	cfg := Config{Levels: noWarn, MinFailures: 3}
	got, _ := run(t, cfg, 25, 25, 5, 25, 25, 25, 25)
	eq(t, "needs three in a row", got, []Status{OK, OK, OK, OK, OK, Alert, Alert})
	// With a warning level, the breaches before ALERT show as WARN.
	got, _ = run(t, Config{Levels: above, MinFailures: 2}, 25, 25)
	eq(t, "warn while counting", got, []Status{Warn, Alert})
	// NO_DATA resets the count.
	got, _ = run(t, Config{Levels: noWarn, MinFailures: 2, NoDataTimeframe: time.Minute}, 5, 25, math.NaN(), math.NaN(), 25, 25)
	eq(t, "no-data resets", got, []Status{OK, OK, OK, NoData, OK, Alert})
}

// TestHysteresisDoesNotFlap is the acceptance criterion: a value oscillating
// inside the band notifies nothing after the first alert.
func TestHysteresisDoesNotFlap(t *testing.T) {
	vals := []float64{25}
	for i := 0; i < 200; i++ {
		vals = append(vals, 16, 21, 19, 24, 15.01)
	}
	_, notes := run(t, Config{Levels: above, MinFailures: 1, Renotify: 0}, vals...)
	if len(notes) != 1 || notes[0].To != Alert {
		t.Fatalf("notifications %v, want one ALERT", notes)
	}
	// Without the band (recovery == critical) the same values flap.
	flappy := spec.Levels{Op: spec.OpGT, Critical: 20, CriticalRecovery: 20}
	_, notes = run(t, Config{Levels: flappy, MinFailures: 1}, vals...)
	if len(notes) < 100 {
		t.Fatalf("without hysteresis only %d notifications: the test above pins nothing", len(notes))
	}
}

func TestRenotify(t *testing.T) {
	cfg := Config{Levels: above, MinFailures: 1, Renotify: 2 * time.Minute}
	vals := make([]float64, 12)
	for i := range vals {
		vals[i] = 25
	}
	_, notes := run(t, cfg, vals...)
	// Alert at step 1 (t+30s), then every 2m: t+2m30 and t+4m30; the last step is t+6m.
	if len(notes) != 3 || notes[0].Notify != NotifyTransition {
		t.Fatalf("notes %v", notes)
	}
	for _, n := range notes[1:] {
		if n.Notify != NotifyRenotify || n.From != Alert || n.To != Alert {
			t.Fatalf("renotify %v", n)
		}
	}
	// WARN does not renotify.
	vals = make([]float64, 12)
	for i := range vals {
		vals[i] = 12
	}
	if _, notes = run(t, cfg, vals...); len(notes) != 1 {
		t.Fatalf("WARN renotified: %v", notes)
	}
}

func TestMuteSuppressesNotifyButAdvancesState(t *testing.T) {
	got, notes := run(t, Config{Levels: above, MinFailures: 1, Muted: true, Renotify: time.Minute}, 25, 25, 25, 1)
	eq(t, "muted", got, []Status{Alert, Alert, Alert, OK})
	if len(notes) != 0 {
		t.Fatalf("muted monitor notified: %v", notes)
	}
}

// TestRenotifyResumesFromPersistedState: a reloaded state carries
// LastNotified, so the timer continues rather than restarting.
func TestRenotifyResumesFromPersistedState(t *testing.T) {
	cfg := Config{Levels: above, MinFailures: 1, Renotify: 10 * time.Minute}
	g := New(t0)
	g, res := Step(g, Observation{Now: t0, WindowEnd: t0, Present: true, Value: 25, DataTs: t0}, cfg)
	if res.Notify != NotifyTransition {
		t.Fatal(res)
	}
	reloaded := g // what the database gives back
	for _, d := range []time.Duration{time.Minute, 5 * time.Minute, 9 * time.Minute} {
		now := t0.Add(d)
		var r Result
		reloaded, r = Step(reloaded, Observation{Now: now, WindowEnd: now, Present: true, Value: 25, DataTs: now}, cfg)
		if r.Notify != NotifyNone {
			t.Fatalf("at +%v: %v (a duplicate)", d, r)
		}
	}
	now := t0.Add(10 * time.Minute)
	if _, r := Step(reloaded, Observation{Now: now, WindowEnd: now, Present: true, Value: 25, DataTs: now}, cfg); r.Notify != NotifyRenotify {
		t.Fatalf("at +10m: %v", r)
	}
}

// genLevels draws a valid set of levels in either direction.
func genLevels(t *rapid.T) spec.Levels {
	crit := rapid.Float64Range(-100, 100).Draw(t, "crit")
	gap := func(name string) float64 { return rapid.Float64Range(0, 50).Draw(t, name) }
	sign := 1.0
	op := rapid.SampledFrom([]spec.Op{spec.OpGT, spec.OpGE, spec.OpLT, spec.OpLE}).Draw(t, "op")
	if op.Below() {
		sign = -1
	}
	lv := spec.Levels{Op: op, Critical: crit, CriticalRecovery: crit - sign*gap("crec")}
	if rapid.Bool().Draw(t, "warn") {
		lv.HasWarning = true
		lv.Warning = crit - sign*(gap("w")+0.001)
		lv.WarningRecovery = lv.Warning - sign*gap("wrec")
	}
	return lv
}

// TestStepProperties is L2: for any value sequence,
// (a) notifications fire only on transitions or renotify,
// (b) the status is a pure function of (previous state, value, options) —
//
//	replaying the same sequence yields the same results.
func TestStepProperties(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		cfg := Config{
			Levels:          genLevels(t),
			MinFailures:     rapid.IntRange(1, 4).Draw(t, "minfail"),
			NoDataTimeframe: time.Duration(rapid.IntRange(0, 5).Draw(t, "nodata")) * time.Minute,
			Renotify:        time.Duration(rapid.IntRange(0, 5).Draw(t, "renotify")) * time.Minute,
			Muted:           rapid.Bool().Draw(t, "muted"),
		}
		n := rapid.IntRange(1, 60).Draw(t, "n")
		obs := make([]Observation, n)
		for i := range obs {
			now := t0.Add(time.Duration(i+1) * 30 * time.Second)
			obs[i] = Observation{Now: now, WindowEnd: now.Add(-30 * time.Second), DataTs: now.Add(-40 * time.Second),
				Present: rapid.IntRange(0, 4).Draw(t, "present") > 0,
				Value:   rapid.Float64Range(-200, 200).Draw(t, "v")}
		}
		play := func() ([]Group, []Result) {
			g := New(t0)
			var gs []Group
			var rs []Result
			for _, o := range obs {
				var r Result
				g, r = Step(g, o, cfg)
				gs, rs = append(gs, g), append(rs, r)
			}
			return gs, rs
		}
		g1, r1 := play()
		g2, r2 := play()
		for i := range r1 {
			if r1[i] != r2[i] || g1[i].Status != g2[i].Status || g1[i].Breaches != g2[i].Breaches {
				t.Fatalf("replay differs at %d: %v vs %v", i, r1[i], r2[i])
			}
			r := r1[i]
			if !r.To.Valid() {
				t.Fatalf("status %q", r.To)
			}
			switch r.Notify {
			case NotifyTransition:
				if !r.Changed() || cfg.Muted {
					t.Fatalf("transition notify without a transition: %v", r)
				}
			case NotifyRenotify:
				if r.Changed() || cfg.Muted || cfg.Renotify == 0 || (r.To != Alert && r.To != NoData) {
					t.Fatalf("bad renotify: %v cfg %+v", r, cfg)
				}
			case NotifyNone:
				if r.Changed() && !cfg.Muted {
					t.Fatalf("transition without notification: %v", r)
				}
			}
			// NO_DATA only after data was seen, only with the option on.
			if r.To == NoData && (cfg.NoDataTimeframe == 0 || obs[i].Present) {
				t.Fatalf("NO_DATA at %d with cfg %+v, present %v", i, cfg, obs[i].Present)
			}
			// ALERT is never entered from a value that does not breach critical.
			if r.Changed() && r.To == Alert && !cfg.Levels.Breach(obs[i].Value, cfg.Levels.Critical) {
				t.Fatalf("entered ALERT at %g with %+v", obs[i].Value, cfg.Levels)
			}
			// Leaving ALERT for OK only past every recovery level.
			if r.From == Alert && r.To == OK && obs[i].Present {
				v := obs[i].Value
				if cfg.Levels.Breach(v, cfg.Levels.CriticalRecovery) ||
					(cfg.Levels.HasWarning && cfg.Levels.Breach(v, cfg.Levels.WarningRecovery)) {
					t.Fatalf("ALERT->OK at %g inside a recovery band %+v", v, cfg.Levels)
				}
			}
		}
	})
}
