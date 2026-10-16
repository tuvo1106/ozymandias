// Package notify delivers monitor notifications (webhook, Discord, email, log)
// with retries, off the evaluation path so a slow notifier never delays
// alerting.
//
// # Alerting needs its own reliability story
//
// Monitoring exists for the moment something is broken, and that is the
// moment the network, the chat service or the mail relay is most likely to be
// broken too. So delivery is built on three rules:
//
//   - It is decoupled from evaluation. The scheduler commits an outbox row in
//     the same transaction as the state change that owes it, then hands the
//     row to [Dispatcher.Enqueue], which never blocks. Each notifier has its
//     own queue and worker, so one channel being down delays only itself.
//   - It is at least once. A row is removed only together with its outcome
//     event; a crash or a failed write in between means it is delivered again.
//     Deleting first would trade a possible duplicate for a possible silence,
//     and silence is the failure monitoring cannot afford.
//   - Every attempt is bounded: ten seconds, three tries with backoff, and a
//     4xx (other than 429) is not retried, because the request will not
//     become valid by waiting. The outcome is an event on the monitor
//     (notify_ok, notify_failed) that the UI shows next to the transition.
//
// Secrets — a Discord webhook URL is itself a credential — come only from
// environment variables named in the config, are never stored in SQLite, never
// returned by the API ([Info] carries a name and a type), and are scrubbed from
// delivery errors before those become events.
package notify
