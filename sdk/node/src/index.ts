/**
 * ozymandias — Node.js SDK.
 *
 * M1 scope: a extended-StatsD-compatible metrics client. Usage:
 *
 * ```ts
 * import { init, statsd } from "ozy";
 * init({ service: "checkout", env: "dev" });   // OZY_* env vars fill the rest
 * statsd.increment("http.request.count", 1, { tags: ["route:/api/items"] });
 * await statsd.timed("image.resize.duration", () => resize(buf));
 * ```
 *
 * Three guarantees shape everything in this package:
 *
 * 1. **Inert unless configured.** Without `OZY_AGENT_HOST` (or
 *    `init({agentHost})`) every call is a no-op and no socket, timer or
 *    process hook is created. Libraries and apps can ship with the SDK wired
 *    in and stay unchanged wherever no agent runs.
 * 2. **Never throws into the host.** Every public entry point catches its own
 *    failures and counts them in `statsd.stats()`. The one deliberate
 *    exception is `timed()`, which rethrows the *caller's* error unchanged —
 *    it must not swallow the app's own exceptions.
 * 3. **Never blocks.** A metric call formats a line into a buffer and
 *    returns; network I/O happens later, over UDP, fire-and-forget.
 *
 * The bytes on the wire are specified by docs/wire-protocol.md §A; the SDK is
 * a convenience on top of that protocol, not a requirement for using it.
 *
 * @module
 */
import { type StatsdStats, type MetricOptions, StatsdClient } from "./client.js";
import { type InitOptions, resolveConfig } from "./config.js";
import { globalState, installExitHooks } from "./state.js";

export type { ClientHooks, MetricOptions, StatsdStats } from "./client.js";
export type { InitOptions } from "./config.js";

/**
 * Configures the SDK. Arguments win over `OZY_*` environment variables,
 * which win over defaults (see {@link InitOptions}). Calling it again replaces
 * the previous client (its buffer is flushed and its socket closed), so it is
 * safe to call from code that may run more than once.
 *
 * With no agent host configured, `init()` leaves the SDK disabled: every
 * `statsd` method is a no-op and nothing is allocated.
 *
 * @param options - service identity, global tags, agent address, tuning.
 */
export function init(options: InitOptions = {}): void {
  try {
    const opts = options ?? {};
    const state = globalState();
    const config = resolveConfig(opts, process.env);
    const previous = state.statsd;
    state.statsd = null;
    if (previous) void previous.close();
    if (!config.enabled) {
      if (config.debug) process.stderr.write("[ozy] disabled: OZY_AGENT_HOST is not set\n");
      return;
    }
    state.statsd = new StatsdClient(config, opts.hooks);
    installExitHooks(state);
  } catch {
    // init() must never break app startup. There is no client to count the
    // error on; the SDK simply stays disabled.
  }
}

/** The metrics API exposed as `statsd`. All methods are non-throwing. */
export interface Statsd {
  /**
   * Adds to a counter (`|c`). The agent sums counts per 10 s bucket and
   * scales sampled counts by `1/sampleRate`.
   */
  increment(name: string, value?: number | MetricOptions, opts?: MetricOptions): void;
  /** Subtracts from a counter: sends `-value|c`. */
  decrement(name: string, value?: number | MetricOptions, opts?: MetricOptions): void;
  /** Sets a gauge (`|g`): the agent keeps the last value per bucket. */
  gauge(name: string, value: number, opts?: MetricOptions): void;
  /**
   * Records a histogram sample (`|h`): the agent computes avg/min/max/median/
   * p95/count per bucket.
   */
  histogram(name: string, value: number, opts?: MetricOptions): void;
  /**
   * Records a distribution sample (`|d`): aggregated globally with a sketch
   * from M2 (treated like a histogram in M1). Prefer this for latencies.
   */
  distribution(name: string, value: number, opts?: MetricOptions): void;
  /** Records a duration in milliseconds (`|ms`, a histogram in the agent). */
  timing(name: string, ms: number, opts?: MetricOptions): void;
  /** Counts distinct members per bucket (`|s`); numbers are sent as strings. */
  set(name: string, member: string | number, opts?: MetricOptions): void;
  /**
   * Runs `fn`, records how long it took with {@link Statsd.timing}, and
   * returns its result. Works for sync functions and for functions returning
   * a promise (the time is taken when the promise settles, and the returned
   * promise settles the same way). An error thrown or rejected by `fn` is
   * recorded, then rethrown unchanged. Runs `fn` even when the SDK is disabled.
   */
  timed<T>(name: string, fn: () => T, opts?: MetricOptions): T;
  /** Sends everything buffered now instead of waiting up to 100 ms. */
  flush(): void;
  /**
   * Flushes and closes the socket. Resolves once buffered datagrams have been
   * handed to the kernel (at most ~2 s); never rejects. Later metric calls are
   * ignored until the next `init()`.
   */
  close(): Promise<void>;
  /** Counters: messages sent, datagrams sent, messages dropped, errors. */
  stats(): StatsdStats;
}

function recordError(client: StatsdClient, err: unknown): void {
  try {
    client.recordError(err);
  } catch {
    // Nothing left to report to.
  }
}

function withClient(fn: (client: StatsdClient) => void): void {
  const client = globalState().statsd;
  if (!client) return;
  try {
    fn(client);
  } catch (err) {
    recordError(client, err);
  }
}

function counterArgs(value: number | MetricOptions | undefined, opts: MetricOptions | undefined): [number, MetricOptions | undefined] {
  if (typeof value === "object" && value !== null) return [1, value];
  return [value ?? 1, opts];
}

function isThenable(value: unknown): value is PromiseLike<unknown> {
  try {
    return (
      value !== null &&
      (typeof value === "object" || typeof value === "function") &&
      typeof (value as { then?: unknown }).then === "function"
    );
  } catch {
    return false; // a throwing `then` getter: treat the value as synchronous
  }
}

const ZERO_STATS: StatsdStats = Object.freeze({ sent: 0, packets: 0, dropped: 0, errors: 0 });

/**
 * The statsd client facade. It forwards to the client created by `init()`
 * (shared process-wide, see state.ts) and is a no-op before `init()` or when
 * the SDK is disabled.
 */
export const statsd: Statsd = {
  increment(name, value, opts) {
    withClient((c) => {
      const [v, o] = counterArgs(value, opts);
      c.metric("c", name, v, o);
    });
  },
  decrement(name, value, opts) {
    withClient((c) => {
      const [v, o] = counterArgs(value, opts);
      c.metric("c", name, -v, o);
    });
  },
  gauge(name, value, opts) {
    withClient((c) => c.metric("g", name, value, opts));
  },
  histogram(name, value, opts) {
    withClient((c) => c.metric("h", name, value, opts));
  },
  distribution(name, value, opts) {
    withClient((c) => c.metric("d", name, value, opts));
  },
  timing(name, ms, opts) {
    withClient((c) => c.metric("ms", name, ms, opts));
  },
  set(name, member, opts) {
    withClient((c) => c.set(name, member, opts));
  },
  timed<T>(name: string, fn: () => T, opts?: MetricOptions): T {
    const client = globalState().statsd;
    let start = 0;
    try {
      if (client) start = client.clock();
    } catch {
      // Clock failure: still run the caller's code; the recorded value may be off.
    }
    const record = () => {
      if (!client) return;
      try {
        client.metric("ms", name, client.clock() - start, opts);
      } catch (err) {
        recordError(client, err);
      }
    };
    let result: T;
    try {
      result = fn();
    } catch (err) {
      record();
      throw err;
    }
    if (isThenable(result)) {
      return result.then(
        (value) => {
          record();
          return value;
        },
        (err: unknown) => {
          record();
          throw err;
        },
      ) as T;
    }
    record();
    return result;
  },
  flush() {
    withClient((c) => c.flush());
  },
  close() {
    const client = globalState().statsd;
    if (!client) return Promise.resolve();
    try {
      return client.close().catch(() => {});
    } catch (err) {
      recordError(client, err);
      return Promise.resolve();
    }
  },
  stats() {
    const client = globalState().statsd;
    if (!client) return { ...ZERO_STATS };
    try {
      return client.stats();
    } catch {
      return { ...ZERO_STATS };
    }
  },
};
