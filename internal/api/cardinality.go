package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
)

// The cardinality view (M3 §2, "Metric Summary"): how many series each metric
// has, and which of its tag keys make them. A metric's series count is the
// number that decides what it costs to store and to query, and the tag key
// with the most values is almost always why it is high — an id, a timestamp
// or a URL path where a route template belonged.
//
// Both endpoints read the store's index and never its samples (ADR-0023),
// and like the other metadata endpoints take no time range: a series counts
// until retention drops it.

// metricCardinality is one row of GET /api/v1/metrics/cardinality.
type metricCardinality struct {
	Name string `json:"name"`
	// Type is the kind the metric was first seen with, or null when this
	// ozyd has no record of one — which is not a type, and is said so.
	Type   *string `json:"type"`
	Series int     `json:"series"`
}

// tagCardinality is one key in GET /api/v1/tags/cardinality.
type tagCardinality struct {
	Key string `json:"key"`
	// Series is how many of the metric's series carry the key; Values how
	// many distinct values it takes. A bare tag counts toward Series only.
	Series int `json:"series"`
	Values int `json:"values"`
}

// metricsCardinality handles GET /api/v1/metrics/cardinality?prefix=&limit=:
// metrics by series count, highest first.
//
// Every matching metric is counted, whatever the limit: "the highest" is a
// question about all of them. The limit bounds the response, and `total`
// with `truncated` say what it left out.
func (m *Metrics) metricsCardinality(w http.ResponseWriter, r *http.Request) {
	limit, ok := listLimit(w, r)
	if !ok {
		return
	}
	counts, err := m.Store.SeriesCounts(r.Context(), r.URL.Query().Get("prefix"))
	if err != nil {
		m.countsFailed(w, r, err)
		return
	}
	// Stable over the store's name order, so ties stay alphabetical and the
	// list does not reshuffle between loads.
	sort.SliceStable(counts, func(i, j int) bool { return counts[i].Series > counts[j].Series })
	total := len(counts)
	if total > limit {
		counts = counts[:limit]
	}
	rows := make([]metricCardinality, 0, len(counts))
	for _, c := range counts {
		rows = append(rows, metricCardinality{Name: c.Metric, Type: m.typeOf(c.Metric), Series: c.Series})
	}
	writeJSON(w, http.StatusOK, map[string]any{"metrics": rows, "total": total, "truncated": total > len(rows)})
}

// tagsCardinality handles GET /api/v1/tags/cardinality?metric=: one metric's
// tag keys, highest value count first — the key most likely to be the reason.
//
// With the metric's own series count, because "no keys" is two different
// answers: a metric whose series carry no tags (series > 0), and a metric
// this store does not have (series 0). The page says which. The count comes
// from the same store read as the keys, so a key is never on more series
// than the metric has.
func (m *Metrics) tagsCardinality(w http.ResponseWriter, r *http.Request) {
	metric := r.URL.Query().Get("metric")
	if metric == "" {
		writeError(w, http.StatusBadRequest, errors.New("metric is required"))
		return
	}
	card, err := m.Store.TagCardinality(r.Context(), metric)
	if err != nil {
		m.countsFailed(w, r, err)
		return
	}
	keys := card.Keys
	// Stable over the store's key order, as above.
	sort.SliceStable(keys, func(i, j int) bool { return keys[i].Values > keys[j].Values })
	rows := make([]tagCardinality, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, tagCardinality{Key: k.Key, Series: k.Series, Values: k.Values})
	}
	writeJSON(w, http.StatusOK, map[string]any{"metric": metric, "type": m.typeOf(metric), "series": card.Series, "keys": rows})
}

// typeOf is the metric's recorded kind, or nil.
func (m *Metrics) typeOf(metric string) *string {
	if m.Types == nil {
		return nil
	}
	md, ok := m.Types.Metric(metric)
	if !ok || md.Type == "" {
		return nil
	}
	t := string(md.Type)
	return &t
}

// countsFailed answers a store failure without describing it: a store's error
// can name files and tables, which are not the caller's business.
func (m *Metrics) countsFailed(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		writeError(w, statusClientClosedRequest, errors.New("the client closed the request"))
		return
	}
	m.Logger.Error("series counts failed", "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, errors.New("the series counts could not be read"))
}
