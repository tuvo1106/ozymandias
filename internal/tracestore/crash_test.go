package tracestore

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// copyDir snapshots a live store directory, which is what the disk holds the
// instant after a kill -9: whatever was fsynced, and nothing of the process's memory.
func copyDir(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.Walk(from, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(to, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(to, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// An Append that returned nil is on disk: the process is "killed" (the store is never
// closed) and a second store opened on a copy of the directory has every span, the
// indexes and the service-map counter. This is what lets ozyd answer 202.
func TestCrash_AcknowledgedSpansSurvive(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(Options{Dir: dir, Clock: testutil.NewFakeClock(t0)}) // synced
	if err != nil {
		t.Fatal(err)
	}
	const n = 50
	for i := 1; i <= n; i++ {
		spans := []wire.Span{sp(i, 1, "api", time.Duration(i)*time.Second, time.Millisecond, func(s *wire.Span) { s.Error = i % 2 }),
			sp(i, 2, "worker", time.Duration(i)*time.Second, time.Millisecond, child(1))}
		if err := s.Append(ctx, spans); err != nil {
			t.Fatal(err)
		}
	}
	snap := t.TempDir()
	copyDir(t, dir, snap)
	t.Cleanup(func() { _ = s.Close() })

	r, err := Open(Options{Dir: snap, Clock: testutil.NewFakeClock(t0)})
	if err != nil {
		t.Fatalf("reopening after the crash: %v", err)
	}
	defer func() { _ = r.Close() }()
	for i := 1; i <= n; i++ {
		if got := must(r.Trace(ctx, tid(i))); len(got) != 2 {
			t.Fatalf("trace %d: %d spans after the crash, want 2", i, len(got))
		}
	}
	if res := must(r.Search(ctx, Filter{}, 0, 0, MaxLimit, "")); len(res.Traces) != 2*n {
		t.Errorf("%d index entries, want %d", len(res.Traces), 2*n)
	}
	if res := must(r.Search(ctx, Filter{Service: "api", ErrorsOnly: true}, 0, 0, MaxLimit, "")); len(res.Traces) != n/2 {
		t.Errorf("%d error entries, want %d", len(res.Traces), n/2)
	}
	if e := must(r.ServiceEdges(ctx, "", 0, t0.Add(time.Hour).UnixMicro())); len(e) != 1 || e[0].Calls != n {
		t.Errorf("edges after the crash: %+v", e)
	}
}

func BenchmarkAppend(b *testing.B) {
	for _, sync := range []bool{false, true} {
		b.Run(fmt.Sprintf("sync=%v", sync), func(b *testing.B) {
			s, _ := open(b, func(o *Options) { o.NoSync = !sync })
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				i++
				// A realistic request: an entry span, a few children, one cross-service.
				spans := []wire.Span{sp(i, 1, "api", 0, time.Millisecond),
					sp(i, 2, "api", 0, time.Microsecond*300, child(1), inner), sp(i, 3, "api", 0, time.Microsecond*200, child(1), inner),
					sp(i, 4, "worker", 0, time.Millisecond, child(1))}
				if err := s.Append(ctx, spans); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Seconds())/float64(i)*1e6, "us/trace")
		})
	}
}

func BenchmarkSearch(b *testing.B) {
	s, _ := open(b, nil)
	var batch []wire.Span
	for i := 1; i <= 20000; i++ {
		batch = append(batch, sp(i, 1, []string{"api", "web", "worker", "billing"}[i%4], time.Duration(i)*time.Millisecond, time.Duration(i%500)*time.Millisecond))
		if len(batch) == 1000 {
			if err := s.Append(ctx, batch); err != nil {
				b.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	b.Run("latest-50-all-services", func(b *testing.B) {
		for b.Loop() {
			if r, err := s.Search(ctx, Filter{}, 0, 0, 50, ""); err != nil || len(r.Traces) != 50 {
				b.Fatal(err)
			}
		}
	})
	b.Run("slow-in-one-service", func(b *testing.B) {
		for b.Loop() {
			if _, err := s.Search(ctx, Filter{Service: "api", MinDurationUs: 400_000}, 0, 0, 50, ""); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("trace-by-id", func(b *testing.B) {
		for b.Loop() {
			if got, err := s.Trace(ctx, tid(777)); err != nil || len(got) != 1 {
				b.Fatal(err)
			}
		}
	})
}
