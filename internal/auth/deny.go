package auth

import (
	"net/http"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// charge is what one ingest request has spent against its key, so that it can be
// given back as a whole. It belongs to one request and one goroutine.
type charge struct {
	size     int64 // bytes charged by the gate (and one request token)
	spans    int
	logBytes int
}

type chargeKey struct{}

func chargeOf(r *http.Request) *charge {
	c, _ := r.Context().Value(chargeKey{}).(*charge)
	return c
}

// DeniedSpans charges n spans against the request's key and, when they are over
// the limit, answers 429 and says so. With auth off there is no key and it never
// denies.
func DeniedSpans(w http.ResponseWriter, r *http.Request, n int) bool {
	p := FromContext(r.Context())
	ok, retry := p.AllowSpans(n)
	if !ok {
		Refuse(w, retry)
		return true
	}
	if c := chargeOf(r); c != nil {
		c.spans += n
	}
	return false
}

// DeniedLogBytes is DeniedSpans for the day's log-byte allowance.
func DeniedLogBytes(w http.ResponseWriter, r *http.Request, n int) bool {
	p := FromContext(r.Context())
	ok, retry := p.AllowLogBytes(n)
	if !ok {
		Refuse(w, retry)
		return true
	}
	if c := chargeOf(r); c != nil {
		c.logBytes += n
	}
	return false
}

// release gives back everything a request spent. Called by the gate when the
// request ends in an answer that tells the sender to try again (429, 5xx): the
// data was not stored, the retry will be charged again, and without this a key
// near a limit pays for the same batch every attempt, and a store that is
// flapping looks like the sender running into its own limits.
func (p *Principal) release(c *charge) {
	if p == nil || c == nil {
		return
	}
	p.requests.give(1)
	p.bytes.give(float64(min(c.size, wire.MaxBodyBytes)))
	p.spans.give(float64(c.spans))
	p.logBytes.sub(int64(c.logBytes), p.clk.Now())
	*c = charge{}
}

// statusRecorder remembers the status a handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the real writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
