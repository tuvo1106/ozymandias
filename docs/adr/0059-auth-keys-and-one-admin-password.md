# ADR-0059: API keys for ingest, one admin password for people

- **Status:** Accepted
- **Date:** 2026-11-14

## Context

ozyd has been open: anything that can reach port 9400 can write metrics, read logs and
edit monitors. M7 §4 asks for keys on intake and a password on the rest, good enough for a
laptop or a single VPS, not multi-user. Four things had to be decided: how a key is stored
and compared, what protects the UI and `/api`, how a sender is limited, and how a missing
credential fails.

## Decision

**Keys are random, shown once, and stored as a SHA-256.** A key is `ozy_` plus 256 random
bits (base32). Only the hash is in `meta.db`, so a backup or a leaked volume cannot be
replayed against intake. A plain hash is right, not a password hash (bcrypt, argon2): a
password hash's cost exists to slow down guessing a low-entropy secret, and there is
nothing to guess here. The keyring is held in memory and compares the presented key's hash
with every stored hash in constant time; revocation takes effect in the same process at
once. `last_used_at` is written at most once a minute per key, from a goroutine, so the
metadata database's writer lock (which a dashboard save or a monitor evaluation can hold)
is never waited on by an intake request. The keyring is a map keyed by the hash, so a
lookup costs one hash however many keys, revoked ones included, were ever made. Rejected alternative: JWTs or signed keys with embedded limits —
revocation then needs a deny-list, which is the table we are trying not to need.

**People get one password and a session cookie.** `POST /api/v1/auth/login` takes the
admin password (from an environment variable the config names, never the file) and sets an
HttpOnly, `SameSite=Strict` cookie. The plan said `Lax`; `Strict` costs nothing here
because the UI's HTML is public and every API call is made by the page itself, same-site,
and it removes cross-site POSTs without a CSRF token. Scripts may send the password as a
bearer token. Sessions are in memory (a restart logs everyone out), capped at 1000, and
last 12 hours by default. Every password guess, whether a login form post or a bearer
token on an admin route, goes through one gate: a wrong guess holds it for 300 ms and at
most 8 may wait, the rest get 429. That bounds guessing to a few per second however it is
parallelised. It does *not* make login immune to a flood: a sustained stream of wrong guesses
keeps the 8 places full, and a correct password then also gets 429 until the flood eases.
A session already held is unaffected, since it never reaches the gate, and nothing locks
for a period after the flood (a counter that locked after N failures would). A per-address
limit would avoid the busy answer but is no limit behind a proxy, where every client has
the proxy's address. The public session probe answers for the cookie only, never for a
bearer password: it would be a free oracle. Rejected alternatives: accounts and roles (a project of
their own, and the plan says not multi-user); basic auth (no way to log out, and the
password is sent on every request).

**What is protected is decided by what a path is, closed by default.** `/v1/*` needs a key.
`/api/*` and `/debug/*` need an admin session. `/healthz`, `/readyz`, the login endpoints
and the UI's static files are public. Anything else under `/api/` is admin until someone
says otherwise, so a new endpoint cannot ship open by omission. An ingest key does not open
`/api`, and the admin password does not open ingest.

**Limits are per key, in memory, and refuse with 429 and `Retry-After`.** Requests/s and
bytes/s are token buckets (burst of about a second, but never smaller than one maximum
body, or a legitimate body could never be admitted). Spans/s is a bucket charged after
decoding; log bytes per day is a UTC-day counter that lets one oversized request through on
an empty day rather than wedging the sender for good. An unknown `Content-Length` is
charged as a maximum body. A request refused for its size gives its request token back,
so the retry is not charged twice. Every refusal is counted in
`ozy.intake.rejected{reason}` with a fixed reason set, registered up front.
In a key's own limits zero means "take `auth.defaults`" and a negative value means
unlimited, so a key can opt out of a default (in the defaults, zero is unlimited). A request
refused for its spans or log bytes, like one refused for its size, gives back the request
and byte tokens the gate took, so retries run into the limit that is the real cause.

**Off means absent.** With `auth.enabled: false` (the default) none of this runs and the
key endpoints are not mounted: an open endpoint that mints credentials nothing checks
would be worse than no endpoint. Enabled with no password is a startup error, not a log
line: a missing environment variable must not become an open server.

**The agent keeps data it cannot deliver for want of a key.** A `401` is retried like an
outage, with its own counter (`ozy.agent.forwarder.unauthorized`), instead of dropped like
a malformed request: the key is the operator's to fix, and what was collected meanwhile
sits in the disk buffer. The key is not stored in the buffer, so a corrected key applies to
what was already spooled.

**Environment variable names avoid `OZY_`.** That prefix is reserved for config overrides
and ozyd warns about any `OZY_*` variable that matches no setting, so the defaults are
`OZYMANDIAS_ADMIN_PASSWORD` and `OZYMANDIAS_API_KEY` (the plan said `OZY_API_KEY`).

## Consequences

- Deferred to a following PR, and the plan amended to say so: max series per metric and
  max active series *per key*, and the per-metric tag allow/deny list. They act at the
  consumer, which in queued mode does not see the request, so the key has to travel in the
  queued record; that is its own wire/queue-format change and not a thing to slip in here.
- Login throttling is global, not per client address (see above): a sustained flood of wrong
  guesses makes new logins 429 for as long as it lasts, though it locks nothing out afterwards
  and does not touch held sessions.
- One password means no audit trail of who did what, and rotating it means restarting ozyd.
- TLS is not terminated here. Run it behind something that does, and set
  `auth.secure_cookie`; over plain HTTP the password and the keys cross the network in the clear.
