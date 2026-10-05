package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// readBody decodes a JSON request body, strictly.
//
// http.MaxBytesReader rather than io.LimitReader: the latter truncates in
// silence, so a body one byte over the limit is reported as malformed JSON —
// "unexpected EOF" — and the caller goes looking for a missing brace that was
// there all along. MaxBytesReader names the real problem.
//
// DisallowUnknownFields catches a misspelled field instead of ignoring it, and
// the trailing-content check catches two JSON objects sent as one body, which
// would otherwise silently use the first and discard the rest.
func readBody(w http.ResponseWriter, r *http.Request, into any) error {
	return readBodyLimit(w, r, maxBodyBytes, into)
}

// readBodyLimit is readBody with the limit named, for the batch endpoint, whose
// body is legitimately fifty times larger than one query's.
func readBodyLimit(w http.ResponseWriter, r *http.Request, limit int64, into any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return fmt.Errorf("body is larger than %d bytes", limit)
		}
		return fmt.Errorf("body: %w", err)
	}
	if dec.More() {
		return errors.New("body: trailing content after the JSON object")
	}
	return nil
}

// queryRequest is what both verbs and both spellings of a query come down to.
//
// A caller sends either `q`, the query language, or the structured parameters
// M1 defined — never both, because a request that says two things needs
// somebody to decide which one it meant, and that somebody should be the
// caller rather than this file.
type queryRequest struct {
	// querySpec is embedded rather than named so that the JSON shape is
	// unchanged — an anonymous struct's fields are flattened by encoding/json —
	// while a batch entry, which has a query and no window of its own, can be
	// the same type and reuse the same translation instead of a second copy of
	// it that drifts.
	querySpec

	// From and To are pointers so that the JSON path can tell an explicit 0
	// — the epoch, which the window rules allow — from a field that was not
	// sent. The query string distinguishes them already ("" versus "0"), and
	// two spellings of one request should not disagree about what `0` means.
	// [queryRequest.window] resolves both to plain seconds.
	From *int64 `json:"from"`
	To   *int64 `json:"to"`
	// Interval needs no pointer: zero already means "you choose", both here
	// and in eval.Request.
	Interval int64 `json:"interval"`

	// Vars binds the query's `$name` template variables, each to zero or more
	// `key:value` tags. Zero values is a dashboard's "all" selection.
	Vars map[string][]string `json:"vars"`
}

// querySpec is the two spellings of one query: the query language, or the M1
// structured parameters. It is what a batch entry is, and what a single request
// carries alongside its window.
type querySpec struct {
	Q string `json:"q"`

	// The M1 structured form. [structuredExpression] turns it into `q`.
	Metric string `json:"metric"`
	Filter string `json:"filter"`
	By     string `json:"by"`
	Agg    string `json:"agg"`
}

// structured reports whether the request used the M1 parameters.
func (q *querySpec) structured() bool {
	return q.Metric != "" || q.Filter != "" || q.By != "" || q.Agg != ""
}

// readQueryRequest reads a query from either verb: GET from the query string,
// POST from a JSON body.
func (m *Metrics) readQueryRequest(w http.ResponseWriter, r *http.Request) (queryRequest, error) {
	if r.Method == http.MethodPost {
		var q queryRequest
		if err := readBody(w, r, &q); err != nil {
			return q, err
		}
		q.Vars = foldVarNames(q.Vars)
		return q, nil
	}

	v := r.URL.Query()
	var errs []error
	q := queryRequest{querySpec: querySpec{
		Q:      v.Get("q"),
		Metric: v.Get("metric"),
		Filter: v.Get("filter"),
		By:     v.Get("by"),
		Agg:    v.Get("agg"),
	}}
	// Parsed only when present, so that a nil pointer means "not sent" and
	// [queryRequest.window] is the only thing that decides a default. The
	// zero passed to intParam is unreachable for the same reason.
	if raw := v.Get("to"); raw != "" {
		q.To = ptr(intParam(raw, 0, "to (unix seconds)", &errs))
	}
	if raw := v.Get("from"); raw != "" {
		q.From = ptr(intParam(raw, 0, "from (unix seconds)", &errs))
	}
	q.Interval = intParam(v.Get("interval"), 0, "interval", &errs)
	// `var.env=env:prod&var.env=env:dev` binds $env to two values. Repeating
	// the parameter is how a multi-select arrives, and it is the shape
	// url.Values already has.
	//
	// An empty value is dropped rather than bound. `?var.env=` is how a
	// cleared template variable arrives from a form, and a query string has no
	// way to say "zero values" otherwise; binding "" instead would refuse the
	// request as "$env is bound to \"\", which is not a key:value tag" — the
	// one thing a cleared "all" selection must not do.
	for name, values := range v {
		after, ok := strings.CutPrefix(name, "var.")
		if !ok {
			continue
		}
		if q.Vars == nil {
			q.Vars = map[string][]string{}
		}
		// Lower-cased to match the lexer, which lower-cases a `$name` as it
		// reads one. `var.Env=` has to bind the `$env` a query writes.
		q.Vars[strings.ToLower(after)] = slices.DeleteFunc(values, func(s string) bool {
			return strings.TrimSpace(s) == ""
		})
	}
	return q, errors.Join(errs...)
}

func ptr[T any](v T) *T { return &v }

// foldVarNames lower-cases the keys of a JSON `vars` object, so that both verbs
// agree with the lexer about which variable a name refers to.
func foldVarNames(vars map[string][]string) map[string][]string {
	if vars == nil {
		return nil
	}
	out := make(map[string][]string, len(vars))
	for name, values := range vars {
		out[strings.ToLower(name)] = values
	}
	return out
}

// window resolves From and To, defaulting to the last hour. It is the one
// place the defaults live, so both verbs get the same ones.
func (q *queryRequest) window(now int64) (from, to int64) {
	to = now
	if q.To != nil {
		to = *q.To
	}
	from = to - defaultRange
	if q.From != nil {
		from = *q.From
	}
	return from, to
}

// query handles GET and POST /api/v1/query.
func (m *Metrics) query(w http.ResponseWriter, r *http.Request) {
	req, err := m.readQueryRequest(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	from, to := req.window(m.Clock.Now().Unix())
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
		From:     from,
		To:       to,
		Interval: req.Interval,
		Vars:     req.Vars,
	})
	switch {
	case errors.Is(err, context.Canceled) && r.Context().Err() != nil:
		// The caller hung up: a new keystroke in the query box, a range
		// change, a navigated-away tab. Nothing failed, nobody is reading the
		// response, and logging it at ERROR would bury the failures that
		// matter under the ones that are just somebody typing.
		m.Logger.Debug("query abandoned by the client", "query", text)
		writeError(w, statusClientClosedRequest, errors.New("the client closed the request"))
		return
	case errors.Is(err, eval.ErrNoSketchStore), errors.Is(err, eval.ErrSketchesDisagree):
		// Both are the server's state rather than the query's: no sketch store
		// configured, or two writers disagreeing about relative accuracy. The
		// message names a metric and nothing internal, so it is kept — it is
		// the only thing that tells an operator where to look.
		m.Logger.Error("query cannot be answered by this server", "query", text, "err", err)
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
// statusClientClosedRequest is nginx's 499. Go has no constant for it because
// it is not in any RFC, but it is what every log aggregator already knows to
// treat as "the client left" rather than as a server error, and no 4xx that is
// in an RFC says that.
const statusClientClosedRequest = 499

// statusFor deliberately does not recognise context.Canceled. Whether a
// cancellation means "the caller hung up" depends on whose context was
// cancelled, which is something only the handler can see; a store that
// cancelled its own work while the request is still live has failed, and
// reporting that as the client's departure would hide it.
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
	if err := readBody(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
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
func (q *querySpec) expression() (string, []string, error) {
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
// aggs is the allowlist for the M1 structured `agg=` parameter. It carries
// `dist` because /api/v1/query/sketch accepts the same structured spelling as
// every other query endpoint — leaving it out made `?metric=lat&agg=dist`
// unreachable there while the docs promised it worked.
var aggs = []string{"avg", "sum", "min", "max", "count", "dist", "p50", "p75", "p90", "p95", "p99"}

// structuredExpression writes the M1 parameters as a query.
//
// Every piece is validated before it is interpolated, for the reason above:
// this function builds a program out of strings the caller supplied. The
// validation is deliberately the same rules the parser would apply, so the
// only queries this can produce are ones the caller could have written by
// hand.
func structuredExpression(q *querySpec) (string, []string, error) {
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
		key = strings.ToLower(strings.TrimSpace(key)) // as the lexer does
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
		// Lower-cased first, because the lexer lower-cases a key as it reads
		// one: `q=…{Env:dev}` evaluates as `env:dev`, so `filter=Env:dev` must
		// too. Validating the original would make this path stricter than the
		// parser it claims to mirror, and reject a request the query language
		// accepts.
		key = strings.ToLower(key)
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
