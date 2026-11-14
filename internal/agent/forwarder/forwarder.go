package forwarder

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/spool"
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
	// Key is the ozyd API key, sent as X-Ozy-Key when not empty. It is never
	// written to the disk buffer: payloads are stored without headers, so a
	// rotated key applies to what is already spooled.
	Key string
	// MaxSeriesPerPayload and MaxPayloadBytes (uncompressed JSON) bound one
	// request. Defaults 5000 (the intake's limit) and 2 MiB.
	MaxSeriesPerPayload int
	MaxPayloadBytes     int
	// MaxQueueBytes bounds the compressed payloads held for retry. When a
	// new payload doesn't fit, the oldest are dropped. Default 64 MiB.
	MaxQueueBytes int
	// BufferDir, if set, is where payloads that do not fit in memory are
	// spilled, and where the ones still queued at shutdown are kept for the
	// next start: one spool per endpoint under it. Empty is memory only, the
	// pre-M7 behaviour. A buffer that cannot be opened is logged and the agent
	// carries on without it (ozy.agent.forwarder.spool_enabled reads 0).
	BufferDir string
	// MaxDiskBytes caps each endpoint's spool; past it the oldest segments are
	// dropped. Default 512 MiB.
	MaxDiskBytes int64
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
	series, sketches, spans endpoint
	// spools is the disk buffer by endpoint key, nil entries if disabled.
	// diskHead is each spool's oldest payload, loaded for sending: it carries
	// the retry state a record on disk cannot.
	spools   map[string]*spool.Spool
	diskHead map[string]*payload

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
	unauthorized                     *selfmetrics.Counter
	queueBytes                       *selfmetrics.Gauge
	spooled, spoolErrors             *selfmetrics.Counter
}

// payload is one gzip'd request body, the endpoint it belongs to, and its
// retry state.
type payload struct {
	ep       string // endpoint key: series, sketches or spans
	url      string
	body     []byte
	series   int
	attempts int
	notUntil time.Time // earliest next attempt
	// item is set for the payload at the head of a spool: it is delivered or
	// refused by acknowledging it there, not by leaving the memory queue.
	item *spool.Item
	// inflight is set while a request for it is running (the lock is not held
	// then). The memory cap must not move such a payload to disk: it would be
	// delivered and still be spooled, and sent again.
	inflight bool
}

// priority is the order endpoints are drained in after an outage: metrics are
// the cheapest and most valuable (M7 §3). Logs are not here: they never queue
// in the agent, the file they came from is their buffer.
var priority = [...]string{"series", "sketches", "spans"}

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
	f := &Forwarder{
		opts:     opts,
		series:   endpoint{url: opts.URL + "/v1/series", key: "series"},
		sketches: endpoint{url: opts.URL + "/v1/sketches", key: "sketches"},
		spans:    endpoint{url: opts.URL + "/v1/traces", key: "spans"},
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
		unauthorized:   reg.Counter("ozy.agent.forwarder.unauthorized"),
		queueBytes:     reg.Gauge("ozy.agent.forwarder.queue_bytes"),
		spooled:        reg.Counter("ozy.agent.forwarder.spooled"),
		spoolErrors:    reg.Counter("ozy.agent.forwarder.spool_errors"),
		spools:         map[string]*spool.Spool{},
		diskHead:       map[string]*payload{},
	}
	f.openSpools()
	return f
}

// openSpools opens one spool per endpoint under BufferDir, if configured.
func (f *Forwarder) openSpools() {
	reg := f.opts.Registry
	if f.opts.BufferDir != "" {
		for _, ep := range priority {
			sp, err := spool.Open(spool.Options{
				Dir: filepath.Join(f.opts.BufferDir, ep), MaxBytes: f.opts.MaxDiskBytes,
				Logger: f.opts.Logger.With("endpoint", ep),
			})
			if err != nil {
				// An unusable buffer must not stop an agent that otherwise
				// works; it must also not look like a working one.
				f.opts.Logger.Error("forwarder: disk buffer unavailable for this endpoint; it is memory only",
					"endpoint", ep, "dir", f.opts.BufferDir, "err", err)
				f.spoolErrors.Inc()
				continue
			}
			f.spools[ep] = sp
			tag := "endpoint:" + ep
			reg.GaugeFunc("ozy.agent.forwarder.spool_bytes", func() float64 { return float64(sp.Stats().Bytes) }, tag)
			reg.GaugeFunc("ozy.agent.forwarder.spool_payloads", func() float64 { return float64(sp.Stats().Payloads) }, tag)
		}
	}
	reg.GaugeFunc("ozy.agent.forwarder.spool_enabled", func() float64 {
		// 1 only while every endpoint has a working spool: an opened one that a
		// failed fsync has stopped is as good as none.
		if len(f.spools) != len(priority) {
			return 0
		}
		for _, sp := range f.spools {
			if sp.Broken() {
				return 0
			}
		}
		return 1
	})
}

// Submit encodes series into payloads and queues them. It never blocks on the
// network; with a disk buffer it can wait on a disk write, but only when the
// memory queue is full (see spillLocked). It is safe to call from the
// aggregator's flush loop.
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

// SubmitSpans queues kept spans for POST /v1/traces (§F). They ride the same
// queue as series, so an ozyd outage drops the oldest of everything together,
// and a span batch cannot starve the metrics of memory or the reverse.
func (f *Forwarder) SubmitSpans(spans []wire.Span) {
	payloads, err := encode(f, f.spans, spans, func(s *wire.Span) string { return s.Name })
	if err != nil {
		f.opts.Logger.Error("forwarder: encoding spans", "err", err)
		f.dropped.Add(int64(len(spans)))
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

// enqueueLocked appends p, first moving the oldest payloads out of memory
// until it fits: to the disk buffer if there is one, else dropped. Oldest
// first, so each endpoint's spool holds older data than its memory queue and
// delivery order stays oldest-first. Without a buffer it is drop-oldest rather
// than drop-newest: after an outage, the recent past is what someone looking
// at a dashboard wants to see.
func (f *Forwarder) enqueueLocked(p *payload) {
	f.shrinkLocked(len(p.body))
	if len(f.queue) == 0 && len(p.body) > f.opts.MaxQueueBytes {
		f.spillLocked(p) // alone it does not fit
		return
	}
	// Over the cap here means the oldest payload is in flight and could not be
	// moved. The queue is allowed to exceed its cap by that one payload and the
	// new one until the request ends (attempt shrinks again): moving anything
	// newer ahead of it onto the disk would put the disk's data after memory's,
	// and a series delivered out of order is refused by the TSDB.
	f.queue = append(f.queue, p)
	f.bytes += len(p.body)
}

// shrinkLocked moves the oldest payloads out of memory until room payload bytes
// more would fit. It stops at one with a request in flight: that one would be
// delivered and still be spooled, and sent again. Always the oldest, so every
// endpoint's disk data stays older than its memory data.
func (f *Forwarder) shrinkLocked(room int) {
	for len(f.queue) > 0 && f.bytes+room > f.opts.MaxQueueBytes && !f.queue[0].inflight {
		old := f.queue[0]
		f.queue = f.queue[1:]
		f.bytes -= len(old.body)
		f.spillLocked(old)
	}
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
		out = append(out, &payload{ep: ep.key, url: ep.url, body: body, series: len(parts)})
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

// next returns the payload to send now, or how long until one will be ready
// (0 if there is none). Endpoints are tried in priority order. Within one, the
// disk buffer's oldest payload goes first, and while it is backing off nothing
// newer of that endpoint overtakes it. Payloads still in memory are different:
// the first ready one goes, and one waiting out its backoff does not hold back
// the ones behind it.
//
// Only the send loop calls this, which is what lets it read the disk buffer
// without the lock: Submit waits on f.mu, and must not wait on a disk.
func (f *Forwarder) next() (*payload, time.Duration) {
	// One reading of the clock for the whole pass. A second, later one for the
	// final wait can land past soonest, make the wait zero, and leave the loop
	// asleep with nothing to wake it.
	now := f.opts.Clock.Now()
	var soonest time.Time
	later := func(t time.Time) {
		if soonest.IsZero() || t.Before(soonest) {
			soonest = t
		}
	}
	for _, ep := range priority {
		h, readFailed := f.loadDiskHead(ep)
		f.mu.Lock()
		if readFailed {
			// The spool could not be read just now. Not empty, so nothing newer
			// of this endpoint may overtake what is on disk; and not ready, so
			// ask again soon rather than waiting for the next Submit.
			later(now.Add(diskRetry))
			f.mu.Unlock()
			continue
		}
		if h != nil {
			ready := !h.notUntil.After(now)
			if ready {
				h.inflight = true
			} else {
				later(h.notUntil)
			}
			f.mu.Unlock()
			if ready {
				return h, 0
			}
			continue
		}
		for _, p := range f.queue {
			if p.ep != ep {
				continue
			}
			if !p.notUntil.After(now) {
				p.inflight = true
				f.mu.Unlock()
				return p, 0
			}
			later(p.notUntil)
		}
		f.mu.Unlock()
	}
	if soonest.IsZero() {
		return nil, 0
	}
	return nil, soonest.Sub(now)
}

// loadDiskHead returns ep's oldest spooled payload, reading it from disk the
// first time and keeping it, with its retry state, until it is settled. Called
// without f.mu.
//
// readFailed is true when the spool could not be read for a reason that is not
// damage; the endpoint is then neither empty nor ready.
func (f *Forwarder) loadDiskHead(ep string) (h *payload, readFailed bool) {
	f.mu.Lock()
	h, sp := f.diskHead[ep], f.spools[ep]
	f.mu.Unlock()
	if h != nil && h.item != nil && !sp.Holds(*h.item) {
		// The cap dropped the segment this came from; it is counted as lost
		// there, so sending it as well would count it twice.
		f.mu.Lock()
		delete(f.diskHead, ep)
		f.mu.Unlock()
		h = nil
	}
	if h != nil || sp == nil {
		return h, false
	}
	it, ok, skipped, err := sp.Peek()
	if skipped > 0 {
		f.dropped.Add(int64(skipped))
	}
	if err != nil {
		f.spoolErrors.Inc()
		return nil, true
	}
	if !ok {
		return nil, false
	}
	h = &payload{ep: ep, url: f.endpointURL(ep), body: it.Body, series: it.Series, item: &it}
	f.mu.Lock()
	f.diskHead[ep] = h
	f.mu.Unlock()
	return h, false
}

// diskRetry is how long the send loop waits before reading a spool again after
// a read that failed.
const diskRetry = time.Second

func (f *Forwarder) endpointURL(ep string) string {
	switch ep {
	case "series":
		return f.series.url
	case "sketches":
		return f.sketches.url
	}
	return f.spans.url
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
	var ack *spool.Item
	p.inflight = false
	switch res {
	case delivered:
		ack = f.settleLocked(p)
		f.sent.Inc()
	case giveUp:
		ack = f.settleLocked(p)
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
	f.shrinkLocked(0) // the request that held the oldest payload back is over
	f.queueBytes.Set(float64(f.bytes))
	f.mu.Unlock()
	if ack != nil {
		// Moving the cursor is disk work, so it is done after the lock is
		// released. The payload has already left diskHead, so the next call
		// to next would load the following one only after this returns: the
		// send loop is the only caller of both.
		f.spools[p.ep].Ack(*ack)
	}
}

// spillLocked moves p out of memory: to its endpoint's spool, or, with none, a
// stopped one, or when the write fails, to the dropped count. Writing the spool
// is a disk write and fsync under the queue lock; it happens only on overflow
// and at shutdown, where the alternative is losing the payload. It reports
// whether the payload is now on disk.
func (f *Forwarder) spillLocked(p *payload) bool {
	sp := f.spools[p.ep]
	if sp == nil || sp.Broken() {
		f.dropped.Add(int64(p.series))
		return false
	}
	lost, err := sp.Put(p.series, p.body)
	if err != nil {
		f.opts.Logger.Error("forwarder: writing the disk buffer", "endpoint", p.ep, "err", err)
		f.spoolErrors.Inc()
		f.dropped.Add(int64(p.series))
		return false
	}
	f.spooled.Inc()
	// lost is older data the spool dropped to make room, not this payload.
	f.dropped.Add(int64(lost))
	return true
}

// settleLocked takes a delivered or refused payload out of wherever it is.
// It returns the spool item to acknowledge, if the payload came from a spool.
func (f *Forwarder) settleLocked(p *payload) *spool.Item {
	if p.item != nil {
		if f.diskHead[p.ep] == p {
			delete(f.diskHead, p.ep)
		}
		return p.item
	}
	f.removeLocked(p)
	return nil
}

func (f *Forwarder) removeLocked(p *payload) {
	for i, q := range f.queue {
		if q == p {
			f.queue = append(f.queue[:i], f.queue[i+1:]...)
			f.bytes -= len(p.body)
			return
		}
	}
	// Not found: it is not in the memory queue (a disk payload, or already settled).
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
	if f.opts.Key != "" {
		req.Header.Set(wire.HeaderKey, f.opts.Key)
	}
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
	case code == http.StatusUnauthorized:
		// A missing, wrong or revoked key is the operator's to fix, and the data
		// is fine: keep it (the disk buffer holds it) and keep trying, slowly,
		// rather than dropping what was collected while the key was wrong.
		f.unauthorized.Inc()
		return retry, 0, fmt.Errorf("HTTP 401: ozyd refused the API key (intake.key_env): %s", bytes.TrimSpace(body))
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
// until ctx expires. What can't be sent in time is written to the disk buffer
// when there is one (and kept for the next start), and otherwise dropped and
// counted: an agent that won't exit is worse than one that loses its last few
// seconds.
func (f *Forwarder) Shutdown(ctx context.Context) error {
	f.cancel()
	<-f.done
	f.mu.Lock()
	pending := append([]*payload(nil), f.queue...)
	f.mu.Unlock()
	// The same order as the send loop: series first, so a short deadline is
	// spent on the most valuable signal. Stable, so each endpoint stays oldest
	// first.
	rank := func(ep string) int {
		for i, e := range priority {
			if e == ep {
				return i
			}
		}
		return len(priority)
	}
	sort.SliceStable(pending, func(i, j int) bool { return rank(pending[i].ep) < rank(pending[j].ep) })

	var lost int
	for _, p := range pending {
		// An endpoint with older payloads on disk is not sent from memory: that
		// would put newer data on the wire ahead of older. It goes to the disk
		// behind them instead.
		sp := f.spools[p.ep]
		behind := sp != nil && !sp.Broken() && sp.Stats().Payloads > 0
		if ctx.Err() == nil && !behind {
			f.attempt(ctx, p)
		}
		f.mu.Lock()
		if f.containsLocked(p) {
			// Failed (or never tried): no time left for backoff. With a disk
			// buffer it is kept for the next start rather than dropped.
			f.removeLocked(p)
			if !f.spillLocked(p) {
				lost += p.series
			}
		}
		f.queueBytes.Set(float64(f.bytes))
		f.mu.Unlock()
	}
	var closeErr error
	for _, sp := range f.spools {
		closeErr = errors.Join(closeErr, sp.Close())
	}
	if lost > 0 {
		return errors.Join(fmt.Errorf("forwarder: %d series not delivered before shutdown", lost), closeErr)
	}
	return closeErr
}

func (f *Forwarder) containsLocked(p *payload) bool {
	for _, q := range f.queue {
		if q == p {
			return true
		}
	}
	return false
}

// Queued returns the number of payloads waiting, in memory and in the disk
// buffer. The bytes are the memory queue's plus the buffer's size on disk, which
// includes records already delivered from a segment that has not been deleted
// yet, so it overstates what is waiting by up to a segment per endpoint.
func (f *Forwarder) Queued() (payloads, bytes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	payloads, bytes = len(f.queue), f.bytes
	for _, sp := range f.spools {
		st := sp.Stats()
		payloads += st.Payloads
		bytes += int(min(st.Bytes, int64(^uint(0)>>1)))
	}
	return payloads, bytes
}
