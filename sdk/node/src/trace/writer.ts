/**
 * The trace writer: a bounded queue of finished chunks, flushed to the agent
 * over HTTP (`POST /v1/traces`, wire-protocol.md §B).
 *
 * Traces are best-effort, and every choice here follows from that:
 *
 * - **Bounded queue, drop-oldest.** When the agent is down the queue fills;
 *   the newest traces are the ones worth keeping, and memory stays bounded
 *   however long the outage lasts. Drops are counted, never silent.
 * - **No retries.** A retry holds spans in memory exactly when the system is
 *   already struggling, and a late trace is worth little. A failed request
 *   drops its chunks and moves on.
 * - **One request in flight.** Flushes that arrive while one is running wait
 *   for the next tick; the queue absorbs the burst.
 * - **Never keeps the process alive.** The timer and the request socket are
 *   `unref`'d. The one exception is the exit flush, which deliberately holds
 *   the loop for at most its budget, because otherwise short scripts would
 *   always lose their last trace.
 * - **`node:http`, not `fetch`.** The SDK wraps `globalThis.fetch` for
 *   tracing; a writer built on it would trace (and recursively flush) its own
 *   traffic.
 * - **No timer while idle.** The flush timer is armed by the first enqueued
 *   chunk, so an initialized-but-quiet process holds nothing.
 *
 * Nothing in this module throws to its caller.
 *
 * @module
 */
import * as http from "node:http";
import { LIMITS, type WireSpan } from "./wire.js";

/** Queue capacity in chunks; the oldest chunk is dropped past it. */
export const MAX_QUEUE_CHUNKS = 1000;
/** A flush fires early once this many chunks are waiting. */
export const FLUSH_AT_CHUNKS = 100;
/** Longest a chunk waits for company before being sent, in ms. */
export const FLUSH_INTERVAL_MS = 1000;
/** Per-request timeout, in ms. */
export const REQUEST_TIMEOUT_MS = 2000;
/** Budget for the flush at process exit, in ms. */
export const EXIT_BUDGET_MS = 1000;
/** A request body stays under the agent's 10 MiB limit with margin. */
export const MAX_BODY_BYTES = 8 * 1024 * 1024;

/** Who is sending, for debugging a mixed fleet (`tracer` in §B). */
export interface TracerInfo {
  lang: string;
  lang_version: string;
  version: string;
}

/** Counters exposed by {@link TraceWriter.stats}. */
export interface WriterStats {
  /** Chunks accepted into the queue. */
  chunksQueued: number;
  /** Chunks the agent answered 200 for. */
  chunksSent: number;
  /** Chunks lost: queue overflow, oversize, non-200 answer, or transport failure. */
  chunksDropped: number;
  /** Requests that failed at the transport level (refused, timeout, bad answer). */
  errors: number;
  /** Chunks waiting right now. */
  queued: number;
}

/** Construction parameters; the intervals are seams for tests. */
export interface WriterOptions {
  host: string;
  port: number;
  info: TracerInfo;
  /** Called with the agent's `rate_by_service` map after every 200 answer. */
  onRates?: (rates: Record<string, number>) => void;
  flushIntervalMs?: number;
  requestTimeoutMs?: number;
  /** Optional sink for SDK-internal events (debug logging). */
  log?: (message: string) => void;
}

/** Queue + flusher for one tracer. */
export class TraceWriter {
  private queue: Array<{ body: string; bytes: number }> = [];
  private timer: NodeJS.Timeout | null = null;
  private inflight: Promise<void> | null = null;
  private closed = false;
  private holdOpen = false;
  private readonly counters = { chunksQueued: 0, chunksSent: 0, chunksDropped: 0, errors: 0 };
  private readonly flushIntervalMs: number;
  private readonly timeoutMs: number;

  /**
   * @param opts - agent address, sender identity and callbacks.
   */
  constructor(private readonly opts: WriterOptions) {
    this.flushIntervalMs = opts.flushIntervalMs ?? FLUSH_INTERVAL_MS;
    this.timeoutMs = opts.requestTimeoutMs ?? REQUEST_TIMEOUT_MS;
  }

  /**
   * Queues one chunk (all spans one process finished for one trace). The
   * chunk is serialized immediately: after this call the tracer may reuse or
   * mutate its spans without racing the flusher, and the queue's memory cost
   * is a known number of bytes.
   *
   * @param chunk - the finished, normalized spans.
   */
  enqueue(chunk: WireSpan[]): void {
    try {
      if (this.closed || chunk.length === 0) return;
      if (chunk.length > LIMITS.maxSpansPerChunk) chunk = chunk.slice(0, LIMITS.maxSpansPerChunk);
      const body = JSON.stringify(chunk);
      const bytes = Buffer.byteLength(body);
      this.counters.chunksQueued++;
      if (this.queue.length >= MAX_QUEUE_CHUNKS) {
        this.queue.shift();
        this.counters.chunksDropped++;
      }
      this.queue.push({ body, bytes });
      if (this.queue.length >= FLUSH_AT_CHUNKS) {
        void this.flush();
      } else {
        this.arm();
      }
    } catch (err) {
      this.counters.errors++;
      this.opts.log?.(`trace enqueue failed: ${String(err)}`);
    }
  }

  /** Counters and queue depth. */
  stats(): WriterStats {
    return { ...this.counters, queued: this.queue.length };
  }

  private arm(): void {
    if (this.timer || this.closed) return;
    this.timer = setTimeout(() => {
      this.timer = null;
      void this.flush();
    }, this.flushIntervalMs);
    this.timer.unref();
  }

  /**
   * Sends what is queued, in requests of bounded size, one at a time. Resolves
   * when the queue it saw has been attempted; never rejects.
   *
   * @returns a promise for the end of the flush.
   */
  flush(): Promise<void> {
    if (this.inflight) return this.inflight;
    if (this.queue.length === 0) return Promise.resolve();
    this.inflight = this.drain().finally(() => {
      this.inflight = null;
      if (this.queue.length > 0) this.arm();
    });
    return this.inflight;
  }

  private async drain(): Promise<void> {
    try {
      while (this.queue.length > 0) {
        const batch: string[] = [];
        let bytes = 0;
        while (this.queue.length > 0 && batch.length < LIMITS.maxChunksPerRequest) {
          const next = this.queue[0]!;
          if (next.bytes > MAX_BODY_BYTES) {
            this.queue.shift();
            this.counters.chunksDropped++;
            continue;
          }
          if (batch.length > 0 && bytes + next.bytes > MAX_BODY_BYTES) break;
          this.queue.shift();
          batch.push(next.body);
          bytes += next.bytes;
        }
        if (batch.length > 0) await this.post(batch);
      }
    } catch (err) {
      this.counters.errors++;
      this.opts.log?.(`trace flush failed: ${String(err)}`);
    }
  }

  private post(chunks: string[]): Promise<void> {
    const body = `{"tracer":${JSON.stringify(this.opts.info)},"traces":[${chunks.join(",")}]}`;
    return new Promise<void>((resolve) => {
      let settled = false;
      const done = (ok: boolean, rates?: unknown) => {
        if (settled) return;
        settled = true;
        if (ok) this.counters.chunksSent += chunks.length;
        else this.counters.chunksDropped += chunks.length;
        if (rates && typeof rates === "object") this.applyRates(rates as Record<string, unknown>);
        resolve();
      };
      try {
        const req = http.request(
          {
            host: this.opts.host,
            port: this.opts.port,
            path: "/v1/traces",
            method: "POST",
            agent: false,
            timeout: this.timeoutMs,
            headers: { "content-type": "application/json", "content-length": Buffer.byteLength(body) },
          },
          (res) => {
            const parts: Buffer[] = [];
            res.on("data", (d: Buffer) => parts.push(d));
            res.on("error", () => done(false));
            res.on("end", () => {
              if (res.statusCode !== 200) return done(false);
              try {
                const parsed = JSON.parse(Buffer.concat(parts).toString("utf8")) as { rate_by_service?: unknown };
                done(true, parsed.rate_by_service);
              } catch {
                // A 200 with an unreadable body still delivered the spans.
                done(true);
              }
            });
          },
        );
        req.on("socket", (sock) => {
          if (!this.holdOpen) sock.unref();
        });
        req.on("timeout", () => req.destroy(new Error("trace request timed out")));
        req.on("error", (err) => {
          this.counters.errors++;
          this.opts.log?.(`trace request failed: ${err.message}`);
          done(false);
        });
        req.end(body);
      } catch (err) {
        this.counters.errors++;
        this.opts.log?.(`trace request failed: ${String(err)}`);
        done(false);
      }
    });
  }

  private applyRates(raw: Record<string, unknown>): void {
    try {
      const rates: Record<string, number> = {};
      for (const [k, v] of Object.entries(raw)) {
        if (typeof v === "number" && Number.isFinite(v)) rates[k] = Math.min(1, Math.max(0, v));
      }
      this.opts.onRates?.(rates);
    } catch {
      // A callback bug must not break delivery.
    }
  }

  /**
   * The exit flush: sends what is queued, holding the event loop open for at
   * most `budgetMs` (the one place this module is allowed to). Anything still
   * queued after the budget is abandoned with the process.
   *
   * @param budgetMs - longest to wait.
   * @returns a promise that resolves by the budget at the latest.
   */
  async flushOnExit(budgetMs = EXIT_BUDGET_MS): Promise<void> {
    if (this.queue.length === 0 && !this.inflight) return;
    this.holdOpen = true;
    let timer: NodeJS.Timeout | undefined;
    try {
      await Promise.race([
        this.flush(),
        new Promise<void>((resolve) => {
          timer = setTimeout(resolve, budgetMs);
        }),
      ]);
    } catch {
      // flush() never rejects; defensive.
    } finally {
      if (timer) clearTimeout(timer);
      this.holdOpen = false;
    }
  }

  /**
   * Flushes with the exit budget, then refuses further chunks and clears the
   * timer. Safe to call twice.
   *
   * @returns a promise that never rejects.
   */
  async close(): Promise<void> {
    await this.flushOnExit();
    this.closed = true;
    if (this.timer) {
      clearTimeout(this.timer);
      this.timer = null;
    }
  }
}
