/**
 * Next.js App Router integration: explicit route wrapping.
 *
 * Why wrapping and not patching: Next bundles server code, so a `require`
 * hook (how dd-trace works, via require-in-the-middle / import-in-the-middle)
 * never sees most modules, and reaching into Next's internals would break on
 * every minor release. An app that already funnels its handlers through one
 * door can add one call there instead. Nothing in this module knows about a
 * particular app: it understands the web-standard `Request` / `Response`.
 *
 * - {@link traceRoute} is the primitive: extract propagation headers, open an
 *   `http.request` span with a normalized resource, run the handler, record
 *   the status.
 * - {@link withTelemetry} wraps a route handler with it.
 *
 * @module
 */
import { NOOP_SPAN, type Span, type SpanContext, tracer } from "../trace/tracer.js";
import { normalizePath } from "../trace/wire.js";
import type { Integration } from "./types.js";

/** The slice of `Request` this integration reads. */
export interface RequestLike {
  url: string;
  method?: string;
  headers: { get(name: string): string | null | undefined };
}

let enabled = true;

/** The `next` integration: wrapping is explicit, so patch/unpatch just switch tracing on and off. */
export const nextIntegration: Integration = {
  name: "next",
  isAvailable: () => true,
  patch() {
    enabled = true;
  },
  unpatch() {
    enabled = false;
  },
};

function pathOf(url: string): { path: string; clean: string } {
  try {
    // A base makes relative request URLs (`/api/x`) parse too.
    const u = new URL(url, "http://localhost");
    const absolute = /^[a-z][a-z0-9+.-]*:/i.test(url);
    return { path: u.pathname, clean: absolute ? `${u.origin}${u.pathname}` : u.pathname };
  } catch {
    return { path: "/", clean: "/" };
  }
}

/**
 * Runs `fn` inside an `http.request` span for `req`.
 *
 * The resource is `"<METHOD> <route>"`, where the route is `routeHint` when
 * the caller knows the pattern (`/api/comics/[id]`) and the normalized path
 * otherwise, so cardinality stays bounded whichever the app supplies. The
 * query string is never recorded. A response with status >= 500 marks the
 * span failed; a thrown error marks it failed with status 500 (what Next will
 * answer) and is re-thrown unchanged. Upstream propagation headers make the
 * span a child of the caller's span.
 *
 * Never throws on its own account: if span setup fails, `fn` still runs.
 *
 * @param req - the inbound request.
 * @param routeHint - the route pattern, if known; replaces the normalized path.
 * @param fn - the handler body.
 * @returns whatever `fn` returns.
 */
export function traceRoute<T>(req: RequestLike, routeHint: string | undefined, fn: (span: Span) => T): T {
  let method = "GET";
  let resource = "";
  let route = "";
  let url = "";
  let parent: SpanContext | null = null;
  let ok = false;
  try {
    if (enabled) {
      method = String(req.method ?? "GET").toUpperCase();
      const p = pathOf(req.url);
      route = routeHint || normalizePath(p.path);
      url = p.clean;
      resource = `${method} ${route}`;
      parent = tracer.extract(req.headers ? { get: (n: string) => req.headers.get(n) } : null);
      ok = true;
    }
  } catch {
    ok = false;
  }
  // Outside the try: a handler error must not be mistaken for a setup error
  // and run the handler twice.
  if (!ok) return fn(NOOP_SPAN);
  const out = tracer.trace(
    "http.request",
    {
      resource,
      type: "web",
      childOf: parent,
      tags: { "http.method": method, "http.route": route, "http.url": url, "span.kind": "server" },
    },
    (span) => {
      // A control-flow throw (redirect(), notFound()) is a normal answer, not a failure:
      // it is recorded with its status and handed back to rethrow after the span has
      // ended without an error, because tracer.trace marks anything thrown through it.
      const settle = (err: unknown): unknown => {
        const status = controlFlowStatus(err);
        span.setTag("http.status_code", status ?? 500);
        if (status !== undefined) return new ControlFlow(err);
        throw err;
      };
      let result: T;
      try {
        result = fn(span);
      } catch (err) {
        return settle(err) as T;
      }
      if (result && typeof (result as { then?: unknown }).then === "function") {
        return (result as unknown as Promise<unknown>).then((res) => {
          recordStatus(span, res);
          return res;
        }, settle) as T;
      }
      recordStatus(span, result);
      return result;
    },
  );
  if (out instanceof ControlFlow) throw out.error;
  if (out && typeof (out as { then?: unknown }).then === "function") {
    return (out as unknown as Promise<unknown>).then((v) => {
      if (v instanceof ControlFlow) throw v.error;
      return v;
    }) as T;
  }
  return out;
}

/** What traceRoute passes out of the span for a thrown redirect or not-found. */
class ControlFlow {
  constructor(readonly error: unknown) {}
}

/**
 * The HTTP status of a Next.js control-flow error, or undefined for a real one. Next
 * implements redirect() and notFound() by throwing an error whose `digest` names it:
 * `NEXT_REDIRECT;replace;/to;307;`, `NEXT_NOT_FOUND`, `NEXT_HTTP_ERROR_FALLBACK;404`.
 */
export function controlFlowStatus(err: unknown): number | undefined {
  const digest = (err as { digest?: unknown } | null)?.digest;
  if (typeof digest !== "string") return undefined;
  if (digest.startsWith("NEXT_REDIRECT")) {
    const code = Number(digest.split(";")[3]);
    return Number.isInteger(code) && code >= 300 && code < 400 ? code : 307;
  }
  if (digest === "NEXT_NOT_FOUND") return 404;
  if (digest.startsWith("NEXT_HTTP_ERROR_FALLBACK;")) {
    const code = Number(digest.split(";")[1]);
    return Number.isInteger(code) && code >= 100 && code < 600 ? code : undefined;
  }
  return undefined;
}

function recordStatus(span: Span, res: unknown): void {
  try {
    const status = (res as { status?: unknown } | null)?.status;
    if (typeof status !== "number") return;
    span.setTag("http.status_code", status);
    if (status >= 500) span.setError(new Error(`HTTP ${status}`));
  } catch {
    // Status capture is best-effort.
  }
}

/** Options for {@link withTelemetry}. */
export interface WithTelemetryOptions {
  /** The route pattern, for example `/api/comics/[id]`. Default: the normalized path. */
  route?: string;
}

/**
 * Wraps an App Router route handler so every call is traced with
 * {@link traceRoute}. The handler's arguments and result pass through
 * untouched, so it can wrap `GET`, `POST`, and so on individually.
 *
 * @param handler - `(req, ...rest) => Response | Promise<Response>`.
 * @param opts - optional route pattern.
 * @returns a handler with the same signature.
 */
export function withTelemetry<A extends [RequestLike, ...unknown[]], R>(
  handler: (...args: A) => R,
  opts: WithTelemetryOptions = {},
): (...args: A) => R {
  return (...args: A): R => traceRoute(args[0], opts.route, () => handler(...args));
}
