/**
 * Process-wide state, kept on `globalThis[Symbol.for("ozy")]`.
 *
 * Why not module-level variables: a module is not guaranteed to be a
 * singleton. Next.js (dev server, and separate server/edge/instrumentation
 * bundles) can evaluate the same package twice, and a package that ships both
 * ESM and CJS builds can be loaded once through `import` and once through
 * `require` (the "dual package hazard"). Each copy would get its own client,
 * its own socket and its own `init()` — and `init()` called in one copy would
 * leave the other silently disabled. `Symbol.for` returns the same symbol
 * from every copy (it is a process-global registry), so every copy reads and
 * writes one shared state object.
 *
 * The state holds *instances*, and a copy of the module calls methods on an
 * instance another copy created. That is fine while all copies are the same
 * version; `version` records the state layout so a future incompatible layout
 * can detect an older one instead of misreading it.
 *
 * @module
 */
import type { AsyncLocalStorage } from "node:async_hooks";
import type { StatsdClient } from "./client.js";
import type { Integration } from "./integrations/types.js";
import type { Span, TraceRuntime } from "./trace/tracer.js";

/** The layout version of {@link GlobalState}. */
export const STATE_VERSION = 2;

/** Everything the SDK keeps per process. Later milestones add the tracer here. */
export interface GlobalState {
  /** Layout version, see {@link STATE_VERSION}. */
  version: number;
  /** The active statsd client, or null when the SDK is disabled / not initialized. */
  statsd: StatsdClient | null;
  /** Whether the process exit hooks have been installed (at most once). */
  exitHooksInstalled: boolean;
  /** The tracer runtime (config, writer, agent-provided rates), or null when tracing is off. */
  trace: TraceRuntime | null;
  /**
   * The async context holding the active span. It lives here, not in the
   * tracer module, so two copies of the package share one context: a span
   * started through one copy is the parent of a span started through the other.
   */
  als: AsyncLocalStorage<Span> | null;
  /** Integrations registered by third parties through `registerIntegration()`. */
  integrations: Map<string, Integration>;
  /** Hosts the fetch integration injects propagation headers for (default none). */
  fetchAllow: string[];
}

const KEY = Symbol.for("ozy");

/**
 * Returns the shared state, creating it on first use.
 *
 * @returns the one {@link GlobalState} for this process.
 */
export function globalState(): GlobalState {
  const holder = globalThis as unknown as Record<symbol, GlobalState | undefined>;
  let state = holder[KEY];
  if (!state) {
    state = {
      version: STATE_VERSION,
      statsd: null,
      exitHooksInstalled: false,
      trace: null,
      als: null,
      integrations: new Map(),
      fetchAllow: [],
    };
    holder[KEY] = state;
  } else if (state.version < STATE_VERSION) {
    // Created by an older copy of the package (M1 layout): fill what it lacks
    // in place, so both copies keep sharing the one object.
    state.trace ??= null;
    state.als ??= null;
    state.integrations ??= new Map();
    state.fetchAllow ??= [];
    state.version = STATE_VERSION;
  }
  return state;
}

/**
 * Installs the exit hooks once per process, and only when some client is
 * enabled — a disabled SDK must leave `process` untouched.
 *
 * - `beforeExit` fires when the event loop drains naturally. A flush there
 *   schedules sends, which keep the loop alive until they complete; the
 *   event then fires again with an empty buffer and the process exits.
 * - `exit` fires on `process.exit()` too, but no further I/O callbacks run
 *   after it. The flush there is best effort: on an already connected socket
 *   the send reaches the kernel synchronously, otherwise the data is lost.
 *
 * Signals (SIGTERM, SIGINT) trigger neither by default. Installing signal
 * handlers would change the host app's shutdown behaviour, so the SDK does
 * not; apps that handle signals should call `statsd.close()` themselves.
 *
 * The hooks read the client from the shared state at exit time, so re-`init()`
 * never adds listeners.
 *
 * @param state - the shared state.
 */
export function installExitHooks(state: GlobalState): void {
  if (state.exitHooksInstalled) return;
  state.exitHooksInstalled = true;
  const flush = () => {
    try {
      globalState().statsd?.flush();
    } catch {
      // Exit paths must never throw.
    }
  };
  // Traces go over HTTP, which needs the event loop: only `beforeExit` can
  // deliver them (`exit` allows no further I/O). The writer holds the loop
  // open for at most its 1 s exit budget, then the event fires again with an
  // empty queue and the process ends.
  const flushTraces = () => {
    try {
      void globalState().trace?.writer.flushOnExit();
    } catch {
      // Exit paths must never throw.
    }
  };
  process.on("beforeExit", () => {
    flush();
    flushTraces();
  });
  process.on("exit", flush);
}
