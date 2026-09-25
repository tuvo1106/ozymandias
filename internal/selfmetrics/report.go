package selfmetrics

import (
	"math"
	"strings"
	"sync"
	"time"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Reporter turns registry snapshots into wire series, so a binary's own
// metrics travel the same path as any app's. Counters in the registry are
// cumulative since process start; the wire's count type is "events in this
// interval", so the Reporter remembers each counter's last value and emits
// the difference.
type Reporter struct {
	reg      *Registry
	interval int64
	tags     []string

	mu     sync.Mutex
	prev   map[string]float64 // counter key → value at the last Collect
	lastTS int64              // bucket timestamp of the last emitted snapshot
}

// NewReporter reports reg's instruments with tags added to each (typically
// host:<name>), stamped at interval boundaries.
func NewReporter(reg *Registry, interval time.Duration, tags ...string) *Reporter {
	iv := int64(interval / time.Second)
	if iv < 1 {
		iv = 10
	}
	return &Reporter{reg: reg, interval: iv, tags: tags, prev: map[string]float64{}}
}

// Collect snapshots the registry. Counters become counts of what happened
// since the previous Collect (the first call reports everything since
// start); gauges become gauges. Non-finite gauges are skipped — the wire has
// no way to say NaN.
//
// A bucket is reported at most once. Collect stamps its points at the start of
// the interval containing now, and callers may call it more than once inside
// one interval — the agent does, because shutdown flushes whatever is open. A
// second snapshot of the same bucket would carry only the delta since the
// first, and storage is last-write-wins, so it would *replace* a full
// interval's counts with a sliver. Losing the sliver is the lesser error: the
// numbers are then always "what happened in this interval", never a fraction
// of it presented as the whole.
func (r *Reporter) Collect(now time.Time) []wire.Series {
	ts := now.Unix() / r.interval * r.interval
	snap := r.reg.Snapshot()
	r.mu.Lock()
	defer r.mu.Unlock()
	if ts == r.lastTS {
		return nil
	}
	r.lastTS = ts
	out := make([]wire.Series, 0, len(snap))
	for _, p := range snap {
		tags := wire.CanonicalTags(append(append([]string{}, p.Tags...), r.tags...))
		s := wire.Series{Metric: p.Name, Tags: tags, Points: []wire.Point{{Timestamp: ts, Value: p.Value}}}
		switch p.Type {
		case TypeCounter:
			key := p.Name + "|" + strings.Join(tags, ",")
			delta := p.Value - r.prev[key]
			r.prev[key] = p.Value
			s.Type, s.Interval, s.Points[0].Value = wire.KindCount, r.interval, delta
		default:
			if math.IsNaN(p.Value) || math.IsInf(p.Value, 0) {
				continue
			}
			s.Type = wire.KindGauge
		}
		out = append(out, s)
	}
	return out
}
