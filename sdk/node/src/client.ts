/**
 * The statsd client: sampling, formatting, buffering and flushing.
 *
 * A metric call does three cheap, synchronous things — decide whether this
 * sample is sent, format one line, append it to an in-memory buffer — and
 * returns. The network is touched only when the buffer is flushed, which
 * happens when:
 *
 * 1. the next line would push the buffer past `maxPayloadBytes` (the buffer
 *    is sent first, then the line starts a new one), or
 * 2. `flushIntervalMs` (100 ms) after the first line entered an empty buffer,
 *    via an **unref'd** one-shot timer, or
 * 3. `flush()` / `close()` is called, or the process is about to exit.
 *
 * Coalescing lines into one datagram (joined by `\n`) is the classic statsd
 * optimisation: a busy request handler emitting ten metrics costs one syscall
 * per 100 ms, not ten per request. The 100 ms bound keeps the added latency
 * far below the agent's 10 s aggregation bucket, so it is invisible in graphs.
 *
 * Why a one-shot timer armed on demand instead of a permanent `setInterval`:
 * an idle process then holds no timer at all, and the timer is unref'd so it
 * never keeps a finished script alive.
 *
 * @module
 */
import { type ResolvedConfig } from "./config.js";
import { type MetricType, formatLine, formatNumber, globalTags, sanitizeTag } from "./format.js";
import {
  type Counters,
  type Lookup,
  type SocketFactory,
  type TimerFns,
  type TimerHandle,
  UdpTransport,
  defaultCreateSocket,
  defaultLookup,
  defaultTimers,
} from "./transport.js";

/** Per-call options accepted by every metric method. */
export interface MetricOptions {
  /** Tags for this call, `key:value` or bare `key`; sent before global tags. */
  tags?: string[];
  /**
   * Fraction of calls to actually send, in (0, 1]. Below 1 the SDK sends with
   * that probability and writes `|@rate` so the agent scales counts by
   * `1/rate`. Values ≥ 1 (and the default) mean "always send"; 0, negative
   * and NaN mean "never send".
   */
  sampleRate?: number;
}

/** Snapshot of the client's counters, returned by `statsd.stats()`. */
export interface StatsdStats {
  /** Messages handed to the kernel successfully. */
  sent: number;
  /** Datagrams handed to the kernel successfully. */
  packets: number;
  /**
   * Messages the SDK knows it did not send: no agent configured yet, queue
   * full, the send call itself failed, or a NaN/Inf value.
   *
   * Not a delivery guarantee in the other direction. UDP has no
   * acknowledgement, so a message counted `sent` was handed to the kernel and
   * nothing more. When the agent's port is closed, the kernel learns that
   * asynchronously (ICMP port-unreachable) *after* the send succeeded: those
   * messages stay counted `sent` and the failure shows up in `errors`
   * instead. Read the two together — a healthy client has `errors` at 0.
   */
  dropped: number;
  /**
   * Internal errors swallowed (DNS, socket, send, unexpected exceptions),
   * including asynchronous ones that arrive after a send appeared to succeed.
   * This is the field that goes non-zero when the agent is not listening.
   */
  errors: number;
}

/**
 * Seams for tests and unusual embeddings. Applications never need these;
 * every field defaults to the real implementation.
 */
export interface ClientHooks {
  /** Random source for sampling, returning [0, 1). Default `Math.random`. */
  random?: () => number;
  /** Monotonic clock in ms for `timed()` and the DNS cache. Default `performance.now`. */
  now?: () => number;
  /** UDP socket factory. Default `dgram.createSocket`. */
  createSocket?: SocketFactory;
  /** Hostname resolver. Default `dns.lookup`, IPv4 preferred. */
  lookup?: Lookup;
  /** Timer functions. Default global `setTimeout` / `clearTimeout`. */
  timers?: TimerFns;
  /** Debug log sink. Default: a `[ozy]` line on stderr. */
  log?: (msg: string) => void;
}

const encoder = new TextEncoder();
const NEWLINE_BYTES = 1;

/**
 * An enabled statsd client bound to one agent address. Created by `init()`
 * when an agent host is configured; the exported `statsd` facade forwards to
 * it. Every public method is non-throwing and returns immediately.
 */
export class StatsdClient {
  private readonly config: ResolvedConfig;
  private readonly global: string[];
  private readonly random: () => number;
  private readonly now: () => number;
  private readonly timers: TimerFns;
  private readonly transport: UdpTransport;
  private readonly log: (msg: string) => void;
  private readonly counters: Counters = { sent: 0, packets: 0, dropped: 0, errors: 0 };
  private buffer: string[] = [];
  private bufferBytes = 0;
  private bufferMessages = 0;
  private timer: TimerHandle | null = null;
  private closed = false;

  /**
   * @param config - resolved configuration; must have `enabled: true`.
   * @param hooks - optional test seams.
   */
  constructor(config: ResolvedConfig, hooks: ClientHooks = {}) {
    this.config = config;
    this.global = globalTags(config.tags, config.service, config.env, config.version);
    this.random = hooks.random ?? Math.random;
    this.now = hooks.now ?? (() => performance.now());
    this.timers = hooks.timers ?? defaultTimers;
    this.log = config.debug ? (hooks.log ?? ((m) => process.stderr.write(`[ozy] ${m}\n`))) : () => {};
    this.transport = new UdpTransport({
      host: config.agentHost,
      port: config.statsdPort,
      counters: this.counters,
      now: this.now,
      createSocket: hooks.createSocket ?? defaultCreateSocket,
      lookup: hooks.lookup ?? defaultLookup,
      timers: this.timers,
      log: this.log,
    });
    this.log(
      `enabled: agent ${config.agentHost}:${config.statsdPort}, global tags [${this.global.join(",")}]`,
    );
  }

  /**
   * Records a numeric metric. Shared by every numeric method; see the facade
   * in index.ts for per-type semantics.
   *
   * @param type - the statsd type.
   * @param name - metric name.
   * @param value - the value; NaN/±Infinity are dropped and counted.
   * @param opts - tags and sample rate.
   */
  metric(type: MetricType, name: string, value: number, opts: MetricOptions | undefined): void {
    const formatted = formatNumber(Number(value));
    if (formatted === null) {
      this.counters.dropped++;
      this.log(`dropped ${name}: value ${String(value)} is not a finite number`);
      return;
    }
    this.emit(type, name, formatted, opts);
  }

  /**
   * Records a set member. Numbers are sent in their string form.
   *
   * @param name - metric name.
   * @param member - the member to count distinct occurrences of.
   * @param opts - tags and sample rate.
   */
  set(name: string, member: string | number, opts: MetricOptions | undefined): void {
    this.emit("s", name, sanitizeTag(String(member)), opts);
  }

  /**
   * Current counters. The object is a copy, so callers can keep it.
   *
   * @returns a snapshot of sent/packets/dropped/errors.
   */
  stats(): StatsdStats {
    return { ...this.counters };
  }

  /** Counts an error swallowed by the public facade. */
  recordError(err: unknown): void {
    this.counters.errors++;
    this.log(`internal error: ${err instanceof Error ? err.message : String(err)}`);
  }

  /** The monotonic clock, shared with `timed()`. */
  clock(): number {
    return this.now();
  }

  /** Sends whatever is buffered now, as one datagram. */
  flush(): void {
    if (this.timer) {
      this.timers.clear(this.timer);
      this.timer = null;
    }
    if (this.bufferMessages === 0) return;
    const bytes = encoder.encode(this.buffer.join("\n"));
    const messages = this.bufferMessages;
    this.buffer = [];
    this.bufferBytes = 0;
    this.bufferMessages = 0;
    this.transport.send({ bytes, messages });
  }

  /**
   * Flushes, then releases the socket once in-flight datagrams are handed to
   * the kernel (bounded wait). Later calls on this client are ignored.
   *
   * @returns a promise that resolves when the socket is closed; never rejects.
   */
  close(): Promise<void> {
    if (!this.closed) {
      this.flush();
      this.closed = true;
    }
    return this.transport.close();
  }

  private emit(type: MetricType, name: string, value: string, opts: MetricOptions | undefined): void {
    if (this.closed) return;
    const requested = opts?.sampleRate;
    const rate = typeof requested === "number" && !(requested >= 1) ? requested : 1;
    // Send when random() < rate. Written negated so a NaN, 0 or negative
    // rate never sends (every comparison with NaN is false).
    if (rate !== 1 && !(this.random() < rate)) return;
    const line = formatLine(String(name), value, type, rate, opts?.tags, this.global);
    this.append(line);
  }

  private append(line: string): void {
    const size = Buffer.byteLength(line);
    const added = this.bufferMessages === 0 ? size : size + NEWLINE_BYTES;
    if (this.bufferMessages > 0 && this.bufferBytes + added > this.config.maxPayloadBytes) {
      this.flush();
      this.append(line);
      return;
    }
    // A single line larger than the limit still goes out, alone: the agent
    // reads datagrams up to 8192 bytes, and truncating would corrupt it.
    this.buffer.push(line);
    this.bufferBytes += added;
    this.bufferMessages++;
    if (this.bufferBytes >= this.config.maxPayloadBytes) {
      this.flush();
      return;
    }
    if (!this.timer) {
      const timer = this.timers.set(() => {
        this.timer = null;
        try {
          this.flush();
        } catch (err) {
          // Backstop: an exception escaping a timer callback would be an
          // uncaught exception in the host app.
          this.recordError(err);
        }
      }, this.config.flushIntervalMs);
      timer.unref?.();
      this.timer = timer;
    }
  }
}
