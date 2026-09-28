package dashboard

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/tuvo1106/ozymandias/internal/query/metricql"
)

// Metrics returns the metric names this definition's queries read, sorted and
// deduplicated.
//
// It exists for template instantiation, which has to answer "which services
// would this dashboard have anything to say about?" and has only the
// definition to answer it from — see the HTTP layer's service discovery. It is
// the metric as *written*, not the series a query ends up selecting: a
// distribution's tags live on its `.count` series, and which metrics are
// distributions is something this package deliberately does not know (see the
// package comment on not touching the data).
//
// An unparseable query is an error rather than a skipped one. Every stored
// definition has been through [Dashboard.Validate], which parses every query,
// so this can only fire on a hand-edited row — and silently dropping its
// metrics would quietly shorten the list of services the template covers,
// which is the kind of wrong answer nobody goes looking for.
func (d *Dashboard) Metrics() ([]string, error) {
	var out []string
	for i := range d.Widgets {
		for j := range d.Widgets[i].Queries {
			q := d.Widgets[i].Queries[j].Q
			if strings.TrimSpace(q) == "" {
				continue
			}
			expr, err := metricql.Parse(q)
			if err != nil {
				return nil, fmt.Errorf("widgets[%d] query[%d]: %w", i, j, err)
			}
			metricql.Walk(expr, func(n metricql.Node) {
				if leaf, ok := n.(*metricql.Query); ok {
					out = append(out, leaf.Metric)
				}
			})
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// ServiceTag is the tag key this template is instantiated over: the `tag` of
// its `service` variable, which is not always "service" — a dashboard may call
// a variable `service` while the store spells the label `service.name`.
//
// Empty if there is no such variable, which [Dashboard.Validate] refuses for a
// template.
func (d *Dashboard) ServiceTag() string {
	for _, v := range d.TemplateVars {
		if strings.EqualFold(strings.TrimSpace(v.Name), TemplateVarService) {
			return strings.ToLower(strings.TrimSpace(v.Tag))
		}
	}
	return ""
}

// Instantiate returns this template bound to one service.
//
// Binding, not rewriting. The instance is the same definition with the
// `service` variable's default set to the service name; the queries' `$service`
// text is untouched, and the evaluator resolves it per request as it does for
// any other variable. Rewriting the query text would mean the instance a reader
// sees and the query the server runs are two different strings, which is one
// more thing that can disagree — and it would put a variable-substitution pass
// in the one package that promises not to interpret data.
//
// What changes besides the default:
//
//   - `template` is cleared. An instance is not itself instantiable, and
//     leaving it set would make a client that lists templates find this one.
//   - `uid` is cleared. A uid is the identity provisioning upserts by, and
//     nothing stores an instance — a uid on one would be a promise that
//     `GET /api/v1/dashboards` can find it, which it cannot.
//   - the title gains the service name, because a picker showing four
//     identically titled dashboards is not a picker.
//
// The result is a definition that passes [Dashboard.Validate] — which is worth
// more than it sounds: it is what lets the UI offer "save a copy of this" and
// have the copy store, and it is why the title is length-capped below rather
// than left to overflow.
//
// An error means the caller asked for something incoherent: a definition that
// is not a template, or an empty service name.
func (d *Dashboard) Instantiate(service string) (Dashboard, error) {
	if !d.Template {
		return Dashboard{}, invalidf("%q is not a template, so there is nothing to instantiate", d.Title)
	}
	if strings.TrimSpace(service) == "" {
		return Dashboard{}, invalidf("a template is instantiated for a named service, and this name is empty")
	}
	out := *d
	out.Template = false
	out.UID = ""
	out.Title = instanceTitle(d.Title, service)
	// Cloned because the loop below writes to an element: TemplateVars is a
	// slice, so *d's copy shares its backing array, and binding in place would
	// edit the template every future instance is made from.
	//
	// Widgets is deliberately *not* cloned. Nothing here writes to a widget —
	// that is the whole point of binding rather than rewriting — so the two
	// definitions sharing them costs nothing and copying a hundred of them,
	// each with its own nested slices, would need a deep copy to mean anything.
	out.TemplateVars = slices.Clone(d.TemplateVars)
	bound := false
	for i := range out.TemplateVars {
		if strings.EqualFold(strings.TrimSpace(out.TemplateVars[i].Name), TemplateVarService) {
			out.TemplateVars[i].Default = service
			bound = true
		}
	}
	if !bound {
		return Dashboard{}, invalidf(
			"%q is marked as a template but declares no %q variable, so there is nothing to bind",
			d.Title, TemplateVarService)
	}
	return out, nil
}

// instanceTitle is a template's title with the service appended, within
// [MaxTitle].
//
// The service name is what distinguishes one instance from another, so it is
// the part that survives a title too long to hold both. Only a service name
// longer than the whole limit — which a tag value, capped at 200 bytes
// including its key, cannot be — trims the name itself.
func instanceTitle(title, service string) string {
	suffix := ": " + service
	// `>= 0`, not `> 0`: a suffix that exactly fills the limit leaves room for
	// no title and is still a title. Written as `> 0` first, which dropped the
	// ": " from a 198-byte service name and was found by the sweep in
	// template_test.go rather than by reading this.
	if room := MaxTitle - len(suffix); room >= 0 {
		return truncate(title, room) + suffix
	}
	return truncate(service, MaxTitle)
}

// truncate cuts s to at most n bytes without splitting a rune. A title is
// bounded in bytes, and half a multi-byte character is not a title.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
