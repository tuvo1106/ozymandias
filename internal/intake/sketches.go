package intake

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/sketch"
	"github.com/tuvo1106/ozymandias/internal/sketchstore"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// SketchStore is the part of internal/sketchstore this package needs.
type SketchStore interface {
	Append(ctx context.Context, entries []sketchstore.Entry) (sketchstore.AppendResult, error)
}

// sketches handles POST /v1/sketches (§D).
//
// A sketch lands in two stores, and the order matters. The four exact
// aggregates go to the TSDB first as ordinary series; only the series it
// accepted get their sketch written. The TSDB is append-only and the sketch
// store is last-write-wins, so letting the stricter one rule keeps one
// decision about what is too old rather than two that can disagree — and
// `<metric>.count` is what a percentile query selects on, so a bucket the
// TSDB refused could not be found anyway. See docs/adr/0015.
func (in *Intake) sketches(w http.ResponseWriter, r *http.Request) {
	data, err := readBody(w, r)
	if errors.Is(err, errTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.opts.Sketches == nil {
		writeError(w, http.StatusServiceUnavailable, "this server has no sketch store")
		return
	}
	now := in.opts.Clock.Now()
	valid, rejects, err := wire.DecodeSketches(data, wire.DecodeOptions{Now: now, MaxAge: in.opts.MaxAge})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	errs := make([]string, 0, len(rejects))
	for _, rj := range rejects {
		errs = append(errs, rj.String())
	}
	resp, err := in.ingestSketches(r.Context(), valid, now, errs)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, resp)
}

// scalar is one of the four ordinary series written beside a sketch.
type scalar struct {
	suffix string
	kind   wire.Kind
	value  func(wire.Sketch) float64
}

// scalars are written for every sketch. count and sum are counts, so they add
// up over time; min and max are gauges, because the minimum of two windows is
// not the sum of their minima and a store that added them would say so.
var scalars = [...]scalar{
	{wire.SuffixCount, wire.KindCount, func(s wire.Sketch) float64 { return s.Count }},
	{wire.SuffixSum, wire.KindCount, func(s wire.Sketch) float64 { return s.Sum }},
	{wire.SuffixMin, wire.KindGauge, func(s wire.Sketch) float64 { return s.Min }},
	{wire.SuffixMax, wire.KindGauge, func(s wire.Sketch) float64 { return s.Max }},
}

func (in *Intake) ingestSketches(ctx context.Context, valid []wire.SketchSeries, now time.Time, errs []string) (wire.IntakeResponse, error) {
	// decoded[i] is the sketches of accepted[i]; the two are built together
	// so a series refused by the metadata registry never reaches either store.
	var accepted []wire.SketchSeries
	var decoded [][]sketchstore.Point
	batch := make([]tsdb.SeriesSamples, 0, len(valid)*len(scalars))

	for _, s := range valid {
		reason, err := in.observeSketch(ctx, s, now)
		if err != nil {
			return wire.IntakeResponse{}, err
		}
		if reason != "" {
			errs = append(errs, fmt.Sprintf("%q: %s", s.Metric, reason))
			continue
		}
		points, reason := decodeSketchPoints(s)
		if reason != "" {
			errs = append(errs, fmt.Sprintf("%q: %s", s.Metric, reason))
			continue
		}
		batch = append(batch, scalarSeries(s)...)
		accepted = append(accepted, s)
		decoded = append(decoded, points)
	}

	res, err := in.opts.Store.Append(ctx, batch)
	if err != nil {
		in.opts.Logger.Error("intake: storing sketch aggregates", "err", err, "series", len(batch))
		return wire.IntakeResponse{}, errors.New("store unavailable")
	}
	// A series whose count was refused is not selectable, so its sketch has
	// nowhere to be found from. Refusing it here keeps the two stores holding
	// the same set of buckets.
	refused := map[string]string{}
	for _, rj := range res.Rejected {
		refused[rj.Series.Key()] = rj.Reason
	}

	entries := make([]sketchstore.Entry, 0, len(accepted))
	for i, s := range accepted {
		ref := tsdb.NewSeriesRef(s.Metric+wire.SuffixCount, s.Tags)
		if reason, ok := refused[ref.Key()]; ok {
			errs = append(errs, fmt.Sprintf("%q: %s", s.Metric, reason))
			continue
		}
		entries = append(entries, sketchstore.Entry{
			Series: tsdb.NewSeriesRef(s.Metric, s.Tags),
			Points: decoded[i],
		})
	}
	sres, err := in.opts.Sketches.Append(ctx, entries)
	if err != nil {
		in.opts.Logger.Error("intake: storing sketches", "err", err, "series", len(entries))
		return wire.IntakeResponse{}, errors.New("sketch store unavailable")
	}
	for _, rj := range sres.Rejected {
		errs = append(errs, fmt.Sprintf("%q: %s", rj.Series.Metric, rj.Reason))
	}

	in.accepted.Add(int64(sres.Series))
	in.rejected.Add(int64(len(errs)))
	in.points.Add(int64(res.Samples))
	in.sketchPoints.Add(int64(sres.Points))

	resp := wire.IntakeResponse{Status: "ok", Accepted: sres.Series, Rejected: len(errs), Errors: errs}
	if len(resp.Errors) > wire.MaxResponseErrors {
		resp.Errors = resp.Errors[:wire.MaxResponseErrors]
	}
	if resp.Errors == nil {
		resp.Errors = []string{}
	}
	return resp, nil
}

// observeSketch records the metric and its four derived names in the
// registry. It returns a reason to reject the series, or an error if the
// registry itself is unavailable.
func (in *Intake) observeSketch(ctx context.Context, s wire.SketchSeries, now time.Time) (string, error) {
	// The derived names carry a suffix, so a metric near the length limit
	// produces a name that is not one. Better to say so than to write three
	// of the four series and drop the fourth.
	for _, sc := range scalars {
		if !wire.ValidMetricName(s.Metric + sc.suffix) {
			return fmt.Sprintf("%s%s is not a valid metric name; the name is too long to carry the suffix",
				s.Metric, sc.suffix), nil
		}
	}
	if err := in.opts.Registry.Observe(ctx, s.Metric, wire.KindDistribution, s.Interval, now); err != nil {
		if !errors.Is(err, meta.ErrTypeConflict) {
			in.opts.Logger.Error("intake: metadata", "err", err)
			return "", errors.New("metadata store unavailable")
		}
		return err.Error(), nil
	}
	for _, sc := range scalars {
		interval := s.Interval
		if sc.kind == wire.KindGauge {
			interval = 0
		}
		if err := in.opts.Registry.Observe(ctx, s.Metric+sc.suffix, sc.kind, interval, now); err != nil {
			if !errors.Is(err, meta.ErrTypeConflict) {
				in.opts.Logger.Error("intake: metadata", "err", err)
				return "", errors.New("metadata store unavailable")
			}
			return err.Error(), nil
		}
	}
	return "", nil
}

// decodeSketchPoints turns a series' payloads into sketches, rejecting the
// series whole if any of them is unusable — the same unit of rejection the
// decoder uses, so a caller does not have to reason about half a series.
func decodeSketchPoints(s wire.SketchSeries) ([]sketchstore.Point, string) {
	points := make([]sketchstore.Point, 0, len(s.Points))
	for _, p := range s.Points {
		sk, err := sketch.FromWire(p.Sketch)
		if err != nil {
			return nil, fmt.Sprintf("point at %d: %v", p.Timestamp, err)
		}
		points = append(points, sketchstore.Point{TimeMs: p.Timestamp * 1000, Sketch: sk})
	}
	return points, ""
}

// scalarSeries builds the four ordinary series for one sketch series.
//
// A bucket that observed nothing contributes its count and no more: min and
// max of no observations are not zero, and writing zero would put a point on
// a chart that nothing measured.
func scalarSeries(s wire.SketchSeries) []tsdb.SeriesSamples {
	out := make([]tsdb.SeriesSamples, 0, len(scalars))
	for _, sc := range scalars {
		samples := make([]tsdb.Sample, 0, len(s.Points))
		for _, p := range s.Points {
			if p.Sketch.Count == 0 && (sc.suffix == wire.SuffixMin || sc.suffix == wire.SuffixMax) {
				continue
			}
			samples = append(samples, tsdb.Sample{T: p.Timestamp * 1000, V: sc.value(p.Sketch)})
		}
		if len(samples) == 0 {
			continue
		}
		out = append(out, tsdb.SeriesSamples{
			Series:  tsdb.NewSeriesRef(s.Metric+sc.suffix, s.Tags),
			Samples: samples,
		})
	}
	return out
}
