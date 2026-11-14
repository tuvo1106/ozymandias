// Package auth protects ozyd: API keys for the ingest endpoints, one admin
// password for everything a person uses, and the limits that bound what one key
// can send.
//
// # Two kinds of caller, two kinds of credential
//
// Machines post to /v1/*. They carry a key in the X-Ozy-Key header. A key is 256
// random bits shown once at creation; ozyd stores only its SHA-256, so a copy of
// meta.db cannot be replayed. Keys are revocable and carry their own limits.
// Because the secret is already random, a fast hash is the right one: a password
// hash's cost exists to slow down guessing, and there is nothing here to guess.
//
// People use the UI and /api/*. They log in once with the admin password and get
// a session cookie (HttpOnly, SameSite=Lax). This is one shared password, not
// accounts: good enough for a laptop or a single VPS behind TLS, and documented
// as not multi-user (ADR-0059). Sessions live in memory, so a restart logs
// everyone out, which is the cheaper failure than persisting bearer tokens.
//
// # Limits
//
// Each key has token buckets for requests per second and bytes per second, and
// counters for spans per second and log bytes per day. A bucket refills at its
// rate and holds about a second of burst; a request that does not fit is refused
// with 429 and a Retry-After saying when it would. Zero means unlimited, and a
// key's zero fields take the server's defaults. Every refusal is counted under
// ozy.intake.rejected{reason}, where reason is one of a fixed set: a value from
// outside never becomes a tag.
//
// # What is not here
//
// Per-key series limits and the per-metric tag allow/deny list act on the metric
// store, not on the request, and belong with the consumers (M7 PR 5b).
// Authentication is also off by default: with auth.enabled false, nothing in this
// package runs and ozyd behaves as it did before M7.
package auth
