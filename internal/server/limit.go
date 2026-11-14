package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/tuvo1106/ozymandias/internal/httpserve"
)

// Defaults for query.max_concurrent, max_queue and queue_wait.
const (
	defaultQueryConcurrency = 8
	defaultQueryQueue       = 32
	defaultQueryWait        = 2 * time.Second
)

// limitQueries bounds the read API. The selector is what a thing *is*: a read
// of the API (GET under /api/) that is not a stream. The live tail is a GET
// too, but it holds its connection for as long as the page is open, so counting
// it would let eight open tabs lock every other query out.
func (s *Server) limitQueries(next http.Handler) http.Handler {
	q := s.cfg.Query
	if q.MaxConcurrent == 0 {
		q.MaxConcurrent = defaultQueryConcurrency
	}
	if q.MaxQueue == 0 {
		q.MaxQueue = defaultQueryQueue
	}
	if q.QueueWait == 0 {
		q.QueueWait = defaultQueryWait
	}
	l := httpserve.NewLimiter(s.clock, q.MaxConcurrent, q.MaxQueue, q.QueueWait)
	s.reg.GaugeFunc("ozy.query.in_flight", func() float64 { return float64(l.InFlight()) })
	s.reg.GaugeFunc("ozy.query.queued", func() float64 { return float64(l.Queued()) })
	s.reg.CounterFunc("ozy.query.rejected", func() float64 { return float64(l.Rejected()) })
	return l.Wrap(next, func(r *http.Request) bool {
		return r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/") &&
			!strings.HasSuffix(r.URL.Path, "/tail")
	})
}
