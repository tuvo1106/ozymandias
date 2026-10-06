# Trace store key layout

Written by `internal/tracestore` (Pebble); read by trace lookup, search, the service map and the
retention sweep. Rationale: [ADR-0045](../adr/0045-trace-store-indexes-entry-spans-only.md).
Conventions as in [README.md](README.md). A key is one prefix byte, then its fields; `0` below is
a single NUL byte, which is why `env` and `service` have NUL replaced by `_` before they enter a key
(otherwise one could shift where the other begins).

`~start` is the span's start in unix microseconds, bitwise-inverted (`^uint64(start)`), big-endian.
Start times are positive (the wire layer refuses others), so the inversion is monotone decreasing
and byte order equals newest-first time order. `trace` is the 16-byte trace id, `span` the 8-byte
span id (the hex ids decoded).

| Prefix | Key | Value | Written for | Answers |
|---|---|---|---|---|
| `s` | `s trace span` | zstd(JSON span) | every span | fetch a trace: a prefix scan of `s trace` |
| `e` | `e env 0 service 0 ~start trace span` | JSON summary | `_top_level` spans | list by service and time; filters on name, duration and status run during the scan |
| `r` | `r env 0 service 0 hash8 ~start trace span` | empty | `_top_level` spans | one resource's entries; `hash8` is FNV-64a of the **lower-cased** resource truncated to 256 bytes |
| `x` | `x env 0 service 0 ~start trace span` | empty | entry spans that failed, or whose trace has a failed span in the same request | "errors only" without reading the successes |
| `g` | `g hour4 env 0 parent 0 child` | `uvarint calls, uvarint errors, uvarint dur_sum_us` | edges | service map; written with the merge operator `ozy.tracestore.edge.v1` (addition) |
| `t` | `t hour4 trace` | empty | each trace, under the hour of its earliest span in the batch | retention: expired hours list their traces |
| `v` | `v env 0 service` | empty | each (env, service) with an entry span | which pairs exist, so a search without a service knows which iterators to open |

`hour4` is `start / 3_600_000_000` as a big-endian u32.

## Summary value (`e`)

JSON: `name`, `resource` (cut to 256 bytes on a rune boundary), `duration` (µs), `error` (0/1, the entry
span's own), `trace_error` (0/1, a span of the same trace in the same request failed, omitted when 0),
`status_code` (the entry's `http.status_code`, omitted when absent).

## Search

One iterator per (env, service) over the chosen index (`r` if a resource is given, else `x` if
errors only, else `e`), bounded by `~to` below and `~from - 1` above, merged by a heap on the key
suffix `~start trace span` (32 bytes). That suffix is also the **cursor**: it is unique to one entry, and
resuming seeks to it and skips the entry itself, so pages neither repeat nor skip, even when many
entries share a timestamp. A search stops after examining 100000 entries and returns the cursor of the last one read.

## Edges

An entry span with a `parent_id` and a different service from its parent adds `{1, error?, duration}`
to `g hour env parent child`, with `hour` from the child's start. The parent's service is found in the
same request, then by a point read of `s`, else the child waits in memory for 60 seconds. The edge is
added only if the child's `e` key did not exist before, so resending a batch changes nothing.

## Retention

For each `t` key in an hour before the cutoff, in batches of 200 under the write lock: read the
trace's spans, delete their `e`, `x` and `r` keys (recomputed from the span), delete the span range, then the
`t` key last (so a crash repeats harmless deletes). `g` keys older than the cutoff go by one range delete.
A negative retention keeps everything.
