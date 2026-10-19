package spec

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func f(v float64) *float64 { return &v }
func i(v int) *int         { return &v }

const example = `{
  "uid": "shop-job-backlog",
  "name": "Judge queue is backing up",
  "type": "metric",
  "query": "avg(last_5m):avg:jobs.queue.depth{service:shop-api} > 20",
  "options": {
    "thresholds": {"critical": 20, "warning": 10, "critical_recovery": 15, "warning_recovery": 8},
    "evaluation_interval_s": 30,
    "evaluation_delay_s": 30,
    "require_full_window": false,
    "notify_no_data": true, "no_data_timeframe_m": 10,
    "renotify_interval_m": 60,
    "min_consecutive_failures": 1
  },
  "message": "Queue depth is {{.Value}} (threshold {{.Threshold}}) for {{.Group}}.\n{{if .IsAlert}}Check worker health.{{end}} @discord-ops",
  "tags": ["team:me", "app:shop"]
}`

func TestParseSpecExample(t *testing.T) {
	c, err := Parse([]byte(example))
	if err != nil {
		t.Fatal(err)
	}
	want := Levels{Op: OpGT, Critical: 20, CriticalRecovery: 15, HasWarning: true, Warning: 10, WarningRecovery: 8}
	if c.Levels != want {
		t.Errorf("levels %+v, want %+v", c.Levels, want)
	}
	if c.Interval != 30*time.Second || c.Delay != 30*time.Second || c.NoDataTimeframe != 10*time.Minute ||
		c.Renotify != time.Hour || c.MinFailures != 1 {
		t.Errorf("options %v %v %v %v %d", c.Interval, c.Delay, c.NoDataTimeframe, c.Renotify, c.MinFailures)
	}
	if got := mentions(c.Message); len(got) != 1 || got[0] != "discord-ops" {
		t.Errorf("mentions %v", got)
	}
}

func TestDefaults(t *testing.T) {
	c, err := Compile(Monitor{Name: "n", Type: TypeMetric, Query: "avg(last_5m):avg:x{*} > 1",
		Options: Options{NotifyNoData: true}})
	if err != nil {
		t.Fatal(err)
	}
	if c.Interval != DefaultIntervalS*time.Second || c.Delay != DefaultDelayS*time.Second ||
		c.MinFailures != 1 || c.Renotify != 0 {
		t.Errorf("defaults %v %v %d %v", c.Interval, c.Delay, c.MinFailures, c.Renotify)
	}
	// No-data timeframe defaults to twice the window.
	if c.NoDataTimeframe != 10*time.Minute {
		t.Errorf("no-data timeframe %v", c.NoDataTimeframe)
	}
	if c.Levels != (Levels{Op: OpGT, Critical: 1, CriticalRecovery: 1}) {
		t.Errorf("levels %+v", c.Levels)
	}
	// A delay of zero is a choice, not "unset".
	c, err = Compile(Monitor{Name: "n", Type: TypeMetric, Query: "avg(last_5m):avg:x{*} > 1",
		Options: Options{EvaluationDelayS: i(0)}})
	if err != nil || c.Delay != 0 || c.NoDataTimeframe != 0 {
		t.Fatalf("delay 0: %v %v", c, err)
	}
}

// TestThresholdValidation walks the direction-aware rules for both directions.
func TestThresholdValidation(t *testing.T) {
	type th = Thresholds
	cases := []struct {
		q    string
		th   Thresholds
		want string // "" = valid
	}{
		{"> 20", th{}, ""},
		{"> 20", th{Critical: f(20), Warning: f(10), CriticalRecovery: f(15), WarningRecovery: f(8)}, ""},
		{"> 20", th{Critical: f(21)}, "must equal the query's threshold"},
		{"> 20", th{Warning: f(20)}, "must be below critical"},
		{"> 20", th{Warning: f(25)}, "must be below critical"},
		{"> 20", th{CriticalRecovery: f(25)}, "recover while still breaching"},
		{"> 20", th{CriticalRecovery: f(20)}, ""},
		{"> 20", th{Warning: f(10), WarningRecovery: f(12)}, "recover while still breaching"},
		{"> 20", th{WarningRecovery: f(5)}, "needs a thresholds.warning"},
		{">= 20", th{Warning: f(10), WarningRecovery: f(10)}, ""},
		// Mirrored: a low-value monitor's levels sit above it.
		{"< 3", th{Warning: f(5), CriticalRecovery: f(4), WarningRecovery: f(7)}, ""},
		{"< 3", th{Warning: f(1)}, "must be above critical"},
		{"< 3", th{CriticalRecovery: f(2)}, "recover while still breaching"},
		{"<= 3", th{Warning: f(5), WarningRecovery: f(4)}, "recover while still breaching"},
		// No direction, no other levels.
		{"== 0", th{}, ""},
		{"!= 0", th{Warning: f(1)}, "no direction"},
		{"== 0", th{CriticalRecovery: f(1)}, "no direction"},
	}
	for _, c := range cases {
		_, err := Compile(Monitor{Name: "n", Type: TypeMetric, Query: "avg(last_5m):avg:x{*} " + c.q,
			Options: Options{Thresholds: c.th}})
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s %+v: %v", c.q, c.th, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s %+v: error %v, want %q", c.q, c.th, err, c.want)
		}
	}
}

func TestCompileErrors(t *testing.T) {
	ok := func() Monitor {
		return Monitor{Name: "n", Type: TypeMetric, Query: "avg(last_5m):avg:x{*} > 1"}
	}
	cases := []struct {
		mod  func(*Monitor)
		want string
	}{
		{func(m *Monitor) { m.Name = "" }, "name"},
		{func(m *Monitor) { m.UID = "Has Spaces" }, "uid"},
		{func(m *Monitor) { m.Type = "trace" }, "want metric or log"},
		{func(m *Monitor) { m.Type = TypeLog }, "type is log but the query is a metric query"},
		{func(m *Monitor) {
			m.Query = `logs("x").rollup("count").last("5m") > 1`
		}, "type is metric but the query is a log query"},
		{func(m *Monitor) {
			m.Type, m.Query, m.Options.NotifyNoData = TypeLog, `logs("x").rollup("count").last("5m") > 1`, true
		}, "zero lines is a value"},
		{func(m *Monitor) { m.Query = "nonsense" }, "query"},
		{func(m *Monitor) { m.Options.EvaluationIntervalS = 5 }, "evaluation_interval_s"},
		{func(m *Monitor) { m.Options.EvaluationIntervalS = 3601 }, "evaluation_interval_s"},
		{func(m *Monitor) { m.Options.EvaluationDelayS = i(-1) }, "evaluation_delay_s"},
		{func(m *Monitor) { m.Options.NoDataTimeframeM = 5 }, "notify_no_data is not"},
		{func(m *Monitor) { m.Options.NotifyNoData, m.Options.NoDataTimeframeM = true, 2000 }, "no_data_timeframe_m"},
		{func(m *Monitor) { m.Options.RenotifyIntervalM = -1 }, "renotify_interval_m"},
		{func(m *Monitor) { m.Options.MinConsecutiveFailures = 101 }, "min_consecutive_failures"},
		{func(m *Monitor) { m.Message = strings.Repeat("x", MaxMessageBytes+1) }, "limit"},
		{func(m *Monitor) { m.Message = "{{.Valu}}" }, "Valu"},
		{func(m *Monitor) { m.Message = "{{if}}" }, "message"},
		{func(m *Monitor) { m.Tags = []string{""} }, "tag"},
	}
	for idx, c := range cases {
		m := ok()
		c.mod(&m)
		_, err := Compile(m)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("case %d: error %v, want %q", idx, err, c.want)
		}
	}
	if _, err := Compile(ok()); err != nil {
		t.Fatalf("baseline: %v", err)
	}
}

func TestParseRefusesUnknownFields(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal([]byte(example), &m); err != nil {
		t.Fatal(err)
	}
	m["options"].(map[string]any)["notify_nodata"] = true
	b, _ := json.Marshal(m)
	if _, err := Parse(b); err == nil || !strings.Contains(err.Error(), "notify_nodata") {
		t.Fatalf("misspelt option: %v", err)
	}
	if _, err := Parse([]byte(example + " {}")); err == nil {
		t.Fatal("trailing data accepted")
	}
}

func TestTemplate(t *testing.T) {
	tm, err := CompileTemplate(`{{.Monitor}} {{.Status}} from {{.PrevStatus}} on {{.Group}} ({{.Group.service}}{{.Group.missing}}): {{printf "%.1f" .Value}} vs {{.Threshold}}{{if .IsRecovery}} recovered{{end}}`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tm.Render(Data{Monitor: Ref{Name: "m"}, Group: Group{"service": "api", "env": "dev"}, Status: "ALERT",
		PrevStatus: "OK", Value: 21.25, Threshold: 20})
	if err != nil {
		t.Fatal(err)
	}
	if want := "m ALERT from OK on env:dev,service:api (api): 21.2 vs 20"; got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if got, _ := tm.Render(Data{Group: Group{}}); !strings.Contains(got, " on * (") {
		t.Errorf("ungrouped: %q", got)
	}
	for _, bad := range []string{
		"{{range 1000000000}}x{{end}}",
		`{{define "x"}}y{{end}}`,
		`{{if .IsAlert}}{{range 3}}{{end}}{{end}}`,
		`{{with .Monitor}}{{range 3}}{{end}}{{end}}`,
		`{{template "message"}}`,
		`{{.Nope}}`,
		`{{if .IsNoData}}{{.Nope}}{{end}}`, // proved against every status
	} {
		if _, err := CompileTemplate(bad); err == nil {
			t.Errorf("%q compiled", bad)
		}
	}
	huge, err := CompileTemplate(`{{printf "%0900000d" 1}}`)
	if err == nil {
		t.Fatalf("a template that always renders past the bound compiled: %v", huge)
	}
}

func TestMentions(t *testing.T) {
	for in, want := range map[string]string{
		"@discord-ops page":            "discord-ops",
		"hey @a and @b, then @a again": "a b",
		"mail ops@example.com please":  "",
		"line\n@hook\t@me_2":           "hook me_2",
		"{{if .IsAlert}}@pager{{end}}": "",  // in a condition: refused at compile, never a recipient
		"{{.Value}} @b c@d":            "b", // whitespace before @ inside a text node
		"{{/* @c */}} {{\" @d\"}}":     "",  // comments and literals are not text
	} {
		if got := strings.Join(mentions(in), " "); got != want {
			t.Errorf("mentions(%q) = %q, want %q", in, got, want)
		}
	}
}

func ExampleParseQuery() {
	q, _ := ParseQuery("avg(last_5m):avg:jobs.queue.depth{service:api} by {queue} > 20")
	fmt.Println(q.Metric.Agg, q.Window, q.Op, q.Threshold, q.Grouped())
	// Output: avg 5m0s > 20 true
}

// TestRecipients: a message goes to every handle in its literal text, and
// to nothing a comment, a string literal or data mentions.
func TestRecipients(t *testing.T) {
	tm, err := CompileTemplate(`@ops {{.Group.user}}{{/* @pager is off */}}{{if eq .Group.user " @x"}}admin{{end}} oncall@example.com @log {{.Value}} @cap`)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tm.Recipients(), " "); got != "ops log cap" {
		t.Fatalf("recipients %q", got)
	}
	// Data is not escaped: it renders, and compares, as it is.
	text, err := tm.Render(Data{Group: Group{"user": "a@b @pager"}, Value: 1, Status: "OK"})
	if err != nil || text != "@ops a@b @pager oncall@example.com @log 1 @cap" {
		t.Fatalf("%q %v", text, err)
	}
}

// TestConditionalMentionsAreRefused: a mention inside a condition would be
// sent for every status while reading as if it were not.
func TestConditionalMentionsAreRefused(t *testing.T) {
	for _, tmpl := range []string{
		"{{if .IsAlert}}@pager{{end}}",
		"{{if .IsAlert}}x{{else}}page @pager{{end}}",
		"{{with .Group.q}}{{if .}} @pager{{end}}{{end}}",
		"{{with .Group.q}} @pager{{end}}",
	} {
		if _, err := CompileTemplate(tmpl); err == nil || !strings.Contains(err.Error(), "@pager is inside") {
			t.Errorf("%q: %v", tmpl, err)
		}
	}
	if _, err := CompileTemplate("{{if .IsAlert}}mail ops@example.com{{end}} @log"); err != nil {
		t.Errorf("an address in a condition: %v", err)
	}
}

// TestMentionAfterAnAction: text straight after an action that prints is
// part of what it printed, so it builds an address rather than a mention; a
// comment prints nothing, so the text around it reads as one.
func TestMentionAfterAnAction(t *testing.T) {
	for tmpl, want := range map[string]string{
		"owner {{.Group.owner}}@example.com @log": "log",
		"{{.Value}}@cap":            "",
		"page {{/* note */}}@pager": "pager",
		"{{/* c */}}@pager hi":      "pager",
		`x{{""}}@pager`:             "",
	} {
		tm, err := CompileTemplate(tmpl)
		if err != nil {
			t.Fatalf("%q: %v", tmpl, err)
		}
		if got := strings.Join(tm.Recipients(), " "); got != want {
			t.Errorf("%q: %q, want %q", tmpl, got, want)
		}
	}
	for _, tmpl := range []string{
		"{{if .IsAlert}}{{if .IsAlert}} @pager{{end}}{{end}}",
		"{{if .IsAlert}} @pager{{end}}{{if .IsWarning}} @pager{{end}}",
	} {
		_, err := CompileTemplate(tmpl)
		if err == nil || strings.Count(err.Error(), "@pager is inside") != 1 {
			t.Errorf("%q: %v", tmpl, err)
		}
	}
	// In a condition, an address built after an action is still fine.
	if _, err := CompileTemplate("{{if .IsAlert}}mail {{.Group.owner}}@example.com{{end}} @log"); err != nil {
		t.Errorf("address in a condition: %v", err)
	}
}

// mentions is who a template's messages go to, or nil if it is refused.
func mentions(text string) []string {
	tm, err := CompileTemplate(text)
	if err != nil {
		return nil
	}
	return tm.Recipients()
}
