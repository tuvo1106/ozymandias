package loghub

import (
	"fmt"
	"sync"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/query/logql"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

func mk(service, status, msg string) wire.Log {
	return wire.Log{Ts: 1, Service: service, Status: status, Message: msg}
}

func parse(t *testing.T, q string) logql.Node {
	t.Helper()
	n, err := logql.Parse(q)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestHub_DeliversOnlyWhatMatches(t *testing.T) {
	h := New(0)
	all, _ := h.Subscribe(nil, 10)
	errs, _ := h.Subscribe(parse(t, "service:api status:error"), 10)
	text, _ := h.Subscribe(parse(t, "timeout"), 10)
	h.Publish([]wire.Log{mk("api", "error", "boom"), mk("web", "error", "x"), mk("api", "info", "request timeout")})
	if len(all.C) != 3 || len(errs.C) != 1 || len(text.C) != 1 {
		t.Fatalf("%d %d %d", len(all.C), len(errs.C), len(text.C))
	}
	if got := (<-errs.C).Message; got != "boom" {
		t.Fatal(got)
	}
	if st := h.Stats(); st.Published != 3 || st.Delivered != 5 || st.Subscribers != 3 {
		t.Fatalf("%+v", st)
	}
}

// A subscriber that does not read loses logs; it does not slow Publish.
func TestHub_ASlowSubscriberDropsAndDoesNotBlock(t *testing.T) {
	h := New(0)
	slow, _ := h.Subscribe(nil, 5)
	fast, _ := h.Subscribe(nil, 100)
	batch := make([]wire.Log, 20)
	for i := range batch {
		batch[i] = mk("api", "info", fmt.Sprint(i))
	}
	h.Publish(batch) // returns, though slow's channel holds 5
	if len(slow.C) != 5 || len(fast.C) != 20 {
		t.Fatalf("%d %d", len(slow.C), len(fast.C))
	}
	if n := slow.TakeDropped(); n != 15 {
		t.Fatalf("dropped %d, want 15", n)
	}
	if n := slow.TakeDropped(); n != 0 {
		t.Fatalf("a second TakeDropped reported %d", n)
	}
	h.Publish(batch[:3])
	if n := slow.TakeDropped(); n != 3 {
		t.Fatalf("dropped %d since the last notice, want 3", n)
	}
	if fast.TakeDropped() != 0 {
		t.Fatal("the fast subscriber lost logs")
	}
	if h.Stats().Dropped != 18 {
		t.Fatalf("%+v", h.Stats())
	}
}

func TestHub_CloseStopsDeliveryAndTheLimitHolds(t *testing.T) {
	h := New(2)
	a, ok1 := h.Subscribe(nil, 1)
	_, ok2 := h.Subscribe(nil, 1)
	if _, ok := h.Subscribe(nil, 1); !ok1 || !ok2 || ok {
		t.Fatal("limit not enforced")
	}
	a.Close()
	a.Close()
	h.Publish([]wire.Log{mk("api", "info", "x")})
	if len(a.C) != 0 {
		t.Fatal("a closed subscription still received")
	}
	if _, ok := h.Subscribe(nil, 1); !ok {
		t.Fatal("closing did not free a slot")
	}
}

func TestHub_ConcurrentPublishAndSubscribe(t *testing.T) {
	h := New(0)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				h.Publish([]wire.Log{mk("api", "info", "x")})
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if s, ok := h.Subscribe(nil, 4); ok {
					s.Close()
				}
			}
		}()
	}
	wg.Wait()
}

// L6: a tail that comes and goes must leave nothing behind.
func TestHub_ThousandSubscribeUnsubscribeCyclesLeakNothing(t *testing.T) {
	testutil.CheckGoroutines(t)
	h := New(0)
	filter := parse(t, "status:error")
	for i := 0; i < 1000; i++ {
		s, ok := h.Subscribe(filter, 4)
		if !ok {
			t.Fatalf("cycle %d: the hub refused a subscriber although none are open", i)
		}
		h.Publish([]wire.Log{mk("a", "error", "x")})
		s.Close()
	}
	if st := h.Stats(); st.Subscribers != 0 {
		t.Fatalf("%d subscribers left after 1000 cycles", st.Subscribers)
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.subs) != 0 {
		t.Fatalf("the subscriber map holds %d entries", len(h.subs))
	}
}
