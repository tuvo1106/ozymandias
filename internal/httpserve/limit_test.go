package httpserve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/testutil"
)

// blocker is a handler that holds its slot until released.
type blocker struct{ enter, release chan struct{} }

func newBlocker() *blocker { return &blocker{make(chan struct{}, 64), make(chan struct{})} }
func (b *blocker) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	b.enter <- struct{}{}
	<-b.release
	w.WriteHeader(http.StatusOK)
}

func do(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

// recv fails the test instead of hanging it: a mutant that stops releasing
// slots must be a failure with a message, not a timeout.
func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting; a slot or a request is stuck")
		panic("unreachable")
	}
}

func isQuery(r *http.Request) bool { return r.URL.Path != "/tail" }

func TestLimiter_SlotsThenQueueThen503(t *testing.T) {
	clk := testutil.NewFakeClock(time.Unix(1, 0))
	l := NewLimiter(clk, 2, 1, time.Second)
	b := newBlocker()
	h := l.Wrap(b, isQuery)
	codes := make(chan int, 8)
	run := func() { go func() { codes <- do(h, "/q").Code }() }

	run()
	run()
	recv(t, b.enter)
	recv(t, b.enter) // both slots held
	run()            // takes the one queue place
	testutil.Eventually(t, 2*time.Second, func() bool { return l.Queued() == 1 }, "third request never queued")

	fourth := make(chan *httptest.ResponseRecorder, 1)
	go func() { fourth <- do(h, "/q") }()
	if rec := recv(t, fourth); rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("fourth request: %d, Retry-After %q; want an immediate 503", rec.Code, rec.Header().Get("Retry-After"))
	}
	if l.Rejected() != 1 || l.InFlight() != 2 {
		t.Fatalf("rejected %d in flight %d", l.Rejected(), l.InFlight())
	}
	close(b.release)
	for range 3 {
		if c := recv(t, codes); c != http.StatusOK {
			t.Fatalf("a held or queued request ended %d", c)
		}
	}
	if l.InFlight() != 0 || l.Queued() != 0 {
		t.Fatalf("slots leaked: in flight %d queued %d", l.InFlight(), l.Queued())
	}
}

func TestLimiter_AQueuedRequestGivesUpAfterTheWait(t *testing.T) {
	clk := testutil.NewFakeClock(time.Unix(1, 0))
	l := NewLimiter(clk, 1, 4, time.Second)
	b := newBlocker()
	h := l.Wrap(b, isQuery)
	go do(h, "/q")
	recv(t, b.enter)
	got := make(chan int, 1)
	go func() { got <- do(h, "/q").Code }()
	testutil.Eventually(t, 2*time.Second, func() bool { return clk.Waiters() == 1 }, "waiter never armed its timer")
	clk.Advance(time.Second)
	if c := recv(t, got); c != http.StatusServiceUnavailable {
		t.Fatalf("%d, want 503 after the wait", c)
	}
	close(b.release)
}

func TestLimiter_ExemptRequestsNeverTakeASlot(t *testing.T) {
	l := NewLimiter(testutil.NewFakeClock(time.Unix(1, 0)), 1, 0, time.Second)
	b := newBlocker()
	h := l.Wrap(b, isQuery)
	for range 3 { // three "tails" on a one-slot limiter
		go do(h, "/tail")
	}
	for range 3 {
		recv(t, b.enter)
	}
	if l.InFlight() != 0 {
		t.Fatalf("an exempt request held a slot")
	}
	close(b.release)
}

func TestLimiter_ACancelledWaiterIsNotARejection(t *testing.T) {
	l := NewLimiter(testutil.NewFakeClock(time.Unix(1, 0)), 1, 4, time.Hour)
	b := newBlocker()
	h := l.Wrap(b, isQuery)
	go do(h, "/q")
	recv(t, b.enter)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/q", nil).WithContext(ctx))
	}()
	testutil.Eventually(t, 2*time.Second, func() bool { return l.Queued() == 1 }, "never queued")
	cancel()
	recv(t, done)
	if l.Rejected() != 0 {
		t.Fatalf("a client that gave up counted as overload: rejected=%d", l.Rejected())
	}
	close(b.release)
}

// Instrument files a request under r.Pattern; the limiter answers before the mux
// has set one, so a rejection would otherwise be "unmatched", beside the 404s.
func TestLimiter_ARejectionCarriesItsOwnRoute(t *testing.T) {
	l := NewLimiter(testutil.NewFakeClock(time.Unix(1, 0)), 1, 0, time.Second)
	b := newBlocker()
	h := l.Wrap(b, isQuery)
	go do(h, "/q")
	recv(t, b.enter)
	req := httptest.NewRequest("GET", "/q", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if req.Pattern != "query-limit" {
		t.Fatalf("Pattern = %q", req.Pattern)
	}
	close(b.release)
}
