package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/dashboard"
	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// tagValues is a TagValueReader over a fixed map, recording what was asked.
//
// The recording is half the point: which series a metric's tags are looked up
// on is a decision (a distribution's live on its `.count`), and a fake that only
// returned values could not tell a right lookup from a lucky one.
type tagValues struct {
	values map[string][]string // "metric/key" -> values
	asked  []string
}

func (f *tagValues) TagValues(_ context.Context, metric, key string, limit int) ([]string, error) {
	f.asked = append(f.asked, metric+"/"+key)
	vals := f.values[metric+"/"+key]
	if len(vals) > limit {
		vals = vals[:limit]
	}
	return vals, nil
}

// types answers what the metadata database would about a metric's kind.
type types map[string]wire.Kind

func (t types) Metric(name string) (meta.Metric, bool) {
	k, ok := t[name]
	return meta.Metric{Name: name, Type: k}, ok
}

const serviceTemplate = `{"uid":"svc","title":"Service overview","template":true,
	"template_vars":[{"name":"service","tag":"service","default":"*"},
	                 {"name":"env","tag":"env","default":"*"}],
	"widgets":[
	  {"id":"w1","type":"timeseries","layout":{"x":0,"y":0,"w":6,"h":3},
	   "queries":[{"q":"sum:http.request.count{$service,$env} by {route}.as_rate()","display":"line"}]},
	  {"id":"w2","type":"timeseries","layout":{"x":6,"y":0,"w":6,"h":3},
	   "queries":[{"q":"p95:http.request.duration{$service}","display":"line"}]}]}`

// templatesAPI serves the dashboard endpoints over a real metadata database,
// with the store faked: the rows are the thing under test, the tag index is not.
func templatesAPI(t *testing.T, vals *tagValues, kinds types, defs ...string) (http.Handler, *meta.DB, *bytes.Buffer) {
	t.Helper()
	db, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for i, def := range defs {
		if _, err := db.CreateDashboard(context.Background(), meta.DashboardRow{
			UID: fmt.Sprintf("d%d", i), Title: fmt.Sprintf("d%d", i), Definition: []byte(def),
		}, now); err != nil {
			t.Fatal(err)
		}
	}
	var logs bytes.Buffer
	mux := http.NewServeMux()
	(&Dashboards{
		Store: db, Values: vals, Types: kinds, Clock: testutil.NewFakeClock(now),
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
	}).Register(mux)
	return mux, db, &logs
}

func stringsOf(t *testing.T, v any, where string) []string {
	t.Helper()
	items, ok := v.([]any)
	if !ok {
		t.Fatalf("%s is %T, not an array", where, v)
	}
	out := make([]string, len(items))
	for i, it := range items {
		s, ok := it.(string)
		if !ok {
			t.Fatalf("%s[%d] is %T, not a string", where, i, it)
		}
		out[i] = s
	}
	return out
}

// The endpoint's whole answer: which services a template covers, and where that
// set came from.
func TestTemplates_ServicesAreTheTagValuesOfTheTemplatesOwnMetrics(t *testing.T) {
	vals := &tagValues{values: map[string][]string{
		"http.request.count/service":          {"web", "checkout"},
		"http.request.duration.count/service": {"checkout", "billing"},
		// A metric no template queries. Nothing should look here.
		"queue.depth/service": {"worker"},
	}}
	h, _, _ := templatesAPI(t, vals, types{"http.request.duration": wire.KindDistribution}, serviceTemplate)

	rec, out := send(t, h, http.MethodGet, "/api/v1/dashboards/services", "")
	if rec.Code != 200 {
		t.Fatalf("%d %v", rec.Code, out)
	}
	got := stringsOf(t, out["services"], "services")
	want := []string{"billing", "checkout", "web"}
	if !slices.Equal(got, want) {
		t.Errorf("services = %v, want %v (the union, sorted and deduplicated)", got, want)
	}
	if out["count"] != float64(len(want)) {
		t.Errorf("count = %v, want %d", out["count"], len(want))
	}
	if out["truncated"] != false {
		t.Errorf("truncated = %v for a list of three", out["truncated"])
	}
	// A distribution's tags are on its `.count` series, because its own name
	// addresses sketches — which carry no tag index. Asking for
	// http.request.duration/service would find nothing, silently.
	sort.Strings(vals.asked)
	wantAsked := []string{"http.request.count/service", "http.request.duration.count/service"}
	if !slices.Equal(vals.asked, wantAsked) {
		t.Errorf("looked up %v, want %v", vals.asked, wantAsked)
	}
}

// A dashboard that is not a template contributes nothing — not its services,
// and not a lookup.
func TestTemplates_ANonTemplateIsNotADiscoverySource(t *testing.T) {
	vals := &tagValues{values: map[string][]string{"queue.depth/service": {"worker"}}}
	plain := `{"uid":"p","title":"Plain","widgets":[{"id":"w1","type":"timeseries",
		"layout":{"x":0,"y":0,"w":6,"h":3},"queries":[{"q":"avg:queue.depth{*}"}]}]}`
	h, _, _ := templatesAPI(t, vals, types{}, plain)

	rec, out := send(t, h, http.MethodGet, "/api/v1/dashboards/services", "")
	if rec.Code != 200 {
		t.Fatalf("%d %v", rec.Code, out)
	}
	if got := stringsOf(t, out["services"], "services"); len(got) != 0 {
		t.Errorf("services = %v for a deployment with no templates", got)
	}
	if len(vals.asked) != 0 {
		t.Errorf("looked up %v for a deployment with no templates", vals.asked)
	}
	// Empty rather than null: a client should not have to tell "none" from
	// "this field does not exist in your version".
	if !strings.Contains(rec.Body.String(), `"services":[]`) {
		t.Errorf("services is not an empty array: %s", rec.Body.String())
	}
}

func TestTemplates_InstantiatesEveryTemplateForTheService(t *testing.T) {
	second := strings.Replace(serviceTemplate, `"uid":"svc","title":"Service overview"`,
		`"uid":"svc2","title":"Service internals"`, 1)
	vals := &tagValues{values: map[string][]string{"http.request.count/service": {"checkout"}}}
	h, _, _ := templatesAPI(t, vals, types{}, serviceTemplate, second)

	rec, out := send(t, h, http.MethodGet, "/api/v1/dashboards/service/checkout", "")
	if rec.Code != 200 {
		t.Fatalf("%d %v", rec.Code, out)
	}
	if out["service"] != "checkout" {
		t.Errorf("service = %v", out["service"])
	}
	list, ok := out["dashboards"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("dashboards = %v, want two instances", out["dashboards"])
	}
	titles := map[string]bool{}
	for i, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("dashboards[%d] is %T", i, item)
		}
		// Provenance: which stored template this came from, so that "this chart
		// is wrong" leads to the file to edit.
		if entry["template_id"] == nil || entry["template_uid"] == nil {
			t.Errorf("dashboards[%d] does not say which template it came from: %v", i, entry)
		}
		def, ok := entry["dashboard"].(map[string]any)
		if !ok {
			t.Fatalf("dashboards[%d] has no nested definition: %v", i, entry)
		}
		title, _ := def["title"].(string)
		titles[title] = true
		if !strings.Contains(title, "checkout") {
			t.Errorf("dashboards[%d] title %q does not name the service", i, title)
		}
		// An instance is not itself a template, and has no uid: nothing stores
		// it, so a uid would promise a lookup that cannot work.
		if def["template"] != nil {
			t.Errorf("dashboards[%d] is still marked as a template", i)
		}
		if def["uid"] != nil {
			t.Errorf("dashboards[%d] carries uid %v", i, def["uid"])
		}
		// Bound, not rewritten.
		vars, ok := def["template_vars"].([]any)
		if !ok {
			t.Fatalf("dashboards[%d] lost its template_vars: %v", i, def)
		}
		bound := ""
		for _, v := range vars {
			m, _ := v.(map[string]any)
			if m["name"] == "service" {
				bound, _ = m["default"].(string)
			}
		}
		if bound != "checkout" {
			t.Errorf("dashboards[%d] bound service to %q", i, bound)
		}
		q := fmt.Sprint(def["widgets"])
		if !strings.Contains(q, "$service") {
			t.Errorf("dashboards[%d] rewrote its queries instead of binding: %v", i, def["widgets"])
		}
	}
	if len(titles) != 2 {
		t.Errorf("two templates produced titles %v; a picker cannot tell them apart", titles)
	}
}

// A typo in a URL pasted into a runbook must not render a grid of empty charts,
// which reads as "the service is down".
func TestTemplates_AnUnknownServiceIs404(t *testing.T) {
	vals := &tagValues{values: map[string][]string{"http.request.count/service": {"checkout"}}}
	h, _, _ := templatesAPI(t, vals, types{}, serviceTemplate)

	rec, out := send(t, h, http.MethodGet, "/api/v1/dashboards/service/chekcout", "")
	if rec.Code != 404 {
		t.Fatalf("%d %v, want 404", rec.Code, out)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "chekcout") {
		t.Errorf("error %q does not name what was asked for", msg)
	}
	if strings.Contains(msg, "truncated") {
		t.Errorf("error %q claims the list was truncated", msg)
	}
}

// Every service of a template that queries nothing the store has is unknown, so
// the endpoint is a 404 rather than an empty dashboard.
func TestTemplates_NoTemplatesMeansEveryServiceIs404(t *testing.T) {
	h, _, _ := templatesAPI(t, &tagValues{}, types{})
	rec, _ := send(t, h, http.MethodGet, "/api/v1/dashboards/service/checkout", "")
	if rec.Code != 404 {
		t.Errorf("%d, want 404", rec.Code)
	}
	rec, out := send(t, h, http.MethodGet, "/api/v1/dashboards/services", "")
	if rec.Code != 200 {
		t.Errorf("%d %v, want an empty 200", rec.Code, out)
	}
}

func TestTemplates_TheListIsCappedAndSaysSo(t *testing.T) {
	many := make([]string, maxServices+50)
	for i := range many {
		many[i] = fmt.Sprintf("svc-%05d", i)
	}
	vals := &tagValues{values: map[string][]string{"http.request.count/service": many}}
	h, _, _ := templatesAPI(t, vals, types{}, serviceTemplate)

	rec, out := send(t, h, http.MethodGet, "/api/v1/dashboards/services", "")
	if rec.Code != 200 {
		t.Fatalf("%d %v", rec.Code, out)
	}
	got := stringsOf(t, out["services"], "services")
	if len(got) != maxServices {
		t.Errorf("returned %d services, want the cap of %d", len(got), maxServices)
	}
	if out["truncated"] != true {
		t.Errorf("truncated = %v; a capped list that does not say so is indistinguishable from a complete one", out["truncated"])
	}
	// A service past the cap is a 404 whose message admits why it might be
	// wrong.
	rec, out = send(t, h, http.MethodGet, "/api/v1/dashboards/service/"+many[len(many)-1], "")
	if rec.Code != 404 {
		t.Fatalf("%d %v", rec.Code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "truncated") {
		t.Errorf("error %q does not admit that the list it looked in was truncated", msg)
	}
}

// The union is capped, not each lookup: twenty metrics of sixty services each
// is 1200 services with every lookup under its own limit.
func TestTemplates_TheCapIsOnTheUnion(t *testing.T) {
	first := make([]string, maxServices-10)
	second := make([]string, 20)
	for i := range first {
		first[i] = fmt.Sprintf("a-%05d", i)
	}
	for i := range second {
		second[i] = fmt.Sprintf("b-%05d", i)
	}
	vals := &tagValues{values: map[string][]string{
		"http.request.count/service":    first,
		"http.request.duration/service": second,
	}}
	h, _, _ := templatesAPI(t, vals, types{}, serviceTemplate)

	_, out := send(t, h, http.MethodGet, "/api/v1/dashboards/services", "")
	if out["truncated"] != true {
		t.Errorf("truncated = %v for %d services found in two lookups of %d and %d",
			out["truncated"], len(first)+len(second), len(first), len(second))
	}
	if got := stringsOf(t, out["services"], "services"); len(got) != maxServices {
		t.Errorf("returned %d services, want %d", len(got), maxServices)
	}
}

// A series tagged with a bare `service` and no value has no service to name, and
// an instance titled ": " is not one.
func TestTemplates_AnEmptyTagValueIsNotAService(t *testing.T) {
	vals := &tagValues{values: map[string][]string{"http.request.count/service": {"", "checkout"}}}
	h, _, _ := templatesAPI(t, vals, types{}, serviceTemplate)

	_, out := send(t, h, http.MethodGet, "/api/v1/dashboards/services", "")
	if got := stringsOf(t, out["services"], "services"); !slices.Equal(got, []string{"checkout"}) {
		t.Errorf("services = %q", got)
	}
}

// A name longer than a whole tag is not a service this store could hold, and a
// 404 quoting a megabyte of URL is its own problem.
func TestTemplates_AnOverlongNameIs400(t *testing.T) {
	h, _, _ := templatesAPI(t, &tagValues{}, types{}, serviceTemplate)
	rec, out := send(t, h, http.MethodGet,
		"/api/v1/dashboards/service/"+strings.Repeat("s", wire.MaxTagLen+1), "")
	if rec.Code != 400 {
		t.Fatalf("%d, want 400", rec.Code)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, fmt.Sprint(wire.MaxTagLen)) {
		t.Errorf("error %q does not say what the limit is", msg)
	}
}

// The route must not be read as a dashboard id: Go 1.22 prefers the literal
// segment over the {id} wildcard, and if it ever stopped doing so this endpoint
// would become "services is not a dashboard id".
func TestTemplates_TheRoutesDoNotCollideWithTheIDWildcard(t *testing.T) {
	h, _, _ := templatesAPI(t, &tagValues{}, types{}, serviceTemplate)
	rec, out := send(t, h, http.MethodGet, "/api/v1/dashboards/services", "")
	if rec.Code != 200 {
		t.Fatalf("%d %v: /services was not routed to the template endpoint", rec.Code, out)
	}
	// And a real id still works.
	rec, _ = send(t, h, http.MethodGet, "/api/v1/dashboards/1", "")
	if rec.Code != 200 {
		t.Errorf("%d for /api/v1/dashboards/1", rec.Code)
	}
}

// A row that says it is a template and does not validate is named, not hidden:
// that a template appearing nowhere is the least debuggable outcome available is
// the reason the validation rule exists in the first place.
func TestTemplates_AnInvalidTemplateRowIsNamedAndLoggedOnce(t *testing.T) {
	// Marked as a template, declares no service variable. Refused by the API and
	// by provisioning, so this is a hand-edited row — and it is *spliceable*,
	// which is why the CRUD list endpoint serves it happily.
	broken := `{"uid":"b","title":"Broken","template":true,
		"widgets":[{"id":"w1","type":"timeseries","layout":{"x":0,"y":0,"w":6,"h":3},
		"queries":[{"q":"sum:http.request.count{*}"}]}]}`
	vals := &tagValues{values: map[string][]string{"http.request.count/service": {"checkout"}}}
	h, _, logs := templatesAPI(t, vals, types{}, serviceTemplate, broken)

	rec, out := send(t, h, http.MethodGet, "/api/v1/dashboards/services", "")
	if rec.Code != 200 {
		t.Fatalf("%d %v: one broken row must not cost the whole answer", rec.Code, out)
	}
	if got := stringsOf(t, out["services"], "services"); !slices.Equal(got, []string{"checkout"}) {
		t.Errorf("services = %v; the working template still answers", got)
	}
	ids, ok := out["unreadable"].([]any)
	if !ok || len(ids) != 1 || ids[0] != float64(2) {
		t.Fatalf("unreadable = %v, want [2]", out["unreadable"])
	}
	if n := strings.Count(logs.String(), "a template dashboard does not validate"); n != 1 {
		t.Errorf("logged the reason %d times, want 1:\n%s", n, logs.String())
	}

	// Said once, not once per request — the endpoint is what a picker polls.
	send(t, h, http.MethodGet, "/api/v1/dashboards/services", "")
	if n := strings.Count(logs.String(), "a template dashboard does not validate"); n != 1 {
		t.Errorf("logged the reason %d times over two requests, want 1:\n%s", n, logs.String())
	}

	// And the CRUD list endpoint, which does not interpret the definition,
	// serves the row without complaint — clearing its own record. The two
	// conditions are independent, so this must not make the template failure
	// audible again.
	if rec, _ := send(t, h, http.MethodGet, "/api/v1/dashboards", ""); rec.Code != 200 {
		t.Fatalf("%d listing dashboards", rec.Code)
	}
	send(t, h, http.MethodGet, "/api/v1/dashboards/services", "")
	if n := strings.Count(logs.String(), "a template dashboard does not validate"); n != 1 {
		t.Errorf("a successful GET /api/v1/dashboards made the template failure audible again (%d lines):\n%s",
			n, logs.String())
	}
}

// A row that is not a template and does not parse is not this endpoint's
// business. GET /api/v1/dashboards is where a row nobody can read belongs.
func TestTemplates_AnInvalidNonTemplateRowIsNotReportedHere(t *testing.T) {
	junk := `{"uid":"j","title":"","widgets":[]}`
	h, _, logs := templatesAPI(t, &tagValues{}, types{}, junk)
	rec, out := send(t, h, http.MethodGet, "/api/v1/dashboards/services", "")
	if rec.Code != 200 {
		t.Fatalf("%d %v", rec.Code, out)
	}
	if ids, _ := out["unreadable"].([]any); len(ids) != 0 {
		t.Errorf("unreadable = %v for a row that never claimed to be a template", out["unreadable"])
	}
	if logs.Len() != 0 {
		t.Errorf("logged something about somebody else's dashboard:\n%s", logs.String())
	}
}

// Both dependencies are optional fields, so that a test of the CRUD endpoints
// needs neither a metric store nor a metadata registry. A deployment that wired
// one and not the other gets a reason rather than "there are no services".
func TestTemplates_WithoutAMetricStoreThePointIs503(t *testing.T) {
	for name, d := range map[string]*Dashboards{
		"no values": {Types: types{}},
		"no types":  {Values: &tagValues{}},
		"neither":   {},
	} {
		mux := http.NewServeMux()
		d.Store = failingStore{errors.New("unused")}
		d.Clock = testutil.NewFakeClock(now)
		d.Register(mux)
		for _, path := range []string{"/api/v1/dashboards/services", "/api/v1/dashboards/service/x"} {
			rec, out := send(t, mux, http.MethodGet, path, "")
			if rec.Code != 503 {
				t.Errorf("%s: %s gave %d, want 503", name, path, rec.Code)
				continue
			}
			if msg, _ := out["error"].(string); !strings.Contains(msg, "metric store") {
				t.Errorf("%s: %s said %q, which does not say what is missing", name, path, msg)
			}
		}
	}
}

// A failure reading the tag index is ours, so the body says nothing about it.
func TestTemplates_ATagIndexFailureIs500AndSaysNothing(t *testing.T) {
	db, err := meta.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.CreateDashboard(context.Background(), meta.DashboardRow{
		UID: "svc", Title: "svc", Definition: []byte(serviceTemplate),
	}, now); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&Dashboards{
		Store: db, Values: failingValues{}, Types: types{}, Clock: testutil.NewFakeClock(now),
		Logger: slog.New(slog.DiscardHandler),
	}).Register(mux)

	for _, path := range []string{"/api/v1/dashboards/services", "/api/v1/dashboards/service/x"} {
		rec, out := send(t, mux, http.MethodGet, path, "")
		if rec.Code != 500 {
			t.Errorf("%s gave %d, want 500", path, rec.Code)
			continue
		}
		if msg, _ := out["error"].(string); strings.Contains(msg, "no such table") {
			t.Errorf("%s leaked the store's internals: %q", path, msg)
		}
	}
}

type failingValues struct{}

func (failingValues) TagValues(context.Context, string, string, int) ([]string, error) {
	return nil, errors.New("pebble: no such table: tags")
}

// The instance is this build's re-encoding of the definition, not the author's
// bytes — so it round-trips through the definition's own schema, which is worth
// asserting because the CRUD endpoints promise the opposite.
func TestTemplates_AnInstanceIsAValidDefinition(t *testing.T) {
	vals := &tagValues{values: map[string][]string{"http.request.count/service": {"checkout"}}}
	h, _, _ := templatesAPI(t, vals, types{}, serviceTemplate)

	rec, _ := send(t, h, http.MethodGet, "/api/v1/dashboards/service/checkout", "")
	if rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	var body struct {
		Dashboards []struct {
			Dashboard json.RawMessage `json:"dashboard"`
		} `json:"dashboards"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Dashboards) != 1 {
		t.Fatalf("%d instances", len(body.Dashboards))
	}
	// The same gate a POST would put it through: this is what makes "save a copy
	// of this instance" possible.
	if _, err := dashboard.Parse(body.Dashboards[0].Dashboard); err != nil {
		t.Errorf("the instance would not be accepted by POST /api/v1/dashboards: %v", err)
	}
}

// A template that breaks, gets fixed and breaks again the same way is reported
// both times. Without that, "say it once" means "say it once ever", and the
// second outage is silent.
func TestTemplates_AFixedThenRebrokenTemplateIsReportedAgain(t *testing.T) {
	broken := `{"uid":"b","title":"Broken","template":true,
		"widgets":[{"id":"w1","type":"timeseries","layout":{"x":0,"y":0,"w":6,"h":3},
		"queries":[{"q":"sum:http.request.count{*}"}]}]}`
	h, db, logs := templatesAPI(t, &tagValues{}, types{}, broken)
	const line = "a template dashboard does not validate"
	ctx := context.Background()

	send(t, h, http.MethodGet, "/api/v1/dashboards/services", "")
	if n := strings.Count(logs.String(), line); n != 1 {
		t.Fatalf("logged %d times, want 1", n)
	}
	if _, err := db.UpdateDashboard(ctx, 1, meta.DashboardRow{
		UID: "b", Title: "Fixed", Definition: []byte(serviceTemplate),
	}, now); err != nil {
		t.Fatal(err)
	}
	send(t, h, http.MethodGet, "/api/v1/dashboards/services", "")
	if _, err := db.UpdateDashboard(ctx, 1, meta.DashboardRow{
		UID: "b", Title: "Broken", Definition: []byte(broken),
	}, now); err != nil {
		t.Fatal(err)
	}
	send(t, h, http.MethodGet, "/api/v1/dashboards/services", "")
	if n := strings.Count(logs.String(), line); n != 2 {
		t.Errorf("logged %d times across break, fix and break, want 2:\n%s", n, logs.String())
	}
}

// A tag value is free text, so `service: ` is a value the intake accepts — and
// a blank name is one Instantiate refuses. Discovering it would put a name in
// the list that the instantiation endpoint then answers 500 for.
func TestTemplates_ABlankTagValueIsNotAService(t *testing.T) {
	vals := &tagValues{values: map[string][]string{
		"http.request.count/service": {" ", "\t", "checkout"},
	}}
	h, _, _ := templatesAPI(t, vals, types{}, serviceTemplate)

	_, out := send(t, h, http.MethodGet, "/api/v1/dashboards/services", "")
	if got := stringsOf(t, out["services"], "services"); !slices.Equal(got, []string{"checkout"}) {
		t.Errorf("services = %q", got)
	}
	// And the name it would have produced is a 404 rather than a 500.
	rec, _ := send(t, h, http.MethodGet, "/api/v1/dashboards/service/%20", "")
	if rec.Code != 404 {
		t.Errorf("a blank service name gave %d, want 404", rec.Code)
	}
}

// bigTemplate is a template naming one distinct metric per query, up to the
// limits a definition may hold: 100 widgets x 10 queries.
func bigTemplate(t *testing.T, metrics int) string {
	t.Helper()
	d := map[string]any{
		"uid": "big", "title": "Big", "template": true,
		"template_vars": []any{map[string]any{"name": "service", "tag": "service", "default": "*"}},
	}
	var widgets []any
	for i := 0; len(widgets) < (metrics+9)/10; i++ {
		var queries []any
		for j := 0; j < 10 && i*10+j < metrics; j++ {
			queries = append(queries, map[string]any{
				"q": fmt.Sprintf("sum:m%04d{$service}", i*10+j), "display": "line",
			})
		}
		widgets = append(widgets, map[string]any{
			"id": fmt.Sprintf("w%d", i), "type": "timeseries",
			"layout":  map[string]any{"x": 6 * (i % 2), "y": 3 * (i / 2), "w": 6, "h": 3},
			"queries": queries,
		})
	}
	d["widgets"] = widgets
	body, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dashboard.Parse(body); err != nil {
		t.Fatalf("the fixture is supposed to be a valid template: %v", err)
	}
	return string(body)
}

// The amplification bound. A template may name a thousand metrics and nothing
// bounds how many templates exist, so without a cap one unauthenticated GET can
// ask the store about tens of thousands of metrics.
func TestTemplates_TheLookupsPerRequestAreBounded(t *testing.T) {
	vals := &tagValues{values: map[string][]string{"m0000/service": {"checkout"}}}
	// Four maximal templates: 4000 metrics named, 500 lookups allowed.
	big := bigTemplate(t, 1000)
	h, _, _ := templatesAPI(t, vals, types{}, big,
		strings.Replace(big, `"uid":"big"`, `"uid":"big2"`, 1),
		strings.Replace(big, `"uid":"big"`, `"uid":"big3"`, 1),
		strings.Replace(big, `"uid":"big"`, `"uid":"big4"`, 1))

	rec, out := send(t, h, http.MethodGet, "/api/v1/dashboards/services", "")
	if rec.Code != 200 {
		t.Fatalf("%d %v", rec.Code, out)
	}
	if len(vals.asked) > maxLookups {
		t.Errorf("one request caused %d tag lookups; the cap is %d", len(vals.asked), maxLookups)
	}
	if out["truncated"] != true {
		t.Errorf("truncated = %v after spending the whole lookup budget", out["truncated"])
	}
	// What it did look up, it still answers from.
	if got := stringsOf(t, out["services"], "services"); !slices.Equal(got, []string{"checkout"}) {
		t.Errorf("services = %v", got)
	}
	// And every template is still instantiable, because that costs no lookups.
	_, out = send(t, h, http.MethodGet, "/api/v1/dashboards/service/checkout", "")
	if out["count"] != float64(4) {
		t.Errorf("count = %v, want all four templates", out["count"])
	}
}

// Once the service cap is reached there is nothing left to learn, so the
// remaining metrics are not asked about.
func TestTemplates_DiscoveryStopsOnceTheServiceCapIsReached(t *testing.T) {
	many := make([]string, maxServices+1)
	for i := range many {
		many[i] = fmt.Sprintf("svc-%05d", i)
	}
	vals := &tagValues{values: map[string][]string{"m0000/service": many}}
	h, _, _ := templatesAPI(t, vals, types{}, bigTemplate(t, 40))

	rec, out := send(t, h, http.MethodGet, "/api/v1/dashboards/services", "")
	if rec.Code != 200 {
		t.Fatalf("%d %v", rec.Code, out)
	}
	if out["truncated"] != true {
		t.Errorf("truncated = %v", out["truncated"])
	}
	if len(vals.asked) != 1 {
		t.Errorf("asked %d metrics after the first one filled the cap: %v", len(vals.asked), vals.asked)
	}
}

// How much a template endpoint costs when most dashboards are not templates.
//
// The interesting number is the non-template row: discovery has to learn one
// boolean from it, and dashboard.Parse answers that by validating the whole
// definition — which means running the metricql parser over every query in it.
// Fifty ordinary dashboards of a thousand queries each is 50,000 query parses
// for a request that wanted none of them.
//
// Measured on an M-series laptop, 50 such dashboards, none of them templates:
//
//	Parse first (what this was):   73ms/op
//	claimsTemplate first:        10.3ms/op
//
// The remaining 10ms is reading 50 definitions out of SQLite and scanning them
// for one field, which is the floor for "is any of these a template" without a
// column to index. If these endpoints ever get hot enough to care, that column —
// or a cache — is the next step, and this benchmark is how to tell.
func BenchmarkDiscover(b *testing.B) {
	t := &testing.T{}
	plain := strings.Replace(bigTemplate(t, 1000), `"template":true,`, "", 1)
	if t.Failed() {
		b.Fatal("fixture")
	}
	defs := make([]string, 50)
	for i := range defs {
		defs[i] = strings.Replace(plain, `"uid":"big"`, fmt.Sprintf(`"uid":"big%d"`, i), 1)
	}
	db, err := meta.Open(filepath.Join(b.TempDir(), "meta.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	for i, def := range defs {
		if _, err := db.CreateDashboard(context.Background(), meta.DashboardRow{
			UID: fmt.Sprintf("d%d", i), Title: "d", Definition: []byte(def),
		}, now); err != nil {
			b.Fatal(err)
		}
	}
	d := &Dashboards{Store: db, Values: &tagValues{}, Types: types{},
		Clock: testutil.NewFakeClock(now), Logger: slog.New(slog.DiscardHandler)}
	b.ResetTimer()
	for range b.N {
		if _, err := d.discover(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}
