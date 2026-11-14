package auth

import (
	"sync"
	"time"

	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Limits are per-key ceilings. Zero is unlimited.
type Limits struct {
	RequestsPerSec float64 `json:"requests_per_sec"`
	BytesPerSec    float64 `json:"bytes_per_sec"`
	SpansPerSec    float64 `json:"spans_per_sec"`
	LogBytesPerDay int64   `json:"log_bytes_per_day"`
}

// withDefaults fills l's zero fields from d.
func (l Limits) withDefaults(d Limits) Limits {
	if l.RequestsPerSec == 0 {
		l.RequestsPerSec = d.RequestsPerSec
	}
	if l.BytesPerSec == 0 {
		l.BytesPerSec = d.BytesPerSec
	}
	if l.SpansPerSec == 0 {
		l.SpansPerSec = d.SpansPerSec
	}
	if l.LogBytesPerDay == 0 {
		l.LogBytesPerDay = d.LogBytesPerDay
	}
	return l
}

// bucket is a token bucket: capacity tokens, refilled at rate per second.
type bucket struct {
	mu       sync.Mutex
	rate     float64
	capacity float64
	tokens   float64
	at       time.Time
}

// newBucket returns a full bucket, or nil for rate 0 (unlimited). capacity is
// at least minCap: a bucket smaller than the largest single request could never
// admit it, however long it waited.
func newBucket(rate, minCap float64, now time.Time) *bucket {
	if rate <= 0 {
		return nil
	}
	c := max(rate, minCap)
	return &bucket{rate: rate, capacity: c, tokens: c, at: now}
}

// take removes n tokens if they are there. If not it removes none and says how
// long until they would be. A nil bucket always admits. n beyond capacity is
// clamped to it, so a request bigger than the burst waits for a full bucket
// instead of for ever.
func (b *bucket) take(n float64, now time.Time) (bool, time.Duration) {
	if b == nil {
		return true, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if el := now.Sub(b.at).Seconds(); el > 0 {
		b.tokens = min(b.capacity, b.tokens+el*b.rate)
		b.at = now
	}
	n = min(n, b.capacity)
	if b.tokens >= n {
		b.tokens -= n
		return true, 0
	}
	return false, time.Duration((n - b.tokens) / b.rate * float64(time.Second))
}

// give returns tokens a refused request did not use.
func (b *bucket) give(n float64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.tokens = min(b.capacity, b.tokens+n)
	b.mu.Unlock()
}

// dayCounter admits up to limit units per UTC day.
type dayCounter struct {
	mu    sync.Mutex
	limit int64
	day   string
	used  int64
}

func newDayCounter(limit int64) *dayCounter {
	if limit <= 0 {
		return nil
	}
	return &dayCounter{limit: limit}
}

// add counts n units, or refuses and says how long until the day turns over.
// A refused request counts nothing: the sender will resend it.
func (c *dayCounter) add(n int64, now time.Time) (bool, time.Duration) {
	if c == nil {
		return true, 0
	}
	now = now.UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	if d := now.Format("2006-01-02"); d != c.day {
		c.day, c.used = d, 0
	}
	if c.used+n > c.limit && c.used > 0 {
		// A single request bigger than the whole day's allowance is let through
		// once, on an empty day: refusing it for ever would wedge the sender.
		next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
		return false, next.Sub(now)
	}
	c.used += n
	return true, 0
}

// Reason values for ozy.intake.rejected. A fixed set, in code: nothing a client
// sends becomes a tag.
const (
	ReasonUnauthorized = "unauthorized"
	ReasonRequests     = "requests"
	ReasonBytes        = "bytes"
	ReasonSpans        = "spans"
	ReasonLogBytesDay  = "log_bytes_day"
)

// Reasons lists them, for registering the counters up front.
var Reasons = [...]string{ReasonUnauthorized, ReasonRequests, ReasonBytes, ReasonSpans, ReasonLogBytesDay}

// Principal is an authenticated key and its live limiters.
type Principal struct {
	ID   int64
	Name string

	clk      clock.Clock
	requests *bucket
	bytes    *bucket
	spans    *bucket
	logBytes *dayCounter
	rej      *Rejections // set per request by the middleware; nil counts nothing
}

func newPrincipal(id int64, name string, l Limits, clk clock.Clock) *Principal {
	now := clk.Now()
	return &Principal{
		ID: id, Name: name, clk: clk,
		requests: newBucket(l.RequestsPerSec, 1, now),
		bytes:    newBucket(l.BytesPerSec, wire.MaxBodyBytes, now),
		spans:    newBucket(l.SpansPerSec, wire.MaxSpansPerRequest, now),
		logBytes: newDayCounter(l.LogBytesPerDay),
	}
}

// Allow charges one request of size bytes. reason names the limit that refused it.
func (p *Principal) Allow(size int64) (ok bool, reason string, retry time.Duration) {
	now := p.clk.Now()
	if ok, w := p.requests.take(1, now); !ok {
		return false, ReasonRequests, w
	}
	if ok, w := p.bytes.take(float64(size), now); !ok {
		p.requests.give(1) // refused: the sender's retry should not pay twice
		return false, ReasonBytes, w
	}
	return true, "", 0
}

// AllowSpans charges n spans.
func (p *Principal) AllowSpans(n int) (bool, time.Duration) {
	if p == nil {
		return true, 0
	}
	ok, w := p.spans.take(float64(n), p.clk.Now())
	if !ok {
		p.rej.Inc(ReasonSpans)
	}
	return ok, w
}

// AllowLogBytes charges n bytes of logs against today's allowance.
func (p *Principal) AllowLogBytes(n int) (bool, time.Duration) {
	if p == nil {
		return true, 0
	}
	ok, w := p.logBytes.add(int64(n), p.clk.Now())
	if !ok {
		p.rej.Inc(ReasonLogBytesDay)
	}
	return ok, w
}
