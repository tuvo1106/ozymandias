package forwarder

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// A refused key keeps the data: a 401 is retried, not dropped as a malformed
// request would be, and the payload arrives once the key is right. The key is
// not part of the stored payload, so what was spooled while it was wrong goes
// out under the current one.
func TestForwarder_AnUnauthorizedPayloadIsKeptAndSentUnderTheRightKey(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	var accepted atomic.Value
	accepted.Store("ozy_other")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get(wire.HeaderKey))
		mu.Unlock()
		if r.Header.Get(wire.HeaderKey) != accepted.Load().(string) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"ok","accepted":1}`))
	}))
	defer srv.Close()
	clk := testutil.NewFakeClock(time.Unix(1790000000, 0))
	reg := selfmetrics.NewRegistry()
	f := New(Options{
		URL: srv.URL, Clock: clk, Registry: reg, Logger: slog.New(slog.DiscardHandler),
		Rand: rand.New(rand.NewPCG(1, 2)), BackoffMin: time.Second, BackoffMax: 2 * time.Second,
		Key: "ozy_wrong",
	})
	f.Start()
	f.Submit(one("m"))
	testutil.Eventually(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(keys) > 0
	}, "the first attempt never arrived")
	accepted.Store("ozy_wrong") // the key is issued (or the agent's corrected): the same payload must now go through
	testutil.Eventually(t, 10*time.Second, func() bool {
		clk.Advance(3 * time.Second)
		mu.Lock()
		defer mu.Unlock()
		return f.seriesSent.Value() >= 1
	}, "the payload was never delivered after the key became valid: %v", keys)
	if got := counterValue(reg, "ozy.agent.forwarder.unauthorized"); got < 1 {
		t.Fatalf("unauthorized counter = %v", got)
	}
	if err := f.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func counterValue(r *selfmetrics.Registry, name string) float64 {
	for _, p := range r.Snapshot() {
		if p.Name == name {
			return p.Value
		}
	}
	return -1
}
