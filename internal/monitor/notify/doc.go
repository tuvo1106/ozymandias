// Package notify delivers monitor notifications (webhook, Discord, email, log)
// with retries, off the evaluation path so a slow notifier never delays
// alerting.
//
// Status: arrives in M6 (docs/plan/M6-monitors.md); this file marks its place in
// the architecture until then.
package notify
