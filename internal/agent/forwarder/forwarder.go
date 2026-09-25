package forwarder

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Options configures a Forwarder. Zero values get the documented defaults.
type Options struct {
	// URL is ozyd's base URL; series go to URL + "/v1/series" and
	// sketches to URL + "/v1/sketches".
	URL string
	// Client does the HTTP. Default: a client with Timeout.
	Client *http.Client
	// Timeout bounds one request. Default 10s.
	Timeout time.Duration
	// Hostname and Version go in the agent identity headers (§0).
	Hostname, Version string
	// MaxSeriesPerPayload and MaxPayloadBytes (uncompressed JSON) bound one
	// request. Defaults 5000 (the intake's limit) and 2 MiB.
	MaxSeriesPerPayload int
	MaxPayloadBytes     int
	// MaxQueueBytes bounds the compressed payloads held for retry. When a
	// new payload doesn't fit, the oldest are dropped. Default 64 MiB.
	MaxQueueBytes int
	// BackoffMin and BackoffMax bound the retry delay. Defaults 1s and 60s.
	BackoffMin, BackoffMax time.Duration
	Clock                  clock.Clock           // default clock.Real()
	Registry               *selfmetrics.Registry // default: a new registry
	Logger                 *slog.Logger          // default slog.Default()
	Rand                   *rand.Rand            // jitter; default randomly seeded
}

// Forwarder delivers series to ozyd at least once per payload, as long
// as the payload fits in memory: every payload is retried until it succeeds
// or is dropped by the memory cap. Delivery is still at-most-once end to end
// — statsd over UDP already lost what it lost — and the agent never blocks a
// caller waiting for the network.
type Forwarder struct {
	opts Options
	// The two intake hops. See [endpoint] on why they share a queue.
	series, sketches endpoint

	mu     sync.Mutex
	queue  []*payload // FIFO by enqueue order
	bytes  int
	wake   chan struct{}
	done   chan struct{}
	randMu sync.Mutex
	// ctx is cancelled by Shutdown; it bounds the loop and its in-flight
	// request, so shutdown never waits out a full request timeout.
	ctx    context.Context
	cancel context.CancelFunc

	sent, dropped, rejected, retries *selfmetrics.Counter
	seriesSent, seriesRejected       *selfmetrics.Counter
	queueBytes                       *selfmetrics.Gauge
}

// payload is one gzip'd request body, the endpoint it belongs to, and its
// retry state.
type payload struct {
	url      string
	body     []byte
	series   int
	attempts int
	notUntil time.Time // earliest next attempt
}

// endpoint is one intake hop: where it posts, and what the JSON envelope
// around its items is called.
//
// Both hops share one queue, one memory budget and one backoff. Two
// forwarders would give sketches their own retry schedule, which sounds
// tidier until ozyd is down: the two would drop different windows of data
// and a dashboard would show a p95 for a minute its own count is missing.
type endpoint struct{ url, key string }

// New returns a Forwarder. Call Start to begin sending.
func New(opts Options) *Forwarder {
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: opts.Timeout}
	}
	if opts.MaxSeriesPerPayload <= 0 {
		opts.MaxSeriesPerPayload = wire.MaxSeriesPerRequest
	}
	if opts.MaxPayloadBytes <= 0 {
		opts.MaxPayloadBytes = 2 << 20
	}
	if opts.MaxQueueBytes <= 0 {
		opts.MaxQueueBytes = 64 << 20
	}
	if opts.BackoffMin <= 0 {
		opts.BackoffMin = time.Second
	}
	if opts.BackoffMax <= 0 {
		opts.BackoffMax = 60 * time.Second
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
	if opts.Rand == nil {
		opts.Rand = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())) //nolint:gosec // jitter, not security
	}
	reg := opts.Registry
	ctx, cancel := context.WithCancel(context.Background())
	return &Forwarder{
		opts:     opts,
		series:   endpoint{url: opts.URL + "/v1/series", key: "series"},
		sketches: endpoint{url: opts.URL + "/v1/sketches", key: "sketches"},
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
		ctx:      ctx,
		cancel:   cancel,

		sent:           reg.Counter("ozy.agent.forwarder.payloads_sent"),
		dropped:        reg.Counter("ozy.agent.forwarder.dropped"),
		rejected:       reg.Counter("ozy.agent.forwarder.payloads_rejected"),
		retries:        reg.Counter("ozy.agent.forwarder.retries"),
		seriesSent:     reg.Counter("ozy.agent.forwarder.series_sent"),
		seriesRejected: reg.Counter("ozy.agent.forwarder.series_rejected"),
		queueBytes:     reg.Gauge("ozy.agent.forwarder.queue_bytes"),
	}
}

// Submit encodes series into payloads and queues them. It never blocks on
// the network, so it is safe to call from the aggregator's flush loop.
func (f *Forwarder) Submit(series []wire.Series) {
	payloads, err := encode(f, f.series, series, func(s *wire.Series) string { return s.Metric })
	if err != nil {
		// encode skips a series it cannot marshal, so what is left here is a
		// gzip failure — which means the process is out of memory, not that
		// the data was bad. Losing the batch is loud, not silent.
		f.opts.Logger.Error("forwarder: encoding series", "err", err)
		f.dropped.Add(int64(len(series)))
		return
	}
	f.enqueue(payloads)
}

// SubmitSketches queues a flush of distributions for POST /v1/sketches.
func (f *Forwarder) SubmitSketches(sketches []wire.SketchSeries) {
	payloads, err := encode(f, f.sketches, sketches, func(s *wire.SketchSeries) string { return s.Metric })
	if err != nil {
		f.opts.Logger.Error("forwarder: encoding sketches", "err", err)
		f.dropped.Add(int64(len(sketches)))
		return
	}
	f.enqueue(payloads)
}

func (f *Forwarder) enqueue(payloads []*payload) {
	f.mu.Lock()
	for _, p := range payloads {
		f.enqueueLocked(p)
	}
	f.queueBytes.Set(float64(f.bytes))
	f.mu.Unlock()
	f.signal()
}

// enqueueLocked appends p, first dropping the oldest payloads until it fits.
// Drop-oldest rather than drop-newest: after an outage, the recent past is
// what someone looking at a dashboard wants to see.
func (f *Forwarder) enqueueLocked(p *payload) {
	for len(f.queue) > 0 && f.bytes+len(p.body) > f.opts.MaxQueueBytes {
		old := f.queue[0]
		f.queue = f.queue[1:]
		f.bytes -= len(old.body)
		f.dropped.Add(int64(old.series))
	}
	if len(p.body) > f.opts.MaxQueueBytes {
		f.dropped.Add(int64(p.series))
		return
	}
	f.queue = append(f.queue, p)
	f.bytes += len(p.body)
}

// encode splits items into payloads of at most MaxSeriesPerPayload items and
// MaxPayloadBytes of JSON, and gzips each. Every item is marshalled on its
// own so the size check is exact; the body is then just the pieces joined
// inside the endpoint's envelope.
//
// A free function rather than a method because Go methods cannot take type
// parameters, and the alternative — two near-identical copies of the
// splitting logic — is how the series path and the sketch path would come to
// disagree about a limit.
func encode[T any](f *Forwarder, ep endpoint, items []T, name func(*T) string) ([]*payload, error) {
	var out []*payload
	prefix, suffix := `{"`+ep.key+`":[`, `]}`
	envelope := len(prefix) + len(suffix)
	var parts [][]byte
	size := envelope
	flush := func() error {
		if len(parts) == 0 {
			return nil
		}
		var raw bytes.Buffer
		raw.WriteString(prefix)
		raw.Write(bytes.Join(parts, []byte(",")))
		raw.WriteString(suffix)
		body, err := gzipBytes(raw.Bytes())
		if err != nil {
			return err
		}
		out = append(out, &payload{url: ep.url, body: body, series: len(parts)})
		parts, size = nil, envelope
		return nil
	}
	for i := range items {
		b, err := json.Marshal(&items[i])
		if err != nil {
			// The only per-item failure here is a value the encoder refuses
			// to write, i.e. a non-finite one. Skip that item and keep the
			// rest: failing the batch would discard every other series in
			// the flush — including the agent's own self-metrics — for as
			// long as whatever produced the bad value keeps producing it.
			f.opts.Logger.Error("forwarder: skipping unencodable series", "metric", name(&items[i]), "err", err)
			f.dropped.Add(1)
			continue
		}
		if len(parts) > 0 && (len(parts) == f.opts.MaxSeriesPerPayload || size+1+len(b) > f.opts.MaxPayloadBytes) {
			if err := flush(); err != nil {
				return nil, err
			}
		}
		parts = append(parts, b)
		size += len(b) + 1
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}

func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (f *Forwarder) signal() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// Start runs the send loop in a goroutine owned by the Forwarder until
// Shutdown.
func (f *Forwarder) Start() { go f.loop() }

// loop sends the oldest ready payload, one request at a time. One in-flight
// request is plenty at this scale and keeps ordering simple; a payload
// waiting out its backoff doesn't hold back the ones behind it.
func (f *Forwarder) loop() {
	defer close(f.done)
	for {
		p, wait := f.next()
		if p != nil {
			ctx, cancel := context.WithTimeout(f.ctx, f.opts.Timeout)
			f.attempt(ctx, p)
			cancel()
			if f.ctx.Err() != nil {
				return
			}
			continue
		}
		var timer clock.Timer
		var fire <-chan time.Time
		if wait > 0 {
			timer = f.opts.Clock.NewTimer(wait)
			fire = timer.C()
		}
		select {
		case <-f.ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-f.wake:
		case <-fire:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// next returns the oldest payload whose backoff has elapsed, or how long
// until one will be ready (0 if the queue is empty).
func (f *Forwarder) next() (*payload, time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.opts.Clock.Now()
	var soonest time.Time
	for _, p := range f.queue {
		if !p.notUntil.After(now) {
			return p, 0
		}
		if soonest.IsZero() || p.notUntil.Before(soonest) {
			soonest = p.notUntil
		}
	}
	if soonest.IsZero() {
		return nil, 0
	}
	return nil, soonest.Sub(now)
}

// outcome of one delivery attempt.
type outcome int

const (
	delivered outcome = iota
	retry
	giveUp
)

// attempt sends p once and settles it: removed on success or a permanent
// failure, rescheduled otherwise.
func (f *Forwarder) attempt(ctx context.Context, p *payload) {
	res, retryAfter, err := f.post(ctx, p)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch res {
	case delivered:
		f.removeLocked(p)
		f.sent.Inc()
	case giveUp:
		f.removeLocked(p)
		f.rejected.Inc()
		f.dropped.Add(int64(p.series))
		f.opts.Logger.Error("forwarder: payload refused, not retrying", "err", err, "series", p.series)
	case retry:
		p.attempts++
		f.retries.Inc()
		delay := max(f.backoff(p.attempts), retryAfter)
		p.notUntil = f.opts.Clock.Now().Add(delay)
		f.opts.Logger.Warn("forwarder: send failed, will retry", "err", err, "attempt", p.attempts, "in", delay)
	}
	f.queueBytes.Set(float64(f.bytes))
}

func (f *Forwarder) removeLocked(p *payload) {
	for i, q := range f.queue {
		if q == p {
			f.queue = append(f.queue[:i], f.queue[i+1:]...)
			f.bytes -= len(p.body)
			return
		}
	}
	// Not found: dropped by the memory cap while in flight. Already counted.
}

// backoff is exponential with full jitter: a uniformly random delay in
// [0, min(max, min·2^(attempt-1))]. Full jitter — rather than a fixed
// exponential — keeps many agents that failed together from retrying
// together and knocking a recovering server over again.
func (f *Forwarder) backoff(attempt int) time.Duration {
	ceiling := f.opts.BackoffMin << min(attempt-1, 30)
	if ceiling <= 0 || ceiling > f.opts.BackoffMax {
		ceiling = f.opts.BackoffMax
	}
	f.randMu.Lock()
	defer f.randMu.Unlock()
	return time.Duration(f.opts.Rand.Int64N(int64(ceiling) + 1))
}

// post makes one request and classifies the result per the retry contract
// (§0): retry on a network error, 408, 429 and 5xx; give up on any other
// 4xx, since sending the same bytes again will get the same answer.
func (f *Forwarder) post(ctx context.Context, p *payload) (outcome, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(p.body))
	if err != nil {
		return giveUp, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set(wire.HeaderAgentVersion, f.opts.Version)
	req.Header.Set(wire.HeaderHost, f.opts.Hostname)
	resp, err := f.opts.Client.Do(req)
	if err != nil {
		return retry, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	switch code := resp.StatusCode; {
	case code >= 200 && code < 300:
		var r wire.IntakeResponse
		if json.Unmarshal(body, &r) == nil {
			f.seriesSent.Add(int64(r.Accepted))
			f.seriesRejected.Add(int64(r.Rejected))
			if r.Rejected > 0 {
				f.opts.Logger.Warn("forwarder: intake rejected series", "rejected", r.Rejected, "errors", r.Errors)
			}
		}
		return delivered, 0, nil
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500:
		return retry, parseRetryAfter(resp.Header.Get("Retry-After")), fmt.Errorf("HTTP %d: %s", code, bytes.TrimSpace(body))
	default:
		return giveUp, 0, fmt.Errorf("HTTP %d: %s", code, bytes.TrimSpace(body))
	}
}

// parseRetryAfter reads the delay-seconds form. (The HTTP-date form isn't
// sent by ozyd, so it isn't worth the parsing here.)
func parseRetryAfter(v string) time.Duration {
	s, err := strconv.Atoi(v)
	if err != nil || s < 0 {
		return 0
	}
	return time.Duration(s) * time.Second
}

// Shutdown stops the send loop — cancelling any request in flight — and
// makes one final attempt at everything still queued, ignoring backoff,
// until ctx expires. What can't be sent in time is dropped and counted: an
// agent that won't exit is worse than one that loses its last few seconds.
func (f *Forwarder) Shutdown(ctx context.Context) error {
	f.cancel()
	<-f.done
	f.mu.Lock()
	pending := append([]*payload(nil), f.queue...)
	f.mu.Unlock()

	var lost int
	for _, p := range pending {
		if ctx.Err() == nil {
			f.attempt(ctx, p)
		}
		f.mu.Lock()
		if f.containsLocked(p) {
			// Failed (or never tried): no time left for backoff.
			f.removeLocked(p)
			f.dropped.Add(int64(p.series))
			lost += p.series
		}
		f.queueBytes.Set(float64(f.bytes))
		f.mu.Unlock()
	}
	if lost > 0 {
		return fmt.Errorf("forwarder: %d series not delivered before shutdown", lost)
	}
	return nil
}

func (f *Forwarder) containsLocked(p *payload) bool {
	for _, q := range f.queue {
		if q == p {
			return true
		}
	}
	return false
}

// Queued returns the number of payloads waiting and their compressed bytes.
func (f *Forwarder) Queued() (payloads, bytes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queue), f.bytes
}
