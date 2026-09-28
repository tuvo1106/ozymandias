package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/query/metricql/eval"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// MetricTypes tells the query layer each metric's type, which decides how its
// samples reduce over time. Implemented by internal/meta.
type MetricTypes interface {
	Metric(name string) (meta.Metric, bool)
}

// Metrics serves the metric query and metadata endpoints.
type Metrics struct {
	Store tsdb.MetricStore
	Types MetricTypes
	// Sketches answers the percentile aggregators. Nil means a percentile
	// query is refused with a reason rather than answered from nothing.
	Sketches eval.SketchReader
	Clock    clock.Clock // default clock.Real()
	// Logger records the store failures the response deliberately does not
	// describe. Nil discards them.
	Logger *slog.Logger

	// eval is built by Register from the fields above. It holds no per-query
	// state, so one serves every request.
	eval *eval.Evaluator
}

// Limits for the metadata endpoints' ?limit=.
const (
	defaultListLimit = 100
	maxListLimit     = 1000
	// defaultRange is the query window when ?from is absent: the last hour.
	defaultRange = 3600
)

// Register mounts the endpoints on mux.
func (m *Metrics) Register(mux *http.ServeMux) {
	if m.Clock == nil {
		m.Clock = clock.Real()
	}
	if m.Logger == nil {
		m.Logger = slog.New(slog.DiscardHandler)
	}
	m.eval = &eval.Evaluator{Store: m.Store, Sketches: m.Sketches, Types: m.Types}
	// POST takes the same query as GET, in a JSON body: a generated dashboard
	// query outgrows a URL long before it outgrows metricql's 8 KiB limit.
	mux.HandleFunc("GET /api/v1/query", m.query)
	mux.HandleFunc("POST /api/v1/query", m.query)
	mux.HandleFunc("GET /api/v1/query/sketch", m.querySketch)
	mux.HandleFunc("POST /api/v1/query/sketch", m.querySketch)
	mux.HandleFunc("POST /api/v1/query/batch", m.queryBatch)
	mux.HandleFunc("POST /api/v1/query/validate", m.validate)
	mux.HandleFunc("GET /api/v1/metrics", m.metrics)
	mux.HandleFunc("GET /api/v1/tags", m.tagKeys)
	mux.HandleFunc("GET /api/v1/tags/values", m.tagValues)
}

// metrics handles GET /api/v1/metrics?prefix=&limit=.
func (m *Metrics) metrics(w http.ResponseWriter, r *http.Request) {
	limit, ok := listLimit(w, r)
	if !ok {
		return
	}
	names, err := m.Store.MetricNames(r.Context(), r.URL.Query().Get("prefix"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"metrics": names})
}

// tagKeys handles GET /api/v1/tags?metric=.
func (m *Metrics) tagKeys(w http.ResponseWriter, r *http.Request) {
	metric := r.URL.Query().Get("metric")
	if metric == "" {
		writeError(w, http.StatusBadRequest, errors.New("metric is required"))
		return
	}
	keys, err := m.Store.TagKeys(r.Context(), metric)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"keys": keys})
}

// tagValues handles GET /api/v1/tags/values?metric=&key=&limit=.
func (m *Metrics) tagValues(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("metric") == "" || q.Get("key") == "" {
		writeError(w, http.StatusBadRequest, errors.New("metric and key are required"))
		return
	}
	limit, ok := listLimit(w, r)
	if !ok {
		return
	}
	vals, err := m.Store.TagValues(r.Context(), q.Get("metric"), q.Get("key"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"values": vals})
}

func listLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	var errs []error
	limit := intParam(r.URL.Query().Get("limit"), defaultListLimit, "limit", &errs)
	if len(errs) > 0 || limit < 1 || limit > maxListLimit {
		writeError(w, http.StatusBadRequest, errors.New("limit must be an integer from 1 to 1000"))
		return 0, false
	}
	return int(limit), true
}

func intParam(s string, def int64, name string, errs *[]error) int64 {
	if s == "" {
		return def
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		*errs = append(*errs, errors.New(name+" must be an integer (unix seconds for from/to)"))
		return def
	}
	return v
}

// writeJSON sends v.
//
// It marshals into a buffer *before* writing the status line, because the
// status cannot be taken back afterwards. Encoding straight to the
// ResponseWriter turns a marshalling bug into "HTTP 200, zero bytes" — a
// success the client cannot parse and the server never mentions. That is not
// hypothetical: it is how a dashboard whose stored JSON ended in a newline
// presented itself, and it cost an afternoon.
func writeJSON(w http.ResponseWriter, code int, v any) { _ = writeJSONErr(w, code, v) }

// writeJSONErr is writeJSON for a response assembled from stored bytes, where a
// marshal failure means a row somebody has to go and find. It answers exactly as
// writeJSON does and hands the error back so the caller can log it with the id.
//
// Without this, an error message naming the dashboard is written and then
// dropped on the floor — which is what the json.Valid check in dashboards.go was
// justified by, and what it was not actually doing.
func writeJSONErr(w http.ResponseWriter, code int, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		// Nothing useful can be said about our own encoder to a caller, but a
		// 500 is at least honest, and callers can tell it from an empty 200.
		//
		// Written by hand rather than with http.Error, which would set
		// text/plain around a JSON body and skip Cache-Control — on the one
		// status a client is most likely to parse defensively. Every response
		// from this API is JSON, including the ones apologising.
		body, code = []byte(`{"status":"error","error":"the response could not be encoded"}`),
			http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(append(body, '\n'))
	return err
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, wire.ErrorResponse{Status: "error", Error: err.Error()})
}
