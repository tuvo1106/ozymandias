# Benchmarks

Measured numbers for the hot paths, so that later milestones can be compared
against something rather than argued about. Every number here is reproducible
with the command above it. Numbers are only meaningful next to the machine
that produced them, so the machine is recorded too.

**Machine:** Apple M5 (10 cores, 32 GB), macOS 26.6.2 (darwin/arm64), Go 1.27.1.
M1 figures were taken on 2026-09-19 at commit `b7ff955`; the M2 storage
figures on 2026-09-23 at commit `838c398`.

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

## The storage engine (M2)

```
go test -run '^$' -bench . -benchmem ./internal/tsdb/...
```

### Chunk encoding — `internal/tsdb/chunkenc`

| Benchmark | ns/op | B/op | allocs/op | Reading |
|---|---:|---:|---:|---|
| `Appender_Append/constant` | 6.1 | 1 | 0 | a counter that does not move: one bit for the time, one for the value |
| `Appender_Append/wobbling` | 43.4 | 16 | 0 | a gauge changing in its low bits — the window-reuse path |
| `Appender_Append/random` | 71.8 | 16 | 0 | values sharing nothing: the full leading/trailing header every sample |
| `Iterator_Next` | 5 029 | 32 | 1 | decoding a full 120-sample chunk — 110 MB/s, 42 ns/sample |

Compression, against the 16 bytes an `(int64, float64)` pair would cost:

| Shape | Bytes/sample | Ratio |
|---|---:|---:|
| constant | 0.37 | **43.6×** |
| gauge wobbling in its low bits | 4.59 | 3.5× |
| unrelated values | 7.31 | 2.2× |

The constant case is the one that matters: most series in a real system are a
counter at a steady interval, and it costs two bits.

These are 10–18% faster than first measured, for an unglamorous reason: the
bitstream's byte-aligned fast path tested the wrong value and had never once
executed, so every bit of every sample — including the 64-bit raw float that
path exists for — went through the per-bit loop. The encoded bytes are
identical either way, which is why the golden chunk did not move and why
nothing noticed. The random case, which writes the most full bytes, gained the
most.

### Write-ahead log — `internal/tsdb/wal`

| Benchmark | ns/op | Reading |
|---|---:|---|
| `WAL_Log/sync=false/record=64B` | 2 020 | buffered write, 32 MB/s |
| `WAL_Log/sync=false/record=4096B` | 5 414 | buffered write, 757 MB/s |
| `WAL_Log/sync=true/record=64B` | 3 938 929 | **one fsync** |
| `WAL_Log/sync=true/record=4096B` | 3 917 436 | one fsync — the size barely matters |
| `WAL_LogBatchThenSync/records=1` | 3 841 156 | 3.8 ms per record |
| `WAL_LogBatchThenSync/records=100` | 4 781 832 | 48 µs per record |
| `WAL_LogBatchThenSync/records=1000` | 6 735 261 | **6.7 µs per record** |
| `Reader_Next` | 57 662 | replaying 1000 records — 1.1 GB/s |

This is the group-commit argument as a measurement. An fsync costs ~3.9 ms
whatever it is flushing, so the only lever is how many records share one:
batching a thousand makes a record **570× cheaper** than syncing each. It is
also why `wal_sync_on_append` can default to on — an agent batch is thousands
of samples, so the durability is nearly free. It would not be if ozyd
acknowledged one sample at a time.

### Head — `internal/tsdb/head`

| Benchmark | ns/op | B/op | allocs/op | Reading |
|---|---:|---:|---:|---|
| `Head_Append/series=1` | 102 | 77 | 3 | one sample, no WAL |
| `Head_Append/series=100` | 118 | 78 | 3 | more series costs almost nothing |
| `Head_Append/series=10000` | 142 | 82 | 3 | 100× the series costs 34% more |
| `Head_AppendBatch` | 103 615 | 161 697 | 2 048 | 1000 series in one call — 104 ns/sample |
| `Head_Select/one series` | 4 701 | 8 400 | 16 | 240 samples out of 240 000 |
| `Head_Select/wildcard` | 1 443 344 | 7 859 750 | 11 730 | `host:h1*` — 111 series, 26 640 samples |
| `Head_Select/all` | 5 817 682 | 10 005 824 | 57 115 | 1000 series, 240 000 samples — 24 ns/sample |

Series count barely moves the append cost. Selecting one series out of a
thousand costs 4.7 µs — the index doing its job; the same query without one
would scan all 240 000 samples.

Appends to the head take a single commit lock, so these are also the numbers
for any number of appenders: resolving the series, writing the log record and
applying the sample have to be one critical section, or a block cut can drop
samples it has already acknowledged (M2 notes, "Three ways the store lost
data"). Making it serial cost 102 ns against 106 before — an uncontended mutex
— and the fsync it contains, when there is one, is four orders of magnitude
larger anyway.

### Blocks — `internal/tsdb/block`

| Benchmark | ns/op | Reading |
|---|---:|---|
| `Write/series=100` | 22 675 999 | 24 000 samples to disk, fsynced: 0.94 µs/sample |
| `Write/series=1000` | 31 274 992 | 240 000 samples: 0.13 µs/sample |
| `Open/series=100` | 37 419 | parse the whole index |
| `Open/series=1000` | 128 783 | 129 ns per series |
| `Select/one series, whole block` | 14 588 | 240 samples |
| `Select/one series, short window` | 7 575 | 11 samples — **half the cost** |
| `Select/all series, whole block` | 11 634 260 | 240 000 samples, 48 ns/sample |

On disk, with the realistic shape (a shared long metric name and environment,
one varying tag):

| | Bytes | Share |
|---|---:|---:|
| `chunks.dat` | 1 118 200 | 96% |
| `index.dat` | 49 680 | **4%** |
| Total per sample | **4.87 B** | vs 16 B raw |

The index being 4% of the block is the symbol table earning its place. The
short-window query costing half of the whole-block one is the per-chunk time
ranges earning theirs.

### The real store against the naive one

The same operations, both engines, same machine:

| Operation | naive (SQLite) | tsdb | Ratio |
|---|---:|---:|---:|
| Append, 1 series | 20 854 ns | 102 ns | **205× faster** |
| Append, 1000 series | 6 236 494 ns | 103 615 ns | **60× faster** |
| Select, one series of 240 samples | 89 693 ns | 4 701 ns | **19× faster** |

The naive store is not a straw man — it is indexed SQLite with prepared
statements — which is the point. A row per sample costs what a row per sample
costs, and the gap is what the chunk encoding and the inverted index buy.

### The whole store — `internal/tsdb/db`

Samples per second through `Append`, which is intake's actual cost. The
dimension that matters is how many samples share one fsync:

| sync | batch | goroutines | ns/sample | samples/sec |
|---|---:|---:|---:|---:|
| on | 100 | 1 | 38 969 | 25 661 |
| on | 1 000 | 1 | 4 317 | **231 638** |
| on | 10 000 | 1 | 609 | 1 641 320 |
| on | 1 000 | 8 | 4 470 | 223 723 |
| on | 1 000 | 64 | 4 902 | 204 018 |
| off | 1 000 | 1 | 312 | 3 207 454 |
| off | 1 000 | 8 | 374 | 2 673 249 |
| off | 1 000 | 64 | 409 | 2 446 912 |

Two things to read here. Batch size is everything: the same store does 25k or
1.6M samples/sec depending only on how many samples one fsync covers, because
an fsync costs ~4 ms whatever it flushes. And concurrency is nothing — 64
appenders are slightly *slower* than one, because the fsync happens inside the
commit lock, so a second appender queues behind the first rather than sharing
its flush. That is the honest limit of the current design: a real group commit
would let the queued appenders ride along on one fsync. With one agent posting
a large batch every 10 seconds it does not matter; with sixty agents it would,
and the workaround today is `wal_sync_on_append: false`, which is group commit
with a bounded window of loss.

Restart cost, measured rather than extrapolated (`OZY_BIG_REPLAY=1`):

| | |
|---|---|
| Log written | 1.00 GiB — 76 790 000 samples across 10 000 series |
| Replay | **3.79 s** (0.26 GiB/sec), all 76 790 000 samples restored |

The criterion was under 30 seconds for a gigabyte. Replay is dominated by
re-encoding chunks, not by reading the file: the log reader alone does 1.1
GB/s.

## Distributions (M2 part two)

### The sketch — `internal/sketch`

| Operation | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `Add` | **11.2** | 0 | 0 |
| `Add`, values spanning 10^8 | 11.3 | 0 | 0 |
| `Add`, monotonically falling | 9.1 | 0 | 0 |
| `Merge` (10 000 observations) | 1 958 | 6 352 | 3 |
| `Quantile` (100 000 observations) | 475 | 0 | 0 |

`Add` is one log, one ceil and a slice index, and it allocates nothing in the
steady state. `Quantile` costs the *width* of the store, not the number of
observations: a sketch of a billion points answers as fast as one of a
thousand.

Two of these numbers moved a long way during development, and both moves came
from benchmarks rather than from tests:

| | before | after |
|---|---:|---:|
| `Merge` | 158 420 ns, 1 776 939 B, 401 allocs | 1 958 ns, 6 352 B, 3 allocs |
| `Add`, falling stream | 369.1 ns, 4 217 B | 9.1 ns, 0 B |

`Merge` reallocated the destination on every ascending index — quadratic in
the width of the source. `Add` on a falling stream recomputed a collapse whose
answer was the floor it already had, copying 16 KiB per observation to achieve
nothing. Neither was visible to any correctness test; both were obvious the
moment there was a number.

### The sketch store — `internal/sketchstore`

| | value |
|---|---|
| Encode, 10 observations (10 buckets) | 316 ns → **74 bytes** stored |
| Encode, 300 observations (156 buckets) | 5.2 µs → **209 bytes** stored |
| Encode, 10 000 observations (255 buckets) | 6.9 µs → **403 bytes** stored |
| Decode one sketch | 3.4 µs |
| `Append` 100 series × 1 bucket (fsync'd) | 5.4 ms |
| `Read` one series' hour (360 buckets) | 1.24 ms |

The stored sizes are the affordability argument for distributions. Ten
thousand raw float64s are 80 KiB; the sketch that answers *any* quantile over
them is 403 bytes, and it is 403 bytes because it has 255 buckets — not
because it saw ten thousand values. Cost tracks the spread of the
distribution, not the traffic through it, which is what makes one sketch per
series per 10-second bucket affordable at cardinality.

Encoding a 300-observation sketch costs nearly as much as a 10 000-observation
one because both are dominated by zstd, not by the varints. Payloads under 128
bytes skip compression entirely, which is why the 10-observation case is 16×
faster than the other two.

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
