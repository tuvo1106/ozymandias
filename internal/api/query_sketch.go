package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/tuvo1106/ozymandias/internal/query/metricql"
	"github.com/tuvo1106/ozymandias/internal/query/metricql/eval"
)

// querySketch handles GET and POST /api/v1/query/sketch: the distribution
// behind a metric, per bucket, rather than a number taken from it.
//
// # Why this is not /api/v1/query
//
// Everything that endpoint returns is a series of floats — addable, divisible,
// comparable to a threshold, drawable as a line. A distribution is none of
// those. It is a shape, and the only thing two of them do is merge. Sharing a
// response type would mean a `series` of nulls with the real answer somewhere
// beside it, so it gets its own endpoint and its own shape, the way the plan
// asks (M3 §2, the heatmap widget).
//
// It takes the same query language, both verbs and the same window rules as
// /api/v1/query, because a person moving between a line chart and a heatmap is
// asking about the same metric and should not have to rewrite the filter.
func (m *Metrics) querySketch(w http.ResponseWriter, r *http.Request) {
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
	res, err := m.eval.Distribution(r.Context(), eval.Request{
		Expr:     expr,
		From:     from,
		To:       to,
		Interval: req.Interval,
		Vars:     req.Vars,
	})
	switch {
	case errors.Is(err, context.Canceled) && r.Context().Err() != nil:
		m.Logger.Debug("distribution abandoned by the client", "query", text)
		writeError(w, statusClientClosedRequest, errors.New("the client closed the request"))
		return
	case errors.Is(err, eval.ErrNoSketchStore), errors.Is(err, eval.ErrSketchesDisagree):
		// The server's state rather than the query's, and the message names a
		// metric and nothing internal. This endpoint hits it more often than
		// /api/v1/query does — every query here needs the sketch store, where
		// there only a percentile does.
		m.Logger.Error("distribution cannot be answered by this server", "query", text, "err", err)
		writeError(w, http.StatusServiceUnavailable, err)
		return
	case err != nil:
		status := statusFor(err)
		if status == http.StatusInternalServerError {
			m.Logger.Error("distribution failed", "query", text, "err", err)
			writeError(w, status, errors.New("the query could not be answered"))
			return
		}
		writeError(w, status, err)
		return
	}
	if res.Warnings = append(warnings, res.Warnings...); res.Warnings == nil {
		res.Warnings = []string{}
	}
	if res.Series == nil {
		// An absent list and an empty one are different answers, and "nothing
		// matched" is a legitimate one.
		res.Series = []eval.DistSeries{}
	}
	writeJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
		// Query is the canonical text of what was evaluated, as on
		// /api/v1/query — which is what an editor's "format" produces and what
		// a dashboard stores.
		Query string `json:"query"`
		eval.Distribution
	}{"ok", expr.String(), res})
}
