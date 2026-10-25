package dashboard

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/tuvo1106/ozymandias/internal/query/metricql"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Limits on a definition. These are not about taste — a 40-widget dashboard is
// somebody's problem but not ours. They bound what one stored row can cost the
// server that has to evaluate it: a dashboard is a single request to
// /api/v1/query/batch, and every widget in it is queries the server runs.
const (
	MaxWidgets          = 100
	MaxQueriesPerWidget = 10
	MaxTemplateVars     = 10
	// MaxTitle and MaxDescription keep a definition from becoming a document
	// store. Generous on purpose: a title is a sentence, not a tweet.
	MaxTitle       = 200
	MaxDescription = 2000
	// MaxMarkdown is a note's body. Long enough for a runbook excerpt, short
	// enough that a dashboard row stays a row.
	MaxMarkdown = 16 << 10
)

// ErrInvalid is what every refusal in this package wraps, so an HTTP layer can
// answer 400 for a bad definition without matching on message text — the same
// reason [eval.ErrBadQuery] exists.
var ErrInvalid = errors.New("invalid dashboard")

func invalidf(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Validate reports whether this definition is worth storing.
//
// It collects every problem rather than stopping at the first, because the
// caller is usually a person editing JSON: telling them about one missing
// field at a time turns one fix into six round trips. The errors are joined,
// so the message names all of them.
//
// What it does *not* check is anything that depends on the data — see the
// package comment. A query that parses is valid even if the metric does not
// exist yet, because the dashboard for a service is often written before the
// service ships.
func (d *Dashboard) Validate() error {
	var errs []error

	if strings.TrimSpace(d.Title) == "" {
		errs = append(errs, invalidf("title is required"))
	}
	if len(d.Title) > MaxTitle {
		errs = append(errs, invalidf("title is %d bytes; the limit is %d", len(d.Title), MaxTitle))
	}
	if len(d.Description) > MaxDescription {
		errs = append(errs, invalidf("description is %d bytes; the limit is %d", len(d.Description), MaxDescription))
	}
	if d.UID != "" && !validUID(d.UID) {
		errs = append(errs, invalidf(
			"uid %q: letters, digits, '-' and '_' only, at most 64 of them — it goes in a URL", d.UID))
	}

	declared := map[string]bool{}
	for i, v := range d.TemplateVars {
		name := strings.ToLower(strings.TrimSpace(v.Name))
		switch {
		case name == "":
			errs = append(errs, invalidf("template_vars[%d]: name is required", i))
			continue
		case !validVarName(name):
			errs = append(errs, invalidf(
				"template_vars[%d]: %q is not a variable name (a letter, then letters, digits, '_' or '.')", i, v.Name))
			continue
		case declared[name]:
			errs = append(errs, invalidf("template_vars[%d]: %q is declared twice", i, name))
			continue
		}
		// The tag key is what the selector lists values of, so it has to be a
		// key the store could actually have. Lower-cased first, as the intake
		// and the lexer both do.
		tag := strings.ToLower(strings.TrimSpace(v.Tag))
		if tag == "" {
			errs = append(errs, invalidf("template_vars[%d] (%s): tag is required — it is the key the selector offers values of", i, name))
		} else if !wire.ValidTag(tag) {
			errs = append(errs, invalidf("template_vars[%d] (%s): %q is not a tag key", i, name, v.Tag))
		}
		declared[name] = true
	}
	if len(d.TemplateVars) > MaxTemplateVars {
		errs = append(errs, invalidf("%d template variables; the limit is %d", len(d.TemplateVars), MaxTemplateVars))
	}

	// A template is instantiated once per value of a variable, so it needs one
	// to be instantiated over. Without this check a "template": true dashboard
	// stores fine and then appears nowhere at all, which is the least
	// debuggable outcome available.
	if d.Template && !declared[TemplateVarService] {
		errs = append(errs, invalidf(
			"a template must declare a %q template variable: it is instantiated once per value of it", TemplateVarService))
	}

	if len(d.Widgets) == 0 {
		errs = append(errs, invalidf("a dashboard needs at least one widget"))
	}
	if len(d.Widgets) > MaxWidgets {
		errs = append(errs, invalidf("%d widgets; the limit is %d", len(d.Widgets), MaxWidgets))
	}
	seen := map[string]bool{}
	for i := range d.Widgets {
		w := &d.Widgets[i]
		where := fmt.Sprintf("widgets[%d]", i)
		if w.ID != "" {
			where = fmt.Sprintf("widgets[%d] (%s)", i, w.ID)
		}
		switch {
		case strings.TrimSpace(w.ID) == "":
			errs = append(errs, invalidf("%s: id is required", where))
		case seen[w.ID]:
			errs = append(errs, invalidf("%s: id %q is used twice", where, w.ID))
		default:
			seen[w.ID] = true
		}
		errs = append(errs, w.validate(where, declared)...)
	}
	return errors.Join(errs...)
}

// TemplateVarService is the variable a template dashboard is instantiated
// over. It is a constant rather than configuration because the URL it produces
// (/dashboards/service/<name>) has to be predictable for a link in a runbook
// to keep working.
const TemplateVarService = "service"

// validate checks one widget, returning every problem with it.
func (w *Widget) validate(where string, declared map[string]bool) []error {
	var errs []error

	if len(w.Title) > MaxTitle {
		errs = append(errs, invalidf("%s: title is %d bytes; the limit is %d", where, len(w.Title), MaxTitle))
	}
	errs = append(errs, w.Layout.validate(where)...)

	known := []Type{TypeTimeseries, TypeQueryValue, TypeToplist, TypeTable, TypeHeatmap, TypeNote,
		TypeMonitorSummary, TypeAlertValue, TypeServiceSummary}
	if !slices.Contains(known, w.Type) {
		strs := make([]string, len(known))
		for i, k := range known {
			strs[i] = string(k)
		}
		// Returning early: every check below is about a type, and "unknown
		// type" followed by six complaints derived from not knowing it is
		// noise, not help.
		return append(errs, invalidf("%s: type %q is not one of %s", where, w.Type, strings.Join(strs, ", ")))
	}

	// A note draws no data, so queries on one are not "extra" — they are a
	// sign the author believes this widget will show something it cannot.
	if w.Type == TypeNote {
		if strings.TrimSpace(w.Markdown) == "" {
			errs = append(errs, invalidf("%s: a note needs markdown", where))
		}
		if len(w.Markdown) > MaxMarkdown {
			errs = append(errs, invalidf("%s: markdown is %d bytes; the limit is %d", where, len(w.Markdown), MaxMarkdown))
		}
		if len(w.Queries) > 0 {
			errs = append(errs, invalidf("%s: a note has no queries", where))
		}
		// Returning here without this would exempt a note from the rule every
		// other type obeys, so a note carrying a limit, a precision, a yaxis
		// and conditional formats would validate — while the docs say a field
		// belonging to another type is an error. The exemption was an accident
		// of where the early return sat.
		errs = append(errs, w.foreignFields(where, "a note", false)...)
		return append(errs, w.feedFields(where, "")...)
	}
	if w.Markdown != "" {
		errs = append(errs, invalidf("%s: markdown belongs to a note, not a %s", where, w.Type))
	}
	if isFeedWidget(w.Type) {
		return append(errs, w.validateFeed(where)...)
	}
	errs = append(errs, w.feedFields(where, "")...)

	switch {
	case len(w.Queries) == 0:
		errs = append(errs, invalidf("%s: a %s needs at least one query", where, w.Type))
	case len(w.Queries) > MaxQueriesPerWidget:
		errs = append(errs, invalidf("%s: %d queries; the limit is %d", where, len(w.Queries), MaxQueriesPerWidget))
	}
	// A heatmap draws one distribution's bins. Two would have to be drawn over
	// each other, and a heatmap of two overlaid distributions is not readable
	// by anybody — better to refuse than to draw something misleading.
	if w.Type == TypeHeatmap && len(w.Queries) > 1 {
		errs = append(errs, invalidf("%s: a heatmap draws one distribution; use two widgets", where))
	}
	for i := range w.Queries {
		errs = append(errs, w.Queries[i].validate(fmt.Sprintf("%s query[%d]", where, i), w.Type, declared)...)
	}

	if w.Type == TypeToplist && (w.Limit < 0 || w.Limit > 100) {
		errs = append(errs, invalidf("%s: limit %d is outside 1…100 (0 means the default)", where, w.Limit))
	}
	if w.Type != TypeToplist && w.Limit != 0 {
		errs = append(errs, invalidf("%s: %s, not %s", where, limitBelongs, aType(w.Type)))
	}
	if w.Precision != nil {
		if w.Type != TypeQueryValue && w.Type != TypeTable {
			errs = append(errs, invalidf("%s: precision belongs to a query_value or a table, not a %s", where, w.Type))
		} else if *w.Precision < 0 || *w.Precision > 10 {
			errs = append(errs, invalidf("%s: precision %d is outside 0…10", where, *w.Precision))
		}
	}
	if len(w.ConditionalFormats) > 0 && w.Type != TypeQueryValue && w.Type != TypeTable {
		errs = append(errs, invalidf("%s: conditional_formats belong to a query_value or a table, not a %s", where, w.Type))
	}
	for i, cf := range w.ConditionalFormats {
		errs = append(errs, cf.validate(fmt.Sprintf("%s conditional_formats[%d]", where, i))...)
	}
	if w.YAxis != nil {
		errs = append(errs, w.YAxis.validate(where, w.Type)...)
	}
	return errs
}

// isFeedWidget is whether a widget draws from its own API rather than from
// metric queries: the monitor list, one monitor, or the APM tables.
func isFeedWidget(t Type) bool {
	return t == TypeMonitorSummary || t == TypeAlertValue || t == TypeServiceSummary
}

// monitorStatuses are the statuses a monitor_filter may name, as the monitor
// API spells them.
var monitorStatuses = []string{"OK", "WARN", "ALERT", "NO_DATA"}

// validateFeed checks a monitor_summary, alert_value or service_summary.
// None of them takes queries: what they show is chosen by their own field,
// and a query on one is a sign the author expects it to draw something it
// will not.
func (w *Widget) validateFeed(where string) []error {
	var errs []error
	if len(w.Queries) > 0 {
		errs = append(errs, invalidf("%s: a %s has no queries", where, w.Type))
	}
	takesLimit := w.Type != TypeAlertValue
	errs = append(errs, w.foreignFields(where, aType(w.Type), takesLimit)...)
	if takesLimit && (w.Limit < 0 || w.Limit > 100) {
		errs = append(errs, invalidf("%s: limit %d is outside 1…100 (0 means the default)", where, w.Limit))
	}
	errs = append(errs, w.feedFields(where, w.Type)...)

	if f := w.MonitorFilter; w.Type == TypeMonitorSummary && f != nil {
		if len(f.Tag) > MaxTitle || len(f.Name) > MaxTitle {
			errs = append(errs, invalidf("%s: monitor_filter tag or q is over %d bytes", where, MaxTitle))
		}
		seen := map[string]bool{}
		for _, st := range f.Statuses {
			if !slices.Contains(monitorStatuses, st) {
				errs = append(errs, invalidf("%s: monitor_filter status %q is not one of %s", where, st, strings.Join(monitorStatuses, ", ")))
			} else if seen[st] {
				errs = append(errs, invalidf("%s: monitor_filter status %q is listed twice", where, st))
			}
			seen[st] = true
		}
	}
	switch {
	case w.Type == TypeAlertValue && strings.TrimSpace(w.MonitorUID) == "":
		errs = append(errs, invalidf("%s: an alert_value needs monitor_uid", where))
	case strings.TrimSpace(w.MonitorUID) != w.MonitorUID:
		// A uid is matched exactly, so " judge-backlog" would name no monitor.
		errs = append(errs, invalidf("%s: monitor_uid %q has spaces around it", where, w.MonitorUID))
	}
	if len(w.MonitorUID) > MaxTitle {
		errs = append(errs, invalidf("%s: monitor_uid is %d bytes; the limit is %d", where, len(w.MonitorUID), MaxTitle))
	}
	if w.Type == TypeServiceSummary {
		// The APM API refuses these in env, because a metric query cannot
		// express them; refusing at save says so where it can be fixed.
		if strings.ContainsAny(w.Env, EnvRefused) {
			errs = append(errs, invalidf("%s: env %q cannot hold a comma, a brace or a line break", where, w.Env))
		}
		if len(w.Env) > MaxTitle {
			errs = append(errs, invalidf("%s: env is %d bytes; the limit is %d", where, len(w.Env), MaxTitle))
		}
	}
	return errs
}

// feedFields reports the feed widgets' fields on a widget they do not belong
// to; own is the feed type whose field is allowed, or "" for none. The one
// place each "X belongs to Y" message for these fields is written.
func (w *Widget) feedFields(where string, own Type) []error {
	var errs []error
	if w.MonitorFilter != nil && own != TypeMonitorSummary {
		errs = append(errs, invalidf("%s: monitor_filter belongs to a monitor_summary, not %s", where, aType(w.Type)))
	}
	if w.MonitorUID != "" && own != TypeAlertValue {
		errs = append(errs, invalidf("%s: monitor_uid belongs to an alert_value, not %s", where, aType(w.Type)))
	}
	if w.Env != "" && own != TypeServiceSummary {
		errs = append(errs, invalidf("%s: env belongs to a service_summary, not %s", where, aType(w.Type)))
	}
	return errs
}

// aType is a type name with its article: "a toplist", "an alert_value".
func aType(t Type) string {
	if t != "" && strings.ContainsRune("aeiou", rune(t[0])) {
		return "an " + string(t)
	}
	return "a " + string(t)
}

// EnvRefused are the characters a service_summary's env cannot hold: the APM
// API refuses the same set (internal/api/apm.go, matcher), because a metric
// query cannot express them.
const EnvRefused = ",{}\n\r"

// limitBelongs is the one statement of which types take a limit.
const limitBelongs = "limit belongs to a toplist, a monitor_summary or a service_summary"

// foreignFields reports the query widgets' display fields on a type that has
// none of them: a note, or a feed widget. takesLimit leaves limit to the
// caller (the summaries take one). It exists because those branches return
// before the per-type checks and would otherwise be allowed to carry anything.
func (w *Widget) foreignFields(where, what string, takesLimit bool) []error {
	var errs []error
	if w.Limit != 0 && !takesLimit {
		errs = append(errs, invalidf("%s: %s, not %s", where, limitBelongs, what))
	}
	if w.Precision != nil {
		errs = append(errs, invalidf("%s: precision belongs to a query_value or a table, not %s", where, what))
	}
	if w.YAxis != nil {
		errs = append(errs, invalidf("%s: yaxis belongs to a timeseries or a heatmap, not %s", where, what))
	}
	if len(w.ConditionalFormats) > 0 {
		errs = append(errs, invalidf("%s: conditional_formats belong to a query_value or a table, not %s", where, what))
	}
	return errs
}

func (l Layout) validate(where string) []error {
	var errs []error
	if l.W <= 0 || l.H <= 0 {
		errs = append(errs, invalidf("%s: layout w and h must be positive, got %dx%d", where, l.W, l.H))
	}
	if l.X < 0 || l.Y < 0 {
		errs = append(errs, invalidf("%s: layout x and y cannot be negative, got (%d,%d)", where, l.X, l.Y))
	}
	// Past the right edge is a widget nobody can see, which stores happily and
	// is then blamed on the browser.
	if l.X >= 0 && l.W > 0 && l.X+l.W > Columns {
		errs = append(errs, invalidf(
			"%s: layout x+w is %d, past the %d-column grid", where, l.X+l.W, Columns))
	}
	return errs
}

func (q *Query) validate(where string, typ Type, declared map[string]bool) []error {
	var errs []error
	if strings.TrimSpace(q.Q) == "" {
		return append(errs, invalidf("%s: q is required", where))
	}
	expr, err := metricql.Parse(q.Q)
	if err != nil {
		// The parse error carries its column, and a dashboard editor wants it:
		// wrapped rather than replaced so that the column survives to the UI.
		return append(errs, fmt.Errorf("%w: %s: %w", ErrInvalid, where, err))
	}
	// The check that needs the whole dashboard: a `$var` is only meaningful
	// against the declarations around it, so no amount of looking at this
	// query alone would find this.
	for _, v := range metricql.Variables(expr) {
		if !declared[v] {
			errs = append(errs, invalidf(
				"%s: $%s is not declared in template_vars, so nothing would resolve it", where, v))
		}
	}
	// A heatmap is drawn from /api/v1/query/sketch and every other widget from
	// /api/v1/query, and those two endpoints refuse each other's queries. Before
	// `dist:` existed neither mistake was expressible; now both are, and this is
	// the one place that can catch them — otherwise the definition stores
	// happily and every draw is a 400, which the reader blames on the browser.
	if leaf, ok := expr.(*metricql.Query); ok {
		switch {
		case typ == TypeHeatmap && leaf.Agg != metricql.Dist:
			errs = append(errs, invalidf(
				"%s: a heatmap draws a distribution, so its query needs the dist aggregator: dist:%s{…}",
				where, leaf.Metric))
		case typ != TypeHeatmap && leaf.Agg == metricql.Dist:
			errs = append(errs, invalidf(
				"%s: dist: answers a distribution, which a %s cannot draw — use a percentile (p50…p99) here, or make this widget a heatmap",
				where, typ))
		}
	} else if typ == TypeHeatmap {
		// An expression rather than one query: /api/v1/query/sketch answers one
		// `dist:` query and nothing else, because merging is the only operation
		// two distributions support.
		errs = append(errs, invalidf(
			"%s: a heatmap draws one dist: query, and %s is an expression", where, q.Q))
	}
	if q.Display != "" {
		if typ != TypeTimeseries {
			errs = append(errs, invalidf("%s: display belongs to a timeseries, not a %s", where, typ))
		} else if !slices.Contains([]Display{DisplayLine, DisplayArea, DisplayBars, DisplayPoint}, q.Display) {
			errs = append(errs, invalidf("%s: display %q is not one of line, area, bars, points", where, q.Display))
		}
	}
	if q.Reducer != "" && !slices.Contains(
		[]Reducer{ReducerLast, ReducerAvg, ReducerSum, ReducerMin, ReducerMax}, q.Reducer) {
		errs = append(errs, invalidf("%s: reducer %q is not one of last, avg, sum, min, max", where, q.Reducer))
	}
	// The widgets that show one number per group have to be told which number.
	// Defaulting silently would make a chart and a toplist of the same query
	// disagree for a reason the reader cannot see.
	if q.Reducer == "" && (typ == TypeQueryValue || typ == TypeToplist || typ == TypeTable) {
		errs = append(errs, invalidf("%s: a %s needs a reducer (last, avg, sum, min or max)", where, typ))
	}
	if q.Reducer != "" && (typ == TypeTimeseries || typ == TypeHeatmap) {
		errs = append(errs, invalidf("%s: a %s draws every bucket, so a reducer would be ignored", where, typ))
	}
	return errs
}

func (c ConditionalFormat) validate(where string) []error {
	var errs []error
	if !slices.Contains([]Op{OpGT, OpGE, OpLT, OpLE, OpEQ, OpNE}, c.Op) {
		errs = append(errs, invalidf("%s: op %q is not one of >, >=, <, <=, =, !=", where, c.Op))
	}
	if strings.TrimSpace(c.Color) == "" {
		errs = append(errs, invalidf("%s: color is required", where))
	}
	return errs
}

func (y *YAxis) validate(where string, typ Type) []error {
	var errs []error
	if typ != TypeTimeseries && typ != TypeHeatmap {
		errs = append(errs, invalidf("%s: yaxis belongs to a timeseries or a heatmap, not a %s", where, typ))
	}
	if y.Min != nil && y.Max != nil && *y.Min >= *y.Max {
		errs = append(errs, invalidf("%s: yaxis min %g is not below max %g", where, *y.Min, *y.Max))
	}
	if y.Scale != "" && y.Scale != "linear" && y.Scale != "log" {
		errs = append(errs, invalidf("%s: yaxis scale %q is not linear or log", where, y.Scale))
	}
	// A log axis cannot show zero, and a chart whose axis silently clips its
	// data at the bottom is worse than one that refuses to be configured.
	if y.Scale == "log" && y.Min != nil && *y.Min <= 0 {
		errs = append(errs, invalidf("%s: a log axis cannot start at %g", where, *y.Min))
	}
	return errs
}

// validUID accepts what is safe in a URL path segment and in a filename,
// since a provisioned dashboard's uid is both.
func validUID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// validVarName matches the grammar's `ident`, because that is what a `$name`
// in a query is lexed as: a name this accepts but the lexer does not would be
// a declaration no query could reference.
func validVarName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	if c := s[0]; c < 'a' || c > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}
