package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/tagcfg"
)

var (
	errUnavailable = errors.New("tag config unavailable")
	errBadBody     = errors.New(`expected {"metric": ..., "mode": "deny"|"allow", "keys": [...]}`)
	errNeedMetric  = errors.New("metric is required")
)

// TagConfigStore is what the tag-rule endpoints need (internal/tagcfg.Config).
type TagConfigStore interface {
	List(ctx context.Context) ([]meta.TagConfig, error)
	Set(ctx context.Context, metric, mode string, keys []string, now time.Time) error
	Delete(ctx context.Context, metric string) error
}

// TagConfig serves the per-metric tag allow/deny rules, the lever for a metric
// whose series count is exploding because of one tag (see internal/tagcfg for what a
// rule does and costs).
//
//	GET    /api/v1/metrics/tag-config                  every rule
//	PUT    /api/v1/metrics/tag-config                  {"metric","mode":"deny|allow","keys":[...]}
//	DELETE /api/v1/metrics/tag-config?metric=NAME      remove one
//
// A rule takes effect on the next request. It does not touch points already stored.
type TagConfig struct {
	Store  TagConfigStore
	Clock  clock.Clock
	Logger *slog.Logger
}

// Register mounts the endpoints.
func (t *TagConfig) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/metrics/tag-config", t.list)
	mux.HandleFunc("PUT /api/v1/metrics/tag-config", t.put)
	mux.HandleFunc("DELETE /api/v1/metrics/tag-config", t.remove)
}

type tagRuleView struct {
	Metric    string    `json:"metric"`
	Mode      string    `json:"mode"`
	Keys      []string  `json:"keys"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (t *TagConfig) list(w http.ResponseWriter, r *http.Request) {
	rules, err := t.Store.List(r.Context())
	if err != nil {
		t.Logger.Error("tag config: listing", "err", err)
		writeError(w, http.StatusInternalServerError, errUnavailable)
		return
	}
	out := make([]tagRuleView, 0, len(rules))
	for _, c := range rules {
		out = append(out, tagRuleView{Metric: c.Metric, Mode: c.Mode, Keys: c.Keys, UpdatedAt: c.UpdatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "rules": out})
}

func (t *TagConfig) put(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Metric string   `json:"metric"`
		Mode   string   `json:"mode"`
		Keys   []string `json:"keys"`
	}
	// Strict: a typo'd field ("key" for "keys") must be an error naming the shape, not a rule
	// with no keys, and a body with something after it is not the body that was meant.
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || dec.More() {
		writeError(w, http.StatusBadRequest, errBadBody)
		return
	}
	if err := tagcfg.Validate(body.Metric, body.Mode, body.Keys); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := t.Store.Set(r.Context(), body.Metric, body.Mode, body.Keys, t.Clock.Now()); err != nil {
		if errors.Is(err, tagcfg.ErrTooManyRules) {
			writeError(w, http.StatusConflict, err)
			return
		}
		t.Logger.Error("tag config: saving", "err", err)
		writeError(w, http.StatusInternalServerError, errUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "metric": body.Metric})
}

func (t *TagConfig) remove(w http.ResponseWriter, r *http.Request) {
	metric := r.URL.Query().Get("metric")
	if metric == "" {
		writeError(w, http.StatusBadRequest, errNeedMetric)
		return
	}
	if len(metric) > 200 {
		writeError(w, http.StatusBadRequest, errNeedMetric)
		return
	}
	if err := t.Store.Delete(r.Context(), metric); err != nil {
		t.Logger.Error("tag config: deleting", "err", err, "metric", url.QueryEscape(metric))
		writeError(w, http.StatusInternalServerError, errUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "metric": metric})
}
