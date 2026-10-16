package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/meta"
)

// Delivery defaults: three attempts, ten seconds each, with growing pauses
// between them (docs/plan/M6-monitors.md §2).
var (
	DefaultBackoff = []time.Duration{time.Second, 5 * time.Second}
	DefaultTimeout = 10 * time.Second
)

// queueLen bounds one notifier's in-memory queue. Overflow is not a loss: the
// row stays in the outbox, and the periodic rescan picks it up.
const queueLen = 256

// rescanInterval is how often the outbox table is re-read for rows that are
// in neither queue — dropped by a full queue, or left by a completion that
// failed to commit.
const rescanInterval = time.Minute

// Outbox is the part of internal/meta the dispatcher needs.
type Outbox interface {
	Outbox(ctx context.Context) ([]meta.OutboxRow, error)
	CompleteOutbox(ctx context.Context, id int64, outcome meta.EventRow) error
}

// Dispatcher delivers owed notifications from the outbox.
//
// # Why every notifier has its own worker
//
// Evaluation must never wait on delivery: a Discord outage is exactly when the
// other channels matter. So the scheduler only writes outbox rows and calls
// [Dispatcher.Enqueue], which never blocks. Each notifier then has its own
// queue and goroutine, so a webhook that takes ten seconds to time out three
// times delays its own messages by thirty seconds and nobody else's.
//
// # Delivery is at least once
//
// A row leaves the outbox only after its attempts are over, in the same
// transaction that records the outcome event. A crash between a successful
// POST and that transaction redelivers on restart. The alternative, deleting
// before sending, loses the notification instead, and a duplicate page is the
// cheaper failure of the two.
type Dispatcher struct {
	Notifiers map[string]Notifier
	Store     Outbox
	Clock     clock.Clock
	Logger    *slog.Logger
	Backoff   []time.Duration // pauses between attempts; len+1 attempts
	Timeout   time.Duration   // per attempt

	mu      sync.Mutex
	queues  map[string]chan meta.OutboxRow
	pending map[int64]bool // queued or in flight
	wg      sync.WaitGroup
	started bool
}

func (d *Dispatcher) defaults() {
	if d.Clock == nil {
		d.Clock = clock.Real()
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.Backoff == nil {
		d.Backoff = DefaultBackoff
	}
	if d.Timeout == 0 {
		d.Timeout = DefaultTimeout
	}
}

// Run starts one worker per notifier, delivers whatever the outbox already
// holds (a restart's leftovers), and rescans periodically until ctx ends. It
// returns once every worker has stopped.
func (d *Dispatcher) Run(ctx context.Context) {
	d.defaults()
	d.mu.Lock()
	d.queues = map[string]chan meta.OutboxRow{}
	d.pending = map[int64]bool{}
	for name, n := range d.Notifiers {
		q := make(chan meta.OutboxRow, queueLen)
		d.queues[name] = q
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.worker(ctx, n, q)
		}()
	}
	d.started = true
	d.mu.Unlock()

	d.rescan(ctx)
	t := d.Clock.NewTicker(rescanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			d.wg.Wait()
			return
		case <-t.C():
			d.rescan(ctx)
		}
	}
}

func (d *Dispatcher) rescan(ctx context.Context) {
	rows, err := d.Store.Outbox(ctx)
	if err != nil {
		if ctx.Err() == nil {
			d.Logger.Error("notify: reading outbox", "err", err)
		}
		return
	}
	d.Enqueue(ctx, rows)
}

// Enqueue hands committed outbox rows to their notifiers' queues. It never
// blocks: a full queue leaves the row for the next rescan. A row naming a
// notifier that is not configured (the config changed since it was written)
// is completed at once as failed, with that reason.
func (d *Dispatcher) Enqueue(ctx context.Context, rows []meta.OutboxRow) {
	var orphans []meta.OutboxRow
	d.mu.Lock()
	if !d.started {
		d.mu.Unlock()
		return // Run's first rescan will find them.
	}
	for _, r := range rows {
		if d.pending[r.ID] {
			continue
		}
		q, ok := d.queues[r.Notifier]
		if !ok {
			d.pending[r.ID] = true
			orphans = append(orphans, r)
			continue
		}
		select {
		case q <- r:
			d.pending[r.ID] = true
		default:
			d.Logger.Warn("notify: queue full; the outbox rescan will retry", "notifier", r.Notifier)
		}
	}
	d.mu.Unlock()
	// Outside the lock, because completing takes it to release the id.
	for _, r := range orphans {
		d.complete(ctx, r, &Permanent{fmt.Errorf("no notifier named %q is configured", r.Notifier)}, 0)
	}
}

func (d *Dispatcher) worker(ctx context.Context, n Notifier, q <-chan meta.OutboxRow) {
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-q:
			var note Notification
			err := json.Unmarshal(r.Payload, &note)
			attempts := 0
			if err != nil {
				err = &Permanent{fmt.Errorf("unreadable payload: %w", err)}
			} else {
				attempts, err = d.deliver(ctx, n, note)
			}
			if ctx.Err() != nil {
				// Shutting down mid-delivery: leave the row for the next
				// start rather than recording an outcome that is not one.
				// (Its pending mark dies with this run; Run starts afresh.)
				return
			}
			d.complete(ctx, r, err, attempts)
		}
	}
}

// deliver makes up to len(Backoff)+1 attempts and returns how many it made.
func (d *Dispatcher) deliver(ctx context.Context, n Notifier, note Notification) (int, error) {
	var err error
	for attempt := 0; ; attempt++ {
		actx, cancel := context.WithTimeout(ctx, d.Timeout)
		err = safeNotify(actx, n, note)
		cancel()
		if err == nil || IsPermanent(err) || attempt >= len(d.Backoff) || ctx.Err() != nil {
			return attempt + 1, err
		}
		t := d.Clock.NewTimer(d.Backoff[attempt])
		select {
		case <-ctx.Done():
			t.Stop()
			return attempt + 1, ctx.Err()
		case <-t.C():
		}
	}
}

// safeNotify turns a notifier's panic into an error: one broken notifier
// must not take the dispatcher, and with it every other channel, down.
func safeNotify(ctx context.Context, n Notifier, note Notification) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &Permanent{fmt.Errorf("notifier panicked: %v", r)}
		}
	}()
	return n.Notify(ctx, note)
}

func (d *Dispatcher) complete(ctx context.Context, r meta.OutboxRow, err error, attempts int) {
	ev := meta.EventRow{Ts: d.Clock.Now(), Kind: "notify_ok", MonitorID: r.MonitorID, GroupKey: r.GroupKey,
		Value: math.NaN(), Text: fmt.Sprintf("%s: delivered", r.Notifier)}
	if err != nil {
		ev.Kind = "notify_failed"
		ev.Text = fmt.Sprintf("%s: %v", r.Notifier, err)
		if attempts > 1 {
			ev.Text += fmt.Sprintf(" (after %d attempts)", attempts)
		}
		d.Logger.Warn("notify: delivery failed", "notifier", r.Notifier, "monitor_id", r.MonitorID,
			"group", r.GroupKey, "attempts", attempts, "err", err)
	}
	// A detached context: the outcome of a delivery that happened is worth
	// recording even as shutdown begins.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if cerr := d.Store.CompleteOutbox(cctx, r.ID, ev); cerr != nil {
		// The row stays, so it is delivered again: at least once.
		d.Logger.Error("notify: recording delivery", "notifier", r.Notifier, "err", cerr)
	}
	d.release(r.ID)
}

func (d *Dispatcher) release(id int64) {
	d.mu.Lock()
	delete(d.pending, id)
	d.mu.Unlock()
}
