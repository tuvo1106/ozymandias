package selfmetrics

import (
	"encoding/json"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Type distinguishes how a value should be interpreted downstream.
type Type string

// The instrument types. They mirror the wire protocol's series types
// (docs/wire-protocol.md §C): a counter becomes a `count`, a gauge a `gauge`.
const (
	TypeCounter Type = "counter"
	TypeGauge   Type = "gauge"
)

// Counter is a monotonically increasing count. The zero value is not usable;
// get one from [Registry.Counter].
type Counter struct{ v atomic.Int64 }

// Add increases the counter by n. Negative n is ignored: a counter that goes
// down would read as a reset downstream and corrupt every rate computed from
// it.
func (c *Counter) Add(n int64) {
	if n > 0 {
		c.v.Add(n)
	}
}

// Inc adds one.
func (c *Counter) Inc() { c.v.Add(1) }

// Value returns the current count.
func (c *Counter) Value() int64 { return c.v.Load() }

// Gauge holds the latest value of something that goes up and down. The zero
// value is not usable; get one from [Registry.Gauge].
type Gauge struct{ bits atomic.Uint64 }

// Set replaces the gauge's value.
func (g *Gauge) Set(v float64) { g.bits.Store(math.Float64bits(v)) }

// Value returns the gauge's current value.
func (g *Gauge) Value() float64 { return math.Float64frombits(g.bits.Load()) }

// Point is one instrument's value at snapshot time.
type Point struct {
	Name  string   `json:"name"`
	Type  Type     `json:"type"`
	Tags  []string `json:"tags"`
	Value float64  `json:"value"`
}

// Registry owns a process's instruments. Instruments are identified by
// (type, name, tag set); asking twice for the same identity returns the same
// instrument, so independent callers can share one without coordination.
type Registry struct {
	mu       sync.Mutex
	counters map[string]*entry[*Counter]
	gauges   map[string]*entry[*Gauge]
	funcs    map[string]*entry[func() float64]
}

type entry[T any] struct {
	name string
	tags []string
	inst T
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		counters: map[string]*entry[*Counter]{},
		gauges:   map[string]*entry[*Gauge]{},
		funcs:    map[string]*entry[func() float64]{},
	}
}

// Counter returns the counter for name and tags ("key:value" strings, in any
// order, duplicates ignored), creating it on first use.
func (r *Registry) Counter(name string, tags ...string) *Counter {
	r.mu.Lock()
	defer r.mu.Unlock()
	return getOrCreate(r.counters, name, tags, func() *Counter { return &Counter{} })
}

// Gauge returns the gauge for name and tags, creating it on first use.
func (r *Registry) Gauge(name string, tags ...string) *Gauge {
	r.mu.Lock()
	defer r.mu.Unlock()
	return getOrCreate(r.gauges, name, tags, func() *Gauge { return &Gauge{} })
}

// GaugeFunc registers a gauge whose value is computed by fn at snapshot time —
// for values that are cheaper to read on demand than to keep updated (uptime,
// queue lengths). Registering the same identity again replaces fn. fn is
// called with no lock held, but may be called concurrently with itself.
func (r *Registry) GaugeFunc(name string, fn func() float64, tags ...string) {
	norm := normalizeTags(tags)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.funcs[key(name, norm)] = &entry[func() float64]{name: name, tags: norm, inst: fn}
}

func getOrCreate[T any](m map[string]*entry[T], name string, tags []string, mk func() T) T {
	norm := normalizeTags(tags)
	k := key(name, norm)
	if e, ok := m[k]; ok {
		return e.inst
	}
	e := &entry[T]{name: name, tags: norm, inst: mk()}
	m[k] = e
	return e.inst
}

// Snapshot returns every instrument's current value, sorted by name then tags
// so output is stable across calls.
func (r *Registry) Snapshot() []Point {
	r.mu.Lock()
	points := make([]Point, 0, len(r.counters)+len(r.gauges)+len(r.funcs))
	for _, e := range r.counters {
		points = append(points, Point{e.name, TypeCounter, e.tags, float64(e.inst.Value())})
	}
	for _, e := range r.gauges {
		points = append(points, Point{e.name, TypeGauge, e.tags, e.inst.Value()})
	}
	funcs := make([]*entry[func() float64], 0, len(r.funcs))
	for _, e := range r.funcs {
		funcs = append(funcs, e)
	}
	r.mu.Unlock()

	// Computed gauges run outside the lock: fn may be slow, and must be free
	// to use the registry itself.
	for _, e := range funcs {
		points = append(points, Point{e.name, TypeGauge, e.tags, e.inst()})
	}
	sort.Slice(points, func(i, j int) bool {
		if points[i].Name != points[j].Name {
			return points[i].Name < points[j].Name
		}
		return strings.Join(points[i].Tags, ",") < strings.Join(points[j].Tags, ",")
	})
	return points
}

// Handler serves the snapshot as JSON: {"metrics": [Point, ...]}. NaN and
// ±Inf gauges are reported as null, since JSON has no spelling for them.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		type jsonPoint struct {
			Name  string   `json:"name"`
			Type  Type     `json:"type"`
			Tags  []string `json:"tags"`
			Value *float64 `json:"value"`
		}
		snap := r.Snapshot()
		out := make([]jsonPoint, len(snap))
		for i, p := range snap {
			out[i] = jsonPoint{Name: p.Name, Type: p.Type, Tags: p.Tags}
			if !math.IsNaN(p.Value) && !math.IsInf(p.Value, 0) {
				v := p.Value
				out[i].Value = &v
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"metrics": out})
	})
}

// normalizeTags sorts and de-duplicates tags — a tag set is a set, and
// ["b:1","a:1"] must identify the same instrument as ["a:1","b:1"]. The same
// rule the agent applies to every incoming metric (wire-protocol.md "Names and
// tags").
func normalizeTags(tags []string) []string {
	out := slices.Clone(tags)
	slices.Sort(out)
	out = slices.Compact(out)
	if out == nil {
		out = []string{}
	}
	return out
}

func key(name string, normTags []string) string {
	return name + "|" + strings.Join(normTags, ",")
}
