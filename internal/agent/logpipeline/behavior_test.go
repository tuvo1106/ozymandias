package logpipeline

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

func one(t *testing.T, p *Pipeline, line string, m Meta) wire.Log {
	t.Helper()
	out := p.Process(line, m)
	if len(out) != 1 {
		t.Fatalf("%q produced %d logs", line, len(out))
	}
	return out[0]
}

func TestPipeline_ExcludedLinesAreDroppedAndCounted(t *testing.T) {
	p, _ := newPipeline(t, Spec{Source: "plain", ExcludeAtMatch: []string{`GET /healthz`}})
	if out := p.Process(`"GET /healthz" 200`, meta()); out != nil {
		t.Fatal("excluded line was emitted")
	}
	one(t, p, "GET /orders", meta())
	if s := p.Stats(); s.Excluded != 1 || s.Emitted != 1 || s.Lines != 2 {
		t.Fatalf("%+v", s)
	}
	if _, err := New(Spec{ExcludeAtMatch: []string{"("}}, Options{}); err == nil {
		t.Fatal("bad exclude pattern accepted")
	}
}

func TestPipeline_RateLimitDropsTheExcessAndRefills(t *testing.T) {
	p, clk := newPipeline(t, Spec{Source: "plain", RateLimit: 10})
	m := meta()
	emit := func() int {
		n := 0
		for i := 0; i < 50; i++ {
			m.Received = clk.Now()
			n += len(p.Process("x", m))
		}
		return n
	}
	if got := emit(); got != 10 {
		t.Fatalf("a burst of 50 at 10/s let %d through, want 10", got)
	}
	clk.Advance(500 * time.Millisecond)
	if got := emit(); got != 5 {
		t.Fatalf("after half a second %d passed, want 5", got)
	}
	if s := p.Stats(); s.RateLimited != 85 {
		t.Fatalf("rate limited %d, want 85", s.RateLimited)
	}
}

func TestPipeline_NumericStatusIsACodeNotASeverity(t *testing.T) {
	p, _ := newPipeline(t, Spec{Source: "json"})
	l := one(t, p, `{"level":"warn","message":"m","status":503}`, meta())
	if l.Status != wire.StatusWarn || l.Attrs["status_code"] == nil {
		t.Fatalf("%+v", l)
	}
	if _, ok := l.Attrs["status"]; ok {
		t.Fatal("status left in attrs")
	}
	// A line that has only a numeric status has no severity: it is info, not 503's class.
	if l := one(t, p, `{"message":"m","status":200}`, meta()); l.Status != wire.StatusInfo {
		t.Fatalf("%+v", l)
	}
}

func TestPipeline_TraceIDs(t *testing.T) {
	p, _ := newPipeline(t, Spec{Source: "json"})
	l := one(t, p, `{"message":"m","trace_id":"0AF7651916CD43DD8448EB211C80319C","span_id":"b7ad6b7169203331"}`, meta())
	if l.TraceID != "0af7651916cd43dd8448eb211c80319c" || l.SpanID != "b7ad6b7169203331" {
		t.Fatalf("%+v", l)
	}
	// Datadog's decimal ids are read as decimal: 1234567890123456 is 462d53c8abac0 in hex.
	l = one(t, p, `{"message":"m","dd.trace_id":"1234567890123456","dd.span_id":"255"}`, meta())
	if l.TraceID != "0000000000000000000462d53c8abac0" || l.SpanID != "00000000000000ff" {
		t.Fatalf("%+v", l)
	}
	// An id that is not one is kept as an attribute rather than invented.
	l = one(t, p, `{"message":"m","trace_id":"nope"}`, meta())
	if l.TraceID != "" || l.Attrs["trace_id"] != "nope" {
		t.Fatalf("%+v", l)
	}
}

func TestPipeline_StderrIsOnlyAFallback(t *testing.T) {
	p, _ := newPipeline(t, Spec{Source: "python"})
	m := meta()
	m.Stderr = true
	if l := one(t, p, "INFO:     started", m); l.Status != wire.StatusInfo {
		t.Fatalf("a recognised INFO line on stderr became %s", l.Status)
	}
	if l := one(t, p, "something odd", m); l.Status != wire.StatusError {
		t.Fatalf("an unrecognised stderr line became %s", l.Status)
	}
	m.Stderr = false
	if l := one(t, p, "something odd", m); l.Status != wire.StatusInfo {
		t.Fatalf("an unrecognised stdout line became %s", l.Status)
	}
}

func TestPipeline_ServiceFromMetaBeatsTheLinesOwn(t *testing.T) {
	p, _ := newPipeline(t, Spec{Source: "json"})
	if l := one(t, p, `{"message":"m","service":"other"}`, meta()); l.Service != "svc" || l.Attrs["service"] != "other" {
		t.Fatalf("%+v", l)
	}
	m := meta()
	m.Service = ""
	if l := one(t, p, `{"message":"m","service":"web-api"}`, m); l.Service != "web-api" {
		t.Fatalf("%+v", l)
	}
	if l := one(t, p, `{"message":"m"}`, m); l.Service != "unknown" {
		t.Fatalf("%+v", l)
	}
}

func TestPipeline_CustomGrokWinsAndConvertsTypes(t *testing.T) {
	p, _ := newPipeline(t, Spec{Source: "plain", Grok: []GrokSpec{{
		Pattern: `^(?P<method>[A-Z]+) (?P<path>\S+) (?P<ms_int>\d+)ms (?P<ratio_float>[0-9.]+)$`,
		Message: "${method} ${path}",
	}}})
	l := one(t, p, "GET /a 12ms 0.5", meta())
	if l.Message != "GET /a" || l.Attrs["ms"] != int64(12) || l.Attrs["ratio"] != 0.5 {
		t.Fatalf("%+v", l)
	}
	if _, err := New(Spec{Grok: []GrokSpec{{Pattern: "("}}}, Options{}); err == nil {
		t.Fatal("bad grok accepted")
	}
}

func TestPipeline_BuiltinPatternsAllCompile(t *testing.T) {
	for name := range builtinGroks {
		if _, err := buildParsers("python", nil); err != nil {
			t.Fatal(name, err)
		}
	}
	for src := range builtinParsers {
		if _, err := New(Spec{Source: src}, Options{}); err != nil {
			t.Fatal(src, err)
		}
	}
}

// Interleaved requests each come out whole, in the order they complete.
func TestRails_InterleavedRequestsAreGroupedByID(t *testing.T) {
	p, _ := newPipeline(t, Spec{Source: "rails", RateLimit: -1})
	a, b := "aaaaaaaa-1111", "bbbbbbbb-2222"
	var out []wire.Log
	for _, l := range []string{
		fmt.Sprintf(`[%s] Started GET "/a" for 1.1.1.1 at 2026-10-04 12:00:00 +0000`, a),
		fmt.Sprintf(`[%s] Started GET "/b?x=1" for 1.1.1.1 at 2026-10-04 12:00:00 +0000`, b),
		fmt.Sprintf(`[%s] Completed 500 Internal Server Error in 30ms`, b),
		fmt.Sprintf(`[%s] Completed 200 OK in 5ms`, a),
	} {
		out = append(out, p.Process(l, meta())...)
	}
	if len(out) != 2 || out[0].Message != "GET /b 500" || out[1].Message != "GET /a 200" {
		t.Fatalf("%+v", out)
	}
	if out[0].Status != wire.StatusError || out[0].Attrs["query"] != "x=1" || out[0].Attrs["duration"] != 30.0 {
		t.Fatalf("%+v", out[0])
	}
}

func TestRails_OpenRequestsFlushAfterTheTimeoutAsIncomplete(t *testing.T) {
	p, clk := newPipeline(t, Spec{Source: "rails", RailsGroupTimeout: 5 * time.Second})
	m := meta()
	m.Received = clk.Now()
	p.Process(`[aaaaaaaa-1111] Started GET "/slow" for 1.1.1.1 at 2026-10-04 12:00:00 +0000`, m)
	if out := p.Flush(clk.Now().Add(4 * time.Second)); len(out) != 0 {
		t.Fatal("flushed early")
	}
	out := p.Flush(clk.Now().Add(5 * time.Second))
	if len(out) != 1 || out[0].Attrs["incomplete"] != true || out[0].Status != wire.StatusWarn {
		t.Fatalf("%+v", out)
	}
	if out := p.Flush(clk.Now().Add(time.Hour)); len(out) != 0 {
		t.Fatal("flushed twice")
	}
}

func TestRails_MemoryIsBounded(t *testing.T) {
	p, _ := newPipeline(t, Spec{Source: "rails", RateLimit: -1})
	var emitted int
	for i := 0; i < maxRailsGroups+10; i++ {
		emitted += len(p.Process(fmt.Sprintf(`[id%08d] Started GET "/x" for 1.1.1.1 at 2026-10-04 12:00:00 +0000`, i), meta()))
	}
	if emitted != 10 || len(p.rails.groups) != maxRailsGroups {
		t.Fatalf("emitted %d, %d open", emitted, len(p.rails.groups))
	}
	// One request cannot buffer unbounded lines either: absorb stops at the cap.
	for i := 0; i < maxRailsLines*3; i++ {
		p.Process(`[id00000500]   Parameters: {"n"=>1}`, meta())
	}
	if g := p.rails.groups["id00000500"]; g == nil || g.lines != maxRailsLines*3+1 {
		t.Fatal("line count not tracked")
	}
}

func TestRails_ALineCapStopsBufferingButTheRequestStillEnds(t *testing.T) {
	p, _ := newPipeline(t, Spec{Source: "rails", RateLimit: -1})
	id := "[cafecafe-0001]"
	p.Process(id+` Started GET "/chatty" for 1.1.1.1 at 2026-10-04 12:00:00 +0000`, meta())
	for i := 0; i < maxRailsLines*2; i++ {
		p.Process(fmt.Sprintf(`%s   Parameters: {"n"=>%d}`, id, i), meta())
	}
	if g := p.rails.groups["cafecafe-0001"]; g.fields["params"] != fmt.Sprintf(`{"n"=>%d}`, maxRailsLines-2) {
		t.Fatalf("params = %v: lines past the cap were still read", g.fields["params"])
	}
	out := p.Process(id+` Completed 200 OK in 9ms`, meta())
	if len(out) != 1 || out[0].Message != "GET /chatty 200" {
		t.Fatalf("%+v", out)
	}
}

func TestPipeline_AnExistingStatusCodeIsNotOverwritten(t *testing.T) {
	p, _ := newPipeline(t, Spec{Source: "json"})
	l := one(t, p, `{"message":"m","status":200,"status_code":201}`, meta())
	if l.Attrs["status_code"] != json.Number("201") || l.Attrs["status"] == nil {
		t.Fatalf("%+v", l.Attrs)
	}
}

// A traceback the tailer joined to its ERROR line is one event: the line's
// pattern must still match, with the whole of it as the message.
func TestPipeline_MultilineEventsStillMatchTheirFirstLinesPattern(t *testing.T) {
	p, _ := newPipeline(t, Spec{Source: "python"})
	l := one(t, p, "ERROR watchdog Account email is backed up\nTraceback (most recent call last):\n  File \"x.py\", line 1\nValueError: no", meta())
	if l.Status != wire.StatusError || l.Attrs["logger"] != "watchdog" || l.Message != "Account email is backed up\nTraceback (most recent call last):\n  File \"x.py\", line 1\nValueError: no" {
		t.Fatalf("%+v", l)
	}
}
