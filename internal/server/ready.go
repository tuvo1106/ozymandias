package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"github.com/tuvo1106/ozymandias/internal/intake"
)

// /healthz and /readyz answer different questions, and an orchestrator that is
// given the wrong one makes things worse.
//
//   - /healthz is "is the process alive": it answers 200 whenever the HTTP
//     server does. A failing liveness check restarts the process, so it must not
//     fail for a reason a restart cannot fix.
//   - /readyz is "should traffic be sent here": it is 503 while ozyd is
//     starting (stores not yet open, the write-ahead logs not yet replayed),
//     while it is draining for a shutdown, and, the case that needs it most,
//     after a store has stopped itself.
//
// That last one is why this exists. A write-ahead log whose fsync failed stops
// for good (wal.ErrSync: the kernel may have dropped the pages, so a retry that
// succeeds proves nothing). The store then refuses every write while the
// process stays up and /healthz stays green. Restarting is the fix, because
// replay is how the log is made trustworthy again, so a stopped store is the
// one thing /readyz reports and /healthz deliberately does not.

// brokenStore is what a store with a write-ahead log can say about having stopped.
type brokenStore interface{ Broken() error }

// readiness holds the process-wide readiness state and answers /readyz.
type readiness struct {
	s *Server
	// draining is set when shutdown begins, so a load balancer stops sending
	// before the listener closes.
	draining atomic.Bool
	// consumers counts the queue consumers running; in queued mode a server
	// whose consumers have not started accepts requests into a queue nobody reads.
	consumers atomic.Int32
	// stores are the stores that can stop themselves, by name. A list, not a
	// lookup of s.store at request time, so that a test can put a store that
	// has stopped in place of a real one: no test can make a disk fail an fsync.
	stores []namedStore
}

type namedStore struct {
	name string
	b    brokenStore
}

// check is one named condition and why it fails, or nil.
type check struct {
	name string
	err  func() error
}

func (r *readiness) checks() []check {
	s := r.s
	cs := []check{
		{"meta", func() error { return s.meta.Ping(context.Background()) }},
		{"draining", func() error {
			if r.draining.Load() {
				return errDraining
			}
			return nil
		}},
	}
	for _, st := range r.stores {
		cs = append(cs, check{st.name, st.b.Broken})
	}
	if s.pipe != nil {
		for _, name := range intake.Topics {
			t := s.pipe.topics[name]
			cs = append(cs, check{"queue:" + name, func() error { return t.Stats().Broken }})
		}
		cs = append(cs, check{"consumers", func() error {
			if int(r.consumers.Load()) < len(intake.Topics) {
				return errNoConsumers
			}
			return nil
		}})
	}
	return cs
}

type readyError string

func (e readyError) Error() string { return string(e) }

const (
	errDraining    readyError = "shutting down"
	errNoConsumers readyError = "the queue consumers are not running"
)

// ServeHTTP answers /readyz: 200 when every check passes, else 503, with the
// reason for each failing check. The body says which, because "not ready" with
// no reason sends an operator to the logs to find out what the endpoint knew.
func (r *readiness) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	out := map[string]string{}
	ready := true
	for _, c := range r.checks() {
		if err := c.err(); err != nil {
			out[c.name] = err.Error()
			ready = false
		} else {
			out[c.name] = "ok"
		}
	}
	status, code := "ready", http.StatusOK
	if !ready {
		status, code = "not_ready", http.StatusServiceUnavailable
	}
	names := make([]string, 0, len(out))
	for n := range out {
		names = append(names, n)
	}
	sort.Strings(names)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "checks": out, "checked_at": time.Now().UTC().Format(time.RFC3339)})
}

// registerBrokenGauges exposes each store's stopped state as 0/1, registered up
// front so that "never broken" is a line at zero and not an absent series.
func (s *Server) registerBrokenGauges() {
	for _, st := range s.ready.stores {
		s.reg.GaugeFunc("ozy.store.broken", func() float64 {
			if st.b.Broken() != nil {
				return 1
			}
			return 0
		}, "store:"+st.name)
	}
}
