package auth

import (
	"fmt"
	"hash/fnv"
	"sync"
	"time"
)

// activeWindow is how long a series counts against a key after the key last sent it.
// A series nobody has sent for this long is forgotten, so a key that rotated a tag
// (a deploy id, say) is not held to its old series for ever, and the limit measures
// what a key is sending now. An hour is long enough that a metric sent every minute
// is never forgotten, and short enough that a runaway tag recovers within one.
const activeWindow = time.Hour

// hardSeriesCap bounds the tracker's memory when a key has a per-metric limit and no
// total: entries are 8-byte hashes plus a metric name, so a million is tens of MB. A
// key with a per-metric limit and no total limit would otherwise be limited per metric
// and unlimited in how many metrics it names, which is the same bomb with another tag.
const hardSeriesCap = 1_000_000

// seriesTracker is the set of series one key has sent within activeWindow.
//
// Where the limit is decided. ADR-0059 expected series limits to act at the consumer,
// with the key travelling in the queued record. They act here instead, at intake, in
// front of the queue (ADR-0062): the sender hears about a refused series in both modes (a
// consumer's refusal is only a counter, and queued mode had already answered 202); the
// queue's record format does not change; and the tracker's memory is bounded by the very
// limit it enforces, since a series is admitted only while there is room.
//
// What it is not. It counts the distinct series this key sent recently, not what the store
// holds (two keys sending the same series each count it), and it is process memory:
// after a restart it starts empty and relearns from traffic, admitting series as they
// arrive, so the limit still holds from then on, but a key at its limit before a restart
// can be admitted a second set of series at most once, up to the limit, until the first set
// ages out of the store's own view. The store's global max_series_per_metric still applies.
type seriesTracker struct {
	perMetric int // 0: no per-metric limit
	total     int // never 0: the key's limit or hardSeriesCap

	mu        sync.Mutex
	seen      map[uint64]seriesEntry
	count     map[string]int // active series per metric
	nextSweep time.Time
}

type seriesEntry struct {
	metric string
	at     time.Time
}

func newSeriesTracker(perMetric, total int) *seriesTracker {
	if perMetric <= 0 && total <= 0 {
		return nil
	}
	if total <= 0 {
		total = hardSeriesCap
	}
	return &seriesTracker{perMetric: max(perMetric, 0), total: total, seen: map[uint64]seriesEntry{}, count: map[string]int{}}
}

func seriesHash(metric string, tags []string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(metric))
	for _, t := range tags {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(t))
	}
	return h.Sum64()
}

// admit says whether the key may send this series now, and refreshes it if it may. A
// series already active is always admitted: the limit is on how many there are, and a
// series that is already counted does not make there be more.
func (t *seriesTracker) admit(metric string, tags []string, now time.Time) (ok bool, reason, why string) {
	if t == nil {
		return true, "", ""
	}
	id := seriesHash(metric, tags)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked(now)
	if e, hit := t.seen[id]; hit {
		e.at = now
		t.seen[id] = e
		return true, "", ""
	}
	if t.perMetric > 0 && t.count[metric] >= t.perMetric {
		return false, ReasonSeriesPerMetric,
			fmt.Sprintf("this key already has %d active series for this metric, its limit; a tag is probably unbounded", t.perMetric)
	}
	if len(t.seen) >= t.total {
		return false, ReasonSeriesTotal, fmt.Sprintf("this key already has %d active series, its limit", t.total)
	}
	t.seen[id] = seriesEntry{metric: metric, at: now}
	t.count[metric]++
	return true, "", ""
}

// sweepLocked forgets series not sent within activeWindow. It runs at most once a
// quarter-window, and costs one pass over at most `total` entries.
func (t *seriesTracker) sweepLocked(now time.Time) {
	if now.Before(t.nextSweep) {
		return
	}
	t.nextSweep = now.Add(activeWindow / 4)
	for id, e := range t.seen {
		if now.Sub(e.at) >= activeWindow {
			delete(t.seen, id)
			if t.count[e.metric]--; t.count[e.metric] <= 0 {
				delete(t.count, e.metric)
			}
		}
	}
}

// AdmitSeries decides whether this key may send a series, counting a refusal under
// ozy.intake.rejected{reason}. why is the message for the sender. Safe on a nil
// Principal (auth off), which admits everything.
func (p *Principal) AdmitSeries(metric string, tags []string) (ok bool, why string) {
	if p == nil {
		return true, ""
	}
	ok, reason, why := p.series.admit(metric, tags, p.clk.Now())
	if !ok {
		p.rej.Inc(reason)
	}
	return ok, why
}
