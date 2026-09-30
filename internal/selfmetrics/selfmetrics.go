package selfmetrics

import (
	"encoding/json"
	"math"
	"net/http"
	"runtime/metrics"
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

	// released marks an instrument every holder has released: this is its
	// last value, and the Reporter drops it once it has sent it.
	released bool
}

// Registry owns a process's instruments. Instruments are identified by
// (type, name, tag set); asking twice for the same identity returns the same
// instrument, so independent callers can share one without coordination.
type Registry struct {
	mu       sync.Mutex
	counters map[string]*entry[*Counter]
	gauges   map[string]*entry[*Gauge]
	funcs    map[string]*entry[func() float64]
	cfuncs   map[string]*entry[func() float64]
}

type entry[T any] struct {
	name string
	tags []string
	inst T
	refs int // Counter/Gauge calls not yet matched by a Release
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		counters: map[string]*entry[*Counter]{},
		gauges:   map[string]*entry[*Gauge]{},
		funcs:    map[string]*entry[func() float64]{},
		cfuncs:   map[string]*entry[func() float64]{},
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

// CounterFunc registers a counter whose cumulative value fn reads at
// snapshot time, from something that already counts (the runtime's GC
// cycles). Like a Counter it is reported as the increase since the last
// report, so a restart does not read as a fall. Registering the same
// identity again replaces fn.
func (r *Registry) CounterFunc(name string, fn func() float64, tags ...string) {
	norm := normalizeTags(tags)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfuncs[key(name, norm)] = &entry[func() float64]{name: name, tags: norm, inst: fn}
}

func getOrCreate[T any](m map[string]*entry[T], name string, tags []string, mk func() T) T {
	norm := normalizeTags(tags)
	k := key(name, norm)
	if e, ok := m[k]; ok {
		e.refs++
		return e.inst
	}
	e := &entry[T]{name: name, tags: norm, inst: mk(), refs: 1}
	m[k] = e
	return e.inst
}

// Release gives up one hold on the counter or gauge name with tags, taken
// by a Counter or Gauge call. Once every hold is given up, the instrument
// is reported one last time and then removed, so a process whose
// instruments come and go — one set per discovered check, tagged with its
// container's name — holds only the live ones. Without it the registry, and
// the Reporter's memory of each counter, grow with every name ever seen.
//
// Instruments nobody releases (almost all of them: a process-lifetime
// counter is never given up) are unaffected. A Counter or Gauge call for a
// released identity before its last report takes it back, value intact.
func (r *Registry) Release(name string, tags ...string) {
	k := key(name, normalizeTags(tags))
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.counters[k]; ok && e.refs > 0 {
		e.refs--
	}
	if e, ok := r.gauges[k]; ok && e.refs > 0 {
		e.refs--
	}
}

// drop removes the instrument p reported, if it is still released: nobody
// has taken it back since the snapshot. It reports whether it did. The Reporter calls it after sending
// p, which is why removal waits for a report rather than happening at
// Release — a counter's increments since the last report would be lost.
func (r *Registry) drop(p Point) bool {
	k := key(p.Name, p.Tags)
	r.mu.Lock()
	defer r.mu.Unlock()
	switch p.Type {
	case TypeCounter:
		if e, ok := r.counters[k]; ok && e.refs == 0 {
			delete(r.counters, k)
			return true
		}
	case TypeGauge:
		if e, ok := r.gauges[k]; ok && e.refs == 0 {
			delete(r.gauges, k)
			return true
		}
	}
	return false
}

// Snapshot returns every instrument's current value, sorted by name then tags
// so output is stable across calls.
func (r *Registry) Snapshot() []Point {
	r.mu.Lock()
	points := make([]Point, 0, len(r.counters)+len(r.gauges)+len(r.funcs))
	for _, e := range r.counters {
		points = append(points, Point{e.name, TypeCounter, e.tags, float64(e.inst.Value()), e.refs == 0})
	}
	for _, e := range r.gauges {
		points = append(points, Point{e.name, TypeGauge, e.tags, e.inst.Value(), e.refs == 0})
	}
	funcs := make([]*entry[func() float64], 0, len(r.funcs))
	for _, e := range r.funcs {
		funcs = append(funcs, e)
	}
	cfuncs := make([]*entry[func() float64], 0, len(r.cfuncs))
	for _, e := range r.cfuncs {
		cfuncs = append(cfuncs, e)
	}
	r.mu.Unlock()

	// Computed instruments run outside the lock: fn may be slow, and must
	// be free to use the registry itself.
	for _, e := range funcs {
		points = append(points, Point{Name: e.name, Type: TypeGauge, Tags: e.tags, Value: e.inst()})
	}
	for _, e := range cfuncs {
		points = append(points, Point{Name: e.name, Type: TypeCounter, Tags: e.tags, Value: e.inst()})
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

// RegisterRuntime adds the Go runtime's view of the process: goroutines, heap
// in use, and GC cycles. They answer "is this process leaking" — a goroutine
// count that climbs with every reconnect, a heap that never comes back down.
//
// Read through runtime/metrics rather than runtime.ReadMemStats, which stops
// the world to take its snapshot; these samples are the ones the runtime
// keeps current anyway, so reading them costs a few loads.
func RegisterRuntime(r *Registry, tags ...string) {
	read := func(name string) func() float64 {
		return func() float64 {
			s := []metrics.Sample{{Name: name}}
			metrics.Read(s)
			switch s[0].Value.Kind() {
			case metrics.KindUint64:
				return float64(s[0].Value.Uint64())
			case metrics.KindFloat64:
				return s[0].Value.Float64()
			}
			return math.NaN() // unknown to this Go version: skipped by the reporter
		}
	}
	r.GaugeFunc("ozy.runtime.goroutines", read("/sched/goroutines:goroutines"), tags...)
	r.GaugeFunc("ozy.runtime.heap_bytes", read("/memory/classes/heap/objects:bytes"), tags...)
	r.CounterFunc("ozy.runtime.gc_runs", read("/gc/cycles/total:gc-cycles"), tags...)
}
