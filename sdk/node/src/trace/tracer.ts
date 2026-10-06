/**
 * The tracer: spans, in-process context, the per-trace buffer and head
 * sampling. M5 spec section 1 is the contract; this header is the mental model.
 *
 * ```
 *   trace("a") ─ als.run ─► span a (local root, buffer B, priority decided here)
 *                              └─ trace("b") ─► span b (buffer B)
 *   finish b ──► B.finished=[b]
 *   finish a ──► root finished: chunk [b, a] ──► writer queue ──► agent
 *   finish c (detached, after a) ──► late chunk [c] ──► writer queue
 * ```
 *
 * Design choices, each with the alternative it rejects:
 *
 * - **`AsyncLocalStorage` holds the active span.** It survives `await` and
 *   timers without the app passing anything around. The alternative, an
 *   explicit context argument, would force every call site in the host app to
 *   change, which is what auto-instrumentation exists to avoid.
 * - **The buffer holds only finished spans.** An unfinished span (an abandoned
 *   `startSpan`) is garbage-collected with its trace instead of pinning the
 *   buffer; the trace simply lacks that span.
 * - **Sampling is decided once, at the local root, from the trace id** (see
 *   {@link sampleKeep}), and is *not* a reason to skip work: an unsampled trace
 *   is still built and sent with priority 0, because the agent computes
 *   request/error/latency statistics from every span before it samples.
 * - **`start` is wall-clock, `duration` is monotonic.** A wall clock can step
 *   backwards mid-span; `process.hrtime` cannot. The start is derived from one
 *   (wall, hrtime) anchor so microsecond resolution is real, and the anchor is
 *   re-taken when the two clocks disagree by more than a second (laptop sleep:
 *   the monotonic clock pauses, the wall clock does not).
 * - **Nothing here may break the host.** Span bookkeeping is wrapped; the
 *   caller's function runs whatever happens, and its own exception is
 *   re-thrown unchanged.
 *
 * Process-wide state (the runtime, the ALS) lives on the shared global state
 * so two copies of this module in one process cooperate.
 *
 * @module
 */
import { AsyncLocalStorage } from "node:async_hooks";
import { randomBytes } from "node:crypto";
import type { ResolvedConfig } from "../config.js";
import { type GlobalState, globalState } from "../state.js";
import { TraceWriter, type TracerInfo, type WriterStats } from "./writer.js";
import {
  HEADER_PARENT_ID,
  HEADER_PRIORITY,
  HEADER_TRACE_ID,
  type WireSpan,
  normalizeWireSpan,
  parsePropagation,
  sampleKeep,
} from "./wire.js";

/** Spans past which a trace's finished spans are flushed without waiting for the root. */
export const PARTIAL_FLUSH_SPANS = 500;

/** The identity of a span on the wire: enough to be a parent in another process. */
export interface SpanContext {
  traceId: string;
  spanId: string;
  /** Sampling priority (-1, 0, 1, 2); positive keeps. */
  priority: number;
}

/** What the tracer hands to user code; also implemented by the no-op span. */
export interface Span {
  readonly traceId: string;
  readonly spanId: string;
  /** Sets a string tag (`meta`). Values are stringified; limits apply at send time. */
  setTag(key: string, value: string | number | boolean): this;
  /** Sets a numeric tag (`metrics`). Non-finite values are dropped at send time. */
  setMetric(key: string, value: number): this;
  /** Replaces the span's resource. */
  setResource(resource: string): this;
  /** Marks the span failed and records `error.type`, `error.message`, `error.stack`. */
  setError(err: unknown): this;
  /** Ends the span. Idempotent: only the first call counts. */
  finish(): void;
  /** The context to propagate to a child, here or in another process. */
  context(): SpanContext;
}

/** Options for {@link Tracer.trace}, {@link Tracer.wrap} and {@link Tracer.startSpan}. */
export interface SpanOptions {
  /** What the span operated on: a route, SQL text, a job name. Default: the span name. */
  resource?: string;
  /** `web`, `db`, `cache`, `queue`, `http`, `worker` or `custom` (default `custom`). */
  type?: string;
  /** Overrides the service from `init()`. A span in another service is a `_top_level` span. */
  service?: string;
  /** Parent override: a span, or a context from {@link Tracer.extract}. Default: the active span. */
  childOf?: Span | SpanContext | null;
  /** Initial string tags. */
  tags?: Record<string, string | number | boolean>;
}

/** Counters from {@link Tracer.stats}. */
export interface TraceStats extends WriterStats {
  /** Spans finished (sent to the writer). */
  spans: number;
  /** Traces started here that the head sampler dropped (still sent, priority 0). */
  unsampled: number;
}

/** What the tracer needs from `init()`. */
export interface TraceConfig {
  service: string;
  env: string;
  version: string;
  /** Default head-sampling rate in [0, 1]. */
  sampleRate: number;
}

/** The per-process tracer state kept on the shared global state. */
export interface TraceRuntime {
  config: TraceConfig;
  writer: TraceWriter;
  /** `rate_by_service` from the agent, keyed `service:<name>,env:<env>`. */
  rates: Map<string, number>;
  spans: number;
  unsampled: number;
}

/** A trace's finished spans, held by the local root's descendants. */
interface TraceBuffer {
  finished: WireSpan[];
  rootDone: boolean;
  priority: number;
  runtime: TraceRuntime;
}

const noopContext: SpanContext = { traceId: "0".repeat(32), spanId: "0".repeat(16), priority: 0 };

/** The span returned when tracing is off or bookkeeping failed; every method is inert. */
class NoopSpan implements Span {
  readonly traceId = noopContext.traceId;
  readonly spanId = noopContext.spanId;
  setTag(): this {
    return this;
  }
  setMetric(): this {
    return this;
  }
  setResource(): this {
    return this;
  }
  setError(): this {
    return this;
  }
  finish(): void {}
  context(): SpanContext {
    return noopContext;
  }
}

/** The shared inert span. */
export const NOOP_SPAN: Span = new NoopSpan();

// ---- ids ------------------------------------------------------------------

const POOL_BYTES = 4096;
let pool: Buffer = Buffer.alloc(0);
let poolPos = 0;
/** Test seam: replaces the entropy source. */
let entropy: (n: number) => Buffer = randomBytes;

/**
 * Overrides the random source (tests only), and resets the pool.
 *
 * @param source - a function returning `n` bytes, or undefined to restore.
 */
export function setEntropyForTest(source?: (n: number) => Buffer): void {
  entropy = source ?? randomBytes;
  pool = Buffer.alloc(0);
  poolPos = 0;
}

/**
 * Returns `bytes` random bytes as lowercase hex, never all zero. Entropy comes
 * from `crypto.randomBytes` in 4 KiB pools: one syscall-backed call per ~250
 * spans instead of one per span, which dominated the per-span cost.
 *
 * @param bytes - 16 for a trace id, 8 for a span id.
 * @returns `2 * bytes` hex characters.
 */
export function newId(bytes: number): string {
  for (;;) {
    if (poolPos + bytes > pool.length) {
      pool = entropy(POOL_BYTES);
      poolPos = 0;
      if (pool.length < bytes) throw new Error("entropy source returned too few bytes");
    }
    const hex = pool.toString("hex", poolPos, poolPos + bytes);
    poolPos += bytes;
    if (/[1-9a-f]/.test(hex)) return hex;
  }
}

// ---- clock ----------------------------------------------------------------

let anchorWallUs = Date.now() * 1000;
let anchorHr = process.hrtime.bigint();

/** Returns `[startUs, hrtime]` for a span starting now; see the module doc for the anchor rule. */
function startClock(): [number, bigint] {
  const hr = process.hrtime.bigint();
  const wall = Date.now() * 1000;
  let us = anchorWallUs + Number((hr - anchorHr) / 1000n);
  if (Math.abs(us - wall) > 1_000_000) {
    anchorWallUs = wall;
    anchorHr = hr;
    us = wall;
  }
  return [us, hr];
}

// ---- shared state ---------------------------------------------------------

function als(state: GlobalState): AsyncLocalStorage<Span> {
  return (state.als ??= new AsyncLocalStorage<Span>());
}

function runtime(): TraceRuntime | null {
  return globalState().trace;
}

/** Identity-based key the agent uses in `rate_by_service`. */
function rateKey(service: string, env: string): string {
  return `service:${service},env:${env}`;
}

// ---- spans ----------------------------------------------------------------

/** The real span implementation. Created only through the tracer. */
/**
 * Brand checked instead of `instanceof`: Next dev can load this module twice, and a span made by one
 * copy sits in the shared AsyncLocalStorage where the other must still recognise it as a span. An
 * `instanceof` against the second copy's class would say no and start a new root trace.
 */
const SPAN_BRAND = Symbol.for("ozy.span");
function isSpanImpl(x: unknown): x is SpanImpl {
  return typeof x === "object" && x !== null && (x as Record<symbol, unknown>)[SPAN_BRAND] === true;
}

class SpanImpl implements Span {
  readonly [SPAN_BRAND] = true;
  readonly traceId: string;
  readonly spanId: string;
  private readonly parentId: string | null;
  private readonly service: string;
  private name: string;
  private resource: string;
  private type: string;
  private readonly startUs: number;
  private readonly startHr: bigint;
  private error: 0 | 1 = 0;
  private meta: Record<string, string>;
  private metrics: Record<string, number> = {};
  private done = false;
  private priority: number;
  private readonly buffer: TraceBuffer;
  private readonly isLocalRoot: boolean;

  constructor(name: string, opts: SpanOptions, rt: TraceRuntime, parent: SpanImpl | SpanContext | null) {
    this.name = name;
    this.resource = opts.resource ?? name;
    this.type = opts.type ?? "custom";
    this.service = opts.service || rt.config.service;
    const [startUs, startHr] = startClock();
    this.startUs = startUs;
    this.startHr = startHr;
    this.spanId = newId(8);
    this.meta = {};
    if (rt.config.env) this.meta["env"] = rt.config.env;
    if (rt.config.version) this.meta["version"] = rt.config.version;
    if (opts.tags) for (const k of Object.keys(opts.tags)) this.meta[k] = String(opts.tags[k]);

    let parentService: string | undefined;
    if (isSpanImpl(parent)) {
      this.traceId = parent.traceId;
      this.parentId = parent.spanId;
      this.priority = parent.priority;
      this.buffer = parent.buffer;
      this.isLocalRoot = false;
      parentService = parent.service;
    } else {
      this.isLocalRoot = true;
      if (parent) {
        // An extracted context: the trace and its sampling verdict belong to
        // upstream, which already decided; re-deciding here could split a trace.
        this.traceId = parent.traceId;
        this.parentId = parent.spanId;
        this.priority = parent.priority;
      } else {
        this.traceId = newId(16);
        this.parentId = null;
        const rate = rt.rates.get(rateKey(this.service, rt.config.env)) ?? rt.config.sampleRate;
        this.priority = sampleKeep(this.traceId, rate) ? 1 : 0;
        if (this.priority === 0) rt.unsampled++;
      }
      this.buffer = { finished: [], rootDone: false, priority: this.priority, runtime: rt };
      this.metrics["_sampling_priority"] = this.priority;
    }
    // A span is a service entry when nothing in this service is its parent.
    if (parentService !== this.service) this.metrics["_top_level"] = 1;
  }

  setTag(key: string, value: string | number | boolean): this {
    if (!this.done) this.meta[key] = String(value);
    return this;
  }

  setMetric(key: string, value: number): this {
    if (!this.done) this.metrics[key] = value;
    return this;
  }

  setResource(resource: string): this {
    if (!this.done) this.resource = String(resource);
    return this;
  }

  setError(err: unknown): this {
    if (this.done) return this;
    this.error = 1;
    try {
      if (err instanceof Error) {
        this.meta["error.type"] = err.name || err.constructor.name;
        this.meta["error.message"] = String(err.message);
        if (err.stack) this.meta["error.stack"] = String(err.stack);
      } else {
        this.meta["error.type"] = typeof err;
        this.meta["error.message"] = String(err);
      }
    } catch {
      // A hostile error object (throwing getters) still marks the span failed.
    }
    return this;
  }

  context(): SpanContext {
    return { traceId: this.traceId, spanId: this.spanId, priority: this.priority };
  }

  finish(): void {
    if (this.done) return;
    this.done = true;
    try {
      const elapsed = Number((process.hrtime.bigint() - this.startHr) / 1000n);
      const wire: WireSpan = normalizeWireSpan({
        trace_id: this.traceId,
        span_id: this.spanId,
        parent_id: this.parentId,
        service: this.service,
        name: this.name,
        resource: this.resource,
        type: this.type,
        start: this.startUs,
        duration: elapsed < 0 ? 0 : elapsed,
        error: this.error,
        meta: this.meta,
        metrics: this.metrics,
      });
      onFinish(this.buffer, wire, this.isLocalRoot);
    } catch {
      // Bookkeeping must never reach the host.
    }
  }
}

/**
 * Applies the buffer rules: the local root's finish flushes the chunk; a span
 * finishing after its root goes out as a late chunk at once; and a trace that
 * has accumulated {@link PARTIAL_FLUSH_SPANS} finished spans flushes them
 * without waiting, so one long trace cannot hold unbounded memory.
 */
function onFinish(buf: TraceBuffer, wire: WireSpan, isLocalRoot: boolean): void {
  buf.runtime.spans++;
  buf.finished.push(wire);
  if (isLocalRoot) buf.rootDone = true;
  if (buf.rootDone || buf.finished.length >= PARTIAL_FLUSH_SPANS) flushBuffer(buf);
}

function flushBuffer(buf: TraceBuffer): void {
  const chunk = buf.finished;
  buf.finished = [];
  if (chunk.length === 0) return;
  // The decision is made on the root, but a partial or late chunk does not
  // contain it: the agent's priority sampler reads the first span of the chunk.
  const first = chunk[0]!;
  first.metrics["_sampling_priority"] ??= buf.priority;
  buf.runtime.writer.enqueue(chunk);
}

// ---- tracer ---------------------------------------------------------------

/** Mutable header carriers: a plain object or anything with `set` (a `Headers`). */
export type HeaderCarrier = Record<string, string | string[] | undefined> | { set(name: string, value: string): void };
/** Readable header sources: a plain object, a `Headers`, or a `Request`-like holder. */
export type HeaderSource =
  | Record<string, unknown>
  | { get(name: string): string | null | undefined }
  | { headers: { get(name: string): string | null | undefined } };

function readHeader(source: HeaderSource, name: string): unknown {
  const s = source as Record<string, unknown>;
  if (typeof s["get"] === "function") return (s["get"] as (n: string) => unknown).call(source, name) ?? undefined;
  const h = s["headers"] as { get?: unknown } | undefined;
  if (h && typeof h.get === "function") return (h.get as (n: string) => unknown).call(h, name) ?? undefined;
  for (const k of Object.keys(s)) {
    if (k.toLowerCase() === name) {
      const v = s[k];
      return Array.isArray(v) ? v[0] : v;
    }
  }
  return undefined;
}

function isThenable(value: unknown): value is PromiseLike<unknown> {
  try {
    return (
      value !== null &&
      (typeof value === "object" || typeof value === "function") &&
      typeof (value as { then?: unknown }).then === "function"
    );
  } catch {
    return false;
  }
}

/** The scope accessor returned by {@link Tracer.scope}. */
export interface Scope {
  /** The active span, or null outside any trace (or when tracing is off). */
  active(): Span | null;
  /** Runs `fn` with `span` active. */
  activate<T>(span: Span, fn: () => T): T;
}

/**
 * The public tracer API. Every method is non-throwing; with tracing disabled
 * (no agent host, or `OZY_TRACE_ENABLED=false`) spans are inert, but the
 * callbacks still run, because instrumenting an app must never change what it
 * does.
 */
export interface Tracer {
  /**
   * Runs `fn` inside a new span (a child of the active span, or a new trace
   * root) and finishes the span when `fn` returns or its promise settles. A
   * thrown or rejected error marks the span and is re-thrown unchanged.
   */
  trace<T>(name: string, fn: (span: Span) => T): T;
  /** As above with options. */
  trace<T>(name: string, opts: SpanOptions, fn: (span: Span) => T): T;
  /** Returns `fn` wrapped so each call runs inside `trace(name, opts, ...)`. */
  wrap<A extends unknown[], R>(name: string, opts: SpanOptions, fn: (...args: A) => R): (...args: A) => R;
  /** Scope accessor: `tracer.scope().active()` is the current span. */
  scope(): Scope;
  /** The manual API: starts a span without activating it; the caller must `finish()` it. */
  startSpan(name: string, opts?: SpanOptions): Span;
  /** Writes the propagation headers for `ctx` (default: the active span) into `carrier`. */
  inject<C extends HeaderCarrier>(ctx: Span | SpanContext | null | undefined, carrier: C): C;
  /** Reads propagation headers; null when absent or malformed. Never throws. */
  extract(source: HeaderSource | null | undefined): SpanContext | null;
  /** Writer and span counters. */
  stats(): TraceStats;
  /** Sends everything queued now; resolves when done and never rejects. */
  flush(): Promise<void>;
}

const ZERO_STATS: TraceStats = Object.freeze({
  spans: 0,
  unsampled: 0,
  chunksQueued: 0,
  chunksSent: 0,
  chunksDropped: 0,
  errors: 0,
  queued: 0,
});

function resolveParent(opts: SpanOptions): SpanImpl | SpanContext | null {
  if (opts.childOf !== undefined) {
    const c = opts.childOf;
    if (c === null) return null;
    if (isSpanImpl(c)) return c;
    if (c === NOOP_SPAN) return null;
    const ctx = c as SpanContext;
    return typeof ctx.traceId === "string" && typeof ctx.spanId === "string" ? ctx : null;
  }
  const active = als(globalState()).getStore();
  return isSpanImpl(active) ? active : null;
}

function create(name: string, opts: SpanOptions): SpanImpl | null {
  const rt = runtime();
  if (!rt) return null;
  try {
    return new SpanImpl(name, opts, rt, resolveParent(opts));
  } catch {
    return null;
  }
}

function runInSpan<T>(span: SpanImpl, fn: (span: Span) => T): T {
  let result: T;
  try {
    result = als(globalState()).run(span, fn, span);
  } catch (err) {
    span.setError(err);
    span.finish();
    throw err;
  }
  if (isThenable(result)) {
    return result.then(
      (value) => {
        span.finish();
        return value;
      },
      (err: unknown) => {
        span.setError(err);
        span.finish();
        throw err;
      },
    ) as T;
  }
  span.finish();
  return result;
}

/** The tracer facade; reads the runtime that `init()` installed on the shared state. */
export const tracer: Tracer = {
  trace<T>(name: string, a: SpanOptions | ((span: Span) => T), b?: (span: Span) => T): T {
    const opts = typeof a === "function" ? {} : (a ?? {});
    const fn = (typeof a === "function" ? a : b) as (span: Span) => T;
    const span = create(name, opts);
    if (!span) return fn(NOOP_SPAN);
    return runInSpan(span, fn);
  },
  wrap<A extends unknown[], R>(name: string, opts: SpanOptions, fn: (...args: A) => R): (...args: A) => R {
    return function (this: unknown, ...args: A): R {
      return tracer.trace(name, opts, () => fn.apply(this, args));
    };
  },
  scope(): Scope {
    return {
      active() {
        try {
          return als(globalState()).getStore() ?? null;
        } catch {
          return null;
        }
      },
      activate<T>(span: Span, fn: () => T): T {
        return als(globalState()).run(span, fn);
      },
    };
  },
  startSpan(name: string, opts: SpanOptions = {}): Span {
    return create(name, opts ?? {}) ?? NOOP_SPAN;
  },
  inject<C extends HeaderCarrier>(ctx: Span | SpanContext | null | undefined, carrier: C): C {
    try {
      const c = ctx ?? als(globalState()).getStore();
      if (!c || !runtime()) return carrier;
      const context: SpanContext = typeof (c as Span).context === "function" ? (c as Span).context() : (c as SpanContext);
      if (context === noopContext) return carrier;
      const values: Array<[string, string]> = [
        [HEADER_TRACE_ID, context.traceId],
        [HEADER_PARENT_ID, context.spanId],
        [HEADER_PRIORITY, String(context.priority)],
      ];
      const set = (carrier as { set?: unknown }).set;
      for (const [k, v] of values) {
        if (typeof set === "function") (set as (n: string, v: string) => void).call(carrier, k, v);
        else (carrier as Record<string, string>)[k] = v;
      }
    } catch {
      // Headers are best-effort.
    }
    return carrier;
  },
  extract(source: HeaderSource | null | undefined): SpanContext | null {
    try {
      if (!source || typeof source !== "object") return null;
      const p = parsePropagation(
        readHeader(source, HEADER_TRACE_ID),
        readHeader(source, HEADER_PARENT_ID),
        readHeader(source, HEADER_PRIORITY),
      );
      return p ? { traceId: p.traceId, spanId: p.parentId, priority: p.priority } : null;
    } catch {
      return null;
    }
  },
  stats(): TraceStats {
    const rt = runtime();
    if (!rt) return { ...ZERO_STATS };
    try {
      return { ...rt.writer.stats(), spans: rt.spans, unsampled: rt.unsampled };
    } catch {
      return { ...ZERO_STATS };
    }
  },
  async flush(): Promise<void> {
    try {
      await runtime()?.writer.flush();
    } catch {
      // never rejects
    }
  },
};

// ---- init wiring ----------------------------------------------------------

/**
 * Installs (or, with `config.traceEnabled` false, removes) the tracer runtime.
 * Called by `init()`; the previous writer is flushed in the background.
 *
 * @param config - the resolved SDK configuration.
 * @param sdkVersion - reported in the `tracer` field of every payload.
 * @param log - debug sink.
 */
export function configureTracer(config: ResolvedConfig, sdkVersion: string, log?: (m: string) => void): void {
  const state = globalState();
  const previous = state.trace;
  state.trace = null;
  if (previous) void previous.writer.close();
  if (!config.enabled || !config.traceEnabled) return;
  const info: TracerInfo = { lang: "node", lang_version: process.versions.node, version: sdkVersion };
  const rt: TraceRuntime = {
    config: {
      service: config.service ?? "unknown",
      env: config.env ?? "",
      version: config.version ?? "",
      sampleRate: config.traceSampleRate,
    },
    writer: undefined as unknown as TraceWriter,
    rates: new Map(),
    spans: 0,
    unsampled: 0,
  };
  rt.writer = new TraceWriter({
    host: config.agentHost,
    port: config.tracePort,
    info,
    log,
    onRates: (rates) => {
      // The agent's answer is the whole table; replacing keeps a service the
      // agent stopped listing from keeping a stale rate forever.
      rt.rates.clear();
      for (const [k, v] of Object.entries(rates)) rt.rates.set(k, v);
    },
  });
  state.trace = rt;
}
