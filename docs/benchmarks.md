# Benchmarks

Measured numbers for the hot paths, so that later milestones can be compared
against something rather than argued about. Every number here is reproducible
with the command above it. Numbers are only meaningful next to the machine
that produced them, so the machine is recorded too.

**Machine:** Apple M5 (10 cores, 32 GB), macOS 26.6.2 (darwin/arm64), Go 1.27.1.
All figures below were taken on 2026-09-19 at commit `b7ff955`.

Re-record this file whenever a hot path changes. The per-milestone notes
(`docs/notes/M<n>.md`) quote the numbers that mattered for that milestone's
acceptance; this file is the full table.

## Microbenchmarks

```
go test -run '^$' -bench . -benchmem ./internal/agent/... ./internal/tsdb/naive/
```

| Benchmark | ns/op | B/op | allocs/op | Reading |
|---|---:|---:|---:|---|
| `Parse` | 45.2 | 0 | 0 | one 89-byte extended StatsD line, 5 tags — 1.97 GB/s |
| `Server_Throughput` | 1804 | 105 | 0 | one datagram written to loopback and consumed |
| `Aggregator_AddExistingContext` | 268 | 224 | 2 | one counter sample into an existing context |
| `Aggregator_AddParallel` | 102 | 128 | 4 | same, 10 goroutines, 16 contexts (wall-clock per op) |
| `Store_Append` (1 series) | 14 625 | 955 | 21 | one transaction, one sample |
| `Store_Append` (100 series) | 529 933 | 29 496 | 912 | 5.3 µs per series |
| `Store_Append` (1000 series) | 5 871 218 | 295 977 | 9 797 | 5.9 µs per series |
| `Store_Select` | 81 794 | 29 490 | 1 131 | one series, 360 points (one hour of 10s buckets) |

What the numbers say:

- **Parsing is free.** At 45 ns and zero allocations, a single core could parse
  22M lines/s. The parser is not, and will not be, the bottleneck; it slices
  the input buffer rather than copying it (`internal/agent/statsd/parse.go`).
- **Aggregation costs 6× parsing.** `Add` allocates twice per sample building
  the context key, even when the context already exists. At the M1 target of
  50k msgs/s that is 13 ms of CPU per second — irrelevant now, and the first
  place to look if the agent ever becomes CPU-bound.
- **`Server_Throughput` measures the sender, not the receiver.** It writes one
  datagram per iteration and does not verify arrival, so read it as "loopback
  sustains ~554k datagrams/s of *offered* load". The kernel drops most of that
  (see the stress test in `server_test.go`); the honest end-to-end delivery
  number is the flood below, where the SDKs' batching applies.
- **The naive store is transaction-bound.** One series costs 14.6 µs but 1000
  cost 5.9 µs each: roughly 9 µs of that first figure is the commit, paid once
  per batch. This is why the forwarder sends whole flushes rather than single
  series. Absolute numbers are poor on purpose — the naive store is a
  correctness reference with a SQLite row per sample, and M2 replaces it.

## End-to-end acceptance (M1)

The agent and `ozyd` were run natively from `./bin` with the stock
`deploy/*.yaml`, and driven by `cmd/loadgen`.

### Sustained throughput

```
./bin/loadgen -scenario statsd-flood -rate 50000 -duration 60s
```

| Measure | Value |
|---|---|
| Messages sent | 3 000 000 in 60.0 s (49 999 msgs/s) |
| Datagrams sent | 150 000 (20 messages per datagram) |
| Send errors | 0 |
| Messages parsed by the agent | 3 000 000 |
| Queue drops / parse errors | 0 / 0 |
| **Lost** | **0 (0.0000%)** |
| Stored total (queried back) | 3 000 000 across 100 series, 500 000 per 10s bucket |
| Payloads forwarded | 10, 1038 series, 0 retries, 0 rejected |

The target was <0.1% loss; nothing was lost at all. Batching is why: 50k
msgs/s is only 2 500 datagrams/s, far below the rate at which the loopback
buffer overflows. An unbatched client at the same message rate would lose
datagrams on the same machine.

### Intake outage

`ozyd` was killed with SIGTERM for 115 s while the agent kept receiving
2 000 msgs/s across 10 series, then restarted.

| Measure | Value |
|---|---|
| Agent RSS before / peak | 27 296 KB / 27 600 KB (+304 KB) |
| Forwarder queue at peak | 9 973 bytes (cap is 64 MiB) |
| Retries during the outage | 89, with full-jitter backoff |
| Dropped payloads | 0 |
| Queue after restart | drained to 0 within ~60 s |
| Messages sent / stored | 400 000 / 400 000 |
| Buckets missing in the outage window | 0 (every bucket exactly 20 000) |

The agent's memory is flat because the queue holds gzipped payloads and the
backoff caps at 60 s: a 2-minute outage at this rate buffers ten kilobytes,
not megabytes. The queue is capped at 64 MiB and drops oldest-first, so a long
enough outage loses the *oldest* data rather than the newest — the opposite of
what an unbounded queue does when it takes the process down with it.
