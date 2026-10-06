package tracerecv

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/sampler"
	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Limits from the plan (§3): a 10 MiB body, 8 concurrent decodes.
const (
	DefaultMaxBody       = 10 << 20
	DefaultMaxConcurrent = 8
	// MaxAge is how old a span may be. A tracer that was offline and replays is
	// not what this endpoint is for.
	MaxAge = 24 * time.Hour
)

// Observer sees every valid span before sampling (the concentrator).
type Observer interface{ Observe([]wire.Span) }

// Decider is the sampler.
type Decider interface {
	Decide(chunk []wire.Span, now time.Time) sampler.Reason
	Rates() map[string]float64
}

// Sink takes the spans to keep (the forwarder).
type Sink interface{ SubmitSpans([]wire.Span) }

// Options configure a Handler.
type Options struct {
	Observer Observer
	Decider  Decider
	Sink     Sink
	// Env and Host fill in what a span did not say.
	Env, Host     string
	MaxBody       int64
	MaxConcurrent int
	Clock         clock.Clock
	Registry      *selfmetrics.Registry
	Logger        *slog.Logger
}

// Handler serves POST /v1/traces.
type Handler struct {
	opts Options
	sem  chan struct{}

	spansIn, spansRejected, chunks, busy, badBody *selfmetrics.Counter
}

// New returns a Handler.
func New(opts Options) *Handler {
	if opts.MaxBody <= 0 {
		opts.MaxBody = DefaultMaxBody
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = DefaultMaxConcurrent
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real()
	}
	if opts.Registry == nil {
		opts.Registry = selfmetrics.NewRegistry()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	r := opts.Registry
	return &Handler{
		opts: opts, sem: make(chan struct{}, opts.MaxConcurrent),
		spansIn:       r.Counter("ozy.agent.traces.spans_received"),
		spansRejected: r.Counter("ozy.agent.traces.spans_rejected"),
		chunks:        r.Counter("ozy.agent.traces.chunks_received"),
		busy:          r.Counter("ozy.agent.traces.requests_busy"),
		badBody:       r.Counter("ozy.agent.traces.requests_invalid"),
	}
}

type response struct {
	RateByService map[string]float64 `json:"rate_by_service"`
	Accepted      int                `json:"accepted"`
	Rejected      int                `json:"rejected"`
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	default:
		h.busy.Inc()
		w.Header().Set("Retry-After", "1")
		http.Error(w, "trace intake is busy", http.StatusTooManyRequests)
		return
	}
	var body io.Reader = http.MaxBytesReader(w, r.Body, h.opts.MaxBody)
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(body)
		if err != nil {
			h.badBody.Inc()
			http.Error(w, "bad gzip body", http.StatusBadRequest)
			return
		}
		defer zr.Close()
		// The decompressed size is bounded too: a small gzip must not become a huge body.
		body = io.LimitReader(zr, h.opts.MaxBody+1)
	}
	raw, err := io.ReadAll(body)
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig) || int64(len(raw)) > h.opts.MaxBody:
		h.badBody.Inc()
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	case err != nil:
		h.badBody.Inc()
		http.Error(w, "reading body: "+err.Error(), http.StatusBadRequest)
		return
	}
	now := h.opts.Clock.Now()
	_, chunks, rejects, err := wire.DecodeTraces(raw, wire.DecodeOptions{Now: now, MaxAge: MaxAge})
	if err != nil {
		h.badBody.Inc()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var kept []wire.Span
	accepted := 0
	for _, c := range chunks {
		h.fill(c)
		accepted += len(c)
		h.opts.Observer.Observe(c)
		if h.opts.Decider.Decide(c, now) != sampler.Dropped {
			kept = append(kept, c...)
		}
	}
	h.chunks.Add(int64(len(chunks)))
	h.spansIn.Add(int64(accepted))
	h.spansRejected.Add(int64(len(rejects)))
	if len(rejects) > 0 {
		h.opts.Logger.Debug("tracerecv: spans refused", "count", len(rejects), "first", rejects[0].String())
	}
	if len(kept) > 0 {
		h.opts.Sink.SubmitSpans(kept)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response{RateByService: h.opts.Decider.Rates(), Accepted: accepted, Rejected: len(rejects)})
}

// fill adds the env and host a span did not carry. It never overwrites: a
// service that says it is in staging is believed over the agent's default.
func (h *Handler) fill(chunk []wire.Span) {
	for i := range chunk {
		sp := &chunk[i]
		if sp.Meta == nil {
			sp.Meta = map[string]string{}
		}
		if sp.Meta["env"] == "" && h.opts.Env != "" {
			sp.Meta["env"] = h.opts.Env
		}
		if sp.Meta["host"] == "" && h.opts.Host != "" {
			sp.Meta["host"] = h.opts.Host
		}
	}
}
