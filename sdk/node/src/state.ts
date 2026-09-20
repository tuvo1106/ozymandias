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
import type { StatsdClient } from "./client.js";

/** The layout version of {@link GlobalState}. */
export const STATE_VERSION = 1;

/** Everything the SDK keeps per process. Later milestones add the tracer here. */
export interface GlobalState {
  /** Layout version, see {@link STATE_VERSION}. */
  version: number;
  /** The active statsd client, or null when the SDK is disabled / not initialized. */
  statsd: StatsdClient | null;
  /** Whether the process exit hooks have been installed (at most once). */
  exitHooksInstalled: boolean;
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
    state = { version: STATE_VERSION, statsd: null, exitHooksInstalled: false };
    holder[KEY] = state;
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
  process.on("beforeExit", flush);
  process.on("exit", flush);
}
