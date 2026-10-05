package tailer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// crashSink accepts logs and, at its crashAt-th Send, freezes what a SIGKILL at
// that instant would leave: the registry file as it is on disk (a kill lands
// after the send returned and before the offset is committed) and how many
// logs the downstream had by then. Everything after that instant never
// happened, so the test discards it.
type crashSink struct {
	mu        sync.Mutex
	got       []string
	sends     int
	crashAt   int
	regPath   string
	frozen    bool
	frozenReg []byte // nil: no registry file yet
	frozenN   int
}

func (s *crashSink) Send(_ context.Context, logs []wire.Log) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range logs {
		s.got = append(s.got, l.Message)
	}
	s.sends++
	if s.sends == s.crashAt && !s.frozen {
		s.frozen, s.frozenN = true, len(s.got)
		s.frozenReg, _ = os.ReadFile(s.regPath)
	}
	return nil
}

// delivered is what reached the downstream before this incarnation died.
func (s *crashSink) delivered() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.frozen {
		return append([]string(nil), s.got[:s.frozenN]...)
	}
	return append([]string(nil), s.got...)
}

// L5: kill the agent at any point of any poll, restart it from whatever the
// registry held on disk, and every line arrives at least once, with each
// crash costing at most one batch of duplicates.
func TestFiles_ACrashAtAnyPointLosesNothingAndRepeatsAtMostOneBatchPerCrash(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dir, err := os.MkdirTemp("", "crash")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.RemoveAll(dir) }()
		logFile := filepath.Join(dir, "a.log")
		regPath := filepath.Join(dir, "reg", "registry.json")
		if err := os.WriteFile(logFile, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		batch := rapid.IntRange(1, 7).Draw(t, "batch")
		clk := testutil.NewFakeClock(t0)
		var want []string
		write := func(n int) {
			f, _ := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY, 0o644)
			defer func() { _ = f.Close() }()
			for i := 0; i < n; i++ {
				line := fmt.Sprintf("line %04d", len(want))
				want = append(want, line)
				_, _ = f.WriteString(line + "\n")
			}
		}
		start := func(crashAt int) (*Files, *crashSink) {
			reg, _ := OpenRegistry(regPath)
			reg.noSync = true
			sink := &crashSink{crashAt: crashAt, regPath: regPath}
			f, err := NewFiles([]FileSource{{Path: logFile, Source: "plain", Service: "svc", StartPosition: "beginning"}},
				Options{Registry: reg, Sink: sink, Clock: clk, BatchLogs: batch})
			if err != nil {
				t.Fatal(err)
			}
			return f, sink
		}

		var all []string
		crashes := rapid.IntRange(1, 4).Draw(t, "crashes")
		f, sink := start(rapid.IntRange(1, 8).Draw(t, "crashAt0"))
		for c := 0; ; c++ {
			for round, rounds := 0, rapid.IntRange(1, 4).Draw(t, fmt.Sprintf("rounds%d", c)); round < rounds; round++ {
				write(rapid.IntRange(0, 25).Draw(t, fmt.Sprintf("w%d.%d", c, round)))
				clk.Advance(time.Second)
				f.Poll(context.Background())
			}
			if c == crashes {
				// The last incarnation is not killed: it drains whatever is left.
				for i := 0; i < 3; i++ {
					clk.Advance(time.Second)
					f.Poll(context.Background())
				}
				all = append(all, sink.delivered()...)
				break
			}
			all = append(all, sink.delivered()...)
			sink.mu.Lock()
			if sink.frozen { // the kill happened: the disk is as it was at that instant
				if sink.frozenReg == nil {
					_ = os.Remove(regPath)
				} else {
					_ = os.WriteFile(regPath, sink.frozenReg, 0o644)
				}
			}
			sink.mu.Unlock()
			crashAt := rapid.IntRange(1, 8).Draw(t, fmt.Sprintf("crashAt%d", c+1))
			if c+1 == crashes {
				crashAt = 0 // the last incarnation runs to completion
			}
			f, sink = start(crashAt)
		}
		verify(t, want, all, crashes, batch)
	})
}

func verify(t *rapid.T, want, all []string, crashes, batch int) {
	t.Helper()
	seen := map[string]int{}
	for _, l := range all {
		seen[l]++
	}
	for _, l := range want {
		if seen[l] == 0 {
			t.Fatalf("%q was lost (%d lines written, %d crashes)", l, len(want), crashes)
		}
	}
	dups := 0
	for _, n := range seen {
		dups += n - 1
	}
	if dups > crashes*batch {
		t.Fatalf("%d duplicates after %d crashes with batches of %d; at most one batch per crash is allowed", dups, crashes, batch)
	}
}
