package intake

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/queue"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// queuedEnv is an intake in queued mode over real topics, with the consumers
// not yet running: tests start them, so "durable but not stored" is observable.
type queuedEnv struct {
	*env
	topics map[string]*queue.Topic
	cons   map[string]*queue.Consumer
	logs   *fakeLogs
	hub    *fakeHub
	spans  *fakeSpans
	// pending is each consumer's oldest-uncommitted-record time, as the server reads it.
	pending map[string]*atomic.Int64
}

func newQueuedEnv(t *testing.T) *queuedEnv {
	t.Helper()
	q := &queuedEnv{topics: map[string]*queue.Topic{}, cons: map[string]*queue.Consumer{}, pending: map[string]*atomic.Int64{},
		logs: &fakeLogs{}, hub: &fakeHub{}, spans: &fakeSpans{}}
	prod := map[string]Producer{}
	for _, name := range Topics {
		tp, err := queue.Open(queue.Options{Dir: filepath.Join(t.TempDir(), name), SyncInterval: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tp.Close() })
		c, err := tp.Consumer(ConsumerName(name))
		if err != nil {
			t.Fatal(err)
		}
		q.topics[name], q.cons[name], prod[name] = tp, c, tp
		q.pending[name] = new(atomic.Int64)
	}
	q.env = newEnv(t, Options{Queue: prod, Logs: q.logs, LogHub: q.hub, Traces: q.spans})
	return q
}

func (q *queuedEnv) start(t *testing.T, topic string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = q.in.RunConsumer(ctx, topic, q.cons[topic], q.pending[topic])
	}()
	t.Cleanup(func() { cancel(); <-done })
}

// caughtUp waits (bounded) for a topic's consumer to commit everything durable.
func (q *queuedEnv) caughtUp(t *testing.T, topic string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st := q.topics[topic].Stats(); st.Next > 0 && st.Lag[ConsumerName(topic)] == 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%s consumer never caught up: %+v", topic, q.topics[topic].Stats())
}

func (q *queuedEnv) sampleCount(t *testing.T, metric string) int {
	t.Helper()
	set, err := q.store.Select(context.Background(), tsdb.Selector{Metric: metric}, 0, 1<<62)
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	n := 0
	for set.Next() {
		it := set.Iterator()
		for it.Next() {
			n++
		}
	}
	return n
}

func TestQueued_SeriesIsDurableBeforeItIsStored(t *testing.T) {
	q := newQueuedEnv(t)
	rec, out := q.post([]byte(payload(good)), false)
	if rec.Code != http.StatusAccepted || out["accepted"] != 1.0 {
		t.Fatalf("%d %v", rec.Code, out)
	}
	if n := q.sampleCount(t, "http.request.count"); n != 0 {
		t.Fatalf("%d samples stored before any consumer ran: a 202 must mean durable, not stored", n)
	}
	if got := q.topics[TopicSeries].Stats().Next; got != 1 {
		t.Fatalf("queue holds %d records, want 1", got)
	}
	q.start(t, TopicSeries)
	q.caughtUp(t, TopicSeries)
	if n := q.sampleCount(t, "http.request.count"); n != 2 {
		t.Fatalf("%d samples after the consumer, want 2", n)
	}
}

func TestQueued_ATypeConflictIsStillAnsweredAtIntake(t *testing.T) {
	q := newQueuedEnv(t)
	q.post([]byte(payload(good)), false)
	gauge := `{"metric":"http.request.count","type":"gauge","interval":10,"tags":[],"points":[[1790000000,1]]}`
	rec, out := q.post([]byte(payload(gauge)), false)
	if rec.Code != http.StatusAccepted || out["accepted"] != 0.0 || out["rejected"] != 1.0 {
		t.Fatalf("%d %v: the sender's own mistake must be reported while it is still listening", rec.Code, out)
	}
	if got := q.topics[TopicSeries].Stats().Next; got != 1 {
		t.Fatalf("a rejected series reached the queue: %d records", got)
	}
}

// A record applied twice leaves the stores as if applied once: the property
// that makes at-least-once delivery safe.
func TestQueued_RedeliveryIsInvisible(t *testing.T) {
	q := newQueuedEnv(t)
	payloadJSON := `[{"metric":"m.c","type":"count","interval":10,"tags":["a:b"],"points":[[1790000000,4],[1790000010,6]]}]`
	for i := 0; i < 3; i++ {
		if err := q.in.Apply(context.Background(), TopicSeries, []byte(payloadJSON)); err != nil {
			t.Fatal(err)
		}
	}
	if n := q.sampleCount(t, "m.c"); n != 2 {
		t.Fatalf("%d samples after three deliveries, want 2", n)
	}
}

func TestQueued_ARecordThatNeverDecodesIsSkippedNotRetried(t *testing.T) {
	q := newQueuedEnv(t)
	if _, err := q.topics[TopicSeries].Produce(context.Background(), []byte("not json")); err != nil {
		t.Fatal(err)
	}
	q.post([]byte(payload(good)), false)
	q.start(t, TopicSeries)
	q.caughtUp(t, TopicSeries)
	if n := q.sampleCount(t, "http.request.count"); n != 2 {
		t.Fatalf("the record behind the poison one was not applied: %d samples", n)
	}
	if v := q.reg.Counter("ozy.consumer.poison_records", "topic:series").Value(); v != 1 {
		t.Fatalf("poison_records = %v, want 1", v)
	}
}

func TestQueued_AStoreFailureIsRetriedNotSkipped(t *testing.T) {
	q := newQueuedEnv(t)
	q.logs.err = errors.New("disk full")
	rec, _ := q.postLogs(`{"logs":[`+logJSON("a")+`]}`, false)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%d", rec.Code)
	}
	q.start(t, TopicLogs)
	// Let it fail at least once, then heal the store and advance the retry timer.
	waitFor(t, func() bool { return q.clk.Waiters() > 0 })
	if q.pending[TopicLogs].Load() == 0 {
		t.Fatal("a consumer stuck on a record reported no pending record: lag_seconds would read 0 in an outage")
	}
	q.logs.mu.Lock()
	q.logs.err = nil
	q.logs.mu.Unlock()
	q.clk.Advance(retryMax)
	q.caughtUp(t, TopicLogs)
	q.logs.mu.Lock()
	defer q.logs.mu.Unlock()
	if len(q.logs.logs) != 1 || len(q.hub.got) != 1 {
		t.Fatalf("stored %d, published %d; want 1 each", len(q.logs.logs), len(q.hub.got))
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition never held")
}

type fullProducer struct{ err error }

func (f fullProducer) Produce(context.Context, []byte) (uint64, error) { return 0, f.err }

func TestQueued_AFullQueueIs429WithRetryAfter(t *testing.T) {
	prod := map[string]Producer{}
	for _, n := range Topics {
		prod[n] = fullProducer{queue.ErrFull}
	}
	e := newEnv(t, Options{Queue: prod, Logs: &fakeLogs{}, Traces: &fakeSpans{}})
	rec, _ := e.post([]byte(payload(good)), false)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "5" {
		t.Fatalf("%d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	for _, path := range []string{"/v1/logs", "/v1/traces", "/v1/sketches"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, req)
		if w.Code == http.StatusAccepted {
			t.Fatalf("%s answered 202 with a full queue", path)
		}
	}
}

func TestQueued_AQueueFailureIs503(t *testing.T) {
	prod := map[string]Producer{}
	for _, n := range Topics {
		prod[n] = fullProducer{errors.New("fsync failed")}
	}
	e := newEnv(t, Options{Queue: prod})
	rec, _ := e.post([]byte(payload(good)), false)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d, want 503: a failed fsync must never be acknowledged", rec.Code)
	}
}

func TestQueued_LogsAndSpansReachTheirStores(t *testing.T) {
	q := newQueuedEnv(t)
	q.postLogs(`{"logs":[`+logJSON("hello")+`]}`, false)
	q.start(t, TopicLogs)
	q.caughtUp(t, TopicLogs)
	q.logs.mu.Lock()
	got := len(q.logs.logs)
	q.logs.mu.Unlock()
	if got != 1 || len(q.hub.got) != 1 {
		t.Fatalf("stored %d, published %d", got, len(q.hub.got))
	}
	if v := q.reg.Counter("ozy.consumer.logs_stored").Value(); v != 1 {
		t.Fatalf("ozy.consumer.logs_stored = %v", v)
	}
	var _ wire.Log
}

// The retry delay doubles up to retryMax, so a store that stays down is not
// hammered at the first delay for ever.
func TestQueued_RetryBacksOff(t *testing.T) {
	q := newQueuedEnv(t)
	q.logs.err = errors.New("down")
	q.postLogs(`{"logs":[`+logJSON("a")+`]}`, false)
	q.start(t, TopicLogs)
	calls := func() int { q.logs.mu.Lock(); defer q.logs.mu.Unlock(); return q.logs.calls }
	waitFor(t, func() bool { return calls() == 1 && q.clk.Waiters() == 1 })
	q.clk.Advance(retryMin) // first wait is retryMin
	waitFor(t, func() bool { return calls() == 2 && q.clk.Waiters() == 1 })
	q.clk.Advance(retryMin) // the second is twice that: this must not fire it
	time.Sleep(50 * time.Millisecond)
	if got := calls(); got != 2 {
		t.Fatalf("%d attempts after half of the doubled delay, want 2", got)
	}
	q.clk.Advance(retryMin)
	waitFor(t, func() bool { return calls() == 3 })
}

// The queue round trip must not change what the log store receives: attrs keep
// numbers exact (wire.DecodeLogs uses UseNumber), so the consumer must too.
func TestQueued_LogAttrsKeepBigNumbersExact(t *testing.T) {
	q := newQueuedEnv(t)
	body := `{"logs":[{"ts":` + strconv.FormatInt(now.UnixMilli(), 10) +
		`,"message":"m","status":"info","service":"s","attrs":{"order_id":9007199254740993,"price":1.50}}]}`
	if rec, _ := q.postLogs(body, false); rec.Code != http.StatusAccepted {
		t.Fatalf("%d", rec.Code)
	}
	q.start(t, TopicLogs)
	q.caughtUp(t, TopicLogs)
	q.logs.mu.Lock()
	defer q.logs.mu.Unlock()
	if len(q.logs.logs) != 1 {
		t.Fatalf("%d logs", len(q.logs.logs))
	}
	a := q.logs.logs[0].Attrs
	if fmt.Sprint(a["order_id"]) != "9007199254740993" || fmt.Sprint(a["price"]) != "1.50" {
		t.Fatalf("attrs changed in the queue: %v", a)
	}
}

func TestQueued_AMissingTopicIs503NotAPanic(t *testing.T) {
	e := newEnv(t, Options{Queue: map[string]Producer{}})
	if rec, _ := e.post([]byte(payload(good)), false); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d, want 503", rec.Code)
	}
}
