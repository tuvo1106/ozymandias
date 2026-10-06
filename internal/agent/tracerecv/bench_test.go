package tracerecv

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/aggregator"
	"github.com/tuvo1106/ozymandias/internal/agent/concentrator"
	"github.com/tuvo1106/ozymandias/internal/agent/sampler"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

type nullSink struct{}

func (nullSink) SubmitSpans([]wire.Span) {}

// BenchmarkHandler is the agent's whole trace path for one request of 10
// five-span traces: decode, validate, normalize, statistics through the real
// aggregator, samplers. spans/s is what the notes record.
func BenchmarkHandler(b *testing.B) {
	clk := testutil.NewFakeClock(start)
	agg := aggregator.New(aggregator.Options{Started: start, Clock: clk})
	h := New(Options{
		Observer: concentrator.New(concentrator.Options{Sink: agg}),
		Decider:  sampler.New(sampler.Options{}), Sink: nullSink{}, Clock: clk,
	})
	var chunks []string
	n := 0
	for c := 0; c < 10; c++ {
		var spans []string
		for s := 0; s < 5; s++ {
			n++
			top := 0
			if s == 0 {
				top = 1
			}
			spans = append(spans, fmt.Sprintf(`{"trace_id":"%032x","span_id":"%016x","service":"api","name":"http.request","resource":"GET /items/:id","type":"web",
			"start":%d,"duration":%d,"error":0,"meta":{"env":"dev","http.status_code":"200","http.method":"GET"},"metrics":{"_top_level":%d,"_sampling_priority":0}}`,
				c+1, n, start.UnixMicro(), 1000+n, top))
		}
		chunks = append(chunks, "["+strings.Join(spans, ",")+"]")
	}
	body := body(chunks...)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/traces", strings.NewReader(body)))
		if w.Code != 200 {
			b.Fatal(w.Code)
		}
	}
	b.ReportMetric(float64(50)*float64(b.N)/b.Elapsed().Seconds(), "spans/s")
	_ = time.Second
}
