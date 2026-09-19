// Package testutil holds the shared test helpers every ozymandias package uses.
// It is the one place the conventions in docs/plan/testing.md §3 are made
// concrete:
//
//   - [FakeClock] — time only moves when the test says so. Use it instead of
//     time.Sleep everywhere; a sleeping test is either slow or flaky, usually
//     both.
//   - [Eventually] — poll a condition with a deadline, for the few places
//     where real concurrency (an HTTP server, a UDP socket) is under test.
//   - [CheckGoroutines] — fail the test if it leaves goroutines behind. Every
//     component with a Close or a context-driven Run gets one of these.
//   - [WriteFiles] / [TempDirWith] — build a directory tree for config and
//     storage tests in one call.
//
// No third-party assertion library: plain `if got != want { t.Errorf(...) }`
// keeps failures readable and the dependency list short.
//
// Helpers take the small [T] interface rather than *testing.T so the helpers
// themselves can be tested against a recording fake (a helper that never
// fails is worse than none).
package testutil
