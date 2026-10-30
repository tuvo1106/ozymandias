package logpipeline

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/testutil"
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
	// Every line shares one received time, so only arrival order says which
	// are oldest: exactly the first ten go, never one picked by map order.
	for i := 0; i < 10; i++ {
		if _, open := p.rails.groups[fmt.Sprintf("id%08d", i)]; open {
			t.Fatalf("id%08d, among the oldest, is still open", i)
		}
	}
	// One request cannot buffer unbounded lines either: absorb stops at the cap.
	for i := 0; i < maxRailsLines*3; i++ {
		p.Process(`[id00000500]   Parameters: {"n"=>1}`, meta())
	}
	if g := p.rails.groups["id00000500"]; g == nil || g.lines != maxRailsLines*3+1 {
		t.Fatal("line count not tracked")
	}
}

// TestRails_FlushKeepsArrivalOrderOnTies: requests timed out together, whose
// first lines share a received time, come out in the order they opened.
func TestRails_FlushKeepsArrivalOrderOnTies(t *testing.T) {
	p, clk := newPipeline(t, Spec{Source: "rails", RailsGroupTimeout: 5 * time.Second, RateLimit: -1})
	m := meta()
	m.Received = clk.Now()
	for i := 0; i < 50; i++ {
		p.Process(fmt.Sprintf(`[tie%05d] Started GET "/x" for 1.1.1.1 at 2026-10-04 12:00:00 +0000`, i), m)
	}
	out := p.Flush(clk.Now().Add(time.Minute))
	if len(out) != 50 {
		t.Fatalf("flushed %d, want 50", len(out))
	}
	for i, l := range out {
		if want := fmt.Sprintf("tie%05d", i); l.Attrs["request_id"] != want {
			t.Fatalf("out[%d] = %v, want %s: ties came out in map order", i, l.Attrs["request_id"], want)
		}
	}
}

// TestRails_ShutdownFlushKeepsArrivalOrder: FlushAll, on shutdown, emits the
// open requests in the order they opened, ties included.
func TestRails_ShutdownFlushKeepsArrivalOrder(t *testing.T) {
	p, clk := newPipeline(t, Spec{Source: "rails", RateLimit: -1})
	m := meta()
	m.Received = clk.Now()
	for i := 0; i < 50; i++ {
		p.Process(fmt.Sprintf(`[end%05d] Started GET "/x" for 1.1.1.1 at 2026-10-04 12:00:00 +0000`, i), m)
	}
	out := p.FlushAll()
	if len(out) != 50 {
		t.Fatalf("flushed %d, want 50", len(out))
	}
	for i, l := range out {
		if want := fmt.Sprintf("end%05d", i); l.Attrs["request_id"] != want {
			t.Fatalf("out[%d] = %v, want %s: shutdown emitted in map order", i, l.Attrs["request_id"], want)
		}
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

// TestEnv_PromotedLikeService: a JSON log's own env becomes its env tag when
// the stream has none, and the stream's wins when it has one.
func TestEnv_PromotedLikeService(t *testing.T) {
	p, _ := newPipeline(t, Spec{Source: "json", RateLimit: -1})
	bare := meta()
	bare.Tags = []string{"team:judge"}
	out := p.Process(`{"message":"job failed","level":"error","env":"prod"}`, bare)
	if len(out) != 1 || !slices.Contains(out[0].Tags, "env:prod") || out[0].Attrs["env"] != nil {
		t.Fatalf("not promoted: %+v", out)
	}
	out = p.Process(`{"message":"job failed","env":"prod"}`, meta()) // the stream says env:dev
	if slices.Contains(out[0].Tags, "env:prod") || out[0].Attrs["env"] != "prod" {
		t.Errorf("the line overrode the stream's env: %+v", out[0])
	}
}

// TestEnv_HostileValuesStayAttributes: a tag is a series to group on, so only
// an environment-shaped value becomes one. Whatever else a line carries stays
// a searchable attribute, and the distinct env tags stay a small, valid set.
func TestEnv_HostileValuesStayAttributes(t *testing.T) {
	p, _ := newPipeline(t, Spec{Source: "json", RateLimit: -1})
	m := meta()
	m.Tags = nil
	hostile := []string{"Prod", "prod staging", "", strings.Repeat("e", 33), "prod,env:x", "../etc", "env:prod", "é", `{"a":1}`}
	envs := map[string]bool{}
	for i, v := range hostile {
		b, _ := json.Marshal(map[string]any{"message": "x", "env": v})
		out := p.Process(string(b), m)
		for _, tag := range out[0].Tags {
			if k, val := wire.SplitTag(tag); k == "env" {
				t.Errorf("hostile[%d] %q became env tag %q", i, v, val)
			}
		}
		if out[0].Attrs["env"] != v {
			t.Errorf("hostile[%d] %q was dropped, not kept as an attribute: %+v", i, v, out[0].Attrs)
		}
	}
	for _, v := range []any{3, true, []string{"prod"}} {
		b, _ := json.Marshal(map[string]any{"message": "x", "env": v})
		if out := p.Process(string(b), m); len(out[0].Tags) != 0 {
			t.Errorf("non-string env %v became a tag: %v", v, out[0].Tags)
		}
	}
	for i := 0; i < 500; i++ {
		b, _ := json.Marshal(map[string]any{"message": "x", "env": fmt.Sprintf("Env %d!", i)})
		for _, tag := range p.Process(string(b), m)[0].Tags {
			envs[tag] = true
		}
	}
	if len(envs) != 0 {
		t.Errorf("500 malformed envs minted %d tags", len(envs))
	}
}

// TestEnv_DistinctValuesAreBounded: a shape is not a set. Well-shaped values
// that keep changing (an app echoing input into env) mint at most
// maxPromotedEnvs tags; the rest stay attributes, and an env already promoted
// keeps being promoted.
func TestEnv_DistinctValuesAreBounded(t *testing.T) {
	p, _ := newPipeline(t, Spec{Source: "json", RateLimit: -1})
	m := meta()
	m.Tags = nil
	tags := map[string]bool{}
	for i := 0; i < 10000; i++ {
		b, _ := json.Marshal(map[string]any{"message": "x", "env": fmt.Sprintf("prod-%d", i)})
		out := p.Process(string(b), m)
		for _, tag := range out[0].Tags {
			tags[tag] = true
		}
		if len(out[0].Tags) == 0 && out[0].Attrs["env"] != fmt.Sprintf("prod-%d", i) {
			t.Fatalf("value %d neither promoted nor kept: %+v", i, out[0])
		}
	}
	if len(tags) != maxPromotedEnvs {
		t.Errorf("10000 distinct envs minted %d tags, want %d", len(tags), maxPromotedEnvs)
	}
	out := p.Process(`{"message":"x","env":"prod-0"}`, m)
	if !slices.Contains(out[0].Tags, "env:prod-0") {
		t.Errorf("an env already promoted stopped being promoted: %+v", out[0])
	}
}

// TestEnv_PromotedAfterRedaction: a value the redactor masks is no longer an
// environment name, so it stays an attribute and never becomes a tag.
func TestEnv_PromotedAfterRedaction(t *testing.T) {
	p, _ := newPipeline(t, Spec{Source: "json", RateLimit: -1, Redact: RedactConfig{Keys: []string{"env"}}})
	m := meta()
	m.Tags = nil
	out := p.Process(`{"message":"x","env":"skabc123"}`, m)
	for _, tag := range out[0].Tags {
		if strings.Contains(tag, "skabc123") {
			t.Fatalf("a value the redactor masks became a tag: %v", out[0].Tags)
		}
	}
	if v, _ := out[0].Attrs["env"].(string); v == "skabc123" || v == "" {
		t.Errorf("env was not redacted in place: %+v", out[0].Attrs)
	}
}

// TestRails_ALateLineOfAnEvictedRequestIsItsOwnEvent: a request evicted at
// the cap was already emitted as incomplete. Its Completed line is emitted on
// its own, with what it says, and opens no group that would evict another.
func TestRails_ALateLineOfAnEvictedRequestIsItsOwnEvent(t *testing.T) {
	var totals Totals
	p, err := New(Spec{Source: "rails", RateLimit: -1}, Options{Clock: testutil.NewFakeClock(received), Totals: &totals})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxRailsGroups+1; i++ { // the last one evicts id00000000
		p.Process(fmt.Sprintf(`[id%08d] Started GET "/x" for 1.1.1.1 at 2026-10-04 12:00:00 +0000`, i), meta())
	}
	out := p.Process(`[id00000000] Completed 200 OK in 9ms`, meta())
	if len(out) != 1 || out[0].Attrs["request_id"] != "id00000000" || out[0].Attrs["late"] != true || out[0].Attrs["status_code"] != int64(200) {
		t.Fatalf("late line: %+v", out)
	}
	if _, open := p.rails.groups["id00000000"]; open {
		t.Error("the late line reopened its request")
	}
	if _, open := p.rails.groups["id00000001"]; !open || len(p.rails.groups) != maxRailsGroups {
		t.Errorf("the late line evicted another request: %d open", len(p.rails.groups))
	}
	if got := totals.Stats().RailsLate; got != 1 {
		t.Errorf("RailsLate = %d, want 1", got)
	}
}

// TestRails_ARequestSlowerThanTheTimeoutKeepsItsStatus: the slowest requests
// are what @status_code and @duration queries look for. The timeout emits the
// request as incomplete; its late Completed line still carries both.
func TestRails_ARequestSlowerThanTheTimeoutKeepsItsStatus(t *testing.T) {
	p, clk := newPipeline(t, Spec{Source: "rails", RailsGroupTimeout: 10 * time.Second, RateLimit: -1})
	m := meta()
	m.Received = clk.Now()
	p.Process(`[slow0001] Started GET "/report" for 1.1.1.1 at 2026-10-04 12:00:00 +0000`, m)
	if out := p.Flush(clk.Now().Add(11 * time.Second)); len(out) != 1 || out[0].Attrs["incomplete"] != true {
		t.Fatalf("not flushed: %+v", out)
	}
	out := p.Process(`[slow0001] Completed 500 Internal Server Error in 12000ms`, m)
	if len(out) != 1 {
		t.Fatalf("late Completed: %+v", out)
	}
	l := out[0]
	if l.Attrs["status_code"] != int64(500) || l.Attrs["duration"] != 12000.0 || l.Status != wire.StatusError || l.Attrs["late"] != true {
		t.Errorf("late Completed lost its status or duration: %+v", l)
	}
	if l.Message != "request slow0001 500" {
		t.Errorf("late Completed message = %q: it should name its request", l.Message)
	}
	if out := p.Flush(clk.Now().Add(time.Hour)); len(out) != 0 {
		t.Errorf("the late line opened a group that flushed later: %+v", out)
	}
}

// TestRails_AReusedIdStartsANewRequest: Rails keeps a client's X-Request-Id,
// so a retry can reuse the id of a request that timed out. Its Started line
// opens a new request rather than being treated as late.
func TestRails_AReusedIdStartsANewRequest(t *testing.T) {
	p, clk := newPipeline(t, Spec{Source: "rails", RailsGroupTimeout: 5 * time.Second, RateLimit: -1})
	m := meta()
	m.Received = clk.Now()
	p.Process(`[retry-0001] Started GET "/pay" for 1.1.1.1 at 2026-10-04 12:00:00 +0000`, m)
	p.Flush(clk.Now().Add(6 * time.Second))
	p.Process(`[retry-0001] Started GET "/pay" for 1.1.1.1 at 2026-10-04 12:00:06 +0000`, m)
	out := p.Process(`[retry-0001] Completed 200 OK in 30ms`, m)
	if len(out) != 1 || out[0].Attrs["late"] != nil || out[0].Message != "GET /pay 200" {
		t.Errorf("the retry was not its own request: %+v", out)
	}
}

// TestRails_AConstantTagLosesNoLines: "[ActiveJob]" matches the request-id
// prefix but never completes. After it times out, every later line is still
// emitted, one event each.
func TestRails_AConstantTagLosesNoLines(t *testing.T) {
	p, clk := newPipeline(t, Spec{Source: "rails", RailsGroupTimeout: 5 * time.Second, RateLimit: -1})
	m := meta()
	m.Received = clk.Now()
	p.Process(`[ActiveJob] Performing MyJob from Async(default)`, m)
	if out := p.Flush(clk.Now().Add(6 * time.Second)); len(out) != 1 {
		t.Fatalf("not flushed: %+v", out)
	}
	n := 0
	for i := 0; i < 5; i++ {
		n += len(p.Process(fmt.Sprintf(`[ActiveJob] Performed MyJob in %dms`, i), m))
	}
	if n != 5 {
		t.Errorf("%d of 5 later lines emitted", n)
	}
}

// TestRails_MemoryOfEndedAndOrderIsBounded: the remembered ids and the
// eviction queue stay bounded however many requests pass through, including
// while one request stays open the whole time.
func TestRails_MemoryOfEndedAndOrderIsBounded(t *testing.T) {
	p, _ := newPipeline(t, Spec{Source: "rails", RateLimit: -1})
	p.Process(`[keepopen] Started GET "/long" for 1.1.1.1 at 2026-10-04 12:00:00 +0000`, meta())
	for i := 0; i < 20*maxRailsGroups; i++ {
		id := fmt.Sprintf("[r%08d]", i)
		p.Process(id+` Started GET "/x" for 1.1.1.1 at 2026-10-04 12:00:00 +0000`, meta())
		if i%3 == 0 { // most finish; the rest are evicted
			p.Process(id+` Completed 200 OK in 1ms`, meta())
		}
	}
	g := p.rails
	if len(g.ended) > maxRailsGroups {
		t.Errorf("ended grew to %d", len(g.ended))
	}
	if len(g.order) > 2*len(g.groups)+64 {
		t.Errorf("order grew to %d for %d open", len(g.order), len(g.groups))
	}
	for _, o := range g.order {
		if o == nil {
			t.Fatal("a nil in the eviction queue")
		}
	}
}

// BenchmarkRails_NewRequestAtTheCap is the hot path the eviction queue exists
// for: every new request id at 1000 open evicts the oldest.
func BenchmarkRails_NewRequestAtTheCap(b *testing.B) {
	p, _ := newPipeline(b, Spec{Source: "rails", RateLimit: -1})
	lines := make([]string, maxRailsGroups+b.N)
	for i := range lines {
		lines[i] = fmt.Sprintf(`[id%08d] Started GET "/x" for 1.1.1.1 at 2026-10-04 12:00:00 +0000`, i)
	}
	for _, l := range lines[:maxRailsGroups] {
		p.Process(l, meta())
	}
	b.ResetTimer()
	for _, l := range lines[maxRailsGroups:] {
		p.Process(l, meta())
	}
}

// TestRails_TheQueueCompactsBelowTheCap: below the cap nothing is evicted, so
// only compaction keeps finished requests from piling up in the queue behind
// one that stays open.
func TestRails_TheQueueCompactsBelowTheCap(t *testing.T) {
	p, _ := newPipeline(t, Spec{Source: "rails", RateLimit: -1})
	p.Process(`[keepopen] Started GET "/long" for 1.1.1.1 at 2026-10-04 12:00:00 +0000`, meta())
	for i := 0; i < 20000; i++ {
		id := fmt.Sprintf("[r%08d]", i)
		p.Process(id+` Started GET "/x" for 1.1.1.1 at 2026-10-04 12:00:00 +0000`, meta())
		p.Process(id+` Completed 200 OK in 1ms`, meta())
	}
	if g := p.rails; len(g.groups) != 1 || len(g.order) > 2*len(g.groups)+64 {
		t.Errorf("%d open, queue of %d", len(g.groups), len(g.order))
	}
}

// TestRails_AFinishedRequestIsNeverEvicted: a request that completed is still
// in the queue until it reaches the front. Eviction must skip it, not emit it
// a second time as incomplete.
func TestRails_AFinishedRequestIsNeverEvicted(t *testing.T) {
	p, _ := newPipeline(t, Spec{Source: "rails", RateLimit: -1})
	p.Process(`[done0001] Started GET "/quick" for 1.1.1.1 at 2026-10-04 12:00:00 +0000`, meta())
	if out := p.Process(`[done0001] Completed 200 OK in 1ms`, meta()); len(out) != 1 {
		t.Fatalf("not completed: %+v", out)
	}
	var evicted []wire.Log
	for i := 0; i < maxRailsGroups+1; i++ {
		evicted = append(evicted, p.Process(fmt.Sprintf(`[id%08d] Started GET "/x" for 1.1.1.1 at 2026-10-04 12:00:00 +0000`, i), meta())...)
	}
	if len(evicted) != 1 || evicted[0].Attrs["request_id"] != "id00000000" {
		t.Errorf("evicted %+v, want only id00000000", evicted)
	}
}

// TestRails_TheRingForgetsOnlyItsOwnEnding: an id ended, reused, and ended
// again has two ring slots. The older slot's recycling must not forget the
// newer ending.
func TestRails_TheRingForgetsOnlyItsOwnEnding(t *testing.T) {
	g := newRailsGrouper(time.Second)
	g.endIncomplete("A")
	delete(g.ended, "A") // a Started line reused it
	g.endIncomplete("B")
	g.endIncomplete("A") // ended again, in a later slot
	// Slots 0 (A), 1 (B), 2 (A again) are used; 999 more fill 3..999 and
	// recycle 0 and 1, leaving A's newer slot 2.
	for i := 0; i < maxRailsGroups-1; i++ {
		g.endIncomplete(fmt.Sprintf("x%d", i))
	}
	if _, ok := g.ended["A"]; !ok {
		t.Error("recycling A's old slot forgot its newer ending")
	}
	if _, ok := g.ended["B"]; ok {
		t.Error("B's slot was recycled but B is still remembered")
	}
	if len(g.ended) > maxRailsGroups {
		t.Errorf("%d remembered", len(g.ended))
	}
}
