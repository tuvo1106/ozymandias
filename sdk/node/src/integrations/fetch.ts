/**
 * `fetch` integration: `http.client` spans around outgoing requests, and
 * propagation headers for hosts the app has said it trusts.
 *
 * - **Wraps whatever `globalThis.fetch` is at the time.** Next.js patches
 *   `fetch` itself (for its data cache); wrapping the current value instead of
 *   a saved original keeps that chain intact, and means the SDK sits outside
 *   Next's layer, timing what the app experiences.
 * - **Idempotent.** The wrapper carries a marker; a second call (or a second
 *   copy of the package) only updates the allow-list.
 * - **Headers only for allow-listed hosts, default none.** Trace headers
 *   reveal trace ids and sampling decisions, and an app calls third parties
 *   (a payment provider, a metadata API) that have no business seeing them.
 *   Matching is exact on `host` or `host:port`, or `*.suffix` for subdomains.
 * - **Only when a trace is active.** A fetch outside any request would be an
 *   orphan root; Next's own background fetches would flood the agent.
 * - **Semantics preserved.** The same arguments go to the original (the
 *   abort signal included, so aborting rejects exactly as before), the same
 *   `Response` object comes back, and a rejection is re-thrown as the same
 *   error. The span ends when headers arrive, not when the body is consumed:
 *   the body stream belongs to the caller.
 *
 * @module
 */
import { globalState } from "../state.js";
import { type Span, tracer } from "../trace/tracer.js";
import type { Integration } from "./types.js";

type FetchFn = typeof globalThis.fetch;
/**
 * One record per process, shared by every copy of the package, describing the
 * wrapper in place. It lives on `globalThis` (not on the wrapper function) so
 * `unpatch` can find it even after another library wrapped fetch on top of ours.
 */
const META = Symbol.for("ozy.fetch.meta");

interface FetchMeta {
  wrapper: FetchFn;
  original: FetchFn;
  active: boolean;
}

function meta(): FetchMeta | undefined {
  return (globalThis as unknown as Record<symbol, FetchMeta | undefined>)[META];
}

/** Options for {@link instrumentFetch}. */
export interface InstrumentFetchOptions {
  /** Hosts to send `x-ozy-*` headers to: `host`, `host:port` or `*.suffix`. Default: none. */
  propagateTo?: string[];
}

function hostAllowed(host: string, allow: string[]): boolean {
  const h = host.toLowerCase();
  return allow.some((entry) => {
    const e = entry.toLowerCase();
    if (e.startsWith("*.")) return h.split(":")[0]!.endsWith(e.slice(1));
    return e === h || (!e.includes(":") && e === h.split(":")[0]);
  });
}

function describe(input: unknown, init: RequestInit | undefined): { method: string; url: URL | null } {
  let method = "GET";
  let raw: string | undefined;
  if (typeof input === "string") raw = input;
  else if (input instanceof URL) raw = input.href;
  else if (input && typeof input === "object") {
    raw = (input as { url?: string }).url;
    method = (input as { method?: string }).method ?? method;
  }
  if (init?.method) method = init.method;
  let url: URL | null = null;
  try {
    if (raw) url = new URL(raw);
  } catch {
    // Relative or malformed: the original fetch will report it.
  }
  return { method: method.toUpperCase(), url };
}

function startClientSpan(input: unknown, init: RequestInit | undefined): { span: Span; host: string } | null {
  if (!tracer.scope().active()) return null;
  const { method, url } = describe(input, init);
  const host = url?.host ?? "unknown";
  // The query and credentials are dropped: they carry secrets and ids.
  const clean = url ? `${url.protocol}//${url.host}${url.pathname}` : "";
  const span = tracer.startSpan("http.client", {
    resource: `${method} ${host}`,
    type: "http",
    tags: { "http.method": method, "http.url": clean, "span.kind": "client" },
  });
  return { span, host };
}

function withHeaders(input: unknown, init: RequestInit | undefined, span: Span): RequestInit {
  const base = init?.headers ?? (input && typeof input === "object" ? (input as { headers?: ConstructorParameters<typeof Headers>[0] }).headers : undefined);
  const headers = new Headers(base);
  tracer.inject(span, headers);
  return { ...init, headers };
}

function makeWrapper(inner: FetchFn, m: { active: boolean }): FetchFn {
  const wrapper = function ozyFetch(this: unknown, input: Parameters<FetchFn>[0], init?: RequestInit): Promise<Response> {
    let started: { span: Span; host: string } | null = null;
    let callInit = init;
    try {
      if (m.active) {
        started = startClientSpan(input, init);
        if (started && hostAllowed(started.host, globalState().fetchAllow)) callInit = withHeaders(input, init, started.span);
      }
    } catch {
      started = null;
      callInit = init;
    }
    if (!started) return inner.call(this, input, callInit);
    const { span } = started;
    let pending: Promise<Response>;
    try {
      pending = tracer.scope().activate(span, () => inner.call(this, input, callInit));
    } catch (err) {
      span.setError(err);
      span.finish();
      throw err;
    }
    return pending.then(
      (res) => {
        try {
          span.setTag("http.status_code", res.status);
          if (res.status >= 500) span.setError(new Error(`HTTP ${res.status}`));
        } catch {
          // best effort
        }
        span.finish();
        return res;
      },
      (err: unknown) => {
        span.setError(err);
        span.finish();
        throw err;
      },
    );
  } as FetchFn;
  return wrapper;
}

/**
 * Wraps `globalThis.fetch` once. Calling it again (also from another copy of
 * the package) updates the allow-list and re-activates a previously
 * `unpatch`ed wrapper instead of stacking another layer.
 *
 * @param opts - which hosts get propagation headers.
 */
export function instrumentFetch(opts: InstrumentFetchOptions = {}): void {
  try {
    const state = globalState();
    if (Array.isArray(opts.propagateTo)) state.fetchAllow = opts.propagateTo.filter((h) => typeof h === "string" && h !== "");
    const existing = meta();
    if (existing) {
      existing.active = true;
      // Put our wrapper back if unpatch() removed it and nothing else replaced it.
      if (globalThis.fetch === existing.original) globalThis.fetch = existing.wrapper;
      return;
    }
    const current = globalThis.fetch;
    if (typeof current !== "function") return;
    const m: FetchMeta = { wrapper: undefined as unknown as FetchFn, original: current, active: true };
    m.wrapper = makeWrapper(current, m);
    (globalThis as unknown as Record<symbol, FetchMeta>)[META] = m;
    globalThis.fetch = m.wrapper;
  } catch {
    // The host's fetch stays as it was.
  }
}

/**
 * Turns the wrapper off. If it is still the outermost `fetch`, the original is
 * restored; if another library wrapped on top of it, the wrapper stays in the
 * chain as a pass-through (cutting a layer out of the middle would drop the
 * other library's instrumentation with it).
 */
export function uninstrumentFetch(): void {
  try {
    const m = meta();
    if (!m) return;
    m.active = false;
    if (globalThis.fetch === m.wrapper) globalThis.fetch = m.original;
  } catch {
    // ignore
  }
}

/** The `fetch` integration. Patched via `init({integrations: ["fetch"]})`; allow-list via {@link instrumentFetch}. */
export const fetchIntegration: Integration = {
  name: "fetch",
  isAvailable: () => typeof globalThis.fetch === "function",
  patch: () => instrumentFetch(),
  unpatch: uninstrumentFetch,
};
