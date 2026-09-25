package statsd

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

type recorder struct {
	mu    sync.Mutex
	lines []string
	n     atomic.Int64
}

func (r *recorder) sink(m *Message, _ time.Time) {
	r.n.Add(1)
	r.mu.Lock()
	r.lines = append(r.lines, string(AppendMessage(nil, *m)))
	r.mu.Unlock()
}

func startServer(t *testing.T, opts Options) (*Server, *selfmetrics.Registry, func()) {
	t.Helper()
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:0"
	}
	if opts.Registry == nil {
		opts.Registry = selfmetrics.NewRegistry()
	}
	opts.Logger = slog.New(slog.DiscardHandler)
	s, err := Listen(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	stop := func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	}
	return s, opts.Registry, stop
}

func send(t *testing.T, addr net.Addr, datagrams ...string) {
	t.Helper()
	c, err := net.Dial("udp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, d := range datagrams {
		if _, err := c.Write([]byte(d)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestServer_ParsesEveryLineAndCountsTheRest(t *testing.T) {
	defer testutil.CheckGoroutines(t)
	rec := &recorder{}
	s, reg, stop := startServer(t, Options{Sink: rec.sink})
	send(t, s.Addr(),
		"a:1|c\nb:2|g|#x:y",
		"garbage\n_e{1,1}:t|x\n_sc|chk|0\nc:3|h",
	)
	testutil.Eventually(t, 2*time.Second, func() bool { return rec.n.Load() == 3 }, "want 3 messages, got %d", rec.n.Load())
	stop()

	for name, want := range map[string]int64{
		"ozy.agent.statsd.packets_received":  2,
		"ozy.agent.statsd.messages_received": 3,
		"ozy.agent.statsd.parse_errors":      1,
	} {
		if got := reg.Counter(name).Value(); got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}
	if reg.Counter("ozy.agent.statsd.unsupported", "kind:event").Value() != 1 ||
		reg.Counter("ozy.agent.statsd.unsupported", "kind:service_check").Value() != 1 {
		t.Error("events / service checks not counted")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if got := strings.Join(rec.lines, " "); !strings.Contains(got, "a:1|c") || !strings.Contains(got, "b:2|g|#x:y") || !strings.Contains(got, "c:3|h") {
		t.Errorf("lines = %q", rec.lines)
	}
}

// A full queue drops and counts rather than blocking the reader.
func TestServer_FullQueueDropsAndCounts(t *testing.T) {
	block := make(chan struct{})
	var got atomic.Int64
	sink := func(*Message, time.Time) { got.Add(1); <-block }
	s, reg, stop := startServer(t, Options{Sink: sink, Workers: 1, QueueSize: 1})
	for range 20 {
		send(t, s.Addr(), "a:1|c")
	}
	received := reg.Counter("ozy.agent.statsd.packets_received")
	dropped := reg.Counter("ozy.agent.statsd.packets_dropped")
	testutil.Eventually(t, 2*time.Second, func() bool { return received.Value() == 20 }, "received %d", received.Value())
	close(block)
	stop()

	// At most two packets can be in flight — one in the worker, one in the
	// one-slot queue — and shutdown drains the queue, so by now every packet
	// has either reached the sink or been counted as dropped.
	//
	// Which of the two it is, is scheduling. Asserting exactly 2 delivered
	// assumed the worker had already taken the first packet off the queue
	// before the second arrived; when it has not, the first is still queued,
	// the second is dropped instead, and the split is 1 and 19. That is the
	// same behaviour — nothing blocked, nothing was lost, every drop was
	// counted — and it failed on CI, where a loaded runner is exactly where
	// the worker does not get scheduled promptly.
	//
	// So: the total must add up, and the in-flight count must respect the
	// capacity. Both are properties of the server; neither is a property of
	// the Go scheduler.
	delivered := got.Load()
	if sum := delivered + dropped.Value(); sum != 20 {
		t.Errorf("delivered %d + dropped %d = %d, want 20 — a packet was lost or counted twice",
			delivered, dropped.Value(), sum)
	}
	if delivered < 1 || delivered > 2 {
		t.Errorf("delivered %d packets; a one-slot queue and one worker can hold at most 2", delivered)
	}
}

// Everything queued before shutdown still reaches the sink.
func TestServer_ShutdownDrainsTheQueue(t *testing.T) {
	gate := make(chan struct{})
	var got atomic.Int64
	sink := func(*Message, time.Time) { <-gate; got.Add(1) }
	s, reg, stop := startServer(t, Options{Sink: sink, Workers: 1, QueueSize: 100})
	for range 10 {
		send(t, s.Addr(), "a:1|c")
	}
	received := reg.Counter("ozy.agent.statsd.packets_received")
	testutil.Eventually(t, 2*time.Second, func() bool { return received.Value() == 10 }, "received %d", received.Value())
	go func() { time.Sleep(20 * time.Millisecond); close(gate) }()
	stop()
	if got.Load() != 10 {
		t.Fatalf("delivered %d of 10 after shutdown", got.Load())
	}
}

func TestListen_Errors(t *testing.T) {
	if _, err := Listen(Options{Addr: "127.0.0.1:0"}); err == nil || !strings.Contains(err.Error(), "no sink") {
		t.Errorf("no sink: %v", err)
	}
	sink := func(*Message, time.Time) {}
	if _, err := Listen(Options{Addr: "nope:::", Sink: sink}); err == nil {
		t.Error("bad address accepted")
	}
	s, err := Listen(Options{Addr: "127.0.0.1:0", Sink: sink})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.conn.Close() }()
	if _, err := Listen(Options{Addr: s.Addr().String(), Sink: sink}); err == nil {
		t.Error("second bind to the same port succeeded")
	}
}

func TestListen_Defaults(t *testing.T) {
	s, err := Listen(Options{Addr: "127.0.0.1:0", Sink: func(*Message, time.Time) {}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.conn.Close() }()
	if s.opts.Readers != 2 || s.opts.Workers != 2 || s.opts.QueueSize != 1024 || s.opts.ReadBuffer != 4<<20 {
		t.Fatalf("defaults = %+v", s.opts)
	}
}

// L6: many concurrent writers; every datagram is either delivered or counted
// as dropped by the server.
//
// Kernel drops are invisible to the server, so the writers are paced to about
// 80k datagrams/s in total, above M1's 50k/s target. Unpaced, eight writers
// on macOS loopback reach ~550k/s and the kernel silently discards two thirds
// of them before any reader sees them. That is the reason statsd is a
// measure-it-yourself protocol: the sender can't know, and the receiver can
// only count what reached it.
func TestServer_ConcurrentWritersAccountForEveryPacket(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}
	var got atomic.Int64
	s, reg, stop := startServer(t, Options{Sink: func(*Message, time.Time) { got.Add(1) }, Readers: 2, Workers: 4})
	const writers, perWriter = 8, 10000
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			c, err := net.Dial("udp", s.Addr().String())
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			for i := range perWriter {
				_, _ = c.Write([]byte("stress.count:1|c|#w:x"))
				if i%10 == 9 {
					time.Sleep(time.Millisecond) // ≈10 per ms per writer
				}
			}
		})
	}
	wg.Wait()
	received := reg.Counter("ozy.agent.statsd.packets_received")
	dropped := reg.Counter("ozy.agent.statsd.packets_dropped")
	testutil.Eventually(t, 5*time.Second, func() bool { return got.Load()+dropped.Value() == received.Value() && received.Value() > 0 },
		"delivered %d + dropped %d != received %d", got.Load(), dropped.Value(), received.Value())
	stop()
	sent := int64(writers * perWriter)
	if received.Value() != sent {
		t.Logf("kernel dropped %d of %d datagrams before the server saw them", sent-received.Value(), sent)
	}
	if received.Value() < sent*99/100 {
		t.Fatalf("received only %d of %d", received.Value(), sent)
	}
}

func BenchmarkServer_Throughput(b *testing.B) {
	var got atomic.Int64
	s, err := Listen(Options{Addr: "127.0.0.1:0", Sink: func(*Message, time.Time) { got.Add(1) },
		Registry: selfmetrics.NewRegistry(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Run(ctx); close(done) }()
	c, err := net.Dial("udp", s.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	msg := []byte("http.request.count:1|c|#service:shop,env:dev,route:/api/comics")
	for b.Loop() {
		_, _ = c.Write(msg)
	}
	c.Close()
	cancel()
	<-done
}
