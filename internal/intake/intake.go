package intake

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Registry records each metric's type on first sight and refuses a
// conflicting one later with an error wrapping meta.ErrTypeConflict
// (implemented by internal/meta). Any other error is a storage failure.
type Registry interface {
	Observe(ctx context.Context, metric string, kind wire.Kind, interval int64, now time.Time) error
}

// Options configures the intake handlers.
type Options struct {
	Store    tsdb.MetricStore
	Registry Registry
	// Sketches stores the DDSketches POST /v1/sketches carries. Nil means
	// the endpoint answers 503 rather than 404: the route exists, this
	// deployment just cannot serve it.
	Sketches SketchStore
	// Logs stores the logs POST /v1/logs carries; nil answers 503, as for Sketches.
	Logs LogStore
	// LogHub, if set, is told of every stored log (live tail).
	LogHub LogPublisher
	// Traces stores the spans POST /v1/traces carries; nil answers 503.
	Traces SpanStore
	// Queue, if set, switches intake to queued mode: a request is validated,
	// made durable on its signal's topic and answered 202, and the stores are
	// fed by Run. Nil is direct mode, the pre-M7 path. It must hold every
	// topic in Topics.
	Queue map[string]Producer
	// MaxAge rejects points older than this; 0 accepts any age.
	MaxAge  time.Duration
	Clock   clock.Clock           // default clock.Real()
	Metrics *selfmetrics.Registry // default: a new registry
	Logger  *slog.Logger          // default slog.Default()
}

// Intake serves the /v1/* endpoints.
type Intake struct {
	opts Options

	// cons is the same code with the consumers' counters (see newConsumer).
	cons *Intake

	accepted, rejected, points   *selfmetrics.Counter
	sketchPoints                 *selfmetrics.Counter
	logsAccepted, logsRejected   *selfmetrics.Counter
	spansAccepted, spansRejected *selfmetrics.Counter
}

// New returns the intake handlers.
func New(opts Options) *Intake {
	if opts.Clock == nil {
		opts.Clock = clock.Real()
	}
	if opts.Metrics == nil {
		opts.Metrics = selfmetrics.NewRegistry()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	in := &Intake{
		opts:     opts,
		accepted: opts.Metrics.Counter("ozy.intake.series_accepted"),
		rejected: opts.Metrics.Counter("ozy.intake.series_rejected"),
		points:   opts.Metrics.Counter("ozy.intake.points_accepted"),

		sketchPoints: opts.Metrics.Counter("ozy.intake.sketch_points_accepted"),

		logsAccepted: opts.Metrics.Counter("ozy.intake.logs_accepted"),
		logsRejected: opts.Metrics.Counter("ozy.intake.logs_rejected"),

		spansAccepted: opts.Metrics.Counter("ozy.intake.spans_accepted"),
		spansRejected: opts.Metrics.Counter("ozy.intake.spans_rejected"),
	}
	in.cons = in.newConsumer()
	for _, t := range Topics {
		if opts.Queue != nil {
			in.shed(t) // registered up front: a counter that appears only after the first shed reads as no shedding
		}
	}
	return in
}

// Register mounts the intake endpoints on mux.
func (in *Intake) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/series", in.series)
	mux.HandleFunc("POST /v1/sketches", in.sketches)
	mux.HandleFunc("POST /v1/logs", in.logs)
	mux.HandleFunc("POST /v1/traces", in.traces)
}

// errTooLarge marks a body over one of the size limits.
var errTooLarge = errors.New("request body too large")

// readBody applies both limits of wire-protocol §0: the body as sent (4 MiB)
// and, if gzip'd, after decompression (16 MiB). The second is the zip-bomb
// guard — a few KiB of gzip can expand to gigabytes.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	body := http.MaxBytesReader(w, r.Body, wire.MaxBodyBytes)
	var src io.Reader = body
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(body)
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				return nil, errTooLarge
			}
			return nil, fmt.Errorf("gzip: %w", err)
		}
		defer zr.Close()
		src = zr
	}
	data, err := io.ReadAll(io.LimitReader(src, wire.MaxDecompressedBytes+1))
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &mbe):
		return nil, errTooLarge
	case err != nil:
		return nil, fmt.Errorf("reading body: %w", err)
	case len(data) > wire.MaxDecompressedBytes:
		return nil, errTooLarge
	}
	return data, nil
}

// series handles POST /v1/series (§C): decode, validate each series, check
// its type against the registry, store what survives, and report per-series
// outcomes. Only a body that can't be read at all is an HTTP error.
func (in *Intake) series(w http.ResponseWriter, r *http.Request) {
	data, err := readBody(w, r)
	if errors.Is(err, errTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := in.opts.Clock.Now()
	valid, rejects, err := wire.DecodeSeries(data, wire.DecodeOptions{Now: now, MaxAge: in.opts.MaxAge})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	errs := make([]string, 0, len(rejects))
	for _, rj := range rejects {
		errs = append(errs, rj.String())
	}
	if in.queued() {
		kept, errs, err := in.admitSeries(r.Context(), valid, now, errs)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		if len(kept) > 0 && !in.enqueue(w, r, TopicSeries, kept) {
			return
		}
		in.accepted.Add(int64(len(kept)))
		in.rejected.Add(int64(len(errs)))
		writeJSON(w, http.StatusAccepted, queuedResponse(len(kept), errs))
		return
	}
	resp, err := in.ingest(r.Context(), valid, now, errs)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, resp)
}

// Ingest stores already-validated series (canonical tags) the way the HTTP
// endpoint does. ozyd uses it to store its own self-metrics, so they
// take exactly the path an agent's would, minus the network.
func (in *Intake) Ingest(ctx context.Context, series []wire.Series) (wire.IntakeResponse, error) {
	now := in.opts.Clock.Now()
	var valid []wire.Series
	var errs []string
	for i := range series {
		if err := wire.ValidateSeries(&series[i], wire.DecodeOptions{Now: now, MaxAge: in.opts.MaxAge}); err != nil {
			errs = append(errs, wire.Rejection{Index: i, Metric: series[i].Metric, Reason: err.Error()}.String())
			continue
		}
		valid = append(valid, series[i])
	}
	return in.ingest(ctx, valid, now, errs)
}

// ingest checks each series' type against the registry and stores what
// survives. An error means a storage failure (retryable, 503), never a bad
// series — those are counted in the response.
func (in *Intake) ingest(ctx context.Context, valid []wire.Series, now time.Time, errs []string) (wire.IntakeResponse, error) {
	kept, errs, err := in.admitSeries(ctx, valid, now, errs)
	if err != nil {
		return wire.IntakeResponse{}, err
	}
	return in.storeSeries(ctx, kept, errs)
}

// admitSeries is the part of a series request that needs only the metadata
// registry: a type conflict is the sender's mistake and is answered now, in
// both modes. What it keeps is what queued mode puts on the queue.
func (in *Intake) admitSeries(ctx context.Context, valid []wire.Series, now time.Time, errs []string) ([]wire.Series, []string, error) {
	kept := make([]wire.Series, 0, len(valid))
	for _, s := range valid {
		if err := in.opts.Registry.Observe(ctx, s.Metric, s.Type, s.Interval, now); err != nil {
			if !errors.Is(err, meta.ErrTypeConflict) {
				in.opts.Logger.Error("intake: metadata", "err", err)
				return nil, nil, errors.New("metadata store unavailable")
			}
			errs = append(errs, fmt.Sprintf("%q: %v", s.Metric, err))
			continue
		}
		kept = append(kept, s)
	}
	return kept, errs, nil
}

// storeSeries appends admitted series to the metric store. It is idempotent,
// which is what lets the queue deliver a record twice: the TSDB no-ops a
// repeated (t, v) and counts an older point as rejected, not as an error.
func (in *Intake) storeSeries(ctx context.Context, valid []wire.Series, errs []string) (wire.IntakeResponse, error) {
	batch := make([]tsdb.SeriesSamples, 0, len(valid))
	for _, s := range valid {
		samples := make([]tsdb.Sample, len(s.Points))
		for i, p := range s.Points {
			samples[i] = tsdb.Sample{T: p.Timestamp * 1000, V: p.Value}
		}
		batch = append(batch, tsdb.SeriesSamples{Series: tsdb.NewSeriesRef(s.Metric, s.Tags), Samples: samples})
	}

	res, err := in.opts.Store.Append(ctx, batch)
	if err != nil {
		in.opts.Logger.Error("intake: storing series", "err", err, "series", len(batch))
		return wire.IntakeResponse{}, errors.New("store unavailable")
	}
	for _, rj := range res.Rejected {
		errs = append(errs, fmt.Sprintf("%q: %s", rj.Series.Metric, rj.Reason))
	}
	in.accepted.Add(int64(res.Series))
	in.rejected.Add(int64(len(errs)))
	in.points.Add(int64(res.Samples))

	resp := wire.IntakeResponse{Status: "ok", Accepted: res.Series, Rejected: len(errs), Errors: errs}
	if len(resp.Errors) > wire.MaxResponseErrors {
		resp.Errors = resp.Errors[:wire.MaxResponseErrors]
	}
	if resp.Errors == nil {
		resp.Errors = []string{}
	}
	return resp, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, wire.ErrorResponse{Status: "error", Error: msg})
}
