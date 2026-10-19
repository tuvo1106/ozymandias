package spec

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"text/template/parse"
	"time"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// MaxRenderedBytes bounds a rendered message. Discord refuses a description
// over 4096 characters, and no notification is improved by being longer.
const MaxRenderedBytes = 8192

// Data is what a message template sees. The field names are the template
// language's API (docs/operations.md#notification-messages), so they do not
// change.
type Data struct {
	Monitor    Ref
	Group      Group
	Status     string
	PrevStatus string
	Value      float64
	Threshold  float64
	IsAlert    bool
	IsWarning  bool
	IsRecovery bool
	IsNoData   bool
	Since      time.Time
	Link       string
}

// Ref names a monitor in a template. `{{.Monitor}}` prints its name.
type Ref struct {
	ID   int64
	UID  string
	Name string
}

func (r Ref) String() string { return r.Name }

// Group is a multi-alert's group as tags. `{{.Group}}` prints the canonical
// group key (`resource:GET /x,service:api`, or `*` for an ungrouped monitor) and
// `{{.Group.service}}` one value: a map, so a template can index it, with a
// String method, so printing it whole reads as the UI does.
type Group map[string]string

func (g Group) String() string {
	if len(g) == 0 {
		return "*"
	}
	keys := slices.Sorted(maps.Keys(g))
	for i, k := range keys {
		keys[i] = wire.JoinTag(k, g[k])
	}
	return strings.Join(keys, ",")
}

// Template is a compiled message.
type Template struct {
	t *template.Template
	// named is every handle the template's literal text mentions (see
	// scanner): who every message from it goes to.
	named []string
}

// CompileTemplate parses a message and proves it renders.
//
// Proving means executing it against sample data: a misspelt `{{.Valu}}`
// parses fine and then fails at the moment an alert fires, which is the moment
// nobody is there to fix it. Run once here, the same mistake is a 400 at save.
// A missing *group* key is different, and renders empty (missingkey=zero):
// which tags a group carries is a fact about the data, not the template, and
// `{{.Group.queue}}` is right for a monitor grouped by queue.
//
// `range`, `template`, `define` and `block` are refused. Nothing in [Data] is
// a list, so range has nothing to iterate except an integer — and
// `{{range 1000000000}}` is a loop the server would run on every
// notification. Template definitions add nothing a message needs.
func CompileTemplate(text string) (*Template, error) {
	t, err := template.New("message").Option("missingkey=zero").Parse(text)
	if err != nil {
		return nil, err
	}
	for _, tt := range t.Templates() {
		if tt.Name() != "message" {
			return nil, errors.New("{{define}} and {{block}} are not supported in a message")
		}
	}
	sc := &scanner{}
	sc.list(t.Root, false)
	if len(sc.errs) > 0 {
		return nil, errors.New(strings.Join(sc.errs, "\n"))
	}
	out := &Template{t: t, named: sc.named}
	for _, sample := range samples() {
		if _, err := out.Render(sample); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// scanner walks a template once. It refuses what a message may not contain
// and collects who it mentions.
//
// `range`, `template`, `define` and `block` are refused (see CompileTemplate).
//
// A mention is `@handle` in the template's own literal text, read in runs of
// adjacent text (the parser drops comments, so `page {{/* x */}}@pager` is
// the run "page @pager", as it renders). It counts at the start of a run that
// starts the message, or after whitespace: an address is not one, and nor is
// text straight after an action that prints (`{{.Group.owner}}@example.com`
// builds an address). Comments, string literals and field names are never
// text.
//
// A mention inside `{{if}}` or `{{with}}` is refused: every message goes to
// every handle (ADR-0052), so it would be sent for every status while reading
// as if it were not. There the start of a branch counts as a boundary too, so
// `{{if .IsAlert}}@pager{{end}}` is refused, not quietly ignored.
type scanner struct {
	named []string
	errs  []string
}

func (s *scanner) fail(msg string) {
	if !slices.Contains(s.errs, msg) {
		s.errs = append(s.errs, msg)
	}
}

func (s *scanner) list(l *parse.ListNode, inCond bool) {
	if l == nil {
		return
	}
	var run strings.Builder
	runStart := 0 // index of the node the current run began at
	flush := func() {
		if run.Len() > 0 {
			s.text(run.String(), inCond, runStart == 0)
			run.Reset()
		}
	}
	for i, n := range l.Nodes {
		if t, ok := n.(*parse.TextNode); ok {
			if run.Len() == 0 {
				runStart = i
			}
			run.Write(t.Text)
			continue
		}
		flush()
		switch n := n.(type) {
		case *parse.RangeNode:
			s.fail("{{range}} is not supported in a message: nothing in it is a list")
		case *parse.TemplateNode:
			s.fail("{{template}} is not supported in a message")
		case *parse.IfNode:
			s.list(n.List, true)
			s.list(n.ElseList, true)
		case *parse.WithNode:
			s.list(n.List, true)
			s.list(n.ElseList, true)
		}
	}
	flush()
}

// text reads one run. atStart: the run begins its list (the message, or a
// conditional branch).
func (s *scanner) text(text string, inCond, atStart bool) {
	for _, m := range mention.FindAllStringSubmatchIndex(text, -1) {
		h := text[m[2]:m[3]]
		if m[0] == 0 && text[0] == '@' && !atStart {
			continue // straight after an action: part of what it printed
		}
		switch {
		case inCond:
			s.fail(fmt.Sprintf("@%s is inside {{if}} or {{with}}; a message goes to every notifier it mentions, "+
				"whatever the status, so write mentions outside conditions (use one monitor per audience)", h))
		case !slices.Contains(s.named, h):
			s.named = append(s.named, h)
		}
	}
}

// samples are the data a template is proved against: one per status, so that
// a field used only inside `{{if .IsNoData}}` is executed too.
func samples() []Data {
	base := Data{
		Monitor: Ref{ID: 1, UID: "sample", Name: "sample monitor"},
		Group:   Group{"service": "api"},
		Value:   1, Threshold: 1, Since: time.Unix(0, 0).UTC(), Link: "http://localhost/monitors/1",
	}
	alert, warn, rec, nodata := base, base, base, base
	alert.Status, alert.PrevStatus, alert.IsAlert = "ALERT", "OK", true
	warn.Status, warn.PrevStatus, warn.IsWarning = "WARN", "OK", true
	rec.Status, rec.PrevStatus, rec.IsRecovery = "OK", "ALERT", true
	nodata.Status, nodata.PrevStatus, nodata.IsNoData = "NO_DATA", "OK", true
	return []Data{alert, warn, rec, nodata}
}

// limitWriter fails once more than max bytes have been written, so a template
// that prints a megabyte stops at the bound rather than after building it all.
type limitWriter struct {
	b   strings.Builder
	max int
}

var errTooLong = fmt.Errorf("message renders to more than %d bytes", MaxRenderedBytes)

func (w *limitWriter) Write(p []byte) (int, error) {
	if w.b.Len()+len(p) > w.max {
		return 0, errTooLong
	}
	return w.b.Write(p)
}

// Render executes the template.
func (t *Template) Render(d Data) (string, error) {
	w := &limitWriter{max: MaxRenderedBytes}
	if err := t.t.Execute(w, d); err != nil {
		return "", err
	}
	return w.b.String(), nil
}

// Recipients is who every message from this template goes to: each handle
// its literal text mentions (see scanner), whatever the status and whatever
// the data (ADR-0052). Not the handles in the rendered text: that would let a
// tag value or log attribute containing `@x` page x, and every rule tried for
// telling the author's mentions from the data's in rendered text had a hole.
func (t *Template) Recipients() []string { return slices.Clone(t.named) }

// mention matches `@handle` at the start of a piece of text or after
// whitespace; scanner decides what each match means.
var mention = regexp.MustCompile(`(?:^|\s)@([A-Za-z0-9_-]+)`)
