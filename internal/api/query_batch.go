package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/tuvo1106/ozymandias/internal/query/metricql"
	"github.com/tuvo1106/ozymandias/internal/query/metricql/eval"
)

// maxBatchBodyBytes bounds a POSTed batch.
//
// Larger than [maxBodyBytes] because a batch legitimately carries fifty queries
// where a single request carries one, and small enough that it is still a
// dashboard rather than an upload: fifty of metricql's 8 KiB limit is 400 KiB,
// and this leaves room for the window and the variables around them.
const maxBatchBodyBytes = 512 << 10

// batchRequest is a dashboard's worth of queries over one window.
//
// The window, the interval and the variables belong to the batch rather than to
// each query, which is not a simplification — it is the thing that makes the
// endpoint worth having. Queries planned onto different grids share no work (see
// [eval.Batch]), and a dashboard has one time picker and one template-variable
// bar, so one window per batch is also what a dashboard means.
type batchRequest struct {
	Queries []querySpec `json:"queries"`

	// From and To are pointers for the same reason they are on a single query:
	// an explicit 0 is the epoch, which the window rules allow, and an absent
	// field is "you choose". [queryRequest.window] resolves both, and is shared
	// so that one endpoint cannot drift from the other about what the default
	// range is.
	From     *int64              `json:"from"`
	To       *int64              `json:"to"`
	Interval int64               `json:"interval"`
	Vars     map[string][]string `json:"vars"`
}

// batchResult is one query's answer within a batch.
//
// A status and a code per entry, inside a 200, because a dashboard is a set of
// independent questions: one widget asking for a percentile of a gauge must not
// blank the other eleven, and a client needs to tell that widget's own error
// apart from a server that is failing. The code is the status this query would
// have been answered with on /api/v1/query — 400 for the query's fault, 503 for
// out of time, 500 for ours — so a client has one rule for both endpoints
// instead of a second vocabulary for this one.
type batchResult struct {
	// Index is the query's position in the request. Present although the array
	// is already in that order, because a result is passed around on its own
	// once a client has fanned these out to widgets, and "which query was this"
	// should survive that.
	Index int `json:"index"`
	// Status is "ok" or "error", so a client can branch without consulting the
	// code table.
	Status string `json:"status"`
	// Query is the canonical text of what was evaluated, present on a failure
	// too: for a structured entry it is the translation, and for a broken one it
	// is the only way to see which query the error belongs to when the index has
	// been lost.
	Query string `json:"query"`

	// Interval is the bucket width this query was evaluated on.
	//
	// Per result rather than per batch, which is not what this endpoint first
	// did. The batch supplies one interval and almost always every query shares
	// it — but `.rollup(method, seconds)` sets the grid of the query it is
	// written on (ADR-0016), so `sum:x{*}` and `sum:x{*}.rollup(sum, 600)` in
	// one batch are evaluated on 10s and 600s buckets. Reporting one interval
	// for the batch meant the response stated a bucket width that some of its
	// own results did not have, and there was nowhere to find the real one. A
	// query that does this shares no work with the others, which is a reason to
	// avoid writing it, not a reason for the answer to be wrong.
	//
	// Zero on a failure: there is no grid, and inventing one would be worse
	// than saying nothing.
	Interval int64 `json:"interval"`

	// Series and Warnings are always present, empty rather than absent or
	// null, on a failure as much as on a success: "this widget has nothing to
	// draw" and "this field does not exist in your version" are different
	// answers and a client should not have to guess which it got. They were
	// `omitempty` in the first draft, which quietly made every empty one of
	// them absent and contradicted the comment saying otherwise — the tests
	// below are what noticed.
	Series   []eval.Series `json:"series"`
	Warnings []string      `json:"warnings"`

	// Code and Error are the other way round: they exist only when there is a
	// failure to describe, so their absence is the signal. Status says which
	// case a client is in without it having to infer either.
	Code  int    `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// failed builds a result for a query that never reached the evaluator.
func failed(i int, query string, code int, err error) batchResult {
	return batchResult{
		Index: i, Status: "error", Query: query, Code: code, Error: err.Error(),
		Series: []eval.Series{}, Warnings: []string{},
	}
}

// queryBatch handles POST /api/v1/query/batch: a whole dashboard in one request.
//
// # Why a 200 can contain failures
//
// The request either is or is not a batch of queries, and that is what the HTTP
// status answers. Whether each *query* worked is a property of that query, and
// there are two of them in flight at once — the alternative, failing the request
// on the first bad query, means one typo in one widget returns nothing for the
// eleven that were fine, which is the behaviour this endpoint exists to avoid.
//
// Request-level problems are still request-level: a body that is not JSON, no
// queries at all, more than [eval.MaxQueriesPerBatch], a window that cannot be
// planned. Those are 400s, because there is no per-query answer to put an error
// in.
func (m *Metrics) queryBatch(w http.ResponseWriter, r *http.Request) {
	var body batchRequest
	if err := readBodyLimit(w, r, maxBatchBodyBytes, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(body.Queries) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("queries is required and must not be empty"))
		return
	}
	if len(body.Queries) > eval.MaxQueriesPerBatch {
		writeError(w, http.StatusBadRequest, fmt.Errorf(
			"a batch carries %d queries; the limit is %d", len(body.Queries), eval.MaxQueriesPerBatch))
		return
	}

	window := queryRequest{From: body.From, To: body.To}
	from, to := window.window(m.Clock.Now().Unix())
	// Once, here, rather than once per query. A window belongs to the request,
	// so a bad one is the request's failure — and the alternative is a 200
	// carrying fifty identical copies of the same sentence.
	if err := eval.ValidateWindow(from, to, body.Interval); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	vars := foldVarNames(body.Vars)

	// Parsed up front, all of them, before anything is evaluated. A query that
	// does not parse is that query's failure and nothing else's, so it becomes a
	// result rather than stopping the batch — but it must not take a slot in the
	// evaluator's batch either, or the indexes of what comes back stop matching
	// the indexes of what was sent.
	results := make([]batchResult, len(body.Queries))
	reqs := make([]eval.Request, 0, len(body.Queries))
	at := make([]int, 0, len(body.Queries))
	for i, spec := range body.Queries {
		text, warnings, err := spec.expression()
		if err != nil {
			results[i] = failed(i, spec.Q, http.StatusBadRequest, err)
			continue
		}
		expr, err := metricql.Parse(text)
		if err != nil {
			results[i] = failed(i, text, http.StatusBadRequest, err)
			continue
		}
		if warnings == nil {
			warnings = []string{}
		}
		results[i] = batchResult{
			Index: i, Query: expr.String(), Warnings: warnings, Series: []eval.Series{},
		}
		reqs = append(reqs, eval.Request{
			Expr: expr, From: from, To: to, Interval: body.Interval, Vars: vars,
		})
		at = append(at, i)
	}

	// Every query was malformed, so there is nothing to evaluate. Still a 200:
	// the answers are per query, and a client that sent twelve queries and got
	// twelve errors is better served by twelve messages than by one.
	if len(reqs) > 0 {
		out, err := m.eval.Batch(r.Context(), reqs)
		if err != nil {
			// A batch-level refusal. The count is checked above, so this is the
			// evaluator disagreeing with this handler about a limit — worth
			// saying out loud rather than translating into per-query noise.
			writeError(w, statusFor(err), err)
			return
		}
		if abandoned(r.Context().Err(), out) {
			// Debug, not Error: logging somebody closing a tab at ERROR buries
			// the failures that matter.
			m.Logger.Debug("batch abandoned by the client", "queries", len(reqs))
			writeError(w, statusClientClosedRequest, errors.New("the client closed the request"))
			return
		}
		for j, o := range out {
			results[at[j]] = m.batchOutcome(results[at[j]], o)
		}
	}

	// The window is the batch's and is reported once. The interval is not: it
	// belongs to each result, because a `.rollup()` can put one query on its own
	// grid — see [batchResult.Interval].
	writeJSON(w, http.StatusOK, struct {
		Status  string        `json:"status"`
		From    int64         `json:"from"`
		To      int64         `json:"to"`
		Results []batchResult `json:"results"`
	}{"ok", from, to, results})
}

// abandoned reports whether the caller hung up on this batch: a time-range
// change, a closed tab.
//
// Both halves matter, as they do on /api/v1/query. A request context that is
// done is not enough — a client that disconnects in the window between the last
// query answering and this check would have a fully computed batch thrown away.
// A cancelled query is not enough either — a store that cancelled its own work
// while the request is still live has *failed*, and reporting that as the
// client's departure would hide it.
//
// A function rather than two clauses inline, because the interesting case is the
// one that cannot be reached through HTTP without a race, and a condition nobody
// can test is a condition nobody can check I got right.
func abandoned(requestErr error, out []eval.Outcome) bool {
	if requestErr == nil {
		return false
	}
	return slices.ContainsFunc(out, func(o eval.Outcome) bool {
		return errors.Is(o.Err, context.Canceled)
	})
}

// batchOutcome fills in one query's answer, keeping the index and canonical text
// the caller's loop already put there.
//
// The error rules are [statusFor]'s, and the reason they are repeated here rather
// than shared with the single-query handler is that there is nowhere to write a
// 500's real message except the log: this endpoint answers 200, so an internal
// failure cannot be reported by the status line, and a store error naming a file
// or a query plan is still not the caller's business.
func (m *Metrics) batchOutcome(into batchResult, o eval.Outcome) batchResult {
	if o.Err == nil {
		into.Status = "ok"
		into.Interval = o.Result.Interval
		into.Series = o.Result.Series
		if into.Series == nil {
			into.Series = []eval.Series{}
		}
		into.Warnings = append(into.Warnings, o.Result.Warnings...)
		if into.Warnings == nil {
			into.Warnings = []string{}
		}
		return into
	}
	into.Status = "error"
	into.Code = statusFor(o.Err)
	// A failed query has nothing to draw, and says so with an empty list rather
	// than by leaving the field out.
	into.Series = []eval.Series{}
	switch {
	case errors.Is(o.Err, eval.ErrNoSketchStore), errors.Is(o.Err, eval.ErrSketchesDisagree):
		// The server's state rather than the query's, and the message names a
		// metric and nothing internal — it is the only thing that tells an
		// operator where to look.
		m.Logger.Error("query in a batch cannot be answered by this server",
			"query", into.Query, "err", o.Err)
		into.Code, into.Error = http.StatusServiceUnavailable, o.Err.Error()
	case into.Code == http.StatusInternalServerError:
		m.Logger.Error("query in a batch failed", "query", into.Query, "err", o.Err)
		into.Error = "the query could not be answered"
	case errors.Is(o.Err, context.DeadlineExceeded):
		// The batch's shared deadline, not this query's alone: it ran out of
		// time because the ones before it used it. Say so, because "try a
		// shorter window" is the wrong advice and "ask for fewer things at
		// once" is the right one.
		into.Error = "the batch ran out of time before this query was evaluated"
	default:
		into.Error = o.Err.Error()
	}
	return into
}
