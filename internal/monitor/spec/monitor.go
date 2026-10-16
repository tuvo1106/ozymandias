package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"
)

// Type is what a monitor watches.
type Type string

// The monitor types.
const (
	TypeMetric Type = "metric"
	TypeLog    Type = "log"
)

// Defaults for options a definition leaves out.
const (
	DefaultIntervalS = 30
	DefaultDelayS    = 30
	// MaxMessageBytes bounds the message template. A message is a sentence
	// and a runbook link; the bound is what keeps a template's rendering cost
	// (see [Template]) small.
	MaxMessageBytes = 4000
)

// Monitor is a definition: what to watch, when it is bad, and who to tell.
//
// The JSON shape is the API's contract (docs/api.md#monitors) and the shape of
// a provisioned file in deploy/monitors/.
type Monitor struct {
	// UID is the stable identity a provisioned monitor is upserted by, as for
	// dashboards. API-created monitors may leave it empty.
	UID     string   `json:"uid,omitempty"`
	Name    string   `json:"name"`
	Type    Type     `json:"type"`
	Query   string   `json:"query"`
	Options Options  `json:"options"`
	Message string   `json:"message,omitempty"`
	Tags    []string `json:"tags,omitempty"`
}

// Options tune evaluation and notification. Pointers where zero is a
// meaningful value distinct from "not written" (a delay of 0 is a choice).
type Options struct {
	Thresholds Thresholds `json:"thresholds"`
	// EvaluationIntervalS is how often the monitor runs.
	EvaluationIntervalS int `json:"evaluation_interval_s,omitempty"`
	// EvaluationDelayS shifts the window back from now, so the newest
	// buckets — still being flushed by agents and forwarded — are not read
	// half-full. See docs/adr/0047-monitors-evaluate-behind-now.md.
	EvaluationDelayS *int `json:"evaluation_delay_s,omitempty"`
	// RequireFullWindow skips a group whose window has a gap, rather than
	// reducing what is there.
	RequireFullWindow bool `json:"require_full_window,omitempty"`
	// NotifyNoData turns a group that had data and stopped into NO_DATA after
	// NoDataTimeframeM minutes without a point.
	NotifyNoData     bool `json:"notify_no_data,omitempty"`
	NoDataTimeframeM int  `json:"no_data_timeframe_m,omitempty"`
	// RenotifyIntervalM re-sends while a group stays in ALERT or NO_DATA. Zero
	// is off.
	RenotifyIntervalM int `json:"renotify_interval_m,omitempty"`
	// MinConsecutiveFailures is how many evaluations in a row must breach
	// critical before ALERT. Default 1.
	MinConsecutiveFailures int `json:"min_consecutive_failures,omitempty"`
}

// Thresholds are the levels. Critical is required (it may be left out of the
// JSON, in which case it is the query's own threshold); the rest are optional.
type Thresholds struct {
	Critical         *float64 `json:"critical,omitempty"`
	Warning          *float64 `json:"warning,omitempty"`
	CriticalRecovery *float64 `json:"critical_recovery,omitempty"`
	WarningRecovery  *float64 `json:"warning_recovery,omitempty"`
}

// Levels are thresholds with every default applied: what the state machine
// reads. HasWarning says whether Warning and WarningRecovery mean anything.
type Levels struct {
	Op               Op
	Critical         float64
	CriticalRecovery float64
	HasWarning       bool
	Warning          float64
	WarningRecovery  float64
}

// Compiled is a validated definition with everything parsed once.
type Compiled struct {
	Monitor
	Parsed   Query
	Levels   Levels
	Template *Template
	Interval time.Duration
	Delay    time.Duration
	// NoDataTimeframe is zero when no-data notification is off.
	NoDataTimeframe time.Duration
	Renotify        time.Duration
	MinFailures     int
}

// Parse decodes and compiles a definition. Unknown fields are refused: a
// misspelt `notify_nodata` would otherwise be a monitor that silently never
// reports no-data.
func Parse(data []byte) (*Compiled, error) {
	var m Monitor
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("monitor definition: %w", err)
	}
	if dec.More() {
		return nil, errors.New("monitor definition: trailing data after the object")
	}
	return Compile(m)
}

var uidPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// Compile validates m and parses its query and message. Every error a later
// evaluation could hit for reasons of the definition alone is found here, so
// that a monitor which saves is a monitor which runs.
func Compile(m Monitor) (*Compiled, error) {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if m.UID != "" && !uidPattern.MatchString(m.UID) {
		bad("uid %q: want lower-case letters, digits, '.', '_' or '-', at most 128", m.UID)
	}
	if m.Name == "" || len(m.Name) > 200 {
		bad("name: want 1 to 200 bytes")
	}
	if len(m.Tags) > 32 {
		bad("tags: at most 32")
	}
	for _, t := range m.Tags {
		if t == "" || len(t) > 200 {
			bad("tag %q: want 1 to 200 bytes", t)
		}
	}
	c := &Compiled{Monitor: m}
	q, err := ParseQuery(m.Query)
	if err != nil {
		bad("query: %w", err)
	} else {
		c.Parsed = q
		switch {
		case m.Type == TypeMetric && q.Log != nil:
			bad("type is metric but the query is a log query")
		case m.Type == TypeLog && q.Metric != nil:
			bad("type is log but the query is a metric query")
		case m.Type != TypeMetric && m.Type != TypeLog:
			bad("type %q: want metric or log", m.Type)
		}
		lv, err := levels(q, m.Options.Thresholds)
		if err != nil {
			errs = append(errs, err)
		}
		c.Levels = lv
	}
	o := m.Options
	c.Interval = seconds(o.EvaluationIntervalS, DefaultIntervalS)
	if c.Interval < 10*time.Second || c.Interval > time.Hour {
		bad("options.evaluation_interval_s: want 10 to 3600")
	}
	c.Delay = DefaultDelayS * time.Second
	if o.EvaluationDelayS != nil {
		c.Delay = time.Duration(*o.EvaluationDelayS) * time.Second
		if *o.EvaluationDelayS < 0 || *o.EvaluationDelayS > 3600 {
			bad("options.evaluation_delay_s: want 0 to 3600")
		}
	}
	if o.NotifyNoData {
		if m.Type == TypeLog {
			// A log count of zero is an answer, not an absence: no matching
			// line in five minutes is exactly what a healthy error monitor
			// sees. "The logs stopped" is a monitor on a log volume metric.
			bad("options.notify_no_data: a log monitor counts lines, and zero lines is a value, not missing data")
		}
		mins := o.NoDataTimeframeM
		if mins == 0 {
			mins = int(2 * q.Window / time.Minute)
		}
		if mins < 1 || mins > 1440 {
			bad("options.no_data_timeframe_m: want 1 to 1440")
		}
		c.NoDataTimeframe = time.Duration(mins) * time.Minute
	} else if o.NoDataTimeframeM != 0 {
		bad("options.no_data_timeframe_m is set but notify_no_data is not")
	}
	if o.RenotifyIntervalM < 0 || o.RenotifyIntervalM > 10080 {
		bad("options.renotify_interval_m: want 0 (off) to 10080")
	}
	c.Renotify = time.Duration(o.RenotifyIntervalM) * time.Minute
	c.MinFailures = o.MinConsecutiveFailures
	if c.MinFailures == 0 {
		c.MinFailures = 1
	}
	if c.MinFailures < 1 || c.MinFailures > 100 {
		bad("options.min_consecutive_failures: want 1 to 100")
	}
	if len(m.Message) > MaxMessageBytes {
		bad("message: %d bytes; the limit is %d", len(m.Message), MaxMessageBytes)
	} else if t, err := CompileTemplate(m.Message); err != nil {
		bad("message: %w", err)
	} else {
		c.Template = t
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return c, nil
}

func seconds(v, def int) time.Duration {
	if v == 0 {
		v = def
	}
	return time.Duration(v) * time.Second
}

// levels fills the threshold defaults and checks that each level sits on the
// side of the others its direction requires.
//
// The rules, for a monitor that alerts on high values (`>`, `>=`); a low-value
// monitor mirrors every one:
//
//   - critical equals the query's threshold. Two numbers that must agree are
//     one number written twice, and the one place they could disagree is the
//     place nobody looks when the monitor does not fire.
//   - warning < critical. A warning at or past critical would never be seen.
//   - critical_recovery <= critical. Recovery *below* critical is the
//     hysteresis band: the value must fall past it to leave ALERT, so a value
//     hovering at the threshold does not flap. Above critical, the monitor
//     would recover while still breaching.
//   - warning_recovery <= warning, for the same reason, and needs a warning.
//
// `==` and `!=` have no direction, so nothing can be "past" them: they take
// no warning and no recovery levels.
func levels(q Query, t Thresholds) (Levels, error) {
	lv := Levels{Op: q.Op, Critical: q.Threshold}
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	finite := func(name string, p *float64) {
		if p != nil && (math.IsNaN(*p) || math.IsInf(*p, 0)) {
			bad("thresholds.%s is not finite", name)
		}
	}
	finite("critical", t.Critical)
	finite("warning", t.Warning)
	finite("critical_recovery", t.CriticalRecovery)
	finite("warning_recovery", t.WarningRecovery)
	if t.Critical != nil && *t.Critical != q.Threshold {
		bad("thresholds.critical (%g) must equal the query's threshold (%g)", *t.Critical, q.Threshold)
	}
	lv.CriticalRecovery = lv.Critical
	if t.CriticalRecovery != nil {
		lv.CriticalRecovery = *t.CriticalRecovery
	}
	if t.Warning != nil {
		lv.HasWarning, lv.Warning, lv.WarningRecovery = true, *t.Warning, *t.Warning
	}
	if t.WarningRecovery != nil {
		if t.Warning == nil {
			bad("thresholds.warning_recovery needs a thresholds.warning")
		}
		lv.WarningRecovery = *t.WarningRecovery
	}
	if !q.Op.Above() && !q.Op.Below() {
		if t.Warning != nil || t.CriticalRecovery != nil || t.WarningRecovery != nil {
			bad("a %s monitor has no direction, so it takes only a critical threshold", q.Op)
		}
		return lv, errors.Join(errs...)
	}
	// before(a, b): a is on the healthy side of b, or equal.
	before := func(a, b float64) bool { return a <= b }
	side, past := "below", "above"
	if q.Op.Below() {
		before = func(a, b float64) bool { return a >= b }
		side, past = "above", "below"
	}
	if lv.HasWarning && (lv.Warning == lv.Critical || !before(lv.Warning, lv.Critical)) {
		bad("thresholds.warning (%g) must be %s critical (%g)", lv.Warning, side, lv.Critical)
	}
	if !before(lv.CriticalRecovery, lv.Critical) {
		bad("thresholds.critical_recovery (%g) is %s critical (%g): it would recover while still breaching", lv.CriticalRecovery, past, lv.Critical)
	}
	if lv.HasWarning && !before(lv.WarningRecovery, lv.Warning) {
		bad("thresholds.warning_recovery (%g) is %s warning (%g): it would recover while still breaching", lv.WarningRecovery, past, lv.Warning)
	}
	return lv, errors.Join(errs...)
}

// Breach reports whether v breaches level in this monitor's direction.
func (l Levels) Breach(v, level float64) bool { return l.Op.Compare(v, level) }
