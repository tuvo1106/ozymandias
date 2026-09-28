package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/tuvo1106/ozymandias/internal/dashboard"
	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// maxServices bounds a services listing, and with it the number of dashboards
// the picker can offer.
//
// 1000 is deliberately the same number as eval.MaxSeriesPerNode: past it, a
// template's own queries would be refused for selecting too many series, so a
// deployment with more services than this has a bigger problem than a
// truncated list. A response that is truncated says so.
const maxServices = 1000

// maxLookups bounds the tag-index lookups one request may cause, across every
// template.
//
// Without it this endpoint is an amplifier. A template may hold
// [dashboard.MaxWidgets] x [dashboard.MaxQueriesPerWidget] queries, each naming
// at least one metric, and the number of *templates* is bounded by nothing at
// all — anybody who can POST a dashboard can mark it as one. So a single
// unauthenticated GET could ask the store for the values of a tag on tens of
// thousands of metrics, which on the naive engine is that many SQL queries. The
// cap is deliberately far above what a real deployment needs (the shipped
// template names two metrics; ten rich ones would name a few hundred), and a
// request that hits it says `truncated` rather than presenting a partial list as
// the whole one.
//
// A count, not a deadline. A 30-second budget like the query path's would bound
// the wall clock and make the answer depend on how busy the machine is, and a
// list of services that changes under load is not something a picker can be
// built on.
const maxLookups = 500

// TagValueReader is what template instantiation needs from the metric store:
// the values a tag key takes on a metric.
//
// An interface, and this narrow, for the usual two reasons — a handler test
// should not need a TSDB to prove that an unknown service is a 404, and the
// dashboard endpoints have no business with the other six methods of
// [tsdb.MetricStore] — Append least of all.
type TagValueReader interface {
	TagValues(ctx context.Context, metric, key string, limit int) ([]string, error)
}

// templateRow is a stored template and its parsed definition.
//
// Both, because the two halves answer different questions: the row supplies the
// provenance a virtual dashboard would otherwise have none of (which file, which
// id), and the definition is what gets instantiated.
type templateRow struct {
	row meta.DashboardRow
	def dashboard.Dashboard
}

// discovery is what both template endpoints have to learn from the store before
// they can answer: which templates exist, and which services they cover.
//
// One type because the two endpoints need exactly the same thing — the list
// endpoint answers `services` and the instantiation endpoint uses it to tell an
// unknown service from a real one — and two functions that each did half of it
// would drift.
type discovery struct {
	// templates are the ones that can be instantiated: a row that says it is a
	// template and does not validate is not here, it is in unreadable.
	templates []templateRow
	services  []string
	truncated bool
	// unreadable names the rows that were dropped, so the response can say a
	// template exists and could not be used — the same bargain
	// [Dashboards.list] strikes, for the same reason: a hand-edited row should
	// cost its own entry, not everybody's dashboards.
	unreadable []int64
}

// discover reads the templates and the services they cover.
//
// # Where the services come from
//
// From the tag values of the metrics the templates themselves query. There is no
// catalogue of services in ozymandias — the metadata database records a metric's
// type and when it was first seen, not which services report it — so the
// question "which services are there?" has to be asked of the data, and the tag
// index is the only thing that can answer it without a scan. ADR-0020 has the
// alternatives and what this costs: the set is "services the store still holds
// one of these metrics for", not "services seen in the last day" as the M3 spec
// asked for.
//
// A service that reports none of a template's metrics is not in the list, which
// is an answer rather than a gap: instantiating the template for it would draw a
// dashboard of empty charts.
//
// # What a failure means here
//
// A row that says it is a template and does not validate is *named* — that is
// what unreadable is for.
//
// Everything after [dashboard.Parse] is an error this build should not be able
// to produce, so it becomes a 500 rather than a quietly shorter list of
// services. Parse runs Validate, which parses every query and requires the
// service variable, so [dashboard.Dashboard.Metrics] has nothing left to refuse,
// and [dashboard.Dashboard.Instantiate] refuses only a definition that is not a
// template and a blank service name — the first excluded by the branch above,
// the second by the filter on the values below.
//
// That last one is why the filter is not just `v != ""`. A tag value is free
// text, so `service: ` reaches the store; it was discovered, and the
// instantiation endpoint answered it with a 500. An unreachable branch here is
// unreachable because something upstream excludes it, and that is worth writing
// down rather than asserting.
func (d *Dashboards) discover(ctx context.Context) (discovery, error) {
	rows, err := d.Store.Dashboards(ctx)
	if err != nil {
		return discovery{}, err
	}
	out := discovery{unreadable: []int64{}}
	seen := map[string]bool{}
	lookups := 0
	for _, row := range rows {
		// The cheap question first. Most rows are somebody's ordinary
		// dashboard, and all this needs from one is a single boolean —
		// while [dashboard.Parse] answers it by validating the whole
		// definition, which runs the metricql parser over every query in it.
		// Fifty ordinary dashboards of a thousand queries each measured 73ms a
		// request in BenchmarkDiscover, for rows the answer then discards.
		if !claimsTemplate(row.Definition) {
			continue
		}
		def, err := dashboard.Parse(row.Definition)
		if err != nil {
			// Reported, because the row *meant* to be a template: that is what
			// the shallow check above establishes, and a template that appears
			// nowhere is the least debuggable outcome there is. An ordinary row
			// that will not parse is not this endpoint's business — the list
			// endpoint is where a row nobody can read belongs.
			d.logDropped(row, err, "a template dashboard does not validate, so it is instantiated for nobody")
			out.unreadable = append(out.unreadable, row.ID)
			continue
		}
		d.forget(report{row.ID, reportTemplate})
		metrics, err := def.Metrics()
		if err != nil {
			return discovery{}, fmt.Errorf("dashboard %d validated and then would not read: %w", row.ID, err)
		}
		key := def.ServiceTag()
		for _, m := range metrics {
			// Two budgets. Spending the lookup one makes the answer partial, so
			// it is declared; reaching the service cap does not, because the
			// truncation below declares it either way — and either way there is
			// nothing left to learn from asking again.
			if lookups >= maxLookups {
				out.truncated = true
				break
			}
			if len(seen) > maxServices {
				break
			}
			lookups++
			// One lookup past the cap, so that a single metric with more values
			// than the cap is caught by the check below rather than looking
			// exactly full.
			vals, err := d.Values.TagValues(ctx, d.seriesOf(m), key, maxServices+1)
			if err != nil {
				return discovery{}, fmt.Errorf("the values of %s on %s: %w", key, m, err)
			}
			for _, v := range vals {
				// A series carrying the bare tag `service`, or one whose value
				// is a space, has no service to name — and an instance titled
				// ": " is not one. Trimmed rather than compared to "": a tag
				// value is free text, so `service: ` is a value the intake
				// accepts, and [dashboard.Dashboard.Instantiate] refuses a blank
				// name. Discovering one would put a name in the list that the
				// instantiation endpoint then could not serve.
				if strings.TrimSpace(v) != "" {
					seen[v] = true
				}
			}
		}
		// Appended even if the loop above spent its budget without asking
		// anything: instantiating a template needs no lookups, so a request for
		// a service that *was* discovered still gets every template's view of
		// it.
		out.templates = append(out.templates, templateRow{row: row, def: def})
	}
	// Sorted, so that the picker's order does not depend on which metric was
	// queried first — and onto an empty slice rather than through slices.Sorted,
	// whose answer for an empty map is nil: `"services": null` and
	// `"services": []` are the same thing to a Go client and not to a
	// JavaScript one.
	out.services = slices.AppendSeq([]string{}, maps.Keys(seen))
	slices.Sort(out.services)
	// Capped on the union rather than per lookup: twenty metrics of sixty
	// services each is 1200 services, with every one of the twenty lookups
	// comfortably under its own limit.
	if len(out.services) > maxServices {
		out.services = out.services[:maxServices]
		out.truncated = true
	}
	return out, nil
}

// claimsTemplate reports whether these bytes are a definition that means to be a
// template, without checking anything else about it.
//
// It does two jobs, and the second one is why it runs before
// [dashboard.Parse] rather than after:
//
//   - it is how a row that says `"template": true` and does not validate gets
//     named instead of vanishing — Parse cannot answer that, because it answers
//     by failing and its error says nothing about what the author intended;
//   - it keeps the cost of these endpoints proportional to the number of
//     *templates* rather than to the number of dashboards. Parse validates, and
//     validating means parsing every query in the definition, which is a lot of
//     work to discover that a row is somebody's ordinary dashboard.
//
// Unknown fields are deliberately allowed: this is not the validating path. A
// definition that is not JSON at all claims nothing and is skipped, which is not
// a hole — such a row is already named by `GET /api/v1/dashboards`, whose
// response is where a row nobody can read belongs.
func claimsTemplate(def []byte) bool {
	var head struct {
		Template bool `json:"template"`
	}
	return json.Unmarshal(def, &head) == nil && head.Template
}

// seriesOf is the series a metric's tags are on: itself, unless it is a
// distribution, whose tags live on its `.count` (wire protocol §D). The same
// mapping the percentile path makes, for the same reason — a distribution's own
// name addresses sketches, which carry no tag index of their own.
//
// A metric the metadata database has never seen is left alone. It is a
// dashboard written before the service shipped, and guessing a suffix for it
// would turn a metric that does not exist into a different metric that does not
// exist.
func (d *Dashboards) seriesOf(metric string) string {
	if m, ok := d.Types.Metric(metric); ok && m.Type == wire.KindDistribution {
		return metric + wire.SuffixCount
	}
	return metric
}

// ready reports whether this build can instantiate templates at all, answering
// 503 if it cannot.
//
// Both dependencies are optional fields on [Dashboards] so that the CRUD
// endpoints — which are the ones a test usually wants — need neither a metric
// store nor a metadata registry. ozyd always wires both. A 503 here is a build
// that wired one or neither, and it says so rather than answering "there are no
// services", which would be a lie with a 200 on it.
func (d *Dashboards) ready(w http.ResponseWriter) bool {
	if d.Values == nil || d.Types == nil {
		writeError(w, http.StatusServiceUnavailable,
			errors.New("this server cannot instantiate dashboard templates: it has no metric store to discover services from"))
		return false
	}
	return true
}

// services handles GET /api/v1/dashboards/services.
func (d *Dashboards) services(w http.ResponseWriter, r *http.Request) {
	if !d.ready(w) {
		return
	}
	disc, err := d.discover(r.Context())
	if err != nil {
		d.fail(w, r, "discovering the services templates cover", err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Status   string   `json:"status"`
		Count    int      `json:"count"`
		Services []string `json:"services"`
		// Truncated says the list is capped, so a client can tell "these are
		// all of them" from "these are the first thousand" instead of guessing
		// from the length.
		Truncated  bool    `json:"truncated"`
		Unreadable []int64 `json:"unreadable"`
	}{"ok", len(disc.services), disc.services, disc.truncated, disc.unreadable})
}

// serviceDashboard is one instantiated template on the wire.
//
// The definition is nested under `dashboard` rather than spliced beside the
// provenance, which is what a stored dashboard does — and the reason is not
// taste. [storedDashboard.MarshalJSON] splices because it is serving the
// author's bytes and has a guarantee to keep about them. An instance is not the
// author's bytes: it is this build's re-encoding of the definition with one
// default changed, so there is nothing to preserve, the "verbatim" guarantee
// does not apply, and a response shaped like the one that makes that promise
// would imply it does.
type serviceDashboard struct {
	// TemplateID and TemplateUID say which stored template this came from, so
	// that "this chart is wrong" leads to the file to edit.
	TemplateID  int64               `json:"template_id"`
	TemplateUID string              `json:"template_uid,omitempty"`
	Service     string              `json:"service"`
	Dashboard   dashboard.Dashboard `json:"dashboard"`
}

// serviceDashboards handles GET /api/v1/dashboards/service/{name}.
//
// It answers a *list*, not one dashboard, because nothing says a deployment has
// only one template: an app repo mounting its own provisioning directory beside
// the stock one is the case the M3 spec asks for. Zero templates and five are
// then the same shape, and a client that wants "the" service dashboard takes
// the first.
func (d *Dashboards) serviceDashboards(w http.ResponseWriter, r *http.Request) {
	if !d.ready(w) {
		return
	}
	name := r.PathValue("name")
	// Bounded before it goes in an error message: a tag is at most 200 bytes
	// including its key, so a longer name is not a service this store could
	// hold, and a 404 quoting a megabyte of URL is its own problem.
	if len(name) > wire.MaxTagLen {
		writeError(w, http.StatusBadRequest, fmt.Errorf(
			"a service name is at most %d bytes, the limit on a whole tag; this one is %d", wire.MaxTagLen, len(name)))
		return
	}
	disc, err := d.discover(r.Context())
	if err != nil {
		d.fail(w, r, "discovering the services templates cover", err)
		return
	}
	// 404 rather than an empty dashboard: a typo in a URL somebody pasted into
	// a runbook would otherwise render a grid of empty charts, which reads as
	// "the service is down" rather than "the service is misspelt".
	if !slices.Contains(disc.services, name) {
		msg := fmt.Errorf(
			"no service named %q reports a metric any template dashboard queries", name)
		if disc.truncated {
			msg = fmt.Errorf("%w — and this server has more than %d services, so the list it was looked up in is truncated",
				msg, maxServices)
		}
		writeError(w, http.StatusNotFound, msg)
		return
	}
	out := make([]serviceDashboard, 0, len(disc.templates))
	for _, t := range disc.templates {
		inst, err := t.def.Instantiate(name)
		if err != nil {
			// Ours, not the caller's: discover returns only templates that
			// validated, and only names it filtered, which between them exclude
			// everything Instantiate refuses. See its comment — including the
			// blank service name that used to get through.
			//
			// Not through d.fail, which reads the ErrInvalid these wrap as "the
			// caller sent a bad definition" and answers 400 with the message.
			// The caller sent a URL; a 400 would tell them to fix something that
			// is not theirs, and the message names a database row.
			d.Logger.Error("a template validated and then would not instantiate",
				"id", t.row.ID, "uid", t.row.UID, "service", name, "err", err)
			writeError(w, http.StatusInternalServerError,
				errors.New("the request could not be completed"))
			return
		}
		out = append(out, serviceDashboard{
			TemplateID:  t.row.ID,
			TemplateUID: t.row.UID,
			Service:     name,
			Dashboard:   inst,
		})
	}
	writeJSON(w, http.StatusOK, struct {
		Status     string             `json:"status"`
		Service    string             `json:"service"`
		Count      int                `json:"count"`
		Dashboards []serviceDashboard `json:"dashboards"`
		Unreadable []int64            `json:"unreadable"`
	}{"ok", name, len(out), out, disc.unreadable})
}
