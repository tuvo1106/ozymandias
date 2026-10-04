package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/loghub"
	"github.com/tuvo1106/ozymandias/internal/logstore"
	"github.com/tuvo1106/ozymandias/internal/query/logql"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// LogQuerier is what the logs API needs of the store (implemented by
// internal/logstore).
type LogQuerier interface {
	Search(ctx context.Context, q logql.Node, from, to int64, opts logstore.SearchOpts) (*logstore.SearchResult, error)
	Aggregate(ctx context.Context, q logql.Node, from, to int64, a logstore.AggSpec) (*logstore.AggResult, error)
	Facets(ctx context.Context, q logql.Node, from, to int64, keys []string, limit int) (*logstore.FacetResult, error)
}

// Logs serves the log endpoints (docs/api.md): list, histogram, facets and live tail.
//
// Times are unix MILLISECONDS here, unlike the metrics API's seconds: a log's
// timestamp is in milliseconds, so are the histogram's buckets and the
// pagination cursor, and a page of logs fed back as a window should not need
// a unit conversion that is easy to get wrong by 1000.
type Logs struct {
	Store  LogQuerier
	Hub    *loghub.Hub
	Clock  clock.Clock
	Logger *slog.Logger
	// ScanBudget bounds the raw bytes one query may decompress; zero is the
	// store's default.
	ScanBudget int64
	// Heartbeat is how often an idle tail sends a comment, so proxies and
	// browsers keep the connection open. Default 15s.
	Heartbeat time.Duration
}

// Register mounts the endpoints.
func (l *Logs) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/logs", l.list)
	mux.HandleFunc("GET /api/v1/logs/aggregate", l.aggregate)
	mux.HandleFunc("GET /api/v1/logs/facets", l.facets)
	mux.HandleFunc("GET /api/v1/logs/tail", l.tail)
}

const (
	defaultLogRange = 15 * time.Minute
	maxFacetKeys    = 10
	maxFacetLimit   = 100
	// targetBuckets is how many bars the automatic histogram interval aims for.
	targetBuckets = 60
	// maxBuckets refuses an explicit interval that would draw absurdly many bars.
	maxBuckets = 5000
)

// logParams is the query string every endpoint shares.
type logParams struct {
	q        logql.Node
	from, to int64
}

func (l *Logs) params(r *http.Request) (logParams, url.Values, error) {
	v := r.URL.Query()
	var p logParams
	var errs []error
	q, err := logql.Parse(v.Get("q"))
	if err != nil {
		return p, v, fmt.Errorf("q: %w", err)
	}
	p.q = q
	now := l.Clock.Now().UnixMilli()
	p.to = intParam(v.Get("to"), now, "to", &errs)
	p.from = intParam(v.Get("from"), p.to-defaultLogRange.Milliseconds(), "from", &errs)
	if len(errs) > 0 {
		return p, v, errors.Join(errs...)
	}
	if p.from > p.to {
		return p, v, fmt.Errorf("from (%d) is after to (%d)", p.from, p.to)
	}
	return p, v, nil
}

type statsJSON struct {
	Streams         int   `json:"streams"`
	BlocksRead      int   `json:"blocks_read"`
	BlocksSkipped   int   `json:"blocks_skipped"`
	BytesRead       int64 `json:"bytes_read"`
	EntriesExamined int   `json:"entries_examined"`
}

func toStats(s logstore.Stats) statsJSON {
	return statsJSON{s.Streams, s.BlocksRead, s.BlocksSkipped, s.BytesRead, s.EntriesExamined}
}

func (l *Logs) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	if errors.Is(err, logstore.ErrBadCursor) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if r.Context().Err() != nil {
		return // the client went away
	}
	l.Logger.Error("logs api: "+what, "err", err)
	writeError(w, http.StatusInternalServerError, fmt.Errorf("%s failed", what))
}

// list handles GET /api/v1/logs?q=&from=&to=&limit=&cursor=&order=desc|asc.
func (l *Logs) list(w http.ResponseWriter, r *http.Request) {
	p, v, err := l.params(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var errs []error
	opts := logstore.SearchOpts{Cursor: v.Get("cursor"), ScanBudget: l.ScanBudget}
	opts.Limit = int(intParam(v.Get("limit"), 0, "limit", &errs))
	if len(errs) > 0 {
		writeError(w, http.StatusBadRequest, errors.Join(errs...))
		return
	}
	switch o := v.Get("order"); o {
	case "", "desc":
	case "asc":
		opts.Order = logstore.Oldest
	default:
		writeError(w, http.StatusBadRequest, fmt.Errorf("order %q: want desc or asc", o))
		return
	}
	res, err := l.Store.Search(r.Context(), p.q, p.from, p.to, opts)
	if err != nil {
		l.fail(w, r, "log search", err)
		return
	}
	logs := res.Logs
	if logs == nil {
		logs = []wire.Log{}
	}
	writeJSON(w, http.StatusOK, struct {
		Logs      []wire.Log `json:"logs"`
		Cursor    string     `json:"cursor,omitempty"`
		Truncated bool       `json:"truncated"`
		Stats     statsJSON  `json:"stats"`
	}{logs, res.Cursor, res.Truncated, toStats(res.Stats)})
}

// niceIntervals are the bar widths the automatic histogram picks from, so
// bars line up with clock time (a bar is a minute, not 61 seconds).
var niceIntervals = []time.Duration{
	time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second,
	time.Minute, 5 * time.Minute, 10 * time.Minute, 30 * time.Minute,
	time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour,
}

// autoInterval is the smallest nice interval that draws at most targetBuckets bars.
func autoInterval(from, to int64) time.Duration {
	span := time.Duration(to-from) * time.Millisecond
	for _, d := range niceIntervals {
		if span/d <= targetBuckets {
			return d
		}
	}
	return niceIntervals[len(niceIntervals)-1]
}

// aggregate handles GET /api/v1/logs/aggregate?q=&from=&to=&interval=&by=.
// interval is in milliseconds; absent, one is chosen for about 60 bars.
func (l *Logs) aggregate(w http.ResponseWriter, r *http.Request) {
	p, v, err := l.params(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var errs []error
	iv := time.Duration(intParam(v.Get("interval"), 0, "interval", &errs)) * time.Millisecond
	if len(errs) > 0 {
		writeError(w, http.StatusBadRequest, errors.Join(errs...))
		return
	}
	switch {
	case iv < 0:
		writeError(w, http.StatusBadRequest, errors.New("interval must be positive"))
		return
	case iv == 0:
		iv = autoInterval(p.from, p.to)
	case (time.Duration(p.to-p.from)*time.Millisecond)/iv > maxBuckets:
		writeError(w, http.StatusBadRequest, fmt.Errorf("interval %v over this range would draw more than %d bars", iv, maxBuckets))
		return
	}
	by := v.Get("by")
	if err := validGroupKey(by); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	res, err := l.Store.Aggregate(r.Context(), p.q, p.from, p.to, logstore.AggSpec{Interval: iv, By: by, ScanBudget: l.ScanBudget})
	if err != nil {
		l.fail(w, r, "log aggregation", err)
		return
	}
	type bucket struct {
		Ts     int64            `json:"ts"`
		Counts map[string]int64 `json:"counts"`
	}
	bs := make([]bucket, 0, len(res.Buckets))
	for _, b := range res.Buckets {
		bs = append(bs, bucket{b.Ts, b.Counts})
	}
	writeJSON(w, http.StatusOK, struct {
		IntervalMs int64     `json:"interval_ms"`
		Buckets    []bucket  `json:"buckets"`
		Truncated  bool      `json:"truncated"`
		Stats      statsJSON `json:"stats"`
	}{iv.Milliseconds(), bs, res.Truncated, toStats(res.Stats)})
}

// validGroupKey accepts "", a reserved label, or "@attr.path".
func validGroupKey(by string) error {
	if by == "" || strings.HasPrefix(by, "@") && len(by) > 1 {
		return nil
	}
	switch by {
	case "service", "source", "host", "env", "status", "trace_id":
		return nil
	}
	return fmt.Errorf("by %q: want a label (service, source, host, env, status), or @attr.path", by)
}

// facets handles GET /api/v1/logs/facets?q=&from=&to=&keys=service,status,@path&limit=.
func (l *Logs) facets(w http.ResponseWriter, r *http.Request) {
	p, v, err := l.params(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var keys []string
	for _, k := range strings.Split(v.Get("keys"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			if err := validGroupKey(k); err != nil {
				writeError(w, http.StatusBadRequest, fmt.Errorf("keys: %w", err))
				return
			}
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 || len(keys) > maxFacetKeys {
		writeError(w, http.StatusBadRequest, fmt.Errorf("keys: give between 1 and %d, comma separated", maxFacetKeys))
		return
	}
	var errs []error
	limit := int(intParam(v.Get("limit"), 10, "limit", &errs))
	if len(errs) > 0 || limit < 1 || limit > maxFacetLimit {
		writeError(w, http.StatusBadRequest, fmt.Errorf("limit must be between 1 and %d", maxFacetLimit))
		return
	}
	res, err := l.Store.Facets(r.Context(), p.q, p.from, p.to, keys, limit)
	if err != nil {
		l.fail(w, r, "log facets", err)
		return
	}
	type fc struct {
		Value string `json:"value"`
		Count int64  `json:"count"`
	}
	out := make(map[string][]fc, len(keys))
	for _, k := range keys {
		vs := make([]fc, 0, len(res.Facets[k]))
		for _, f := range res.Facets[k] {
			vs = append(vs, fc{f.Value, f.Count})
		}
		out[k] = vs
	}
	capped := res.Capped
	if capped == nil {
		capped = []string{}
	}
	writeJSON(w, http.StatusOK, struct {
		Facets    map[string][]fc `json:"facets"`
		Capped    []string        `json:"capped"`
		Truncated bool            `json:"truncated"`
		Stats     statsJSON       `json:"stats"`
	}{out, capped, res.Truncated, toStats(res.Stats)})
}

// tail handles GET /api/v1/logs/tail?q= as server-sent events.
//
//	event: log      data: <a log as JSON>
//	event: dropped  data: {"dropped": n}   (this subscriber was too slow)
//	: keep-alive                          (a comment, every Heartbeat)
//
// Only logs that arrive after the connection opens are sent: tail is the
// present. A client that wants the recent past reads /api/v1/logs first and
// then tails, and may see a log in both (it is up to the client to dedupe on
// content; the tail has no cursor to offer).
func (l *Logs) tail(w http.ResponseWriter, r *http.Request) {
	if l.Hub == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("live tail is not available on this server"))
		return
	}
	q, err := logql.Parse(r.URL.Query().Get("q"))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("q: %w", err))
		return
	}
	sub, ok := l.Hub.Subscribe(q, 0)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, errors.New("too many live tails open"))
		return
	}
	defer sub.Close()
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // a buffering proxy would hold the stream back
	w.WriteHeader(http.StatusOK)
	// Headers and a first comment go out now, so the client's EventSource opens
	// before the first log arrives.
	_, _ = w.Write([]byte(": connected\n\n"))
	if err := rc.Flush(); err != nil {
		return
	}
	hb := l.Heartbeat
	if hb <= 0 {
		hb = 15 * time.Second
	}
	tick := l.Clock.NewTicker(hb)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case lg := <-sub.C:
			data, err := json.Marshal(lg)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: log\ndata: %s\n\n", data); err != nil {
				return
			}
			// Flush only when nothing else is waiting: a burst goes out in one write.
			if len(sub.C) == 0 {
				if err := rc.Flush(); err != nil {
					return
				}
			}
		case <-tick.C():
			if n := sub.TakeDropped(); n > 0 {
				if _, err := fmt.Fprintf(w, "event: dropped\ndata: {\"dropped\":%s}\n\n", strconv.FormatInt(n, 10)); err != nil {
					return
				}
			}
			if _, err := w.Write([]byte(": keep-alive\n\n")); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}
