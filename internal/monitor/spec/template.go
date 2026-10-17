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
	// named is every handle the source mentions: the only notifiers a
	// message from it can ever reach.
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
		if err := refuse(tt.Root); err != nil {
			return nil, err
		}
	}
	out := &Template{t: t, named: Mentions(text)}
	for _, sample := range samples() {
		if _, err := out.Render(sample); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func refuse(n parse.Node) error {
	switch n := n.(type) {
	case *parse.ListNode:
		if n == nil {
			return nil
		}
		for _, c := range n.Nodes {
			if err := refuse(c); err != nil {
				return err
			}
		}
	case *parse.RangeNode:
		return errors.New("{{range}} is not supported in a message: nothing in it is a list")
	case *parse.TemplateNode:
		return errors.New("{{template}} is not supported in a message")
	case *parse.IfNode:
		return errors.Join(refuse(n.List), refuse(n.ElseList))
	case *parse.WithNode:
		return errors.Join(refuse(n.List), refuse(n.ElseList))
	}
	return nil
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

// Recipients is who the message for d notifies: each handle the template's
// source names that appears as `@handle` in the message rendered from d with
// every `@` removed from d's text.
//
// Rendered, so `{{if .IsAlert}}@pager{{end}}` pages only for alerts. Named in
// the source, and rendered from data without `@`, because a tag value, a log
// attribute or a monitor name is data, and "x @discord-ops" in one must not
// choose who is paged. No boundary is required before the handle in the
// render: the source already decided it is a mention, and `CPU:{{if
// .IsAlert}}@pager{{end}}` renders `CPU:@pager`. A template that fails to
// render reaches everyone it names, so the failure is still seen.
func (t *Template) Recipients(d Data) []string {
	d.Monitor.Name = strings.ReplaceAll(d.Monitor.Name, "@", "")
	d.Monitor.UID = strings.ReplaceAll(d.Monitor.UID, "@", "")
	d.Link = strings.ReplaceAll(d.Link, "@", "")
	if d.Group != nil {
		g := make(Group, len(d.Group))
		for k, v := range d.Group {
			g[strings.ReplaceAll(k, "@", "")] = strings.ReplaceAll(v, "@", "")
		}
		d.Group = g
	}
	text, err := t.Render(d)
	if err != nil {
		return slices.Clone(t.named)
	}
	var out []string
	for _, h := range t.named {
		re := regexp.MustCompile(`@` + regexp.QuoteMeta(h) + `(?:[^A-Za-z0-9_-]|$)`)
		if re.MatchString(text) {
			out = append(out, h)
		}
	}
	return out
}

// mention is `@handle` at the start of the text, after whitespace, or after
// the `}` that closes a template action: an email address in a message is not
// read as one, and `{{if .IsAlert}}@pager{{end}}` is.
var mention = regexp.MustCompile(`(?:^|[\s}])@([A-Za-z0-9_-]+)`)

// Mentions returns the distinct handles in text, in order of appearance.
//
// It is run on the template source: at save, to check that every handle that
// could ever be mentioned names a configured notifier, and by Recipients to
// bound who a message can reach.
func Mentions(text string) []string {
	var out []string
	for _, m := range mention.FindAllStringSubmatch(text, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}
