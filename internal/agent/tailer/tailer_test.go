package tailer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/logpipeline"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

type recSink struct {
	sizes []int
	mu    sync.Mutex
	logs  []wire.Log
	fail  error
	sent  int // Send calls
}

func (s *recSink) Send(_ context.Context, logs []wire.Log) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent++
	s.sizes = append(s.sizes, len(logs))
	if s.fail != nil {
		return s.fail
	}
	s.logs = append(s.logs, logs...)
	return nil
}

func (s *recSink) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, l := range s.logs {
		out = append(out, l.Message)
	}
	return out
}

type rig struct {
	t    *testing.T
	dir  string
	clk  *testutil.FakeClock
	sink *recSink
	reg  *Registry
	f    *Files
}

var t0 = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func newRig(t *testing.T, src FileSource, tune ...func(*Options)) *rig {
	t.Helper()
	r := &rig{t: t, dir: t.TempDir(), clk: testutil.NewFakeClock(t0), sink: &recSink{}}
	r.reg, _ = OpenRegistry(filepath.Join(r.dir, "registry", "registry.json"))
	src.Path = filepath.Join(r.dir, "logs", "*.log")
	if src.Source == "" {
		src.Source = "plain"
	}
	if src.Service == "" {
		src.Service = "svc"
	}
	if src.Pipeline.RateLimit == 0 {
		src.Pipeline.RateLimit = -1
	}
	if err := os.MkdirAll(filepath.Join(r.dir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.build(src, tune...)
	return r
}

func (r *rig) build(src FileSource, tune ...func(*Options)) {
	opts := Options{Registry: r.reg, Sink: r.sink, Host: "box", Clock: r.clk}
	for _, f := range tune {
		f(&opts)
	}
	f, err := NewFiles([]FileSource{src}, opts)
	if err != nil {
		r.t.Fatal(err)
	}
	r.f = f
}

func (r *rig) path(name string) string { return filepath.Join(r.dir, "logs", name) }

func (r *rig) write(name, s string) {
	r.t.Helper()
	f, err := os.OpenFile(r.path(name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(s); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) poll(advance time.Duration) {
	r.t.Helper()
	r.clk.Advance(advance)
	r.f.Poll(context.Background())
}

func eq(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestFiles_AppendedLinesAreDeliveredOnce(t *testing.T) {
	r := newRig(t, FileSource{StartPosition: "beginning"})
	r.write("a.log", "one\ntwo\n")
	r.poll(time.Second)
	r.write("a.log", "three\n")
	r.poll(time.Second)
	r.poll(time.Second)
	eq(t, r.sink.messages(), "one", "two", "three")
	l := r.sink.logs[0]
	if l.Service != "svc" || l.Source != "plain" || l.Host != "box" {
		t.Fatalf("%+v", l)
	}
}

func TestFiles_FirstDiscoveryStartsAtTheEndUnlessAsked(t *testing.T) {
	r := newRig(t, FileSource{})
	r.write("a.log", "history\n")
	r.poll(time.Second)
	r.write("a.log", "new\n")
	r.poll(time.Second)
	eq(t, r.sink.messages(), "new")

	b := newRig(t, FileSource{StartPosition: "beginning"})
	b.write("a.log", "history\n")
	b.poll(time.Second)
	eq(t, b.sink.messages(), "history")

	if _, err := NewFiles([]FileSource{{Path: "x", StartPosition: "middle"}}, Options{}); err == nil {
		t.Fatal("bad start_position accepted")
	}
	if _, err := NewFiles([]FileSource{{Path: "["}}, Options{}); err == nil {
		t.Fatal("bad glob accepted")
	}
	if _, err := NewFiles([]FileSource{{Path: "x", MultilineStart: "("}}, Options{}); err == nil {
		t.Fatal("bad multiline accepted")
	}
}

func TestFiles_AFileThatAppearsLaterStartsAtZero(t *testing.T) {
	r := newRig(t, FileSource{})
	r.write("a.log", "old\n")
	r.poll(time.Second)
	r.write("b.log", "first line of a rotation product\n")
	r.poll(DefaultScanInterval)
	eq(t, r.sink.messages(), "first line of a rotation product")
}

func TestFiles_APartialLineWaitsForItsNewline(t *testing.T) {
	r := newRig(t, FileSource{StartPosition: "beginning"})
	r.write("a.log", "whole\npart")
	r.poll(time.Second)
	eq(t, r.sink.messages(), "whole")
	r.write("a.log", "ial\n")
	r.poll(time.Second)
	eq(t, r.sink.messages(), "whole", "partial")
}

func TestFiles_AnUnterminatedLastLineIsEmittedAfterTheIdleTimeout(t *testing.T) {
	r := newRig(t, FileSource{StartPosition: "beginning"})
	r.write("a.log", "no newline")
	r.poll(time.Second)
	r.poll(PartialFlushAfter - 2*time.Second)
	if len(r.sink.messages()) != 0 {
		t.Fatal("emitted before the timeout")
	}
	r.poll(2 * time.Second)
	eq(t, r.sink.messages(), "no newline")
	r.poll(time.Second)
	eq(t, r.sink.messages(), "no newline")
}

func TestFiles_ALongLineIsTruncatedNotBuffered(t *testing.T) {
	r := newRig(t, FileSource{StartPosition: "beginning"})
	r.write("a.log", strings.Repeat("x", MaxLineBytes*2+10)+"\nnext\n")
	r.poll(time.Second)
	m := r.sink.messages()
	if len(m) != 2 || len(m[0]) != MaxLineBytes || m[1] != "next" {
		t.Fatalf("%d messages, first %d bytes", len(m), len(m[0]))
	}
	if r.f.Stats().TruncatedLines != 1 {
		t.Fatalf("%+v", r.f.Stats())
	}
	// A long line arriving in pieces, with no newline yet, is capped as well.
	r2 := newRig(t, FileSource{StartPosition: "beginning"})
	r2.write("a.log", strings.Repeat("y", MaxLineBytes+5))
	r2.poll(time.Second)
	if m := r2.sink.messages(); len(m) != 1 || len(m[0]) != MaxLineBytes {
		t.Fatalf("an over-long unterminated line should be emitted at once, got %d messages", len(m))
	}
	r2.write("a.log", strings.Repeat("y", 100)+"\nafter\n")
	r2.poll(time.Second)
	m = r2.sink.messages()
	if len(m) != 2 || len(m[0]) != MaxLineBytes || m[1] != "after" {
		t.Fatalf("%d messages: %q", len(m), m)
	}
}

func TestFiles_CRLFLinesLoseTheCarriageReturn(t *testing.T) {
	r := newRig(t, FileSource{StartPosition: "beginning"})
	r.write("a.log", "dos\r\n")
	r.poll(time.Second)
	eq(t, r.sink.messages(), "dos")
}

func TestFiles_RotationByRenameKeepsReadingTheOldFileThenTheNew(t *testing.T) {
	r := newRig(t, FileSource{})
	r.write("app.log", "")
	r.poll(time.Second)
	r.write("app.log", "before\n")
	r.poll(time.Second)
	// logrotate: rename, then the app writes the last bits to the old handle's
	// file and creates a new one at the old path.
	if err := os.Rename(r.path("app.log"), r.path("app.log.1")); err != nil {
		t.Fatal(err)
	}
	r.write("app.log.1", "late write to the rotated file\n") // no longer matches *.log
	r.write("app.log", "after\n")
	r.poll(DefaultScanInterval)
	r.poll(time.Second)
	eq(t, r.sink.messages(), "before", "late write to the rotated file", "after")
	// The old file is closed once idle for IdleClose, and its registry entry goes.
	r.poll(DefaultIdleClose)
	r.poll(time.Second)
	if n := r.f.Stats().Files; n != 1 {
		t.Fatalf("%d files still tailed after the idle timeout, want only the new one", n)
	}
}

func TestFiles_CopytruncateResetsTheOffset(t *testing.T) {
	r := newRig(t, FileSource{})
	r.write("a.log", "")
	r.poll(time.Second)
	r.write("a.log", "line one\nline two\n")
	r.poll(time.Second)
	if err := os.Truncate(r.path("a.log"), 0); err != nil {
		t.Fatal(err)
	}
	r.write("a.log", "fresh\n")
	r.poll(time.Second)
	eq(t, r.sink.messages(), "line one", "line two", "fresh")
	if r.f.Stats().Truncations != 1 {
		t.Fatalf("%+v", r.f.Stats())
	}
}

func TestFiles_ADeletedFileIsFinishedThenForgotten(t *testing.T) {
	r := newRig(t, FileSource{})
	r.write("a.log", "")
	r.poll(time.Second)
	r.write("a.log", "last words\n")
	if err := os.Remove(r.path("a.log")); err != nil {
		t.Fatal(err)
	}
	// The open handle still reads what was written before the delete.
	r.poll(DefaultScanInterval)
	eq(t, r.sink.messages(), "last words")
	r.poll(DefaultIdleClose)
	r.poll(time.Second)
	if r.f.Stats().Files != 0 {
		t.Fatal("deleted file still tailed")
	}
	if r.reg.Len() != 0 {
		t.Fatalf("registry still has %d entries for a deleted file", r.reg.Len())
	}
}

func TestFiles_OffsetsAreCommittedOnlyAfterTheSinkAccepts(t *testing.T) {
	r := newRig(t, FileSource{StartPosition: "beginning"})
	r.write("a.log", "one\ntwo\n")
	r.sink.fail = errors.New("intake down")
	r.poll(time.Second)
	r.poll(time.Second)
	if len(r.sink.messages()) != 0 || r.f.Stats().SendErrors != 2 {
		t.Fatalf("%v %+v", r.sink.messages(), r.f.Stats())
	}
	for _, e := range snapshot(r.reg) {
		if e.Offset != 0 {
			t.Fatalf("offset %d committed while the sink was failing", e.Offset)
		}
	}
	r.sink.fail = nil
	r.poll(time.Second)
	eq(t, r.sink.messages(), "one", "two")
	for _, e := range snapshot(r.reg) {
		if e.Offset != int64(len("one\ntwo\n")) {
			t.Fatalf("offset %d after the retry succeeded", e.Offset)
		}
	}
}

func snapshot(r *Registry) map[string]Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]Entry{}
	for k, v := range r.m {
		out[k] = v
	}
	return out
}

// A restart resumes at the committed offset: nothing is skipped, nothing already
// acknowledged is sent again.
func TestFiles_ARestartResumesFromTheRegistry(t *testing.T) {
	r := newRig(t, FileSource{})
	r.write("a.log", "")
	r.poll(time.Second)
	r.write("a.log", "seen\n")
	r.poll(time.Second)
	if err := r.reg.Flush(); err != nil {
		t.Fatal(err)
	}
	// While the agent is down the app writes more.
	r.write("a.log", "while down\n")

	reg2, err := OpenRegistry(filepath.Join(r.dir, "registry", "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	sink2 := &recSink{}
	f2, _ := NewFiles([]FileSource{{Path: r.path("*.log"), Source: "plain", Service: "svc", Pipeline: logpipeline.Spec{RateLimit: -1}}},
		Options{Registry: reg2, Sink: sink2, Clock: r.clk})
	f2.Poll(context.Background())
	r.write("a.log", "after restart\n")
	r.clk.Advance(time.Second)
	f2.Poll(context.Background())
	eq(t, sink2.messages(), "while down", "after restart")
}

func TestFiles_AFailedSendRewindsMultilineAndPartialState(t *testing.T) {
	r := newRig(t, FileSource{StartPosition: "beginning", MultilineStart: `^\d{4}-`})
	r.write("a.log", "2026-01-01 first\n  cont\n2026-01-02 second\npartial")
	r.sink.fail = errors.New("down")
	r.poll(2 * time.Second)
	r.sink.fail = nil
	r.write("a.log", " done\n")
	r.poll(2 * time.Second)
	r.poll(2 * time.Second)
	eq(t, r.sink.messages(), "2026-01-01 first\n  cont", "2026-01-02 second\npartial done")
}

func TestFiles_MultilineJoinsTracebacksAndFlushesAfterTheTimeout(t *testing.T) {
	r := newRig(t, FileSource{StartPosition: "beginning", MultilineStart: `^(INFO|ERROR)`})
	r.write("a.log", "INFO ok\nERROR boom\nTraceback (most recent call last):\n  File \"x.py\", line 1\nValueError: no\n")
	r.poll(0)
	eq(t, r.sink.messages(), "INFO ok") // the error event may still grow
	r.poll(MultilineFlushAfter)
	eq(t, r.sink.messages(), "INFO ok", "ERROR boom\nTraceback (most recent call last):\n  File \"x.py\", line 1\nValueError: no")
}

func TestMultiline_CapsLinesAndBytes(t *testing.T) {
	m := newMultiline(mustRe(`^S`))
	var evs []event
	evs = append(evs, m.add("S head", 1, t0, false)...)
	for i := 0; i < MaxMultilineLines+5; i++ {
		evs = append(evs, m.add("cont", int64(i+2), t0, false)...)
	}
	if len(evs) != 1 || strings.Count(evs[0].text, "\n") != MaxMultilineLines-1 {
		t.Fatalf("%d events, first has %d lines", len(evs), strings.Count(evs[0].text, "\n")+1)
	}
	m = newMultiline(mustRe(`^S`))
	m.add("S head", 1, t0, false)
	big := strings.Repeat("z", MaxMultilineBytes)
	evs = m.add(big, 2, t0, false)
	if len(evs) != 1 || evs[0].text != "S head" {
		t.Fatalf("a line that overflows the byte cap should close the event before it: %+v", evs)
	}
	if got := m.flush(t0, false); len(got) != 0 {
		t.Fatal("flushed before the timeout")
	}
	if got := m.flush(t0.Add(MultilineFlushAfter), false); len(got) != 1 {
		t.Fatal("did not flush at the timeout")
	}
	if got := newMultiline(nil).add("x", 3, t0, false); len(got) != 1 || got[0].end != 3 {
		t.Fatal("no start pattern means one event per line")
	}
}

func TestFiles_RunPollsOnTheTickerAndFlushesOnShutdown(t *testing.T) {
	r := newRig(t, FileSource{StartPosition: "beginning", MultilineStart: `^S`})
	r.write("a.log", "S pending\n")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.f.Run(ctx, time.Second); close(done) }()
	waitFor(t, func() bool { return r.clk.Waiters() > 0 })
	r.clk.Advance(time.Second)
	// The event is pending (multiline); shutdown must deliver it.
	cancel()
	<-done
	eq(t, r.sink.messages(), "S pending")
	if _, err := os.Stat(filepath.Join(r.dir, "registry", "registry.json")); err != nil {
		t.Fatal("registry not written on shutdown:", err)
	}
}

func mustRe(s string) *regexp.Regexp { return regexp.MustCompile(s) }

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFiles_ABacklogIsReadInSlicesAndSentInBatches(t *testing.T) {
	r := newRig(t, FileSource{StartPosition: "beginning"}, func(o *Options) { o.BatchLogs = 100 })
	line := strings.Repeat("a", 99) + "\n"        // 100 bytes
	r.write("a.log", strings.Repeat(line, 11000)) // 1.1 MB
	r.poll(time.Second)
	n := len(r.sink.logs)
	if n == 0 || n > maxPollBytes/100+1 {
		t.Fatalf("one poll delivered %d lines of a 11000-line backlog; it should read about %d bytes at a time", n, maxPollBytes)
	}
	for _, sz := range r.sink.sizes {
		if sz > 100 {
			t.Fatalf("a batch of %d logs exceeded BatchLogs", sz)
		}
	}
	if len(r.sink.sizes) < n/100 {
		t.Fatalf("%d lines went out in %d sends", n, len(r.sink.sizes))
	}
	for i := 0; i < 3; i++ {
		r.poll(time.Second)
	}
	if got := len(r.sink.logs); got != 11000 {
		t.Fatalf("%d lines delivered in the end", got)
	}
}

func TestFiles_ARegistryOffsetPastTheEndOfFileRestartsAtZero(t *testing.T) {
	r := newRig(t, FileSource{})
	r.write("a.log", "short\n")
	fi, _ := os.Stat(r.path("a.log"))
	id, _ := idOf(fi)
	r.reg.Set("file:"+id.String(), Entry{Offset: 9999, LastSeen: t0.Unix()})
	r.poll(time.Second)
	eq(t, r.sink.messages(), "short")
	if r.f.Stats().Truncations != 0 {
		t.Fatal("a stale registry offset is not a truncation: it should be dropped on open")
	}
}

func TestFiles_AFileThatLeavesTheGlobAndComesBackIsNotClosed(t *testing.T) {
	r := newRig(t, FileSource{})
	r.write("a.log", "")
	r.poll(time.Second)
	if err := os.Rename(r.path("a.log"), r.path("a.txt")); err != nil {
		t.Fatal(err)
	}
	r.poll(DefaultScanInterval) // seen as gone
	if err := os.Rename(r.path("a.txt"), r.path("b.log")); err != nil {
		t.Fatal(err)
	}
	r.poll(DefaultScanInterval) // back, under a new name
	r.write("b.log", "still followed\n")
	r.poll(time.Second)
	r.poll(DefaultIdleClose + time.Second) // quiet for longer than IdleClose
	r.poll(time.Second)
	eq(t, r.sink.messages(), "still followed")
	if r.f.Stats().Files != 1 {
		t.Fatal("the file was closed although it was back in the glob")
	}
	for _, e := range snapshot(r.reg) {
		if filepath.Base(e.Path) != "b.log" {
			t.Fatalf("registry path %q was not updated by the rename", e.Path)
		}
	}
}

func TestMultiline_AnEventIsStderrIfItsFirstLineWas(t *testing.T) {
	m := newMultiline(mustRe(`^S`))
	m.add("S first", 1, t0, true)
	m.add("continuation", 2, t0, false)
	if evs := m.flush(t0, true); len(evs) != 1 || !evs[0].stderr {
		t.Fatalf("%+v", evs)
	}
	m.add("S next", 3, t0, false)
	m.add("c", 4, t0, true)
	if evs := m.flush(t0, true); len(evs) != 1 || evs[0].stderr {
		t.Fatalf("%+v", evs)
	}
}

// A daily-rotating logger (winston-daily-rotate-file) writes app-2026-10-04.log
// until midnight, then creates app-2026-10-05.log. Lines written around the
// switch, to either file, must all arrive, in order within each file, however
// the scan and the poll fall relative to midnight.
func TestFiles_MidnightRotationOfADailyFileLosesNothing(t *testing.T) {
	r := newRig(t, FileSource{})
	day1, day2 := "app-2026-10-04.log", "app-2026-10-05.log"
	r.write(day1, "") // present at agent start: tailed from its end
	r.poll(time.Second)
	var want []string
	appendLine := func(name, s string) {
		r.write(name, s+"\n")
		want = append(want, s)
	}
	for i := 0; i < 5; i++ {
		appendLine(day1, fmt.Sprintf("late evening %d", i))
	}
	r.poll(time.Second)
	// Midnight: the new file appears and the logger writes to it, while a straggler
	// still lands in yesterday's, and the agent has not rescanned yet.
	appendLine(day2, "00:00:00 first line of the new day")
	appendLine(day1, "23:59:59 straggler to the old file")
	r.poll(time.Second) // no scan yet: only day1 is open
	appendLine(day2, "00:00:01 second line of the new day")
	r.poll(DefaultScanInterval) // the scan finds day2 and reads it from its start
	r.poll(time.Second)
	got := r.sink.messages()
	have := map[string]bool{}
	for _, m := range got {
		have[m] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("lost %q; got %q", w, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d delivered for %d written: %q", len(got), len(want), got)
	}
	// Within one file, order is preserved.
	var d2 []string
	for _, m := range got {
		if strings.HasPrefix(m, "00:00") {
			d2 = append(d2, m)
		}
	}
	eq(t, d2, "00:00:00 first line of the new day", "00:00:01 second line of the new day")
}
