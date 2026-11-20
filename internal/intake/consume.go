package intake

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tuvo1106/ozymandias/internal/queue"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
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

// Syncer is a store that can leave its fsync out of Append: Append returns once the data is
// written, and Sync makes everything appended before the call durable. The consumer syncs once
// per batch instead of once per record.
type Syncer interface{ Sync() error }

// ConsumerName is the queue consumer name for a topic's store.
func ConsumerName(topic string) string { return consumerPrefix + "-" + topic }

// RunConsumer drains one topic into its store until ctx ends: poll, apply
// each record, sync the stores that deferred their fsync, then commit the
// batch's last offset. It returns nil on a clean stop.
//
// The order is the whole point. Commit comes after apply, so a crash between
// them redelivers the batch (at-least-once) and idempotent stores make that
// invisible; commit-before-apply would turn the same crash into silent loss.
// The same goes for the sync: a store that left its fsync out of Append has
// only written the batch into the operating system's cache, so committing the
// offset before Sync would let a power cut take records the queue has already
// let go of. Sync comes after the whole batch, so one fsync covers up to
// consumeBatch records (ADR-0063).
//
// A failed Sync is retried with the whole batch applied again first. After a
// failed fsync the kernel may have dropped the dirty pages it could not write, so
// a second fsync can succeed for data that is no longer there; rewriting the
// batch (every store absorbs a repeat, within the limits ADR-0063 notes) is how a
// store that is not fail-stop recovers. The WALs ozyd writes are fail-stop (wal.Options.StopOnSyncError): after
// a failed fsync they refuse everything until restart, the retry fails, the topic
// backs up, and the restart redelivers the uncommitted batch from the queue.
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
	// Registered only for a topic that has something to sync: in direct-sync mode a
	// counter that can never move would read as a zero that means health.
	syncers := in.opts.CommitSync[topic]
	var syncs, syncFailures *selfmetrics.Counter
	if len(syncers) > 0 {
		syncs = in.opts.Metrics.Counter("ozy.consumer.syncs", "topic:"+topic)
		syncFailures = in.opts.Metrics.Counter("ozy.consumer.sync_errors", "topic:"+topic)
	}
	// pause waits out a failure of the queue (a Poll read error, a Commit fsync
	// error) or of a store's sync, and counts it. Giving up would leave intake
	// answering 202 for a store nobody feeds, with every health signal green
	// until the topic fills.
	delay := retryMin
	pause := func(op string, err error, count *selfmetrics.Counter) bool {
		count.Add(1)
		in.opts.Logger.Warn("consumer: "+op+" failed; retrying", "topic", topic, "err", err, "after", delay)
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
			if !pause("queue poll", err, failures) {
				return nil
			}
			continue
		}
		delay = retryMin
		pending.Store(recs[0].Time.UnixNano())
		for {
			skipped, ok := in.applyBatch(ctx, topic, recs)
			if !ok {
				return nil // ctx ended mid-retry: nothing of this batch is committed
			}
			if len(syncers) == 0 {
				poisoned.Add(skipped)
				break
			}
			err := syncAll(syncers)
			if err == nil {
				syncs.Add(1)
				poisoned.Add(skipped)
				break
			}
			if !pause("store sync", err, syncFailures) {
				return nil
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
			if !pause("queue commit", err, failures) {
				return nil
			}
		}
		delay = retryMin
		// pending is left as is: the lag_seconds gauge reads 0 on its own when
		// nothing is uncommitted, and between batches of a backlog the last
		// batch's age is the best available answer, where 0 would be wrong.
	}
}

// applyBatch applies a polled batch record by record. It returns how many records were
// poison (counted by the caller once the batch is committed to, so a re-applied batch does not
// count them twice), and false if ctx ended before the batch was applied.
func (in *Intake) applyBatch(ctx context.Context, topic string, recs []queue.Record) (poison int64, ok bool) {
	for _, rec := range recs {
		if err := in.applyWithRetry(ctx, topic, rec.Payload); err != nil {
			if errors.Is(err, errPoison) {
				poison++
				in.opts.Logger.Error("consumer: skipping an undecodable record",
					"topic", topic, "offset", rec.Offset, "err", err)
				continue
			}
			return poison, false
		}
	}
	return poison, true
}

// syncAll syncs every store of a topic at once, and all of them even when one fails, so a store
// that can flush is not left unflushed behind one that cannot. The stores are independent files, so
// a topic that writes two (sketches) pays one fsync's latency, not two.
func syncAll(syncers []Syncer) error {
	if len(syncers) == 1 {
		return syncers[0].Sync()
	}
	errs := make([]error, len(syncers))
	var wg sync.WaitGroup
	for i, s := range syncers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.Sync()
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
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
