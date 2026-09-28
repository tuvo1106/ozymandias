package dashboard

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// template is good() marked as one, with the variable a template must declare.
func template() Dashboard {
	d := good()
	d.UID = "service"
	d.Title = "Service overview"
	d.Template = true
	d.TemplateVars = append(d.TemplateVars, TemplateVar{Name: "service", Tag: "service", Default: "*"})
	d.Widgets[0].Queries[0].Q = "sum:http.request.count{$service,$env} by {route}.as_rate()"
	return d
}

func TestTemplate_TheFixtureIsAValidTemplate(t *testing.T) {
	d := template()
	if err := d.Validate(); err != nil {
		t.Fatalf("the fixture is supposed to be valid: %v", err)
	}
}

func TestInstantiate_BindsTheServiceVariable(t *testing.T) {
	d := template()
	inst, err := d.Instantiate("checkout")
	if err != nil {
		t.Fatal(err)
	}
	var got *TemplateVar
	for i := range inst.TemplateVars {
		if inst.TemplateVars[i].Name == "service" {
			got = &inst.TemplateVars[i]
		}
	}
	if got == nil {
		t.Fatal("the instance dropped the service variable, so its queries have nothing to resolve $service against")
	}
	if got.Default != "checkout" {
		t.Errorf("service default is %q, want %q", got.Default, "checkout")
	}
	// Bound, not rewritten: the query text is the template's, and the evaluator
	// is what resolves it. A rewrite here would mean the definition a reader
	// sees and the query the server runs are two different strings.
	if q := inst.Widgets[0].Queries[0].Q; !strings.Contains(q, "$service") {
		t.Errorf("query %q no longer mentions $service, so something rewrote it", q)
	}
	// The other variable is left alone — an instance still has an env selector.
	for _, v := range inst.TemplateVars {
		if v.Name == "env" && v.Default != "*" {
			t.Errorf("env default is %q; instantiating a service should not touch it", v.Default)
		}
	}
}

// The bug a shallow copy would have: TemplateVars is a slice, so an instance
// made from *d shares its backing array and binding in place edits the
// template every later instance is made from. Two instances would then both
// name whichever service was asked for last.
func TestInstantiate_DoesNotEditTheTemplate(t *testing.T) {
	d := template()
	first, err := d.Instantiate("checkout")
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.Instantiate("billing")
	if err != nil {
		t.Fatal(err)
	}
	if got := d.ServiceTag(); got != "service" {
		t.Fatalf("the template's service tag is %q", got)
	}
	for _, v := range d.TemplateVars {
		if v.Name == "service" && v.Default != "*" {
			t.Errorf("the template's service default is now %q; instantiating edited the template", v.Default)
		}
	}
	if !d.Template {
		t.Error("the template is no longer marked as one")
	}
	if d.UID != "service" {
		t.Errorf("the template's uid is now %q", d.UID)
	}
	if d.Title != "Service overview" {
		t.Errorf("the template's title is now %q", d.Title)
	}
	firstDefault, secondDefault := "", ""
	for _, v := range first.TemplateVars {
		if v.Name == "service" {
			firstDefault = v.Default
		}
	}
	for _, v := range second.TemplateVars {
		if v.Name == "service" {
			secondDefault = v.Default
		}
	}
	if firstDefault != "checkout" || secondDefault != "billing" {
		t.Errorf("instances bound %q and %q; the second overwrote the first", firstDefault, secondDefault)
	}
}

func TestInstantiate_ClearsWhatAnInstanceIsNot(t *testing.T) {
	d := template()
	inst, err := d.Instantiate("checkout")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Template {
		t.Error("the instance is still marked as a template, so a client listing templates would find it")
	}
	// A uid is the identity provisioning upserts by, and nothing stores an
	// instance: carrying one would promise that GET /api/v1/dashboards can find
	// it.
	if inst.UID != "" {
		t.Errorf("the instance carries uid %q", inst.UID)
	}
	if !strings.Contains(inst.Title, "checkout") {
		t.Errorf("title %q does not name the service, so a picker shows N identical rows", inst.Title)
	}
}

// The property that makes "save a copy of this instance" possible: an instance
// is a definition the API would accept.
func TestInstantiate_TheInstanceValidates(t *testing.T) {
	for _, service := range []string{"checkout", "a", strings.Repeat("s", 150), "Mixed-Case.svc"} {
		d := template()
		inst, err := d.Instantiate(service)
		if err != nil {
			t.Fatalf("%q: %v", service, err)
		}
		if err := inst.Validate(); err != nil {
			t.Errorf("the instance for %q does not validate: %v", service, err)
		}
		if len(inst.Title) > MaxTitle {
			t.Errorf("the instance for %q has a %d-byte title; the limit is %d", service, len(inst.Title), MaxTitle)
		}
	}
}

// A title long enough to crowd out the service name loses its own tail, not the
// name: the name is the only thing that tells two instances apart.
func TestInstantiate_ALongTitleKeepsTheServiceName(t *testing.T) {
	d := template()
	d.Title = strings.Repeat("x", MaxTitle)
	inst, err := d.Instantiate("checkout")
	if err != nil {
		t.Fatal(err)
	}
	if len(inst.Title) > MaxTitle {
		t.Errorf("title is %d bytes, over the %d limit", len(inst.Title), MaxTitle)
	}
	if !strings.HasSuffix(inst.Title, ": checkout") {
		t.Errorf("title %q does not end with the service name", inst.Title)
	}
}

// Truncating by bytes must not split a rune, or the title is not valid UTF-8 —
// and JSON encoding turns half a character into U+FFFD, silently.
func TestInstantiate_ALongTitleIsCutBetweenRunes(t *testing.T) {
	d := template()
	// 'é' is two bytes, so a byte-exact cut lands inside one for one of these.
	for _, n := range []int{MaxTitle, MaxTitle - 1, MaxTitle + 1} {
		d.Title = strings.Repeat("é", n)
		inst, err := d.Instantiate("svc")
		if err != nil {
			t.Fatal(err)
		}
		if !utf8Valid(inst.Title) {
			t.Errorf("title from %d é's is not valid UTF-8: %q", n, inst.Title)
		}
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

func TestInstantiate_RefusesWhatItCannotInstantiate(t *testing.T) {
	notATemplate := good()
	if _, err := notATemplate.Instantiate("checkout"); err == nil {
		t.Error("a definition that is not a template was instantiated anyway")
	} else if !errors.Is(err, ErrInvalid) {
		t.Errorf("%v does not classify as ErrInvalid", err)
	}

	d := template()
	if _, err := d.Instantiate("   "); err == nil {
		t.Error("an empty service name was accepted, so the instance is titled \": \"")
	}

	// Marked as a template with no service variable. Validate refuses this, so
	// it only reaches Instantiate from a hand-edited row — which is exactly the
	// case the HTTP layer reports rather than crashing on.
	handEdited := good()
	handEdited.Template = true
	if _, err := handEdited.Instantiate("checkout"); err == nil {
		t.Error("a template with no service variable was instantiated anyway")
	} else if !strings.Contains(err.Error(), TemplateVarService) {
		t.Errorf("%v does not name the missing variable", err)
	}
}

func TestServiceTag_IsTheTagNotTheName(t *testing.T) {
	d := template()
	// The whole reason Tag is separate from Name: a dashboard calls it
	// `service` while the store spells the label something else.
	d.TemplateVars[1].Tag = "Service.Name"
	if got := d.ServiceTag(); got != "service.name" {
		t.Errorf("ServiceTag() = %q, want the lower-cased tag key", got)
	}
	plain := good()
	if got := plain.ServiceTag(); got != "" {
		t.Errorf("ServiceTag() = %q for a dashboard with no service variable", got)
	}
}

func TestMetrics_AreTheMetricsTheQueriesRead(t *testing.T) {
	d := template()
	d.Widgets = append(d.Widgets,
		Widget{
			ID: "note", Type: TypeNote, Layout: Layout{X: 0, Y: 3, W: 6, H: 1},
			Markdown: "no queries here",
		},
		Widget{
			ID: "ratio", Type: TypeQueryValue, Layout: Layout{X: 0, Y: 4, W: 6, H: 2},
			Queries: []Query{{
				// An expression, and one of its two leaves repeats a metric the
				// first widget already asked for.
				Q:       "sum:http.request.count{$service,status:5*} / sum:http.request.count{$service} * 100",
				Reducer: ReducerLast,
			}},
		},
		Widget{
			ID: "latency", Type: TypeTimeseries, Layout: Layout{X: 0, Y: 6, W: 6, H: 2},
			Queries: []Query{{Q: "p95:http.request.duration{$service}"}},
		},
		// Last in the definition and first in the answer, so that a missing
		// sort is a failure rather than a fixture that happened to be in order.
		Widget{
			ID: "connections", Type: TypeTimeseries, Layout: Layout{X: 0, Y: 8, W: 6, H: 2},
			Queries: []Query{{Q: "avg:db.connections.active{$service}"}},
		},
	)
	if err := d.Validate(); err != nil {
		t.Fatalf("the fixture is supposed to be valid: %v", err)
	}
	got, err := d.Metrics()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"db.connections.active", "http.request.count", "http.request.duration"}
	if len(got) != len(want) {
		t.Fatalf("Metrics() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Metrics() = %v, want %v (sorted and deduplicated)", got, want)
		}
	}
	// The metric as written: a percentile selects on <metric>.count, and which
	// metrics are distributions is something this package does not know.
	for _, m := range got {
		if strings.HasSuffix(m, ".count") && m != "http.request.count" {
			t.Errorf("Metrics() returned %q, a derived series rather than the metric", m)
		}
	}
}

func TestMetrics_AnUnparseableQueryIsAnError(t *testing.T) {
	d := template()
	// Not reachable through Validate, which parses every query — so this is a
	// hand-edited row. Reported rather than skipped: a dropped metric quietly
	// shortens the list of services the template covers.
	d.Widgets[0].Queries[0].Q = "sum:{"
	if _, err := d.Metrics(); err == nil {
		t.Error("an unparseable query was skipped rather than reported")
	}
	// An empty query is not a parse error, because Validate already has a
	// better message for it.
	d.Widgets[0].Queries[0].Q = "  "
	got, err := d.Metrics()
	if err != nil {
		t.Errorf("an empty query: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Metrics() = %v for a dashboard whose only query is blank", got)
	}
}

// The title invariant, swept over every length that can matter: inside the byte
// limit, still valid UTF-8, and still naming the service as far as the limit
// allows.
//
// A sweep rather than a property test, and that is a correction: the first
// version of this drew titles from rapid and passed with the rune-boundary walk
// removed *and* with the service name dropped entirely, because rapid biases
// toward small values — over 100 runs its longest title was 45 bytes against a
// 200-byte limit, so the truncation branch never ran. The region where anything
// is cut is small enough to enumerate, so it is enumerated. Four rune widths,
// because the bug this guards is byte arithmetic on a string measured in runes.
func TestInstanceTitle_StaysWithinTheLimitAndKeepsTheName(t *testing.T) {
	for _, pad := range []rune{'a', 'é', 'ᚠ', '😀'} {
		w := utf8.RuneLen(pad)
		for runes := 0; runes <= (MaxTitle+8)/w; runes++ {
			title := strings.Repeat(string(pad), runes)
			for _, service := range []string{
				"a", "checkout", strings.Repeat("s", MaxTitle-3), strings.Repeat("s", MaxTitle-2),
				strings.Repeat("s", MaxTitle-1), strings.Repeat("s", MaxTitle+1),
			} {
				got := instanceTitle(title, service)
				where := fmt.Sprintf("%d×%q + %d-byte service", runes, pad, len(service))
				if len(got) > MaxTitle {
					t.Fatalf("%s: %d bytes, over the %d limit", where, len(got), MaxTitle)
				}
				if !utf8.ValidString(got) {
					t.Fatalf("%s: not valid UTF-8: %q", where, got)
				}
				// The service is what tells two instances apart, so it survives
				// whole whenever it fits, and the title is what gets cut when it
				// does not.
				if len(service)+2 > MaxTitle {
					continue
				}
				if !strings.HasSuffix(got, ": "+service) {
					t.Fatalf("%s: %q does not end with the service name", where, got)
				}
				if kept := truncate(title, MaxTitle-len(service)-2); !strings.HasPrefix(got, kept) {
					t.Fatalf("%s: %q lost more of the title than it had to (%d bytes were available)",
						where, got, len(kept))
				}
			}
		}
	}
}
