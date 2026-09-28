package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// Columns is the width of the grid a dashboard is laid out on. Twelve because
// it divides by 2, 3, 4 and 6, so halves, thirds and quarters are all exact —
// the same reason every CSS framework picked it.
const Columns = 12

// Dashboard is a stored definition: what to draw, not what was drawn.
//
// The JSON shape is the API's contract and is spelled in docs/dashboards.md.
// Fields are omitted when empty so that a hand-written definition in an app's
// repo stays short, and so a round trip through this struct does not fill a
// file with nulls somebody then has to read past.
type Dashboard struct {
	// UID is the stable identity a provisioned dashboard is upserted by. It is
	// how a file in git survives being edited: the numeric id is assigned by
	// whichever database happened to insert it first, so two environments would
	// disagree about it, while a uid is chosen by the author and does not move.
	// Definitions created through the API may leave it empty.
	UID string `json:"uid,omitempty"`

	Title       string `json:"title"`
	Description string `json:"description,omitempty"`

	// Template marks a definition that is not shown as itself but instantiated
	// per service, so a newly onboarded app has an overview before anybody
	// writes JSON for it. A template must declare the variable it is
	// instantiated over; see [Dashboard.Validate].
	Template bool `json:"template,omitempty"`

	TemplateVars []TemplateVar `json:"template_vars,omitempty"`
	Widgets      []Widget      `json:"widgets"`
}

// TemplateVar is one selector on the variable bar.
type TemplateVar struct {
	// Name is what queries write as `$name`.
	Name string `json:"name"`
	// Tag is the tag key the selector offers values of. It is separate from
	// Name so that a dashboard can call a thing `env` while it is stored as
	// `deployment_environment` — renaming the label in one place beats
	// rewriting every query.
	Tag string `json:"tag"`
	// Default is the value selected when the dashboard is opened with nothing
	// in the URL. "*" — or empty — means every value, which resolves to no
	// constraint rather than to a filter that happens to match everything.
	Default string `json:"default,omitempty"`
}

// Type is a widget kind. A dashboard naming one this build does not have is
// refused rather than stored: the alternative is a definition that validates
// today and draws a blank square forever.
type Type string

// The widget types this milestone draws. Log, trace and monitor widgets join
// them in later milestones (docs/plan/ui.md has the end-state catalog).
const (
	// TypeTimeseries is a line, area or bar chart over time.
	TypeTimeseries Type = "timeseries"
	// TypeQueryValue is one number, reduced from the query's last window.
	TypeQueryValue Type = "query_value"
	// TypeToplist ranks groups by a reducer and keeps the first Limit.
	TypeToplist Type = "toplist"
	// TypeTable is groups as rows and several queries as columns.
	TypeTable Type = "table"
	// TypeHeatmap draws a distribution's sketch bins over time.
	TypeHeatmap Type = "heatmap"
	// TypeNote is markdown: a title, a runbook link, an explanation of what
	// the chart next to it means.
	TypeNote Type = "note"
)

// Widget is one tile.
//
// It is one struct with optional fields rather than a per-type union, because
// JSON has no tagged unions and every scheme for faking one — an inner
// "options" object, a second decode pass — costs more at every call site than
// it saves here. [Widget.validate] is what makes the combinations legal: the
// fields that do not belong to a type must be absent, so a `markdown` on a
// timeseries is an error and not a field quietly ignored.
type Widget struct {
	// ID is unique within the dashboard. The UI uses it as a React key and for
	// per-widget URL state (a full-screen widget, a copied link), so it has to
	// survive reordering — which an index would not.
	ID     string `json:"id"`
	Type   Type   `json:"type"`
	Title  string `json:"title,omitempty"`
	Layout Layout `json:"layout"`

	// Queries is what the widget asks for. Several are allowed so that a chart
	// can overlay this week on last week, and a table can put one query per
	// column.
	Queries []Query `json:"queries,omitempty"`

	// YAxis applies to timeseries and heatmap.
	YAxis *YAxis `json:"yaxis,omitempty"`

	// Precision is the decimal places a query_value shows. A pointer because 0
	// is a real answer — "round it to whole requests" — and absent means "pick
	// something sensible".
	Precision *int `json:"precision,omitempty"`
	// ConditionalFormats colour a query_value by threshold, in order: the
	// first match wins.
	ConditionalFormats []ConditionalFormat `json:"conditional_formats,omitempty"`

	// Limit is how many rows a toplist keeps.
	Limit int `json:"limit,omitempty"`

	// Markdown is a note's body.
	Markdown string `json:"markdown,omitempty"`
}

// Layout places a widget on the grid. Units are grid cells, not pixels, so a
// dashboard looks the same on a laptop and a wall display.
type Layout struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// Display is how a timeseries draws a line.
type Display string

// The display styles.
const (
	DisplayLine  Display = "line"
	DisplayArea  Display = "area"
	DisplayBars  Display = "bars"
	DisplayPoint Display = "points"
)

// Reducer collapses a line into the single number a query_value, toplist or
// table cell shows.
type Reducer string

// The reducers. These reduce over *time*, across the buckets of one line —
// the aggregation across series already happened in the query.
const (
	ReducerLast Reducer = "last"
	ReducerAvg  Reducer = "avg"
	ReducerSum  Reducer = "sum"
	ReducerMin  Reducer = "min"
	ReducerMax  Reducer = "max"
)

// Query is one thing a widget asks for.
type Query struct {
	// Q is metricql. It is stored as text, not as a parsed tree: text is what
	// the author typed, what a diff of the definition shows, and what survives
	// a change to the AST's shape.
	Q string `json:"q"`
	// Name labels this query where a widget shows several — a table column
	// header, or the left side of an arithmetic expression in a legend.
	Name string `json:"name,omitempty"`
	// Display applies to timeseries.
	Display Display `json:"display,omitempty"`
	// Reducer applies to the widgets that show one number per group.
	Reducer Reducer `json:"reducer,omitempty"`
}

// YAxis bounds and labels a chart's vertical axis.
type YAxis struct {
	// Min and Max are pointers because 0 is a meaningful bound and the common
	// case — "start the axis at zero so a 2% wobble looks like 2%" — is
	// exactly the one a non-pointer could not express.
	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`
	// Unit is a label, not a conversion: "req/s", "ms", "%".
	Unit string `json:"unit,omitempty"`
	// Scale is "linear" (default) or "log".
	Scale string `json:"scale,omitempty"`
}

// Op is a conditional format's comparison.
type Op string

// The comparisons.
const (
	OpGT Op = ">"
	OpGE Op = ">="
	OpLT Op = "<"
	OpLE Op = "<="
	OpEQ Op = "="
	OpNE Op = "!="
)

// ConditionalFormat colours a value that compares true.
type ConditionalFormat struct {
	Op    Op      `json:"op"`
	Value float64 `json:"value"`
	// Color is a name the UI maps to a palette entry, not CSS: a definition in
	// git should not encode this build's hex codes, and a palette that has to
	// change for contrast should not require editing every dashboard.
	Color string `json:"color"`
}

// Stored is a dashboard as the database holds it: the definition plus the
// things the database owns rather than the author.
type Stored struct {
	ID int64 `json:"id"`
	// Provisioned marks a dashboard that came from a file. The API refuses to
	// write to one, because the next restart would overwrite the edit and the
	// person who made it would have no way to know why it vanished.
	Provisioned bool      `json:"provisioned"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Dashboard
}

// Parse decodes and validates a definition in one step, because a definition
// that has been decoded but not checked is the thing every caller here would
// otherwise have to remember not to use.
//
// Unknown fields are an error. A misspelled `widget` or `template_var` that
// was quietly dropped would produce a dashboard missing whatever the author
// meant to add, with nothing anywhere saying so.
func Parse(data []byte) (Dashboard, error) {
	var d Dashboard
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return Dashboard{}, fmt.Errorf("dashboard json: %w", err)
	}
	if dec.More() {
		return Dashboard{}, fmt.Errorf("dashboard json: trailing content after the object")
	}
	if err := d.Validate(); err != nil {
		return Dashboard{}, err
	}
	return d, nil
}
