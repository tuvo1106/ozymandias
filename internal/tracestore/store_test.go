package tracestore

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

var t0 = time.UnixMicro(1_790_000_000_000_000)

func open(t testing.TB, mod func(*Options)) (*Store, *testutil.FakeClock) {
	t.Helper()
	clk := testutil.NewFakeClock(t0)
	o := Options{Dir: t.TempDir(), NoSync: true, Clock: clk}
	if mod != nil {
		mod(&o)
	}
	s, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clk
}

func tid(n int) string { return fmt.Sprintf("%032x", n) }
func sid(n int) string { return fmt.Sprintf("%016x", n) }

// sp builds an entry (top-level) span unless opts say otherwise.
func sp(trace, span int, service string, startOff time.Duration, dur time.Duration, mod ...func(*wire.Span)) wire.Span {
	s := wire.Span{TraceID: tid(trace), SpanID: sid(span), Service: service, Name: "http.request", Resource: "GET /a", Type: "web",
		Start: t0.Add(startOff).UnixMicro(), Duration: dur.Microseconds(),
		Meta: map[string]string{"env": "dev", "http.status_code": "200"}, Metrics: map[string]float64{wire.MetricTopLevel: 1}}
	for _, m := range mod {
		m(&s)
	}
	return s
}

func child(parent int) func(*wire.Span) { return func(s *wire.Span) { s.ParentID = sid(parent) } }
func inner(s *wire.Span)                { s.Metrics = map[string]float64{}; s.Name = "postgres.query"; s.Type = "db" }
func failed(s *wire.Span)               { s.Error = 1 }

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

var ctx = context.Background()

func TestAppendAndTrace(t *testing.T) {
	s, _ := open(t, nil)
	spans := []wire.Span{sp(1, 1, "api", 0, time.Second), sp(1, 2, "api", 10*time.Millisecond, time.Millisecond, child(1), inner)}
	if err := s.Append(ctx, spans); err != nil {
		t.Fatal(err)
	}
	// Spans of one trace arriving in two requests from two processes assemble.
	if err := s.Append(ctx, []wire.Span{sp(1, 3, "worker", 20*time.Millisecond, time.Millisecond, child(2))}); err != nil {
		t.Fatal(err)
	}
	got := must(s.Trace(ctx, tid(1)))
	if len(got) != 3 || got[0].SpanID != sid(1) || got[2].SpanID != sid(3) {
		t.Fatalf("trace = %+v", got)
	}
	if got := must(s.Trace(ctx, tid(2))); got != nil {
		t.Errorf("an unknown trace returned %v", got)
	}
	if _, err := s.Trace(ctx, "xyz"); err == nil {
		t.Error("a malformed trace id was accepted")
	}
}

func TestAppend_IsIdempotent(t *testing.T) {
	s, _ := open(t, nil)
	spans := []wire.Span{sp(1, 1, "api", 0, time.Second), sp(1, 2, "worker", 0, time.Second, child(1))}
	for i := 0; i < 3; i++ {
		if err := s.Append(ctx, spans); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(must(s.Trace(ctx, tid(1)))); n != 2 {
		t.Errorf("%d spans after three identical appends", n)
	}
	if r := must(s.Search(ctx, Filter{}, 0, 0, 100, "")); len(r.Traces) != 2 {
		t.Errorf("%d index entries", len(r.Traces))
	}
	edges := must(s.ServiceEdges(ctx, "", 0, t0.UnixMicro()+1))
	if len(edges) != 1 || edges[0].Calls != 1 {
		t.Errorf("a resent batch changed the service map: %+v", edges)
	}
}

func TestSearch_OnlyEntrySpansAreIndexedNewestFirstAcrossServices(t *testing.T) {
	s, _ := open(t, nil)
	var spans []wire.Span
	for i := 0; i < 10; i++ {
		svc := []string{"api", "web", "worker"}[i%3]
		spans = append(spans, sp(i+1, i+1, svc, time.Duration(i)*time.Second, time.Millisecond))
		spans = append(spans, sp(i+1, 100+i, svc, time.Duration(i)*time.Second, time.Millisecond, child(i+1), inner))
	}
	if err := s.Append(ctx, spans); err != nil {
		t.Fatal(err)
	}
	r := must(s.Search(ctx, Filter{}, 0, 0, 100, ""))
	if len(r.Traces) != 10 {
		t.Fatalf("%d results, want only the 10 entry spans", len(r.Traces))
	}
	for i := 1; i < len(r.Traces); i++ {
		if r.Traces[i].StartUs > r.Traces[i-1].StartUs {
			t.Fatal("not newest first across services")
		}
	}
	if r := must(s.Search(ctx, Filter{Service: "api"}, 0, 0, 100, "")); len(r.Traces) != 4 {
		t.Errorf("service filter: %d", len(r.Traces))
	}
	if r := must(s.Search(ctx, Filter{Env: "prod"}, 0, 0, 100, "")); len(r.Traces) != 0 {
		t.Errorf("env filter: %d", len(r.Traces))
	}
}

func TestSearch_TimeWindowIsInclusive(t *testing.T) {
	s, _ := open(t, nil)
	var spans []wire.Span
	for i := 0; i < 5; i++ {
		spans = append(spans, sp(i+1, i+1, "api", time.Duration(i)*time.Second, time.Millisecond))
	}
	_ = s.Append(ctx, spans)
	from, to := t0.Add(time.Second).UnixMicro(), t0.Add(3*time.Second).UnixMicro()
	r := must(s.Search(ctx, Filter{}, from, to, 100, ""))
	if len(r.Traces) != 3 || r.Traces[0].StartUs != to || r.Traces[2].StartUs != from {
		t.Errorf("window [%d,%d] returned %+v", from, to, r.Traces)
	}
}

func TestSearch_Filters(t *testing.T) {
	s, _ := open(t, nil)
	spans := []wire.Span{
		sp(1, 1, "api", 1*time.Second, 5*time.Millisecond),
		sp(2, 2, "api", 2*time.Second, 500*time.Millisecond, func(s *wire.Span) { s.Resource = "POST /b"; s.Meta["http.status_code"] = "500"; s.Error = 1 }),
		sp(3, 3, "api", 3*time.Second, 50*time.Millisecond, func(s *wire.Span) { s.Name = "arq.job"; s.Resource = "judge" }),
		// The entry span succeeded, a database child failed: the trace is an error trace.
		sp(4, 4, "api", 4*time.Second, 20*time.Millisecond),
		sp(4, 5, "api", 4*time.Second, time.Millisecond, child(4), inner, failed),
	}
	_ = s.Append(ctx, spans)
	n := func(f Filter) int { return len(must(s.Search(ctx, f, 0, 0, 100, "")).Traces) }
	for name, c := range map[string]struct {
		f    Filter
		want int
	}{
		"all":                {Filter{}, 4},
		"resource":           {Filter{Resource: "POST /b"}, 1},
		"resource, no match": {Filter{Resource: "GET /nope"}, 0},
		"name":               {Filter{Name: "arq.job"}, 1},
		"errors only":        {Filter{ErrorsOnly: true}, 2},
		"errors + resource":  {Filter{ErrorsOnly: true, Resource: "GET /a"}, 1},
		"min duration":       {Filter{MinDurationUs: 100_000}, 1},
		"max duration":       {Filter{MaxDurationUs: 10_000}, 1},
		"duration band":      {Filter{MinDurationUs: 10_000, MaxDurationUs: 100_000}, 2},
		"status":             {Filter{StatusCode: 500}, 1},
	} {
		if got := n(c.f); got != c.want {
			t.Errorf("%s: %d, want %d", name, got, c.want)
		}
	}
}

// Paging with any limit visits every entry exactly once, in the order one big page gives.
func TestSearch_PaginationIsExactForEveryPageSize(t *testing.T) {
	s, _ := open(t, nil)
	var spans []wire.Span
	for i := 0; i < 60; i++ {
		// Many share a timestamp, across services: the cursor must still be exact.
		spans = append(spans, sp(i+1, i+1, []string{"api", "web"}[i%2], time.Duration(i/6)*time.Second, time.Millisecond, func(s *wire.Span) {
			if i%7 == 0 {
				s.Error = 1
			}
		}))
	}
	_ = s.Append(ctx, spans)
	for _, f := range []Filter{{}, {ErrorsOnly: true}, {Resource: "GET /a"}} {
		full := must(s.Search(ctx, f, 0, 0, MaxLimit, ""))
		for _, limit := range []int{1, 2, 7, 13, 59, 60, 61} {
			var got []string
			cur := ""
			for pages := 0; pages < 200; pages++ {
				r := must(s.Search(ctx, f, 0, 0, limit, cur))
				for _, tr := range r.Traces {
					got = append(got, tr.TraceID)
				}
				if cur = r.Next; cur == "" {
					break
				}
			}
			if len(got) != len(full.Traces) {
				t.Fatalf("filter %+v limit %d: %d results, want %d", f, limit, len(got), len(full.Traces))
			}
			for i := range got {
				if got[i] != full.Traces[i].TraceID {
					t.Fatalf("filter %+v limit %d: result %d differs", f, limit, i)
				}
			}
		}
	}
}

// A search that examines too much stops with a cursor that resumes after the last entry read.
func TestSearch_BudgetStopsAndResumes(t *testing.T) {
	s, _ := open(t, nil)
	var spans []wire.Span
	for i := 0; i < 100; i++ {
		spans = append(spans, sp(i+1, i+1, "api", time.Duration(i)*time.Second, time.Duration(i+1)*time.Millisecond))
	}
	_ = s.Append(ctx, spans)
	f := Filter{MinDurationUs: 90_000, MaxExamined: 25}
	var got []string
	cur, rounds := "", 0
	for ; rounds < 20; rounds++ {
		r := must(s.Search(ctx, f, 0, 0, 100, cur))
		if r.Examined > 25 {
			t.Fatalf("examined %d over a budget of 25", r.Examined)
		}
		for _, tr := range r.Traces {
			got = append(got, tr.TraceID)
		}
		if cur = r.Next; cur == "" {
			break
		}
	}
	if len(got) != 11 || rounds < 3 {
		t.Errorf("%d results in %d rounds, want 11 over several budgeted rounds", len(got), rounds)
	}
	if _, err := s.Search(ctx, Filter{}, 0, 0, 10, "not-a-cursor"); err == nil {
		t.Error("a bad cursor was accepted")
	}
}

func TestEdges_ParentInBatchStoredEarlierAndArrivingLater(t *testing.T) {
	s, clk := open(t, nil)
	// 1: parent and child in one batch.
	_ = s.Append(ctx, []wire.Span{sp(1, 1, "api", 0, time.Second), sp(1, 2, "worker", 0, time.Second, child(1))})
	// 2: parent stored first.
	_ = s.Append(ctx, []wire.Span{sp(2, 1, "api", 0, time.Second)})
	_ = s.Append(ctx, []wire.Span{sp(2, 2, "worker", 0, time.Second, child(1), failed)})
	// 3: child first; the parent arrives within the TTL.
	_ = s.Append(ctx, []wire.Span{sp(3, 2, "worker", 0, time.Second, child(1))})
	clk.Advance(10 * time.Second)
	if edges := must(s.ServiceEdges(ctx, "", 0, t0.UnixMicro())); len(edges) != 1 || edges[0].Calls != 2 {
		t.Fatalf("before the late parent: %+v", edges)
	}
	_ = s.Append(ctx, []wire.Span{sp(3, 1, "api", 0, time.Second)})
	// 4: the parent never comes.
	_ = s.Append(ctx, []wire.Span{sp(4, 2, "worker", 0, time.Second, child(9))})
	clk.Advance(2 * time.Minute)
	s.ExpirePending()
	_ = s.Append(ctx, []wire.Span{sp(4, 9, "api", 0, time.Second)}) // too late: the edge was given up on

	edges := must(s.ServiceEdges(ctx, "", 0, t0.UnixMicro()))
	if len(edges) != 1 {
		t.Fatalf("%+v", edges)
	}
	e := edges[0]
	if e.Parent != "api" || e.Child != "worker" || e.Calls != 3 || e.Errors != 1 || e.DurationSumUs != 3*1_000_000 {
		t.Errorf("edge = %+v, want api→worker 3 calls, 1 error, 3s", e)
	}
	if n := s.npend; n != 0 {
		t.Errorf("%d edges still pending", n)
	}
}

func TestEdges_SameServiceEntrySpansAreNotEdgesAndEnvFilters(t *testing.T) {
	s, _ := open(t, nil)
	_ = s.Append(ctx, []wire.Span{
		sp(1, 1, "api", 0, time.Second), sp(1, 2, "api", 0, time.Second, child(1)), // same service
		sp(2, 1, "api", 0, time.Second, func(s *wire.Span) { s.Meta["env"] = "prod" }),
		sp(2, 2, "worker", 0, time.Second, child(1), func(s *wire.Span) { s.Meta["env"] = "prod" }),
	})
	all := must(s.ServiceEdges(ctx, "", 0, t0.UnixMicro()))
	if len(all) != 1 || all[0].Env != "prod" {
		t.Errorf("%+v", all)
	}
	if got := must(s.ServiceEdges(ctx, "dev", 0, t0.UnixMicro())); len(got) != 0 {
		t.Errorf("dev: %+v", got)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	open1 := func() *Store {
		s, err := Open(Options{Dir: dir, Clock: testutil.NewFakeClock(t0)})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := open1()
	_ = s.Append(ctx, []wire.Span{sp(1, 1, "api", 0, time.Second), sp(1, 2, "worker", 0, time.Second, child(1))})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open1()
	defer func() { _ = s.Close() }()
	if len(must(s.Trace(ctx, tid(1)))) != 2 || len(must(s.Search(ctx, Filter{}, 0, 0, 10, "")).Traces) != 2 {
		t.Error("data lost across reopen")
	}
	if len(s.Services()) != 2 {
		t.Errorf("services = %v", s.Services())
	}
	if e := must(s.ServiceEdges(ctx, "", 0, t0.UnixMicro())); len(e) != 1 || e[0].Calls != 1 {
		t.Errorf("edges = %+v", e)
	}
	if err := s.Append(ctx, nil); err != nil {
		t.Error(err)
	}
}

func TestClosedStoreErrorsInsteadOfPanicking(t *testing.T) {
	s, _ := open(t, nil)
	_ = s.Close()
	if s.Append(ctx, []wire.Span{sp(1, 1, "api", 0, 1)}) == nil {
		t.Error("Append on a closed store")
	}
	if _, err := s.Trace(ctx, tid(1)); err == nil {
		t.Error("Trace")
	}
	if _, err := s.Search(ctx, Filter{}, 0, 0, 1, ""); err == nil {
		t.Error("Search")
	}
	if _, err := s.ServiceEdges(ctx, "", 0, 1); err == nil {
		t.Error("ServiceEdges")
	}
	if _, err := s.Sweep(ctx); err == nil {
		t.Error("Sweep")
	}
	if s.Close() != nil {
		t.Error("second Close")
	}
}

func TestSweep_DeletesExpiredTracesAndTheirIndexesOnly(t *testing.T) {
	s, clk := open(t, func(o *Options) { o.Retention = 24 * time.Hour })
	old := []wire.Span{sp(1, 1, "api", 0, time.Second, failed), sp(1, 2, "worker", 0, time.Second, child(1))}
	_ = s.Append(ctx, old)
	clk.Advance(30 * time.Hour)
	fresh := []wire.Span{{}}
	fresh[0] = sp(2, 1, "api", 30*time.Hour, time.Second)
	_ = s.Append(ctx, fresh)

	n, err := s.Sweep(ctx)
	if err != nil || n != 1 {
		t.Fatalf("swept %d, %v", n, err)
	}
	if got := must(s.Trace(ctx, tid(1))); got != nil {
		t.Errorf("the expired trace survived: %d spans", len(got))
	}
	if len(must(s.Trace(ctx, tid(2)))) != 1 {
		t.Error("the fresh trace was deleted")
	}
	for name, f := range map[string]Filter{"entry": {}, "errors": {ErrorsOnly: true}, "resource": {Resource: "GET /a"}} {
		r := must(s.Search(ctx, f, 0, 0, 100, ""))
		for _, tr := range r.Traces {
			if tr.TraceID == tid(1) {
				t.Errorf("%s index still lists the swept trace", name)
			}
		}
	}
	if e := must(s.ServiceEdges(ctx, "", 0, clk.Now().UnixMicro())); len(e) != 0 {
		t.Errorf("an expired edge survived: %+v", e)
	}
	// Reads skip an index entry whose summary is gone, which would hide a leaked
	// key; count the raw keys instead. Only the fresh trace's remain.
	want := map[byte]int{prefixSpan: 1, prefixEntry: 1, prefixResource: 1, prefixError: 0, prefixSeen: 1, prefixEdge: 0}
	for prefix, n := range want {
		if got := s.countKeys(prefix); got != n {
			t.Errorf("%d %q keys left, want %d", got, prefix, n)
		}
	}
	if n, _ := s.Sweep(ctx); n != 0 {
		t.Errorf("a second sweep deleted %d", n)
	}
}

func TestSweep_ManyTracesInBatches(t *testing.T) {
	s, clk := open(t, func(o *Options) { o.Retention = time.Hour })
	var spans []wire.Span
	for i := 0; i < sweepBatch*2+17; i++ {
		spans = append(spans, sp(i+1, 1, "api", 0, time.Millisecond))
	}
	_ = s.Append(ctx, spans)
	clk.Advance(3 * time.Hour)
	if n, err := s.Sweep(ctx); err != nil || n != len(spans) {
		t.Fatalf("swept %d of %d: %v", n, len(spans), err)
	}
	if r := must(s.Search(ctx, Filter{}, 0, 0, 10, "")); len(r.Traces) != 0 {
		t.Error("index entries left behind")
	}
}

func TestConcurrentAppendSearchAndSweep(t *testing.T) {
	s, clk := open(t, func(o *Options) { o.Retention = time.Hour })
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				n := w*1000 + i + 1
				if err := s.Append(ctx, []wire.Span{sp(n, 1, "api", 0, time.Millisecond), sp(n, 2, "worker", 0, time.Millisecond, child(1))}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	var readers sync.WaitGroup
	for r := 0; r < 3; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = s.Search(ctx, Filter{}, 0, 0, 20, "")
				_, _ = s.ServiceEdges(ctx, "", 0, t0.UnixMicro())
				_, _ = s.Sweep(ctx)
				clk.Advance(time.Second)
			}
		}()
	}
	wg.Wait()
	close(stop)
	readers.Wait()
	// Nothing was old enough to expire (the clock moved a few minutes at most), so every trace is there.
	if r := must(s.Search(ctx, Filter{}, 0, 0, MaxLimit, "")); len(r.Traces) != MaxLimit {
		t.Errorf("%d results", len(r.Traces))
	}
}

func (s *Store) countKeys(prefix byte) int {
	it, _ := s.db.NewIter(&pebble.IterOptions{LowerBound: []byte{prefix}, UpperBound: []byte{prefix + 1}})
	n := 0
	for ok := it.First(); ok; ok = it.Next() {
		n++
	}
	_ = it.Close()
	return n
}

func TestSweep_NegativeRetentionKeepsEverything(t *testing.T) {
	s, clk := open(t, func(o *Options) { o.Retention = -1 })
	_ = s.Append(ctx, []wire.Span{sp(1, 1, "api", 0, time.Second)})
	clk.Advance(10 * 365 * 24 * time.Hour)
	if n, err := s.Sweep(ctx); err != nil || n != 0 || len(must(s.Trace(ctx, tid(1)))) != 1 {
		t.Errorf("swept %d, %v", n, err)
	}
}
