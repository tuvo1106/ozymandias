package notify

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

// gatedOutbox lets a test hold a rescan between its read and its enqueue.
type gatedOutbox struct {
	*memOutbox
	hold chan chan struct{} // a read sends a channel and waits on it
}

func (g *gatedOutbox) Outbox(ctx context.Context) ([]meta.OutboxRow, error) {
	rows, err := g.memOutbox.Outbox(ctx)
	select {
	case wait := <-g.hold:
		<-wait
	default:
	}
	return rows, err
}

// TestStaleCopiesAreNotRedelivered: a row completed after a rescan read it,
// or before the committer's own Enqueue got to it, is delivered once.
func TestStaleCopiesAreNotRedelivered(t *testing.T) {
	testutil.CheckGoroutines(t)
	ob := &gatedOutbox{memOutbox: &memOutbox{}, hold: make(chan chan struct{}, 1)}
	var calls atomic.Int32
	release := make(chan struct{})
	n := notifierFunc(func(ctx context.Context) error {
		calls.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})
	d := &Dispatcher{Notifiers: map[string]Notifier{"n": n}, Store: ob, Backoff: []time.Duration{}}
	r := ob.add(1, "n")
	stop := start(t, d)
	defer stop()
	testutil.Eventually(t, 5*time.Second, func() bool { return calls.Load() == 1 }, "in flight")

	// A rescan reads the row while it is in flight, and is held there.
	wait := make(chan struct{})
	ob.hold <- wait
	rescanned := make(chan struct{})
	go func() { d.rescan(context.Background()); close(rescanned) }()
	testutil.Eventually(t, 5*time.Second, func() bool { return len(ob.hold) == 0 }, "rescan read")
	// The delivery completes and the row is deleted; then the rescan goes on.
	close(release)
	waitEvents(t, ob.memOutbox, 1)
	close(wait)
	<-rescanned
	// And the committer's Enqueue arrives late with its copy.
	d.Enqueue(context.Background(), []meta.OutboxRow{r})
	time.Sleep(50 * time.Millisecond)
	if c := calls.Load(); c != 1 {
		t.Fatalf("delivered %d times", c)
	}
}
