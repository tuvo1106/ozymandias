package statsd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
)

// MaxDatagram is the largest datagram the server reads (§A). A larger one is
// truncated by the kernel; its last line is then most likely malformed and
// counted as a parse error.
const MaxDatagram = 8192

// Sink receives every parsed metric line. The Message aliases a pooled
// buffer that is reused as soon as Sink returns, so Sink must copy whatever
// it keeps. now is the time the datagram was dequeued, which is the
// receive time for bucketing.
type Sink func(m *Message, now time.Time)

// Options configures a Server. Zero values get the documented defaults.
type Options struct {
	// Addr is the UDP listen address. Default ":8125".
	Addr string
	// ReadBuffer is the socket receive buffer (SO_RCVBUF) in bytes. Default
	// 4 MiB. The kernel buffer is what absorbs a burst while the readers are
	// busy; when it fills, the kernel drops datagrams without telling
	// anyone. The OS may cap it (sysctl kern.ipc.maxsockbuf,
	// net.core.rmem_max).
	ReadBuffer int
	// Readers is the number of goroutines reading the socket. Default 2.
	Readers int
	// Workers is the number of goroutines parsing datagrams. Default 2.
	Workers int
	// QueueSize bounds the datagrams waiting between readers and workers.
	// When it is full new datagrams are dropped and counted: a reader must
	// never block, or the kernel buffer fills and drops silently instead.
	// Default 1024.
	QueueSize int
	Sink      Sink
	Clock     clock.Clock           // default clock.Real()
	Registry  *selfmetrics.Registry // default: a new registry
	Logger    *slog.Logger          // default slog.Default()
}

// Server is an extended StatsD UDP server.
type Server struct {
	opts  Options
	conn  *net.UDPConn
	queue chan *datagram
	pool  sync.Pool

	packetsReceived  *selfmetrics.Counter
	packetsDropped   *selfmetrics.Counter
	messagesReceived *selfmetrics.Counter
	parseErrors      *selfmetrics.Counter
	events           *selfmetrics.Counter
	serviceChecks    *selfmetrics.Counter
}

type datagram struct {
	buf [MaxDatagram]byte
	n   int
}

// Listen binds the UDP socket. Binding happens here, not in Run, so a port
// conflict fails the agent at startup with a clear error.
func Listen(opts Options) (*Server, error) {
	if opts.Addr == "" {
		opts.Addr = ":8125"
	}
	if opts.ReadBuffer <= 0 {
		opts.ReadBuffer = 4 << 20
	}
	if opts.Readers <= 0 {
		opts.Readers = 2
	}
	if opts.Workers <= 0 {
		opts.Workers = 2
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = 1024
	}
	if opts.Sink == nil {
		return nil, errors.New("statsd: no sink")
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
	addr, err := net.ResolveUDPAddr("udp", opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("statsd addr %q: %w", opts.Addr, err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("statsd listen %s: %w", opts.Addr, err)
	}
	if err := conn.SetReadBuffer(opts.ReadBuffer); err != nil {
		opts.Logger.Warn("statsd: could not set the socket receive buffer", "bytes", opts.ReadBuffer, "err", err)
	}
	reg := opts.Registry
	s := &Server{
		opts:  opts,
		conn:  conn,
		queue: make(chan *datagram, opts.QueueSize),

		packetsReceived:  reg.Counter("ozy.agent.statsd.packets_received"),
		packetsDropped:   reg.Counter("ozy.agent.statsd.packets_dropped"),
		messagesReceived: reg.Counter("ozy.agent.statsd.messages_received"),
		parseErrors:      reg.Counter("ozy.agent.statsd.parse_errors"),
		events:           reg.Counter("ozy.agent.statsd.unsupported", "kind:event"),
		serviceChecks:    reg.Counter("ozy.agent.statsd.unsupported", "kind:service_check"),
	}
	s.pool.New = func() any { return new(datagram) }
	reg.GaugeFunc("ozy.agent.statsd.queue_length", func() float64 { return float64(len(s.queue)) })
	return s, nil
}

// Addr returns the bound address (useful when Addr asked for port 0).
func (s *Server) Addr() net.Addr { return s.conn.LocalAddr() }

// Close releases the socket of a server that will not Run.
func (s *Server) Close() error { return s.conn.Close() }

// Run reads and parses until ctx ends. Shutdown closes the socket, lets the
// workers finish every datagram already queued, and returns once all of
// them are handed to the sink — so a final aggregator flush after Run
// returns includes everything that was received.
func (s *Server) Run(ctx context.Context) error {
	var readers, workers sync.WaitGroup
	for range s.opts.Readers {
		readers.Go(s.read)
	}
	for range s.opts.Workers {
		workers.Go(s.work)
	}
	s.opts.Logger.Info("statsd listening", "addr", s.Addr().String())
	<-ctx.Done()
	err := s.conn.Close()
	readers.Wait()
	close(s.queue)
	workers.Wait()
	return err
}

func (s *Server) read() {
	for {
		d := s.pool.Get().(*datagram)
		n, err := s.conn.Read(d.buf[:])
		if err != nil {
			s.pool.Put(d)
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.opts.Logger.Warn("statsd read", "err", err)
			continue
		}
		s.packetsReceived.Inc()
		d.n = n
		select {
		case s.queue <- d:
		default:
			s.packetsDropped.Inc()
			s.pool.Put(d)
		}
	}
}

func (s *Server) work() {
	for d := range s.queue {
		now := s.opts.Clock.Now()
		Lines(d.buf[:d.n], func(line []byte) {
			m, err := Parse(line)
			switch {
			case err == nil:
				s.messagesReceived.Inc()
				s.opts.Sink(&m, now)
			case errors.Is(err, ErrEvent):
				s.events.Inc()
			case errors.Is(err, ErrServiceCheck):
				s.serviceChecks.Inc()
			default:
				s.parseErrors.Inc()
				s.opts.Logger.Debug("statsd parse error", "err", err, "line", string(line))
			}
		})
		s.pool.Put(d)
	}
}
