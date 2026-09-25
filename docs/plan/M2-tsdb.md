# M2 — The real TSDB

**Goal:** replace the naive SQLite `MetricStore` with a from-scratch time-series
engine, and make percentiles real with DDSketch. Same interface, so nothing
above it changes.

**Concepts learned:** write-ahead logging and crash recovery; why TSDBs are
append-only and time-partitioned; delta-of-delta and XOR compression; inverted
indexes and postings intersection; compaction; why percentiles cannot be
averaged and what a mergeable sketch buys you; the real cost of cardinality.

Build order: `chunkenc` → `wal` → `index` (in-memory) → `head` → `block`
writer/reader → `db` (ties head+blocks, implements `MetricStore`) → `compact`
+ retention → `sketch` + `sketchstore` → rollups (may be deferred until after
M3, since it needs the query engine to be useful).

Every on-disk structure starts with a magic number + format version and is
specified byte-for-byte in `docs/formats/`.

## 1. `internal/tsdb/chunkenc` — Gorilla compression

A chunk holds up to 120 samples of one series.

- Bitstream writer/reader (`bstream`) over `[]byte`.
- Header: `uint16` sample count.
- Timestamps (ms): first `t0` as varint; second as varint delta; then
  delta-of-delta `dod`: `0` → bit `0`; fits 14 bits → `10`+14; 17 bits →
  `110`+17; 20 bits → `1110`+20; else `1111`+64. (Prometheus's buckets —
  millisecond timestamps need wider buckets than the paper's seconds.)
- Values: first as raw 64 bits; then XOR with previous: `0` → bit `0`; else
  `1` then: if leading/trailing zero window fits the previous window → `0` +
  meaningful bits; else `1` + 5 bits leading-zero count + 6 bits length
  (0 means 64) + meaningful bits.
- API: `Appender{Append(t,v)}`, `Iterator{Next,At,Err}`, `Chunk.Bytes()`,
  `FromBytes`. Append rejects `t <= last t`.

## 2. `internal/tsdb/wal`

- Directory of segments `%08d.wal`, 32 MiB each (config).
- Record: `len uint32 | type uint8 | crc32c(payload) uint32 | payload`.
  Types: `1 Series{ref uvarint, metric, tags}`, `2 Samples{[]{ref uvarint, t varint, v float64}}`
  (one record per Append batch), `3 Tombstone` (reserved).
- `Log(records…)` buffers; `Sync()` fsyncs. Policy: group commit — fsync every
  `wal.sync_interval` (default 100 ms) *and* `Append` on the store returns only
  after its batch is synced when `wal.sync_on_append: true` (default true; the
  intake can afford it because agent batches are large and infrequent).
- Replay: iterate segments in order; on a bad length/CRC in the **last**
  segment → truncate there and continue (torn tail); in an **earlier** segment
  → return a corruption error naming the segment/offset (operator decides;
  `--wal-repair` flag truncates everything after).
- `Truncate(beforeSegment)` deletes segments wholly covered by persisted
  blocks. A series record needed by later samples is re-logged into a
  checkpoint file first (`checkpoint.%08d`), as Prometheus does.
- This package is reused by LogStore (M4) and the queue (M7) — keep it generic
  over record types.

## 3. `internal/tsdb/index` and `head`

- **Series identity:** `SeriesRef` canonical key = metric + `\x00` + sorted
  `k\x00v` pairs. Series id = monotonically increasing `uint64`, assigned at
  first sight, logged to the WAL.
- **Head:** `stripeSeries` — 256 lock stripes keyed by hash(key) →
  `*memSeries{ref, lset, chunks []*memChunk, app Appender, lastT}`. A chunk is
  cut at 120 samples or when crossing a block-range boundary.
- **MemPostings:** `map[tagKey]map[tagValue][]uint64` (sorted ids) plus the
  special `__name__` key for the metric and an all-series postings list.
- **Selection:** `Equal` → one list; `Wildcard` → union over matching values;
  `NotEqual`/`NotWildcard` → all-series minus match. Intersect smallest-first
  with a galloping merge.
- **Out-of-order rule:** a sample with `t <= series.lastT` is rejected
  (`ErrOutOfOrder`), counted in `ozy.tsdb.ooo_rejected`; a sample older
  than `head.minValidTime` (the last block cut) is rejected
  (`ErrOutOfBounds`). Duplicate `(t,v)` is silently accepted as a no-op so
  agent retries are idempotent.
- **Series limit:** `max_series_per_metric` (default 10 000) — new series
  beyond it are rejected and counted. (Cardinality protection; revisited in M7.)
- Head GC: after a block cut, drop chunks older than the cut and series with
  no chunks left; remove them from postings.

## 4. Blocks

Block range default 2h (config; tests use 1m), aligned to range multiples. The
head is cut when it spans ≥ 1.5 × range: the oldest aligned range is written
out as a block, then WAL is truncated.

```
data/tsdb/
  wal/…
  blocks/<ULID>/
    meta.json   {ulid, version, minTime, maxTime, stats{series,samples,chunks},
                 compaction{level, sources[]}, resolution_s: 0}
    chunks.dat  magic|version| repeated: len uvarint | encoding u8 | data | crc32c
    index.dat   magic|version| symbols | series | postings | postings-offset-table | TOC | crc32c
```

- **index.dat:** *symbols* = sorted unique strings (metric names, tag keys,
  tag values), referenced by ordinal. *series* = per series: id, metric sym,
  tag (k,v) syms, chunk refs `[minT, maxT, offset, len]`. *postings* = per
  (key,value): delta-varint sorted series ids. *offset table* = (key,value) →
  postings offset, sorted for binary search. *TOC* at the tail points to each
  section.
- Write via temp dir + fsync files + fsync dir + atomic rename. A block
  without `meta.json` is garbage from a crash and is deleted at startup.
- Reader: load index fully into memory at open (simple; note mmap as the
  production approach in the docs), read chunks with `pread`.
- **Querier:** `Select` = merge of per-block queriers + head querier for blocks
  overlapping `[from,to]`; series merged by identity; samples of the same
  series from different sources concatenated in time order (sources never
  overlap in time except during compaction handover — dedupe by timestamp).

## 5. `internal/tsdb/compact` and retention

- Leveled: when 3 adjacent blocks of level L exist and their combined range
  is ≤ `max_block_range` (default 54h), merge into one level L+1 block
  (k-way merge of series; re-encode chunks to full 120 samples), then delete
  sources (mark with `tombstone` file → delete, so a crash mid-delete is safe).
- Retention: blocks with `maxTime < now - retention` (default 15d) deleted;
  also a size cap `max_bytes`.
- Runs in one background goroutine, one compaction at a time, cancellable.

## 6. DDSketch — `internal/sketch`, `internal/sketchstore`

- Relative accuracy α = 0.01 → γ = (1+α)/(1−α). Value v>0 maps to bucket
  `k = ceil(log_γ v)`; bucket estimate `2γ^k/(γ+1)`. Separate zero count;
  negative store mirrors the positive one. Tracks exact `count,sum,min,max`.
- Bounded size: max 2048 bins; on overflow collapse the **lowest** bins
  together (tail-accuracy preserved — p95/p99 matter, p1 doesn't).
- `Add(v, weight)`, `Merge(other)` (same γ required), `Quantile(q)`,
  wire encode/decode per wire-protocol §D.
- **Agent:** statsd type `d` now aggregates into a sketch per context per
  bucket and ships via `POST /v1/sketches` (forwarder gains a second endpoint).
- **ozyd:** `SketchStore` on Pebble. Key
  `s | seriesID u64 BE | ts u32 BE` → zstd(varint bins). Series identity and
  tag index are *shared with the TSDB* (a sketch series registers in the same
  head/index with metric type `distribution`), so selection works unchanged.
  Additionally the four scalars are written as ordinary series
  `<metric>.count/.sum/.min/.max` so `avg`/`sum` style queries are cheap.
- Query support (extends the M1 endpoint; grammar in M3): aggregators
  `p50|p75|p90|p95|p99` → per output bucket, **merge** all sketches of all
  selected series in the group, then take the quantile.
- Retention sweeper: Pebble `DeleteRange` per series older than retention.

## 7. Rollups (downsampling) — may land after M3

Compaction of level ≥ 1 blocks additionally writes a sibling block with
`resolution_s: 300`: per series, five chunks tagged with an aggregate kind
(`min,max,sum,count,last`). The querier picks the 5-minute resolution when the
query interval is ≥ 300s and the range starts before raw retention or spans
> 24h; reducer mapping: avg = Σsum/Σcount, etc. Raw retention 15d, rollup
retention 90d (config).

## 8. Wiring

`ozyd.yaml`: `metric_store: tsdb | naive`. Default flips to `tsdb` at the
end of this milestone. `ozy.tsdb.*` self metrics: head series, head
chunks, samples appended, WAL fsync duration, WAL size, blocks, compactions,
compaction duration, ooo_rejected, series_limit_rejected.

`cmd/loadgen`: scenarios `series-churn`, `steady-10k-series`, `statsd-flood`;
`cmd/ozyd tsdb inspect <block>` prints meta, series count, compression
ratio (bytes/sample), top tag cardinalities — a learning tool as much as a
debug tool.

## 9. Test plan

- **L1** bstream edge cases; every dod and XOR branch hit explicitly; WAL
  segment rollover, checkpointing, truncate; postings ops; head chunk cutting
  at 120 and at range boundary; OOO/duplicate rules; series limit; block
  write→read equality; compaction merge incl. series present in only some
  sources; retention by time and by size; sketch quantiles on known
  distributions (uniform, exponential, bimodal, constant, single value);
  collapse behaviour; querier resolution choice.
- **L2** chunk round-trip (arbitrary in-order samples incl. NaN bit patterns,
  ±0, denormals, MaxInt64 gaps); WAL round-trip + any-truncation-yields-valid-prefix;
  postings vs naive sets; sketch relative-error bound for every quantile on
  arbitrary positive inputs; merge == sketch-of-concatenation, commutative,
  associative; block: `read(write(head)) == head`.
- **L3** differential vs the naive store: random workload (series churn, mixed
  matchers, ranges spanning head + multiple blocks + compacted blocks), every
  `Select`, `TagKeys`, `TagValues` equal.
- **L4** fuzz: chunk decoder, WAL reader, index reader, sketch decoder,
  `wire.DecodeSketches` — arbitrary bytes never panic or over-allocate.
- **L5** crash loop ≥ 50 iterations: SIGKILL during append / block cut /
  compaction / WAL truncation → restart → all acknowledged samples present,
  no duplicates, no partial block visible. Torn-write + bit-flip tests for
  every file type (WAL tail, WAL middle, chunks.dat, index.dat, meta.json
  missing). Fault FS: ENOSPC during block write leaves store consistent and
  serving; EIO on fsync surfaces as Append error (never acked).
- **L6** stress: 16 appenders + 4 selectors + background cut/compaction for
  30s under `-race`; per-series monotonic timestamps and total sample count
  invariants. Leak check on `Close()`.
- **L7** format goldens: committed WAL segment, block dir, sketch blob —
  must stay readable forever.
- **L8** in-process pipeline: statsd `d` messages → `p95` query within α of
  exact p95 computed from the raw inputs.
- **L9** smoke v2: adds a distribution metric and a `p95` query; restarts
  ozyd mid-test and asserts continuity.
- **L12** benchmarks: append samples/sec (1, 8, 64 goroutines); bytes/sample
  for realistic gauge, counter and random series; select latency vs series
  count; WAL replay time per GiB; naive-vs-tsdb comparison table in the notes.

## 10. Docs deliverables
`docs/formats/{wal,chunk,block-index,sketch}.md` (byte-level, with worked hex
example of a small chunk); DESIGN.md sections: TSDB write path, read path,
compaction, crash-safety argument (what is durable when, and why), DDSketch,
cardinality; diagrams `tsdb-write-path.mmd`, `tsdb-read-path.mmd`; ADRs:
in-order-only ingestion, index-in-memory, sketches-on-Pebble, fsync policy;
runnable `Example` funcs for chunkenc/wal/sketch/tsdb; operations.md: data dir
layout, retention, `tsdb inspect`, WAL repair; `docs/notes/M2.md` with
measured compression ratios and the crash-safety walkthrough.

## 11. Acceptance criteria

- [ ] `metric_store: tsdb` is default; app-node charts unchanged vs naive (differential test green).
- [ ] Realistic 10s gauge/counter series compress to ≤ 2 bytes/sample (report actual).
- [ ] Crash loop (50×) green in CI; torn-write suite green.
- [ ] `p95:http.request.duration{service:app-node} by {route}` renders and is within 1% relative error of exact in the L8 test.
- [ ] Ingest ≥ 200k samples/sec on the dev machine with sync-on-append (report actual); restart with a 1 GiB WAL replays in < 30s (report actual).
- [ ] Blocks are cut, compacted and expired on schedule in a fake-clock integration test covering 3 simulated days.
- [ ] Docs deliverables complete; `docs/notes/M2.md` has evidence and numbers.
