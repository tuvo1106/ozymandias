package dashboard

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/query/metricql"
)

// good is the smallest dashboard that is worth storing. Every test below
// changes exactly one thing about it, so a failure names the rule that broke
// rather than "the fixture".
func good() Dashboard {
	return Dashboard{
		Title: "checkout",
		TemplateVars: []TemplateVar{
			{Name: "env", Tag: "env", Default: "*"},
		},
		Widgets: []Widget{{
			ID:     "w1",
			Type:   TypeTimeseries,
			Title:  "req/s by route",
			Layout: Layout{X: 0, Y: 0, W: 6, H: 3},
			Queries: []Query{{
				Q:       "sum:http.request.count{$env} by {route}.as_rate()",
				Display: DisplayLine,
			}},
		}},
	}
}

func TestValidate_TheSmallestUsefulDashboardIsValid(t *testing.T) {
	d := good()
	if err := d.Validate(); err != nil {
		t.Fatalf("the fixture is supposed to be valid: %v", err)
	}
}

// The rule that cannot be checked one widget at a time, and the reason this
// validator exists at all: a `$var` is only meaningful against the
// declarations around it.
func TestValidate_AnUndeclaredVariableIsRefused(t *testing.T) {
	d := good()
	d.Widgets[0].Queries[0].Q = "sum:http.request.count{$env,$region} by {route}"
	err := d.Validate()
	if err == nil {
		t.Fatal("a query referencing an undeclared $region was accepted")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("%v does not classify as ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "$region") {
		t.Errorf("%v does not name the variable", err)
	}
	// And the declared one is not also complained about.
	if strings.Contains(err.Error(), "$env") {
		t.Errorf("%v complains about the declared variable too", err)
	}

	// Declaring it fixes it, and case does not matter — the lexer lower-cases
	// a variable as it reads one.
	d.TemplateVars = append(d.TemplateVars, TemplateVar{Name: "Region", Tag: "region"})
	if err := d.Validate(); err != nil {
		t.Errorf("declaring $Region did not satisfy $region: %v", err)
	}
}

// A person editing JSON should get every problem at once, not one per save.
func TestValidate_ReportsEveryProblemAtOnce(t *testing.T) {
	d := Dashboard{
		Widgets: []Widget{
			{ID: "", Type: TypeTimeseries, Layout: Layout{W: 99, H: 1},
				Queries: []Query{{Q: "this is not a query"}}},
			{ID: "dup", Type: TypeNote, Layout: Layout{W: 1, H: 1}, Markdown: "ok"},
			{ID: "dup", Type: "cuckoo", Layout: Layout{W: 1, H: 1}},
		},
	}
	err := d.Validate()
	if err == nil {
		t.Fatal("that dashboard is not valid")
	}
	msg := err.Error()
	for _, want := range []string{
		"title is required",
		"id is required",
		"past the 12-column grid",
		"is used twice",
		`type "cuckoo"`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message does not mention %q:\n%s", want, msg)
		}
	}
}

// A field that belongs to another type is a sign the author expects something
// this widget will not do. Ignoring it silently is how a dashboard ends up
// with a "precision" nobody can find the effect of.
func TestValidate_FieldsBelongToTheirType(t *testing.T) {
	three := 3
	for _, tc := range []struct {
		name string
		edit func(*Widget)
		want string
	}{
		{"markdown on a chart", func(w *Widget) { w.Markdown = "hi" }, "markdown belongs to a note"},
		{"limit on a chart", func(w *Widget) { w.Limit = 5 }, "limit belongs to a toplist"},
		{"precision on a chart", func(w *Widget) { w.Precision = &three }, "precision belongs to a query_value"},
		{"a reducer on a chart", func(w *Widget) { w.Queries[0].Reducer = ReducerLast }, "draws every bucket"},
		{"display on a toplist", func(w *Widget) {
			w.Type = TypeToplist
			w.Queries[0].Reducer = ReducerAvg
			w.Queries[0].Display = DisplayArea
		}, "display belongs to a timeseries"},
		{"yaxis on a note", func(w *Widget) {
			w.Type = TypeNote
			w.Markdown = "hi"
			w.Queries = nil
		}, ""}, // a note returns early; yaxis is not reached, and that is fine
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := good()
			tc.edit(&d.Widgets[0])
			err := d.Validate()
			if tc.want == "" {
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want a refusal mentioning %q", err, tc.want)
			}
		})
	}
}

// A widget that shows one number per group has to say which number. Picking
// one silently would make a chart and a toplist of the same query disagree for
// a reason the reader cannot see.
func TestValidate_TheOneNumberWidgetsNeedAReducer(t *testing.T) {
	for _, typ := range []Type{TypeQueryValue, TypeToplist, TypeTable} {
		t.Run(string(typ), func(t *testing.T) {
			d := good()
			w := &d.Widgets[0]
			w.Type = typ
			w.Queries[0].Display = ""
			if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "needs a reducer") {
				t.Errorf("got %v, want a refusal about the reducer", err)
			}
			w.Queries[0].Reducer = ReducerLast
			if err := d.Validate(); err != nil {
				t.Errorf("with a reducer: %v", err)
			}
		})
	}
}

func TestValidate_ANoteIsMarkdownAndNothingElse(t *testing.T) {
	d := good()
	w := &d.Widgets[0]
	w.Type = TypeNote
	w.Queries[0].Display = ""

	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "a note has no queries") {
		t.Errorf("got %v, want a refusal about the queries", err)
	}
	w.Queries = nil
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "needs markdown") {
		t.Errorf("got %v, want a refusal about the missing markdown", err)
	}
	w.Markdown = "## why this matters"
	if err := d.Validate(); err != nil {
		t.Errorf("a valid note: %v", err)
	}
}

// A heatmap of two overlaid distributions is not readable by anybody.
func TestValidate_AHeatmapDrawsOneDistribution(t *testing.T) {
	d := good()
	w := &d.Widgets[0]
	w.Type = TypeHeatmap
	w.Queries = []Query{
		{Q: "p95:http.request.duration{$env}"},
		{Q: "p99:http.request.duration{$env}"},
	}
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "draws one distribution") {
		t.Errorf("got %v, want a refusal", err)
	}
	w.Queries = w.Queries[:1]
	if err := d.Validate(); err != nil {
		t.Errorf("one query: %v", err)
	}
}

func TestValidate_Layout(t *testing.T) {
	for _, tc := range []struct {
		name   string
		layout Layout
		want   string
	}{
		{"the full width", Layout{X: 0, W: Columns, H: 1}, ""},
		{"the right-hand half", Layout{X: 6, W: 6, H: 1}, ""},
		{"one past the edge", Layout{X: 6, W: 7, H: 1}, "past the 12-column grid"},
		{"zero width", Layout{X: 0, W: 0, H: 1}, "must be positive"},
		{"zero height", Layout{X: 0, W: 1, H: 0}, "must be positive"},
		{"negative x", Layout{X: -1, W: 1, H: 1}, "cannot be negative"},
		{"far down the page", Layout{X: 0, Y: 900, W: 1, H: 1}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := good()
			d.Widgets[0].Layout = tc.layout
			err := d.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("%+v should be valid: %v", tc.layout, err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("%+v gave %v, want %q", tc.layout, err, tc.want)
			}
		})
	}
}

// A template that declares no service variable is instantiated over nothing
// and appears nowhere — the least debuggable outcome available.
func TestValidate_ATemplateNeedsItsServiceVariable(t *testing.T) {
	d := good()
	d.Template = true
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), TemplateVarService) {
		t.Errorf("got %v, want a refusal naming the service variable", err)
	}
	d.TemplateVars = append(d.TemplateVars, TemplateVar{Name: "service", Tag: "service"})
	if err := d.Validate(); err != nil {
		t.Errorf("with a service variable: %v", err)
	}
}

func TestValidate_TemplateVars(t *testing.T) {
	for _, tc := range []struct {
		name string
		vars []TemplateVar
		want string
	}{
		{"fine", []TemplateVar{{Name: "env", Tag: "env"}}, ""},
		{"a renamed tag", []TemplateVar{{Name: "env", Tag: "deployment_environment"}}, ""},
		{"no name", []TemplateVar{{Name: "", Tag: "env"}}, "name is required"},
		{"no tag", []TemplateVar{{Name: "env", Tag: ""}}, "tag is required"},
		{"declared twice", []TemplateVar{{Name: "env", Tag: "env"}, {Name: "env", Tag: "e"}}, "declared twice"},
		{"not an identifier", []TemplateVar{{Name: "1env", Tag: "env"}}, "not a variable name"},
		{"not a tag key", []TemplateVar{{Name: "env", Tag: "not a key"}}, "not a tag key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := good()
			d.TemplateVars = tc.vars
			// Keep the widget's query referencing only what is declared.
			d.Widgets[0].Queries[0].Q = "sum:http.request.count{*} by {route}"
			err := d.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("should be valid: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("got %v, want %q", err, tc.want)
			}
		})
	}
}

// The parse error has to survive validation with its column intact, because
// the editor underlines with it.
func TestValidate_AParseErrorKeepsItsColumn(t *testing.T) {
	d := good()
	d.Widgets[0].Queries[0].Q = "sum:x{a:b by {k}"
	err := d.Validate()
	if err == nil {
		t.Fatal("that query does not parse")
	}
	var perr *metricql.Error
	if !errors.As(err, &perr) {
		t.Fatalf("%v does not carry a *metricql.Error", err)
	}
	if perr.Col != 14 {
		t.Errorf("col %d, want 14", perr.Col)
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("%v does not also classify as ErrInvalid", err)
	}
}

func TestValidate_YAxis(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	for _, tc := range []struct {
		name string
		axis YAxis
		want string
	}{
		{"zero-based", YAxis{Min: f(0), Unit: "req/s"}, ""},
		{"bounded", YAxis{Min: f(0), Max: f(100), Unit: "%"}, ""},
		{"inverted", YAxis{Min: f(100), Max: f(0)}, "is not below max"},
		{"equal", YAxis{Min: f(1), Max: f(1)}, "is not below max"},
		{"a log axis from zero", YAxis{Scale: "log", Min: f(0)}, "cannot start at 0"},
		{"a log axis from one", YAxis{Scale: "log", Min: f(1)}, ""},
		{"an unknown scale", YAxis{Scale: "sqrt"}, "not linear or log"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := good()
			axis := tc.axis
			d.Widgets[0].YAxis = &axis
			err := d.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("should be valid: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidate_ConditionalFormats(t *testing.T) {
	d := good()
	w := &d.Widgets[0]
	w.Type = TypeQueryValue
	w.Queries[0].Display = ""
	w.Queries[0].Reducer = ReducerLast
	w.ConditionalFormats = []ConditionalFormat{{Op: "=~", Value: 1, Color: "red"}, {Op: OpGT, Value: 2}}
	err := d.Validate()
	if err == nil {
		t.Fatal("those formats are not valid")
	}
	for _, want := range []string{`op "=~"`, "color is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%v does not mention %q", err, want)
		}
	}
}

func TestValidate_UID(t *testing.T) {
	for _, tc := range []struct {
		uid string
		ok  bool
	}{
		{"", true}, // assigned by the API rather than the author
		{"home", true},
		{"checkout-overview_v2", true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
		{"has space", false},
		{"../../etc/passwd", false},
		{"slash/es", false},
	} {
		d := good()
		d.UID = tc.uid
		err := d.Validate()
		if (err == nil) != tc.ok {
			t.Errorf("uid %q: err = %v, want ok = %v", tc.uid, err, tc.ok)
		}
	}
}

func TestParse(t *testing.T) {
	valid := `{
		"uid": "checkout",
		"title": "checkout",
		"template_vars": [{"name": "env", "tag": "env", "default": "*"}],
		"widgets": [{
			"id": "w1", "type": "timeseries", "layout": {"x": 0, "y": 0, "w": 6, "h": 3},
			"queries": [{"q": "sum:http.request.count{$env} by {route}", "display": "line"}]
		}]
	}`
	d, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if d.UID != "checkout" || len(d.Widgets) != 1 {
		t.Fatalf("%+v", d)
	}

	for _, tc := range []struct{ name, body, want string }{
		{"a misspelled field", `{"title":"x","widgits":[]}`, "unknown field"},
		{"not an object", `[]`, "dashboard json"},
		{"truncated", `{"title":`, "dashboard json"},
		{"two objects", `{"title":"x","widgets":[]} {"title":"y"}`, "trailing content"},
		{"valid json, invalid dashboard", `{"title":"x","widgets":[]}`, "at least one widget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.body)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want %q", err, tc.want)
			}
		})
	}
}

// The limits bound what one stored row can cost the server that evaluates it:
// a dashboard is one /api/v1/query/batch request, and every widget in it is
// queries somebody's laptop asked for.
func TestValidate_Limits(t *testing.T) {
	widget := func(id string) Widget {
		return Widget{
			ID: id, Type: TypeTimeseries, Layout: Layout{W: 1, H: 1},
			Queries: []Query{{Q: "sum:a{*}"}},
		}
	}
	t.Run("too many widgets", func(t *testing.T) {
		d := good()
		d.Widgets = nil
		for i := range MaxWidgets + 1 {
			d.Widgets = append(d.Widgets, widget(fmt.Sprintf("w%d", i)))
		}
		if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "the limit is 100") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("too many queries in one widget", func(t *testing.T) {
		d := good()
		w := widget("w1")
		for range MaxQueriesPerWidget + 1 {
			w.Queries = append(w.Queries, Query{Q: "sum:a{*}"})
		}
		d.Widgets = []Widget{w}
		if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "the limit is 10") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("too many template variables", func(t *testing.T) {
		d := good()
		d.TemplateVars = nil
		for i := range MaxTemplateVars + 1 {
			d.TemplateVars = append(d.TemplateVars, TemplateVar{Name: fmt.Sprintf("v%d", i), Tag: "t"})
		}
		d.Widgets[0].Queries[0].Q = "sum:a{*}"
		if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "the limit is 10") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("an essay for a title", func(t *testing.T) {
		d := good()
		d.Title = strings.Repeat("t", MaxTitle+1)
		if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "title is") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("an essay for a description", func(t *testing.T) {
		d := good()
		d.Description = strings.Repeat("d", MaxDescription+1)
		if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "description is") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("an essay for a widget title", func(t *testing.T) {
		d := good()
		d.Widgets[0].Title = strings.Repeat("t", MaxTitle+1)
		if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "title is") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("a book for a note", func(t *testing.T) {
		d := good()
		d.Widgets[0] = Widget{ID: "n", Type: TypeNote, Layout: Layout{W: 1, H: 1},
			Markdown: strings.Repeat("m", MaxMarkdown+1)}
		if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "markdown is") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("toplist limit", func(t *testing.T) {
		for _, tc := range []struct {
			limit int
			ok    bool
		}{{0, true}, {1, true}, {100, true}, {101, false}, {-1, false}} {
			d := good()
			w := &d.Widgets[0]
			w.Type = TypeToplist
			w.Queries[0].Display = ""
			w.Queries[0].Reducer = ReducerAvg
			w.Limit = tc.limit
			if err := d.Validate(); (err == nil) != tc.ok {
				t.Errorf("limit %d: err = %v, want ok = %v", tc.limit, err, tc.ok)
			}
		}
	})
	t.Run("precision", func(t *testing.T) {
		for _, tc := range []struct {
			precision int
			ok        bool
		}{{0, true}, {10, true}, {11, false}, {-1, false}} {
			d := good()
			w := &d.Widgets[0]
			w.Type = TypeQueryValue
			w.Queries[0].Display = ""
			w.Queries[0].Reducer = ReducerLast
			p := tc.precision
			w.Precision = &p
			if err := d.Validate(); (err == nil) != tc.ok {
				t.Errorf("precision %d: err = %v, want ok = %v", tc.precision, err, tc.ok)
			}
		}
	})
	t.Run("an empty query", func(t *testing.T) {
		d := good()
		d.Widgets[0].Queries[0].Q = "   "
		if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "q is required") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("an unknown reducer", func(t *testing.T) {
		d := good()
		w := &d.Widgets[0]
		w.Type = TypeQueryValue
		w.Queries[0].Display = ""
		w.Queries[0].Reducer = "median"
		if err := d.Validate(); err == nil || !strings.Contains(err.Error(), `reducer "median"`) {
			t.Errorf("got %v", err)
		}
	})
	t.Run("an unknown display", func(t *testing.T) {
		d := good()
		d.Widgets[0].Queries[0].Display = "candlestick"
		if err := d.Validate(); err == nil || !strings.Contains(err.Error(), `display "candlestick"`) {
			t.Errorf("got %v", err)
		}
	})
}

// A variable name this accepts but the lexer does not would be a declaration
// no query could ever reference.
func TestValidVarName_MatchesTheLexersIdent(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"env", true},
		{"e", true},
		{"env_2", true},
		{"a.b", true},
		{"", false},
		{"1env", false},
		{"_env", false},
		{".env", false},
		{"en-v", false},
		{"en v", false},
		{strings.Repeat("e", 64), true},
		{strings.Repeat("e", 65), false},
	} {
		if got := validVarName(tc.name); got != tc.ok {
			t.Errorf("validVarName(%q) = %v, want %v", tc.name, got, tc.ok)
		}
		// Whatever this accepts, the parser must accept as a variable.
		if tc.ok {
			if _, err := metricql.Parse("sum:m{$" + tc.name + "}"); err != nil {
				t.Errorf("validVarName accepts %q but the parser does not: %v", tc.name, err)
			}
		}
	}
}

// docExample is the worked example in docs/dashboards.md, which is normative.
//
// A document that says "this is a valid definition" next to a validator that
// refuses it is worse than no document, and hand-written JSON in prose is
// exactly the kind of thing that rots quietly: nobody runs it. This test runs
// it. If it fails, either the doc or the rules changed and the other has to
// follow — in the same commit, per AGENTS.md §2.4.
func TestDocsExampleIsValid(t *testing.T) {
	const doc = "../../docs/dashboards.md"
	md, err := os.ReadFile(doc)
	if err != nil {
		t.Fatal(err)
	}
	blocks := jsonBlocks(string(md))
	if len(blocks) == 0 {
		t.Fatalf("%s has no ```json block; this test has stopped checking anything", doc)
	}
	// The first block is the complete example; the rest are fragments of one
	// field, which are not whole definitions.
	d, err := Parse([]byte(blocks[0]))
	if err != nil {
		t.Fatalf("%s's worked example does not validate: %v", doc, err)
	}
	// And it is the example it claims to be, so that trimming it to "{}" some
	// day does not leave this test passing on nothing.
	if len(d.Widgets) < 5 {
		t.Errorf("the example has %d widgets; it is supposed to show every type", len(d.Widgets))
	}
	types := map[Type]bool{}
	for _, w := range d.Widgets {
		types[w.Type] = true
	}
	for _, want := range []Type{TypeTimeseries, TypeQueryValue, TypeToplist, TypeHeatmap, TypeNote} {
		if !types[want] {
			t.Errorf("the example never shows a %s", want)
		}
	}
}

// jsonBlocks returns the contents of every ```json fence in md.
func jsonBlocks(md string) []string {
	var out []string
	for rest := md; ; {
		_, after, found := strings.Cut(rest, "```json\n")
		if !found {
			return out
		}
		body, remainder, closed := strings.Cut(after, "```")
		if !closed {
			return out
		}
		out = append(out, body)
		rest = remainder
	}
}
