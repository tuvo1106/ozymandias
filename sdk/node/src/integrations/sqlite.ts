/**
 * better-sqlite3 integration: `sqlite.query` spans around statement execution.
 *
 * better-sqlite3 is synchronous, so a span is started and finished around the
 * call and no async context is involved. Choices:
 *
 * - **Wrap `prepare` on the class prototype, once.** Statements are created
 *   by `prepare`; wrapping it lets every statement of every connection be
 *   timed without touching app code that holds `db`. The first `db` passed to
 *   {@link instrumentSqlite} identifies the prototype; the library is never
 *   imported, so the SDK has no dependency on it.
 * - **Only when a trace is active.** A query outside any request (a boot-time
 *   migration, a cron tick) would otherwise become a one-span root trace of
 *   noise, and the agent would count it as a service entry.
 * - **The resource is the statement source, never its parameters.** Bound
 *   values are user data (emails, tokens); the SDK never reads the arguments
 *   to `run/get/all/iterate` at all, which is the only way to be sure.
 * - **A throwing statement marks the span and re-throws unchanged.**
 *
 * @module
 */
import { type Span, tracer } from "../trace/tracer.js";
import type { Integration } from "./types.js";

type Fn = (this: unknown, ...args: unknown[]) => unknown;
const METHODS = ["run", "get", "all", "iterate"] as const;
/** Marks a patched `prepare` so a second `instrumentSqlite` (or module copy) does not double-wrap. */
const PATCHED = Symbol.for("ozy.sqlite.patched");

interface Patch {
  proto: Record<string | symbol, unknown>;
  original: Fn;
  wrapper: Fn;
}
const patches: Patch[] = [];

function startQuery(stmt: unknown): Span | null {
  const active = tracer.scope().active();
  if (!active) return null;
  const source = (stmt as { source?: unknown }).source;
  const span = tracer.startSpan("sqlite.query", {
    resource: typeof source === "string" ? source : "sqlite.query",
    type: "db",
    tags: { "db.system": "sqlite", "span.kind": "client" },
  });
  return span;
}

function timed(stmt: unknown, method: string, original: Fn, self: unknown, args: unknown[]): unknown {
  const span = startQuery(stmt);
  if (!span) return original.apply(self, args);
  let result: unknown;
  try {
    result = original.apply(self, args);
  } catch (err) {
    span.setError(err);
    span.finish();
    throw err;
  }
  if (method === "iterate") return traceIterator(result as Iterator<unknown>, span);
  const changes = (result as { changes?: unknown } | null)?.changes;
  if (method === "run" && typeof changes === "number") span.setMetric("db.rowcount", changes);
  span.finish();
  return result;
}

/**
 * An iterator's cost is the whole traversal, so the span ends when the caller
 * finishes (or abandons, via `return()`) the iteration, not when `iterate()` returns.
 */
function traceIterator(it: Iterator<unknown>, span: Span): IterableIterator<unknown> {
  const wrapper: IterableIterator<unknown> = {
    next(...a: [] | [undefined]) {
      let r: IteratorResult<unknown>;
      try {
        r = it.next(...a);
      } catch (err) {
        span.setError(err);
        span.finish();
        throw err;
      }
      if (r.done) span.finish();
      return r;
    },
    return(value?: unknown) {
      span.finish();
      return it.return ? it.return(value) : { done: true, value };
    },
    [Symbol.iterator]() {
      return wrapper;
    },
  };
  return wrapper;
}

function wrapStatement(stmt: object): void {
  for (const method of METHODS) {
    const original = (stmt as Record<string, unknown>)[method];
    if (typeof original !== "function") continue;
    Object.defineProperty(stmt, method, {
      configurable: true,
      writable: true,
      enumerable: false,
      value: function (this: unknown, ...args: unknown[]) {
        return timed(stmt, method, original as Fn, this, args);
      },
    });
  }
}

function patchProto(proto: Record<string | symbol, unknown>): void {
  if (proto[PATCHED]) return;
  const original = proto["prepare"] as Fn;
  const wrapper: Fn = function (this: unknown, ...args: unknown[]) {
    const stmt = original.apply(this, args);
    try {
      if (stmt && typeof stmt === "object") wrapStatement(stmt);
    } catch {
      // An unwrappable statement is still a working statement.
    }
    return stmt;
  };
  Object.defineProperty(wrapper, PATCHED, { value: true });
  proto["prepare"] = wrapper;
  proto[PATCHED] = true;
  patches.push({ proto, original, wrapper });
}

/**
 * Instruments the better-sqlite3 `Database` class that `db` is an instance
 * of: statements prepared from now on (on any connection) are traced.
 * Idempotent, and never throws.
 *
 * @param db - any better-sqlite3 `Database` instance.
 * @returns `db`, for chaining.
 */
export function instrumentSqlite<D extends object>(db: D): D {
  try {
    const proto = Object.getPrototypeOf(db) as Record<string | symbol, unknown> | null;
    if (proto && typeof proto["prepare"] === "function") patchProto(proto);
  } catch {
    // Instrumentation is optional; the database keeps working.
  }
  return db;
}

/** The `sqlite` integration: `unpatch()` restores every instrumented prototype. */
export const sqliteIntegration: Integration = {
  name: "sqlite",
  isAvailable: () => true,
  patch() {
    // Patching needs a database instance (the class is the app's import, not
    // ours); `instrumentSqlite(db)` is the entry point. Re-apply after unpatch:
    for (const p of patches) {
      if (p.proto["prepare"] !== p.wrapper) {
        p.proto["prepare"] = p.wrapper;
        p.proto[PATCHED] = true;
      }
    }
  },
  unpatch() {
    for (const p of patches) {
      if (p.proto["prepare"] === p.wrapper) p.proto["prepare"] = p.original;
      p.proto[PATCHED] = false;
    }
  },
};
