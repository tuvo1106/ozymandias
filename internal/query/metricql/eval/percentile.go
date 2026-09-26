package eval

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/tuvo1106/ozymandias/internal/query/metricql"
	"github.com/tuvo1106/ozymandias/internal/sketch"
	"github.com/tuvo1106/ozymandias/internal/sketchstore"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// percentile answers p50…p99: for each output bucket, merge every sketch of
// every selected series in the group, then take the quantile of the merge.
//
// Merge first, quantile second, and never the other way round. The mean of two
// hosts' p95s is not the fleet's p95, and it is not an approximation of it
// either — it is a different number with no error bound. That merging a sketch
// *is* exact is the only reason sketches are stored at all.
//
// Series selection runs against `<metric>.count`, the ordinary series written
// beside every sketch: same tags, same request, same index, same matchers. So
// "select the series, then read their sketches" needs no second index. See
// docs/adr/0015-sketch-storage-and-identity.md.
func (e *Evaluator) percentile(ctx context.Context, q *metricql.Query, g grid, st *state, kind wire.Kind) (frame, error) {
	quantile, _ := q.Agg.Quantile()
	if kind != "" && kind != wire.KindDistribution {
		return frame{}, fmt.Errorf("%s is a %s, not a distribution, so it has no percentiles", q.Metric, kind)
	}
	if e.Sketches == nil {
		return frame{}, fmt.Errorf("%s: %w", q.Agg, ErrNoSketchStore)
	}
	sel, post, err := e.selector(q, st)
	if err != nil {
		return frame{}, err
	}
	sel.Metric = q.Metric + wire.SuffixCount

	set, err := e.Store.Select(ctx, sel, g.first*1000, g.endMs()-1)
	if err != nil {
		return frame{}, err
	}
	defer set.Close()

	groups := map[string]*sketchGroup{}
	var order []string
	selected := 0
	for set.Next() {
		counted := set.Series()
		if !post.matches(counted) {
			continue
		}
		if selected++; selected > MaxSeriesPerNode {
			return frame{}, tooManySeries(q)
		}
		// The selected series is <metric>.count; the sketches are filed under
		// the metric itself, with the same tags.
		ref := tsdb.SeriesRef{Metric: q.Metric, Tags: counted.Tags}
		key, tags := groupOf(counted, q.By)
		sg := groups[key]
		if sg == nil {
			sg = &sketchGroup{tags: tags, buckets: make([]*sketch.Sketch, g.n)}
		}
		seen := false
		err := e.Sketches.ReadEach(ctx, ref, g.first*1000, g.endMs()-1, func(p sketchstore.Point) error {
			if err := sg.add(p, g); err != nil {
				return err
			}
			seen = true
			return nil
		})
		if errors.Is(err, sketch.ErrIncompatible) {
			return frame{}, fmt.Errorf("%s:%s: %w", q.Agg, q.Metric, err)
		}
		if err != nil {
			return frame{}, err
		}
		// A group appears only once something has landed in it, so a series
		// with no sketches in the window does not draw an empty line.
		if seen && groups[key] == nil {
			groups[key] = sg
			order = append(order, key)
		}
	}
	if err := set.Err(); err != nil {
		return frame{}, err
	}

	f := frame{metric: q.Metric, groups: make([]*group, 0, len(order))}
	for _, key := range order {
		sg := groups[key]
		grp := &group{tags: sg.tags, values: make([]float64, g.n)}
		for i := range grp.values {
			grp.values[i] = sg.quantile(i, quantile)
		}
		f.groups = append(f.groups, grp)
	}
	return applyModifiers(f, q, g, kind)
}

// sketchGroup holds one group's merged sketch per output bucket.
type sketchGroup struct {
	tags    map[string]string
	buckets []*sketch.Sketch
}

// add folds one stored sketch into the bucket it belongs to.
//
// The first sketch to reach a bucket is cloned rather than kept: it belongs to
// the store, and merging into it would write through into data the store
// handed us.
func (s *sketchGroup) add(p sketchstore.Point, g grid) error {
	i, ok := g.index(p.TimeMs / 1000)
	if !ok {
		return nil
	}
	if s.buckets[i] == nil {
		s.buckets[i] = p.Sketch.Clone()
		return nil
	}
	// Two sketches of one metric built at different relative accuracies.
	// Answering from whichever subset happened to agree would be exactly the
	// confident wrong answer sketches exist to avoid, so this is loud.
	return s.buckets[i].Merge(p.Sketch)
}

func (s *sketchGroup) quantile(i int, q float64) float64 {
	if s.buckets[i] == nil {
		return math.NaN()
	}
	v, err := s.buckets[i].Quantile(q)
	if err != nil {
		return math.NaN()
	}
	return v
}
