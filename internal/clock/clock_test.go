package clock_test

import (
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/clock"
)

// The real clock is a thin wrapper over package time; these tests only prove
// the wrapper is wired to the right methods, with generous timeouts.

func TestReal_NowIsCurrent(t *testing.T) {
	before := time.Now()
	got := clock.Real().Now()
	if got.Before(before) || got.Sub(before) > time.Second {
		t.Fatalf("Now() = %v, want close to %v", got, before)
	}
}

func TestReal_TickerTicksAndStops(t *testing.T) {
	tk := clock.Real().NewTicker(time.Millisecond)
	defer tk.Stop()
	select {
	case <-tk.C():
	case <-time.After(2 * time.Second):
		t.Fatal("ticker never ticked")
	}
	tk.Reset(time.Hour)
	tk.Stop()
}

func TestReal_TimerFiresOnceAndReportsState(t *testing.T) {
	tm := clock.Real().NewTimer(time.Millisecond)
	select {
	case <-tm.C():
	case <-time.After(2 * time.Second):
		t.Fatal("timer never fired")
	}
	if tm.Stop() {
		t.Error("Stop() after firing = true, want false")
	}
	if tm.Reset(time.Hour) {
		t.Error("Reset() on a fired timer = true, want false")
	}
	if !tm.Stop() {
		t.Error("Stop() on an armed timer = false, want true")
	}
}
