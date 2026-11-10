package intake

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/tuvo1106/ozymandias/internal/queue"
)

// Consumer pacing. A batch is the unit of commit: one offset-file fsync per
// batch, so a bigger batch amortizes the fsync and a smaller one bounds how
// much is redelivered after a crash. Redelivery is harmless (every store is
// idempotent), so the batch is sized for throughput.
const (
	consumeBatch   = 64
	retryMin       = 100 * time.Millisecond
	retryMax       = 5 * time.Second
	consumerPrefix = "store"
)

// ConsumerName is the queue consumer name for a topic's store.
func ConsumerName(topic string) string { return consumerPrefix + "-" + topic }

// RunConsumer drains one topic into its store until ctx ends: poll, apply
// each record, then commit the batch's last offset. It returns nil on a clean
// stop.
//
// The order is the whole point. Commit comes after apply, so a crash between
// them redelivers the batch (at-least-once) and idempotent stores make that
// invisible; commit-before-apply would turn the same crash into silent loss.
//
// pending is set to the produce time of each batch's first record; the server
// reads it for ozy.queue.lag_seconds, which is 0 when nothing is uncommitted.
//
// A store error is not the record's fault, so the consumer does not skip: it
// retries the same record with backoff and the topic backs up behind it. That
// is the right behaviour for a full disk or a stalled compaction (intake turns
// the backlog into 429s long before the queue cap), and the wrong one for a
// record the store will never accept — which is why a record that does not
// even decode is counted and committed past (errPoison), not retried.
func (in *Intake) RunConsumer(ctx context.Context, topic string, c *queue.Consumer, pending *atomic.Int64) error {
	poisoned := in.opts.Metrics.Counter("ozy.consumer.poison_records", "topic:"+topic)
	failures := in.opts.Metrics.Counter("ozy.consumer.queue_errors", "topic:"+topic)
	// pause waits out a queue-side failure (a Poll read error, a Commit fsync
	// error). Giving up would leave intake answering 202 for a store nobody
	// feeds, with every health signal green until the topic fills.
	delay := retryMin
	pause := func(op string, err error) bool {
		failures.Add(1)
		in.opts.Logger.Warn("consumer: queue "+op+" failed; retrying", "topic", topic, "err", err, "after", delay)
		t := in.opts.Clock.NewTimer(delay)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-t.C():
		}
		delay = min(delay*2, retryMax)
		return true
	}
	for {
		recs, err := c.Poll(ctx, consumeBatch)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, queue.ErrClosed) {
				return nil
			}
			// A broken topic never recovers until restart; polling it again is
			// harmless and keeps the failure counted.
			if !pause("poll", err) {
				return nil
			}
			continue
		}
		delay = retryMin
		pending.Store(recs[0].Time.UnixNano())
		for _, rec := range recs {
			if err := in.applyWithRetry(ctx, topic, rec.Payload); err != nil {
				if errors.Is(err, errPoison) {
					poisoned.Add(1)
					in.opts.Logger.Error("consumer: skipping an undecodable record",
						"topic", topic, "offset", rec.Offset, "err", err)
					continue
				}
				return nil // ctx ended mid-retry: nothing of this batch is committed
			}
		}
		for {
			err := c.Commit(recs[len(recs)-1].Offset)
			if err == nil {
				break
			}
			if errors.Is(err, queue.ErrClosed) || ctx.Err() != nil {
				return nil
			}
			if !pause("commit", err) {
				return nil
			}
		}
		delay = retryMin
		// pending is left as is: the lag_seconds gauge reads 0 on its own when
		// nothing is uncommitted, and between batches of a backlog the last
		// batch's age is the best available answer, where 0 would be wrong.
	}
}

// applyWithRetry retries a store failure until it succeeds or ctx ends.
// A nil result means applied; errPoison is returned as is; any other
// non-nil error means ctx ended.
func (in *Intake) applyWithRetry(ctx context.Context, topic string, payload []byte) error {
	delay := retryMin
	for {
		err := in.Apply(ctx, topic, payload)
		if err == nil || errors.Is(err, errPoison) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		in.opts.Logger.Warn("consumer: store failed; retrying", "topic", topic, "err", err, "after", delay)
		t := in.opts.Clock.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C():
		}
		delay = min(delay*2, retryMax)
	}
}
