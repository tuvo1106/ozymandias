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
// pending is set to the produce time of the oldest uncommitted record while a
// batch is in flight and to zero between batches; the server reads it for
// ozy.queue.lag_seconds.
//
// A store error is not the record's fault, so the consumer does not skip: it
// retries the same record with backoff and the topic backs up behind it. That
// is the right behaviour for a full disk or a stalled compaction (intake turns
// the backlog into 429s long before the queue cap), and the wrong one for a
// record the store will never accept — which is why a record that does not
// even decode is counted and committed past (errPoison), not retried.
func (in *Intake) RunConsumer(ctx context.Context, topic string, c *queue.Consumer, pending *atomic.Int64) error {
	poisoned := in.opts.Metrics.Counter("ozy.consumer.poison_records", "topic:"+topic)
	for {
		recs, err := c.Poll(ctx, consumeBatch)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, queue.ErrClosed) {
				return nil
			}
			return err
		}
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
		if err := c.Commit(recs[len(recs)-1].Offset); err != nil {
			if errors.Is(err, queue.ErrClosed) {
				return nil
			}
			return err
		}
		pending.Store(0)
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
