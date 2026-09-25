package testutil

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

// recv returns the value waiting on ch, or fails if there is none. FakeClock
// delivers synchronously inside Advance, so a value is either there already
// or never coming — no waiting needed.
func recv(t *testing.T, ch <-chan time.Time) time.Time {
	t.Helper()
	select {
	case v := <-ch:
		return v
	default:
		t.Fatal("expected a value on the channel, found none")
		return time.Time{}
	}
}

func assertEmpty(t *testing.T, ch <-chan time.Time) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("unexpected value %v on the channel", v)
	default:
	}
}

func TestFakeClock_NowOnlyMovesWhenAdvanced(t *testing.T) {
	c := NewFakeClock(t0)
	if !c.Now().Equal(t0) {
		t.Fatalf("Now = %v, want %v", c.Now(), t0)
	}
	c.Advance(90 * time.Second)
	if want := t0.Add(90 * time.Second); !c.Now().Equal(want) {
		t.Fatalf("Now = %v, want %v", c.Now(), want)
	}
}

func TestFakeClock_TickerFiresAtEachPeriodWithTheTickTime(t *testing.T) {
	c := NewFakeClock(t0)
	tk := c.NewTicker(10 * time.Second)
	c.Advance(9 * time.Second)
	assertEmpty(t, tk.C())
	c.Advance(time.Second)
	if got := recv(t, tk.C()); !got.Equal(t0.Add(10 * time.Second)) {
		t.Fatalf("tick = %v, want %v", got, t0.Add(10*time.Second))
	}
	c.Advance(10 * time.Second)
	if got := recv(t, tk.C()); !got.Equal(t0.Add(20 * time.Second)) {
		t.Fatalf("tick = %v, want %v", got, t0.Add(20*time.Second))
	}
}

// Matches package time: a slow receiver sees one tick, not a backlog.
func TestFakeClock_TickerDropsTicksWhenReceiverIsBehind(t *testing.T) {
	c := NewFakeClock(t0)
	tk := c.NewTicker(time.Second)
	c.Advance(5 * time.Second)
	if got := recv(t, tk.C()); !got.Equal(t0.Add(time.Second)) {
		t.Fatalf("first tick = %v, want the first deadline", got)
	}
	assertEmpty(t, tk.C())
}

// A big Advance must cost the same as a small one. The catch-up loop used to
// run once per period, so simulating a long retention window against a short
// maintenance ticker took minutes of CPU to deliver a single tick.
func TestFakeClock_AdvancingFarIsNotProportionalToTheSpan(t *testing.T) {
	c := NewFakeClock(t0)
	tk := c.NewTicker(time.Second)

	start := time.Now()
	c.Advance(10_000 * time.Hour) // 36 million periods
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Advance took %v; it is iterating per period, not skipping", elapsed)
	}

	// Semantics are unchanged: the first missed deadline is delivered, the
	// rest are dropped, and the ticker is re-armed after the new Now.
	if got := recv(t, tk.C()); !got.Equal(t0.Add(time.Second)) {
		t.Errorf("tick = %v, want the first deadline %v", got, t0.Add(time.Second))
	}
	assertEmpty(t, tk.C())
	if want := t0.Add(10_000 * time.Hour); !c.Now().Equal(want) {
		t.Errorf("Now = %v, want %v", c.Now(), want)
	}
	c.Advance(time.Second)
	if got := recv(t, tk.C()); !got.Equal(c.Now()) {
		t.Errorf("tick after the skip = %v, want %v", got, c.Now())
	}
}

func TestFakeClock_TickerStopAndReset(t *testing.T) {
	c := NewFakeClock(t0)
	tk := c.NewTicker(time.Second)
	tk.Stop()
	if c.Waiters() != 0 {
		t.Fatalf("Waiters after Stop = %d, want 0", c.Waiters())
	}
	c.Advance(time.Hour)
	assertEmpty(t, tk.C())

	tk.Reset(time.Minute)
	c.Advance(time.Minute)
	recv(t, tk.C())
	c.Advance(time.Minute) // still periodic with the new period
	recv(t, tk.C())
}

func TestFakeClock_NewTickerPanicsOnNonPositivePeriod(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewTicker(0) did not panic")
		}
	}()
	NewFakeClock(t0).NewTicker(0)
}

func TestFakeClock_TimerFiresOnceThenDisarms(t *testing.T) {
	c := NewFakeClock(t0)
	tm := c.NewTimer(time.Minute)
	c.Advance(time.Minute)
	recv(t, tm.C())
	c.Advance(time.Hour)
	assertEmpty(t, tm.C())
	if tm.Stop() {
		t.Error("Stop after firing = true, want false")
	}
}

func TestFakeClock_TimerStopPreventsFiring(t *testing.T) {
	c := NewFakeClock(t0)
	tm := c.NewTimer(time.Minute)
	if !tm.Stop() {
		t.Fatal("Stop on armed timer = false, want true")
	}
	c.Advance(time.Hour)
	assertEmpty(t, tm.C())
}

func TestFakeClock_TimerResetReportsPriorState(t *testing.T) {
	c := NewFakeClock(t0)
	tm := c.NewTimer(time.Minute)
	if !tm.Reset(time.Hour) {
		t.Error("Reset on armed timer = false, want true")
	}
	c.Advance(time.Minute)
	assertEmpty(t, tm.C())
	c.Advance(time.Hour)
	recv(t, tm.C())
	if tm.Reset(time.Second) {
		t.Error("Reset on fired timer = true, want false")
	}
	c.Advance(time.Second)
	recv(t, tm.C())
}

func TestFakeClock_ZeroTimerFiresOnNextAdvance(t *testing.T) {
	c := NewFakeClock(t0)
	tm := c.NewTimer(0)
	c.Advance(0)
	recv(t, tm.C())
}

// Receivers that read Now while handling a tick must see the tick's time.
func TestFakeClock_FiresInDeadlineOrderAndStepsNow(t *testing.T) {
	c := NewFakeClock(t0)
	late := c.NewTimer(3 * time.Second)
	early := c.NewTimer(time.Second)
	c.Advance(5 * time.Second)
	if got := recv(t, early.C()); !got.Equal(t0.Add(time.Second)) {
		t.Errorf("early fired at %v", got)
	}
	if got := recv(t, late.C()); !got.Equal(t0.Add(3 * time.Second)) {
		t.Errorf("late fired at %v", got)
	}
	if !c.Now().Equal(t0.Add(5 * time.Second)) {
		t.Errorf("Now = %v, want t0+5s", c.Now())
	}
}

// A wall-clock step backwards (NTP, a VM resume) must not re-fire or hang.
func TestFakeClock_SetBackwardsOnlyMovesNow(t *testing.T) {
	c := NewFakeClock(t0)
	tm := c.NewTimer(time.Minute)
	c.Set(t0.Add(-time.Hour))
	assertEmpty(t, tm.C())
	if !c.Now().Equal(t0.Add(-time.Hour)) {
		t.Fatalf("Now = %v", c.Now())
	}
	if c.Waiters() != 1 {
		t.Fatalf("Waiters = %d, want the timer still armed", c.Waiters())
	}
}

// Review finding: since Go 1.23, Stop and Reset guarantee no stale value is
// received afterwards. The fake must match, or components that Reset a
// fired-but-unread timer see a spurious fire only in tests.
func TestFakeClock_StopAndResetDiscardUnreadFires(t *testing.T) {
	c := NewFakeClock(t0)

	tm := c.NewTimer(time.Second)
	c.Advance(time.Second) // fired, not read
	tm.Reset(time.Minute)
	assertEmpty(t, tm.C())

	tm2 := c.NewTimer(time.Second)
	c.Advance(time.Second)
	tm2.Stop()
	assertEmpty(t, tm2.C())

	tk := c.NewTicker(time.Second)
	c.Advance(time.Second)
	tk.Reset(time.Hour)
	assertEmpty(t, tk.C())
	c.Advance(time.Second)
	tk.Stop()
	assertEmpty(t, tk.C())
}

// Review finding: a timer that fired but was never read still counts as
// armed in Go 1.23+ (checked against package time: Stop and Reset both
// return true). If the fake returned false after draining, the classic
// `if !t.Stop() { <-t.C }` would block forever, only in tests. A timer
// whose fire was already received returns false, as before.
func TestFakeClock_FiredUnreadTimerCountsAsArmed(t *testing.T) {
	c := NewFakeClock(t0)

	tm := c.NewTimer(time.Second)
	c.Advance(time.Second)
	if !tm.Stop() {
		t.Error("Stop on fired-but-unread timer = false, want true (Go 1.23+)")
	}

	tm2 := c.NewTimer(time.Second)
	c.Advance(time.Second)
	if !tm2.Reset(time.Minute) {
		t.Error("Reset on fired-but-unread timer = false, want true (Go 1.23+)")
	}

	tm3 := c.NewTimer(time.Second)
	c.Advance(time.Second)
	recv(t, tm3.C())
	if tm3.Stop() {
		t.Error("Stop on fired-and-read timer = true, want false")
	}
}
