package tracerecv

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/sampler"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

var start = time.UnixMicro(1_790_000_000_000_000)

type obs struct {
	mu   sync.Mutex
	seen []wire.Span
}

func (o *obs) Observe(s []wire.Span) { o.mu.Lock(); o.seen = append(o.seen, s...); o.mu.Unlock() }

type sink struct {
	mu   sync.Mutex
	kept []wire.Span
}

func (s *sink) SubmitSpans(sp []wire.Span) {
	s.mu.Lock()
	s.kept = append(s.kept, sp...)
	s.mu.Unlock()
}

// decider keeps chunks whose first span's service is "keep".
type decider struct{}

func (decider) Decide(c []wire.Span, _ time.Time) sampler.Reason {
	if c[0].Service == "keep" {
		return sampler.Priority
	}
	return sampler.Dropped
}
func (decider) Rates() map[string]float64 { return map[string]float64{"service:keep,env:dev": 0.5} }

func spanJSON(service string, i int) string {
	return fmt.Sprintf(`{"trace_id":"%032x","span_id":"%016x","service":%q,"name":"http.request","resource":"GET /","type":"web",
	"start":%d,"duration":1000,"error":0,"metrics":{"_top_level":1}}`, i+1, i+1, service, start.UnixMicro())
}

func body(chunks ...string) string {
	return `{"tracer":{"lang":"test"},"traces":[` + strings.Join(chunks, ",") + `]}`
}

func setup(opts Options) (*Handler, *obs, *sink) {
	o, s := &obs{}, &sink{}
	opts.Observer, opts.Sink = o, s
	if opts.Decider == nil {
		opts.Decider = decider{}
	}
	opts.Clock = testutil.NewFakeClock(start)
	return New(opts), o, s
}

func post(h http.Handler, b string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/traces", strings.NewReader(b))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// Every span is observed; only chunks the decider keeps are forwarded.
func TestHandler_ObservesEverythingForwardsOnlyKept(t *testing.T) {
	h, o, s := setup(Options{Env: "dev", Host: "h1"})
	w := post(h, body("["+spanJSON("keep", 1)+"]", "["+spanJSON("drop", 2)+","+spanJSON("drop", 3)+"]"))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(o.seen) != 3 {
		t.Errorf("observed %d spans, want all 3: statistics must precede sampling", len(o.seen))
	}
	if len(s.kept) != 1 || s.kept[0].Service != "keep" {
		t.Errorf("kept %+v", s.kept)
	}
	var resp response
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Accepted != 3 || resp.Rejected != 0 || resp.RateByService["service:keep,env:dev"] != 0.5 {
		t.Errorf("response %+v", resp)
	}
}

func TestHandler_FillsEnvAndHostWithoutOverwriting(t *testing.T) {
	h, _, s := setup(Options{Env: "dev", Host: "h1"})
	staged := strings.Replace(spanJSON("keep", 2), `"metrics"`, `"meta":{"env":"staging"},"metrics"`, 1)
	post(h, body("["+spanJSON("keep", 1)+","+staged+"]"))
	if s.kept[0].Meta["env"] != "dev" || s.kept[0].Meta["host"] != "h1" {
		t.Errorf("defaults not applied: %v", s.kept[0].Meta)
	}
	if s.kept[1].Meta["env"] != "staging" {
		t.Errorf("an explicit env was overwritten: %v", s.kept[1].Meta)
	}
}

func TestHandler_BadSpansAreRefusedAndCountedGoodOnesKept(t *testing.T) {
	h, _, s := setup(Options{})
	bad := strings.Replace(spanJSON("keep", 2), `"duration":1000`, `"duration":-5`, 1)
	w := post(h, body("["+spanJSON("keep", 1)+","+bad+"]"))
	var resp response
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != 200 || resp.Accepted != 1 || resp.Rejected != 1 || len(s.kept) != 1 {
		t.Errorf("%d %+v kept=%d", w.Code, resp, len(s.kept))
	}
}

func TestHandler_Errors(t *testing.T) {
	h, _, _ := setup(Options{MaxBody: 2000})
	for name, c := range map[string]struct {
		body string
		hdr  []string
		code int
	}{
		"not json":   {"{", nil, 400},
		"no traces":  {`{"tracer":{}}`, nil, 400},
		"too big":    {body("[" + strings.Repeat(spanJSON("keep", 1)+",", 20) + spanJSON("keep", 1) + "]"), nil, 413},
		"bad gzip":   {"not gzip", []string{"Content-Encoding", "gzip"}, 400},
		"empty body": {"", nil, 400},
	} {
		if w := post(h, c.body, c.hdr...); w.Code != c.code {
			t.Errorf("%s: %d, want %d", name, w.Code, c.code)
		}
	}
}

func TestHandler_GzipBodyAndItsDecompressedSizeIsBounded(t *testing.T) {
	h, _, s := setup(Options{MaxBody: 1500})
	gz := func(b string) string {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write([]byte(b))
		_ = zw.Close()
		return buf.String()
	}
	if w := post(h, gz(body("["+spanJSON("keep", 1)+"]")), "Content-Encoding", "gzip"); w.Code != 200 || len(s.kept) != 1 {
		t.Fatalf("gzip body: %d", w.Code)
	}
	// A bomb: tiny on the wire, over the limit once inflated.
	if w := post(h, gz(body("["+strings.Repeat(spanJSON("keep", 1)+",", 30)+spanJSON("keep", 1)+"]")), "Content-Encoding", "gzip"); w.Code != 413 {
		t.Errorf("a gzip bomb: %d, want 413", w.Code)
	}
}

// With every slot taken the handler answers 429 at once instead of queueing.
func TestHandler_BusyIs429(t *testing.T) {
	h, _, _ := setup(Options{MaxConcurrent: 2})
	h.sem <- struct{}{}
	h.sem <- struct{}{}
	w := post(h, body())
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Errorf("%d %v", w.Code, w.Header())
	}
	<-h.sem
	if w := post(h, body()); w.Code != 200 {
		t.Errorf("after a slot freed: %d", w.Code)
	}
}

func TestHandler_ConcurrentRequests(t *testing.T) {
	h, _, s := setup(Options{MaxConcurrent: 64})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			post(h, body("["+spanJSON("keep", i)+"]"))
		}()
	}
	wg.Wait()
	if len(s.kept) != 50 {
		t.Errorf("kept %d", len(s.kept))
	}
}
