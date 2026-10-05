package logpipeline

import "testing"

// The agent's per-line cost: what bounds lines/second per core.
func benchLines(b *testing.B, spec Spec, lines []string) {
	p, _ := newPipeline(b, spec)
	m := meta()
	b.ReportAllocs()
	b.ResetTimer()
	n := 0
	for i := 0; i < b.N; i++ {
		n += len(p.Process(lines[i%len(lines)], m))
	}
	if n == 0 {
		b.Fatal("the pipeline emitted nothing")
	}
}

func BenchmarkPipeline_JSONLine(b *testing.B) {
	benchLines(b, Spec{Source: "json", RateLimit: -1}, []string{
		`{"level":"info","message":"GET /api/orders 200","timestamp":"2026-10-04T12:00:00.123Z","route":"/api/orders","status":200,"ms":12,"user":"bob"}`,
	})
}

func BenchmarkPipeline_JSONLineWithSecretsRedacted(b *testing.B) {
	benchLines(b, Spec{Source: "json", RateLimit: -1}, []string{
		`{"level":"info","message":"login ok for jane.doe@example.test","timestamp":"2026-10-04T12:00:00.123Z","token":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1In0.c2lnbmF0dXJl","password":"hunter2"}`,
	})
}

func BenchmarkPipeline_PlainPythonLine(b *testing.B) {
	benchLines(b, Spec{Source: "python", RateLimit: -1}, []string{
		`2026-10-04 12:00:00,123 INFO app.worker job 4411 finished in 38ms`,
	})
}

// What redaction costs: the same JSON line with every default rule turned off.
// The difference between this and BenchmarkPipeline_JSONLine is the price of
// "no secret is ever stored", paid on every line.
func BenchmarkPipeline_JSONLineNoRedaction(b *testing.B) {
	var spec Spec
	spec.Source, spec.RateLimit = "json", -1
	spec.Redact.Disable = []string{"jwt", "authorization", "url-secret", "kv-secret", "email"}
	benchLines(b, spec, []string{
		`{"level":"info","message":"GET /api/orders 200","timestamp":"2026-10-04T12:00:00.123Z","route":"/api/orders","status":200,"ms":12,"user":"bob"}`,
	})
}

// A backlog is many seconds of lines read in one poll, all stamped with the
// same instant. With no rate_limit set none of it may be dropped: the app never
// exceeded its rate, the agent was just catching up.
func TestPipeline_ABacklogIsNotRateLimitedByDefault(t *testing.T) {
	p, _ := newPipeline(t, Spec{Source: "plain"})
	n := 0
	for i := 0; i < 20_000; i++ {
		n += len(p.Process("a backlog line", meta()))
	}
	if n != 20_000 {
		t.Fatalf("%d of 20000 lines survived a default pipeline", n)
	}
}
