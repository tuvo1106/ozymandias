// Package clock is the seam between ozymandias and wall-clock time.
//
// Almost everything in an observability pipeline is a function of time: the
// agent's aggregator closes 10-second buckets, the forwarder backs off, the
// TSDB cuts blocks every two hours, monitors evaluate on an interval and go
// NO_DATA after silence. Code that calls time.Now or time.NewTicker directly
// can only be tested by sleeping, which makes tests slow and flaky and makes
// the interesting cases (a bucket boundary, a week of retention) impractical.
//
// So every time-dependent component takes a [Clock] in its constructor.
// Production passes [Real]; tests pass testutil.FakeClock, which only moves
// when the test calls Advance and fires tickers and timers deterministically.
//
// The interface is deliberately the small subset of package time that the
// code actually needs. Adding a method is cheap; if you find yourself
// reaching for time.Now inside a component, take a Clock instead.
package clock
