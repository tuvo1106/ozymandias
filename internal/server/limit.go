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
	switch {
	case q.MaxQueue == 0:
		q.MaxQueue = defaultQueryQueue
	case q.MaxQueue < 0: // fail fast: no waiting at all
		q.MaxQueue = 0
	}
	if q.QueueWait == 0 {
		q.QueueWait = defaultQueryWait
	}
	l := httpserve.NewLimiter(s.clock, q.MaxConcurrent, q.MaxQueue, q.QueueWait)
	s.reg.GaugeFunc("ozy.query.in_flight", func() float64 { return float64(l.InFlight()) })
	s.reg.GaugeFunc("ozy.query.queued", func() float64 { return float64(l.Queued()) })
	s.reg.CounterFunc("ozy.query.rejected", func() float64 { return float64(l.Rejected()) })
	return l.Wrap(next, isQuery)
}

// postReads are the POST routes that only read: the query endpoints take a body
// because a query is too big for a URL, not because they change anything. They
// are the heaviest reads there are (a dashboard's widgets arrive as batches),
// so a selector that took GET as "is a read" would exempt exactly them.
var postReads = map[string]bool{
	"/api/v1/query":             true,
	"/api/v1/query/batch":       true,
	"/api/v1/query/sketch":      true,
	"/api/v1/query/validate":    true,
	"/api/v1/monitors/backtest": true,
}

// liveTail is the one route that streams. Matched exactly: a suffix match would
// also exempt any /api/ path that merely ends in /tail (a service named "tail").
const liveTail = "/api/v1/logs/tail"

func isQuery(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet:
		return strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != liveTail
	case http.MethodPost:
		return postReads[r.URL.Path]
	}
	return false
}
