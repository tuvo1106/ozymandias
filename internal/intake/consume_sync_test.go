package intake

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// probeSyncer stands in for a store that left its fsync out of Append. It reports what the
// world looked like at the moment it was asked to sync, which is what the commit protocol is
// about: by then the whole batch must be applied, and none of it committed.
type probeSyncer struct {
	mu     sync.Mutex
	calls  int
	err    error
	onSync func(call int)
}

func (p *probeSyncer) Sync() error {
	p.mu.Lock()
	p.calls++
	n, fn, err := p.calls, p.onSync, p.err
	p.mu.Unlock()
	if fn != nil {
		fn(n)
	}
	return err
}

func (p *probeSyncer) set(err error) { p.mu.Lock(); p.err = err; p.mu.Unlock() }
func (p *probeSyncer) count() int    { p.mu.Lock(); defer p.mu.Unlock(); return p.calls }

func (q *queuedEnv) appliedLogs() int {
	q.logs.mu.Lock()
	defer q.logs.mu.Unlock()
	return len(q.logs.logs)
}

func (q *queuedEnv) lag(topic string) uint64 {
	return q.topics[topic].Stats().Lag[ConsumerName(topic)]
}

func (q *queuedEnv) postLogLines(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if rec, _ := q.postLogs(`{"logs":[`+logJSON(fmt.Sprint("line ", i))+`]}`, false); rec.Code != 202 {
			t.Fatalf("post %d: %d", i, rec.Code)
		}
	}
}

func withSync(topic string, s ...Syncer) func(*Options) {
	return func(o *Options) { o.CommitSync = map[string][]Syncer{topic: s} }
}

// One fsync covers the batch: not one per record, and not before the last record is applied.
// It happens while nothing is committed, so a power cut at that moment redelivers the batch
// rather than losing it.
func TestConsumerSync_OnceForTheWholeBatchAfterItIsAppliedAndBeforeItIsCommitted(t *testing.T) {
	probe := &probeSyncer{}
	q := newQueuedEnv(t, withSync(TopicLogs, probe))
	var appliedAtSync int
	var lagAtSync uint64
	probe.onSync = func(call int) {
		if call == 1 {
			appliedAtSync, lagAtSync = q.appliedLogs(), q.lag(TopicLogs)
		}
	}
	q.postLogLines(t, 10)
	q.start(t, TopicLogs)
	q.caughtUp(t, TopicLogs)
	if got := probe.count(); got != 1 {
		t.Fatalf("%d syncs for one batch of 10 records, want 1", got)
	}
	if appliedAtSync != 10 {
		t.Fatalf("the sync came after %d of 10 records were applied: it must follow the whole batch", appliedAtSync)
	}
	if lagAtSync != 10 {
		t.Fatalf("lag was %d at the sync, want 10: the offset was committed before the data was durable", lagAtSync)
	}
	if v := q.reg.Counter("ozy.consumer.syncs", "topic:logs").Value(); v != 1 {
		t.Fatalf("ozy.consumer.syncs = %v, want 1", v)
	}
}

// A failed fsync can leave the kernel having dropped the pages it could not write, so a second
// fsync may report success for data that is gone. The batch is written again before it is synced
// again, and nothing is committed in between.
func TestConsumerSync_AFailedSyncCommitsNothingAndAppliesTheBatchAgain(t *testing.T) {
	probe := &probeSyncer{err: errors.New("EIO")}
	q := newQueuedEnv(t, withSync(TopicLogs, probe))
	q.postLogLines(t, 5)
	q.start(t, TopicLogs)
	waitFor(t, func() bool { return q.clk.Waiters() > 0 }) // paused in its backoff after the failed sync
	if lag := q.lag(TopicLogs); lag != 5 {
		t.Fatalf("lag %d after a failed sync, want 5: the batch was committed without being durable", lag)
	}
	if q.pending[TopicLogs].Load() == 0 {
		t.Fatal("a consumer stuck on a failing sync reported no pending record: lag_seconds would read 0 in an outage")
	}
	probe.set(nil)
	q.clk.Advance(retryMax)
	q.caughtUp(t, TopicLogs)
	if got := probe.count(); got != 2 {
		t.Fatalf("%d sync calls, want 2 (the failure and the retry)", got)
	}
	if got := q.appliedLogs(); got != 10 {
		t.Fatalf("%d appends, want 10: the batch must be written again after a failed sync, not just synced again", got)
	}
	if v := q.reg.Counter("ozy.consumer.sync_errors", "topic:logs").Value(); v != 1 {
		t.Fatalf("ozy.consumer.sync_errors = %v, want 1", v)
	}
	if v := q.reg.Counter("ozy.consumer.queue_errors", "topic:logs").Value(); v != 0 {
		t.Fatalf("a store's failed sync was counted as a queue error: %v", v)
	}
}

// The undecodable record behind a failed sync is counted once, not once per attempt.
func TestConsumerSync_APoisonRecordIsCountedOnceAcrossAReappliedBatch(t *testing.T) {
	probe := &probeSyncer{err: errors.New("EIO")}
	q := newQueuedEnv(t, withSync(TopicLogs, probe))
	if _, err := q.topics[TopicLogs].Produce(t.Context(), []byte("not json")); err != nil {
		t.Fatal(err)
	}
	q.postLogLines(t, 1)
	q.start(t, TopicLogs)
	waitFor(t, func() bool { return q.clk.Waiters() > 0 })
	probe.set(nil)
	q.clk.Advance(retryMax)
	q.caughtUp(t, TopicLogs)
	if v := q.reg.Counter("ozy.consumer.poison_records", "topic:logs").Value(); v != 1 {
		t.Fatalf("poison_records = %v after one bad record and one re-applied batch, want 1", v)
	}
}

// Each topic syncs its own stores and no others, and a topic with none commits as before.
func TestConsumerSync_EachTopicSyncsOnlyItsOwnStores(t *testing.T) {
	logsSync, spansSync := &probeSyncer{}, &probeSyncer{}
	q := newQueuedEnv(t, func(o *Options) {
		o.CommitSync = map[string][]Syncer{TopicLogs: {logsSync}, TopicTraces: {spansSync}}
	})
	q.postLogLines(t, 3)
	if rec, _ := q.postSpans(`{"spans":[` + spanBody(1, "") + `]}`); rec.Code != 202 {
		t.Fatalf("spans: %d", rec.Code)
	}
	q.post([]byte(payload(good)), false)
	for _, topic := range []string{TopicLogs, TopicTraces, TopicSeries} {
		q.start(t, topic)
		q.caughtUp(t, topic)
	}
	if logsSync.count() != 1 || spansSync.count() != 1 {
		t.Fatalf("logs store synced %d times, traces store %d; want 1 each", logsSync.count(), spansSync.count())
	}
	if n := q.sampleCount(t, "http.request.count"); n != 2 {
		t.Fatalf("the series topic, which has nothing to sync, stored %d samples, want 2", n)
	}
}

// A topic with two stores (sketches write the sketch store and the TSDB) syncs both, even when
// the first fails: a store that can flush is not left unflushed behind one that cannot.
func TestSyncAll_RunsEveryStoreAndReportsEveryFailure(t *testing.T) {
	a, b, c := &probeSyncer{err: errors.New("a failed")}, &probeSyncer{}, &probeSyncer{err: errors.New("c failed")}
	err := syncAll([]Syncer{a, b, c})
	if a.count() != 1 || b.count() != 1 || c.count() != 1 {
		t.Fatalf("calls %d %d %d, want one each", a.count(), b.count(), c.count())
	}
	if err == nil || !errors.Is(err, a.err) || !errors.Is(err, c.err) {
		t.Fatalf("err = %v, want both failures", err)
	}
	if err := syncAll(nil); err != nil {
		t.Fatalf("nothing to sync: %v", err)
	}
}

// The stores of a topic are synced at once, not one after another: each Sync here waits until
// all of them have started, so a serial syncAll never gets past the first.
func TestSyncAll_SyncsTheStoresConcurrently(t *testing.T) {
	const n = 3
	var started sync.WaitGroup
	started.Add(n)
	gate := make(chan struct{})
	go func() { started.Wait(); close(gate) }()
	var syncers []Syncer
	for i := 0; i < n; i++ {
		syncers = append(syncers, &probeSyncer{onSync: func(int) {
			started.Done()
			select {
			case <-gate:
			case <-time.After(5 * time.Second):
			}
		}})
	}
	done := make(chan error, 1)
	go func() { done <- syncAll(syncers) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("syncAll never finished")
	}
	select {
	case <-gate:
	default:
		t.Fatal("the stores were synced one after another: the second never started while the first ran")
	}
}
