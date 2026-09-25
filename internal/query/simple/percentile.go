package simple

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/tuvo1106/ozymandias/internal/sketch"
	"github.com/tuvo1106/ozymandias/internal/sketchstore"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Percentile aggregators (M2 §6). The set is fixed rather than "any q"
// because a fixed set is what a dashboard picker offers and what a monitor
// can be written against; an arbitrary q is an M3 grammar question.
const (
	P50 Agg = "p50"
	P75 Agg = "p75"
	P90 Agg = "p90"
	P95 Agg = "p95"
	P99 Agg = "p99"
)

// quantileOf returns the q an aggregator asks for, and whether it is one.
func quantileOf(a Agg) (float64, bool) {
	switch a {
	case P50:
		return 0.50, true
	case P75:
		return 0.75, true
	case P90:
		return 0.90, true
	case P95:
		return 0.95, true
	case P99:
		return 0.99, true
	default:
		return 0, false
	}
}

// ErrNoSketchStore means a percentile was asked of a deployment that stores
// no sketches. It is a server configuration answer, not a bad request, so the
// API reports it as such.
var ErrNoSketchStore = errors.New("this server has no sketch store")

// isPercentile reports whether a is one of the percentile aggregators.
func isPercentile(a Agg) bool {
	_, ok := quantileOf(a)
	return ok
}

// SketchReader is the part of internal/sketchstore the query layer needs.
//
// Streaming, not a slice: the query's own limit is on output buckets, and a
// year of one series at a ten-second flush is three million sketches. Each
// one is folded into its bucket and dropped, so a percentile costs the
// buckets it answers with rather than the window it reads.
type SketchReader interface {
	ReadEach(ctx context.Context, ref tsdb.SeriesRef, fromMs, toMs int64, fn func(sketchstore.Point) error) error
}

// runPercentile answers p50…p99: for each output bucket, merge every sketch
// of every selected series in the group, then take the quantile.
//
// Merge first, quantile second, and never the other way round. The mean of
// two hosts' p95s is not the fleet's p95 — it is not an approximation of it
// either, it is a different number with no error bound — and the only reason
// a sketch exists is that merging one *is* exact.
//
// Series selection runs against `<metric>.count`, the ordinary series written
// beside every sketch. It carries the same tags, is written in the same
// request, and goes through the same index and the same matchers as any other
// query, so "select the series, then read their sketches" needs no second
// index. See docs/adr/0015-sketch-storage-and-identity.md.
func runPercentile(ctx context.Context, store tsdb.MetricStore, sketches SketchReader, req Request, q float64) (Result, error) {
	if sketches == nil {
		return Result{}, fmt.Errorf("%s: %w", req.Agg, ErrNoSketchStore)
	}
	first := floorTo(req.From, req.Interval)
	n := int((req.To-first)/req.Interval) + 1
	res := Result{From: req.From, To: req.To, Interval: req.Interval, Series: []Series{}}

	sel := tsdb.Selector{Metric: req.Metric + wire.SuffixCount, Matchers: req.Filters}
	set, err := store.Select(ctx, sel, first*1000, req.To*1000)
	if err != nil {
		return Result{}, err
	}
	defer set.Close()

	groups := map[string]*sketchGroup{}
	var order []string
	for set.Next() {
		// The selected series is <metric>.count; the sketches are filed
		// under the metric itself, with the same tags.
		counted := set.Series()
		ref := tsdb.SeriesRef{Metric: req.Metric, Tags: counted.Tags}

		key, tags := groupKey(counted, req.By)
		g := groups[key]
		if g == nil {
			g = &sketchGroup{tags: tags, buckets: make([]*sketch.Sketch, n)}
		}
		seen := false
		err := sketches.ReadEach(ctx, ref, first*1000, req.To*1000, func(p sketchstore.Point) error {
			if err := g.add(p, first, req.Interval, n); err != nil {
				return err
			}
			seen = true
			return nil
		})
		if errors.Is(err, sketch.ErrIncompatible) {
			return Result{}, fmt.Errorf("%s:%s: %w", req.Agg, req.Metric, err)
		}
		if err != nil {
			return Result{}, err
		}
		// A group is created only once something lands in it, so a series
		// with no sketches in the window does not draw an empty line.
		if seen && groups[key] == nil {
			groups[key] = g
			order = append(order, key)
		}
	}
	if err := set.Err(); err != nil {
		return Result{}, err
	}

	for _, key := range order {
		g := groups[key]
		pts := make([]Point, n)
		for i := range pts {
			pts[i] = Point{T: (first + int64(i)*req.Interval) * 1000, V: g.quantile(i, q)}
		}
		res.Series = append(res.Series, Series{Metric: req.Metric, Tags: g.tags, Points: pts})
	}
	sortSeries(res.Series)
	return res, nil
}

// sketchGroup holds one group's merged sketch per output bucket.
type sketchGroup struct {
	tags    map[string]string
	buckets []*sketch.Sketch
}

// add folds one sketch into the group's buckets.
//
// The first sketch to reach a bucket is cloned rather than kept: it belongs
// to the store, and merging into it would write through into data the store
// handed us.
func (g *sketchGroup) add(p sketchstore.Point, first, interval int64, n int) error {
	i := (p.TimeMs/1000 - first) / interval
	if i < 0 || i >= int64(n) {
		return nil
	}
	if g.buckets[i] == nil {
		g.buckets[i] = p.Sketch.Clone()
		return nil
	}
	// Two sketches of the same metric built at different relative
	// accuracies. Answering from whichever subset happened to agree would be
	// the confident wrong answer the whole design exists to avoid, so this
	// is loud.
	return g.buckets[i].Merge(p.Sketch)
}

func (g *sketchGroup) quantile(i int, q float64) float64 {
	s := g.buckets[i]
	if s == nil {
		return math.NaN()
	}
	v, err := s.Quantile(q)
	if err != nil {
		return math.NaN()
	}
	return v
}
