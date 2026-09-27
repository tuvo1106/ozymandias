package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/tuvo1106/ozymandias/internal/query/metricql"
	"github.com/tuvo1106/ozymandias/internal/query/metricql/eval"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// maxBodyBytes bounds a POSTed query. POST exists because a generated
// dashboard query outgrows a URL, not because it should be able to outgrow
// memory; metricql's own limit is 8 KiB and this leaves room for the window
// and the variables around it.
const maxBodyBytes = 64 << 10

// queryRequest is what both verbs and both spellings of a query come down to.
//
// A caller sends either `q`, the query language, or the structured parameters
// M1 defined — never both, because a request that says two things needs
// somebody to decide which one it meant, and that somebody should be the
// caller rather than this file.
type queryRequest struct {
	Q string `json:"q"`

	// The M1 structured form. [structuredExpression] turns it into `q`.
	Metric string `json:"metric"`
	Filter string `json:"filter"`
	By     string `json:"by"`
	Agg    string `json:"agg"`

	From     int64 `json:"from"`
	To       int64 `json:"to"`
	Interval int64 `json:"interval"`

	// Vars binds the query's `$name` template variables, each to zero or more
	// `key:value` tags. Zero values is a dashboard's "all" selection.
	Vars map[string][]string `json:"vars"`
}

// structured reports whether the request used the M1 parameters.
func (q *queryRequest) structured() bool {
	return q.Metric != "" || q.Filter != "" || q.By != "" || q.Agg != ""
}

// readQueryRequest reads a query from either verb: GET from the query string,
// POST from a JSON body.
func (m *Metrics) readQueryRequest(r *http.Request) (queryRequest, error) {
	now := m.Clock.Now().Unix()
	if r.Method == http.MethodPost {
		var q queryRequest
		dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&q); err != nil {
			return q, fmt.Errorf("body: %w", err)
		}
		if q.To == 0 {
			q.To = now
		}
		if q.From == 0 {
			q.From = q.To - defaultRange
		}
		return q, nil
	}

	v := r.URL.Query()
	var errs []error
	q := queryRequest{
		Q:      v.Get("q"),
		Metric: v.Get("metric"),
		Filter: v.Get("filter"),
		By:     v.Get("by"),
		Agg:    v.Get("agg"),
	}
	q.To = intParam(v.Get("to"), now, "to", &errs)
	q.From = intParam(v.Get("from"), q.To-defaultRange, "from", &errs)
	q.Interval = intParam(v.Get("interval"), 0, "interval", &errs)
	// `var.env=env:prod&var.env=env:dev` binds $env to two values. Repeating
	// the parameter is how a multi-select arrives, and it is the shape
	// url.Values already has.
	for name, values := range v {
		if after, ok := strings.CutPrefix(name, "var."); ok {
			if q.Vars == nil {
				q.Vars = map[string][]string{}
			}
			q.Vars[after] = values
		}
	}
	return q, errors.Join(errs...)
}

// query handles GET and POST /api/v1/query.
func (m *Metrics) query(w http.ResponseWriter, r *http.Request) {
	req, err := m.readQueryRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	text, warnings, err := req.expression()
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	expr, err := metricql.Parse(text)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	res, err := m.eval.Eval(r.Context(), eval.Request{
		Expr:     expr,
		From:     req.From,
		To:       req.To,
		Interval: req.Interval,
		Vars:     req.Vars,
	})
	switch {
	case errors.Is(err, eval.ErrNoSketchStore):
		writeError(w, http.StatusServiceUnavailable, err)
		return
	case err != nil:
		status := statusFor(err)
		if status == http.StatusInternalServerError {
			// A store's error can name a file, a table or a query plan.
			// None of that is the caller's business.
			m.Logger.Error("query failed", "query", text, "err", err)
			writeError(w, status, errors.New("the query could not be answered"))
			return
		}
		writeError(w, status, err)
		return
	}
	// An absent list and an empty one mean different things to a caller
	// reading JSON, and this endpoint always has an answer for "what should
	// I know?" — even when it is "nothing".
	if res.Warnings = append(warnings, res.Warnings...); res.Warnings == nil {
		res.Warnings = []string{}
	}
	writeJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
		// Query is the canonical text of what was evaluated. For a structured
		// request it is the query language equivalent, which is how a caller
		// migrating from the M1 parameters finds out what to write.
		Query string `json:"query"`
		eval.Result
	}{"ok", expr.String(), res})
}

// statusFor decides whose fault a failed evaluation was.
//
// The evaluator marks the refusals that are about the query itself — a
// percentile of a gauge, an unbound variable, more series than the limit
// allows — by wrapping [eval.ErrBadQuery]. Those are 400s, and their message
// is written to be shown to whoever typed the query. A deadline is a 503: the
// server ran out of time rather than the caller asking for the impossible.
// Everything left is a store failure, which is ours, so it is a 500 and the
// body says nothing useful on purpose.
func statusFor(err error) int {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable
	case errors.Is(err, eval.ErrBadQuery), errors.As(err, new(*metricql.Error)):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// validate handles POST /api/v1/query/validate: does this parse, and if not,
// where. It answers 200 either way — the request is well formed whatever the
// query in it turns out to be — because it exists for an editor that calls it
// on every keystroke and wants a column to underline, not an exception.
func (m *Metrics) validate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Q string `json:"q"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("body: %w", err))
		return
	}
	type position struct {
		Msg string `json:"msg"`
		Col int    `json:"col"`
	}
	expr, err := metricql.Parse(body.Q)
	if err != nil {
		var perr *metricql.Error
		if !errors.As(err, &perr) {
			perr = &metricql.Error{Msg: err.Error(), Col: 1}
		}
		writeJSON(w, http.StatusOK, struct {
			OK    bool     `json:"ok"`
			Error position `json:"error"`
		}{false, position{perr.Msg, perr.Col}})
		return
	}
	writeJSON(w, http.StatusOK, struct {
		OK bool `json:"ok"`
		// Query is the canonical spelling, which is what the editor's
		// "format" does and what a dashboard stores.
		Query string `json:"query"`
	}{true, expr.String()})
}

// expression returns the query language text to evaluate, plus any warnings
// raised in getting there.
func (q *queryRequest) expression() (string, []string, error) {
	switch {
	case q.Q != "" && q.structured():
		return "", nil, errors.New("send either q= or the metric/filter/by/agg parameters, not both")
	case q.Q != "":
		return q.Q, nil, nil
	case q.Metric == "":
		return "", nil, errors.New("q (a query) or metric (the M1 parameters) is required")
	}
	return structuredExpression(q)
}

// aggs is the set the structured `agg` parameter accepts. It is a list rather
// than a call into the parser because the value is interpolated into text
// below: anything not on this list must not reach the query string, or an
// `agg` of `x{*}} + sum:secrets{*` would be a query of the caller's choosing
// rather than the one the parameters describe.
var aggs = []string{"avg", "sum", "min", "max", "count", "p50", "p75", "p90", "p95", "p99"}

// structuredExpression writes the M1 parameters as a query.
//
// Every piece is validated before it is interpolated, for the reason above:
// this function builds a program out of strings the caller supplied. The
// validation is deliberately the same rules the parser would apply, so the
// only queries this can produce are ones the caller could have written by
// hand.
func structuredExpression(q *queryRequest) (string, []string, error) {
	agg := q.Agg
	if agg == "" {
		agg = "avg"
	}
	if !slices.Contains(aggs, agg) {
		return "", nil, fmt.Errorf("agg %q: want one of %s", q.Agg, strings.Join(aggs, ", "))
	}
	if !wire.ValidMetricName(q.Metric) {
		return "", nil, fmt.Errorf("metric %q is not a valid metric name", q.Metric)
	}

	var b strings.Builder
	b.WriteString(agg)
	b.WriteByte(':')
	b.WriteString(q.Metric)
	b.WriteByte('{')
	terms, warnings, err := filterTerms(q.Filter)
	if err != nil {
		return "", nil, err
	}
	if len(terms) == 0 {
		b.WriteByte('*')
	}
	b.WriteString(strings.Join(terms, ","))
	b.WriteByte('}')

	var keys []string
	for key := range strings.SplitSeq(q.By, ",") {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if !wire.ValidTag(key) {
			return "", nil, fmt.Errorf("group-by key %q is not a tag key", key)
		}
		keys = append(keys, key)
	}
	if len(keys) > 0 {
		b.WriteString(" by {")
		b.WriteString(strings.Join(keys, ","))
		b.WriteByte('}')
	}
	return b.String(), warnings, nil
}

// filterTerms translates the M1 filter syntax term by term.
func filterTerms(filter string) ([]string, []string, error) {
	var terms, warnings []string
	for term := range strings.SplitSeq(filter, ",") {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		neg := strings.HasPrefix(term, "!")
		rest := strings.TrimPrefix(term, "!")
		key, value := wire.SplitTag(rest)
		if key == "" {
			return nil, nil, fmt.Errorf("filter term %q has no tag key", term)
		}
		if !wire.ValidTag(key) {
			return nil, nil, fmt.Errorf("filter term %q: %q is not a tag key", term, key)
		}
		if strings.ContainsAny(value, "{}") {
			// A brace would close the filter early and turn the rest of the
			// value into more query. The parser refuses braces in a value, so
			// refusing them here only says so sooner.
			return nil, nil, fmt.Errorf("filter term %q: a tag value cannot contain '{' or '}'", term)
		}
		if value == "" {
			// M1 read a bare `k` as "has the bare tag k". The query language
			// has no spelling for that — `k:` is a parse error on purpose —
			// so the nearest thing is "has the key at all", which is wider.
			// Saying so is better than either failing or quietly changing the
			// question.
			warnings = append(warnings, fmt.Sprintf(
				"the filter term %q asked for the bare tag %q, which the query language cannot express; it was widened to %s:* — use q= to be exact",
				term, key, key))
			value = "*"
		}
		if neg {
			terms = append(terms, "!"+key+":"+value)
			continue
		}
		terms = append(terms, key+":"+value)
	}
	return terms, warnings, nil
}
