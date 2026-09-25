package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/query/simple"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// MetricTypes tells the query API each metric's type (implemented by
// internal/meta).
type MetricTypes interface {
	Metric(name string) (meta.Metric, bool)
}

// Metrics serves the metric query and metadata endpoints.
type Metrics struct {
	Store tsdb.MetricStore
	Types MetricTypes
	// Sketches answers the percentile aggregators. Nil means a percentile
	// query is refused with a reason rather than answered from nothing.
	Sketches simple.SketchReader
	Clock    clock.Clock // default clock.Real()
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
	mux.HandleFunc("GET /api/v1/query", m.query)
	mux.HandleFunc("GET /api/v1/metrics", m.metrics)
	mux.HandleFunc("GET /api/v1/tags", m.tagKeys)
	mux.HandleFunc("GET /api/v1/tags/values", m.tagValues)
}

// query handles GET /api/v1/query?metric=&filter=&by=&agg=&from=&to=&interval=.
func (m *Metrics) query(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var errs []error
	now := m.Clock.Now().Unix()
	to := intParam(q.Get("to"), now, "to", &errs)
	from := intParam(q.Get("from"), to-defaultRange, "from", &errs)
	interval := intParam(q.Get("interval"), 0, "interval", &errs)
	filters, err := simple.ParseFilter(q.Get("filter"))
	if err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		writeError(w, http.StatusBadRequest, errors.Join(errs...))
		return
	}
	req := simple.Request{
		Metric:   q.Get("metric"),
		Filters:  filters,
		By:       splitList(q.Get("by")),
		Agg:      simple.Agg(q.Get("agg")),
		From:     from,
		To:       to,
		Interval: interval,
		// Empty, not a kind: an unrecorded metric is read as a level for time
		// aggregation, but "unknown" and "known to be a gauge" are different
		// answers to "does this have percentiles?".
		Kind: "",
	}
	if md, ok := m.Types.Metric(req.Metric); ok {
		req.Kind = md.Type
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	res, err := simple.Run(r.Context(), m.Store, m.Sketches, req)
	if errors.Is(err, simple.ErrNoSketchStore) {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
		simple.Result
	}{"ok", res})
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

func splitList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, wire.ErrorResponse{Status: "error", Error: err.Error()})
}
