/**
 * Node HTTP server integration: one `http.request` span and the request
 * metrics per request, as Connect-style middleware (Express, Connect, or a
 * plain `node:http` handler).
 *
 * Why middleware and not patching `http.Server`: the same reason as next.ts.
 * There is no require hook in this SDK, and an app adds one line
 * (`app.use(traceRequests())`) where a patch would have to guess which server
 * is the app's. The middleware speaks Node's `IncomingMessage` /
 * `ServerResponse`, so it does not import Express; it only reads the two
 * fields Express adds to the request (`route`, `baseUrl`) when they exist.
 *
 * Mental model, the same as the Python ASGI/WSGI middlewares:
 *
 * - The span starts when the request reaches the middleware and is made the
 *   active span (AsyncLocalStorage) for everything `next()` runs, so spans the
 *   handlers start (queries, outgoing fetches) are its children, across
 *   `await`.
 * - It ends on the response's `finish` (handed to the OS) or `close` (the
 *   connection went first). The status is `res.statusCode` then. A connection
 *   that closed before any header was sent is `499`, the client's doing, and
 *   not an error; one that closed mid-body keeps the status already sent.
 * - The route is read at the end, after routing: Express's matched
 *   `req.route.path` behind the `req.baseUrl` of the routers it went through.
 *   No match is `route:unmatched`, so a scanner is one series. Other
 *   frameworks pass `route: (req) => pattern`.
 *
 * Emitted metrics (`http.request.count` c, `http.request.duration` d ms),
 * tags `route`, `method`, `status`, `status_class`: the names and tags the
 * Python middlewares emit, so one dashboard reads both.
 *
 * **The bound on `route`.** Express keeps the *matched text* of a mount, not
 * its pattern: under `app.use("/:org/users", users)` the base is
 * `/acme/users`. A parameter value is put back as `:name` when Express still
 * lists it in `req.params` (a router with `mergeParams`), and the rest goes
 * through {@link normalizePath}, which turns ids into `:id`; a slug in a mount
 * path can survive both. So each middleware also caps the number of distinct
 * routes it reports (`maxRoutes`, default 200): past it, a new route is
 * reported as `other`. A tag is a series, and an unbounded one is a bill.
 *
 * Errors: Express answers a thrown handler error with a 500 page and the
 * middleware never sees the error. {@link traceErrors}, added after the
 * routes, records it so the span names it.
 *
 * Rejected alternative: patching Express's router (dd-trace's approach), which
 * does recover the mount pattern. It needs a require hook and tracks Express
 * internals across majors, the two things this SDK chose not to do.
 *
 * @module
 */
import type { IncomingMessage, ServerResponse } from "node:http";
import { globalState, integrationOff, setIntegrationOn } from "../state.js";
import { NOOP_SPAN, type Span, tracer } from "../trace/tracer.js";
import { normalizePath } from "../trace/wire.js";
import type { Integration } from "./types.js";

/** The route tag of a request no route matched. */
export const UNMATCHED = "unmatched";
/** The route tag past the `maxRoutes` cap. */
export const OVERFLOW = "other";
const CLIENT_CLOSED = 499;
const SERVER_ERROR = 500;
const METHODS: ReadonlySet<string> = new Set([
  "GET",
  "POST",
  "PUT",
  "PATCH",
  "DELETE",
  "HEAD",
  "OPTIONS",
]);
/** Marks a request the outermost middleware has claimed, so a second one passes it through. */
const CLAIMED = Symbol.for("ozy.http.claimed");
/** Where {@link traceErrors} leaves the error for the span. */
const ERROR = Symbol.for("ozy.http.error");

/** `next` as Connect and Express call it. */
export type Next = (err?: unknown) => void;

/** Options for {@link traceRequests}. */
export interface TraceRequestsOptions {
  /** Exact paths (no query string) to leave untraced and uncounted: health checks. */
  excludePaths?: string | Iterable<string>;
  /**
   * The route pattern of a finished request, for frameworks that are not
   * Express. Return undefined for "no match". Default: Express's.
   */
  route?: (req: IncomingMessage) => string | undefined;
  /** Distinct routes reported before new ones become `other`. Default 200. */
  maxRoutes?: number;
  /** Metric name for the counter. Default `http.request.count`. */
  countName?: string;
  /** Metric name for the duration. Default `http.request.duration`. */
  durationName?: string;
}

/** The `http` integration: the middleware is explicit, so patch/unpatch switch it on and off. */
export const httpIntegration: Integration = {
  name: "http",
  isAvailable: () => true,
  patch() {
    setIntegrationOn("http", true);
  },
  unpatch() {
    setIntegrationOn("http", false);
  },
};

/** The slice of an Express request this module reads. */
interface ExpressFields {
  originalUrl?: unknown;
  baseUrl?: unknown;
  route?: { path?: unknown };
  params?: unknown;
  [CLAIMED]?: boolean;
  [ERROR]?: unknown;
}

function excludeKey(path: string): string {
  const lower = path.toLowerCase();
  return lower.length > 1 && lower.endsWith("/") ? lower.slice(0, -1) : lower;
}

function pathOnly(url: unknown): string {
  if (typeof url !== "string" || url === "") return "/";
  const cut = url.search(/[?#]/);
  return cut >= 0 ? url.slice(0, cut) || "/" : url;
}

/**
 * Express's route for a finished request: the routers' base, with known
 * parameter values put back as `:name` and the remaining ids normalized,
 * followed by the matched route's own pattern. Undefined when nothing matched.
 *
 * Known limit: parameter values are put back by value, not position, so a literal base segment equal
 * to a parameter's value (a router at `/users/:user` hit as `/users/users/42`) is labelled `:user`
 * too. The label stays bounded; it is only a wrong name for that one request shape.
 */
export function expressRoute(req: IncomingMessage): string | undefined {
  const r = req as IncomingMessage & ExpressFields;
  const pattern = r.route?.path;
  if (typeof pattern !== "string") return undefined;
  const base = typeof r.baseUrl === "string" ? r.baseUrl : "";
  if (base === "") return pattern;
  const params =
    r.params && typeof r.params === "object"
      ? (r.params as Record<string, unknown>)
      : {};
  const byValue = new Map<string, string>();
  for (const [name, value] of Object.entries(params)) {
    if (typeof value === "string" && value !== "" && !byValue.has(value))
      byValue.set(value, name);
  }
  const segs = base.split("/").map((seg) => {
    const name = byValue.get(seg);
    return name === undefined ? seg : `:${name}`;
  });
  // normalizePath leaves `:name` alone (it is not id-like) and turns the rest into `:id`.
  const prefix = normalizePath(segs.join("/"));
  return pattern === "/" ? prefix : prefix + pattern;
}

function statusOf(res: ServerResponse): number {
  const code = res.statusCode;
  return Number.isInteger(code) && code >= 100 && code <= 599
    ? code
    : SERVER_ERROR;
}

/**
 * Returns Connect-style middleware that traces and counts every request it
 * sees. Add it first, so it times everything after it:
 *
 * ```ts
 * app.use(traceRequests({ excludePaths: ["/healthz"] }));
 * // ...routes...
 * app.use(traceErrors());
 * ```
 *
 * For a plain `node:http` server: `(req, res) => mw(req, res, () => handler(req, res))`.
 * With neither tracing nor metrics enabled it only calls `next()`. Never
 * throws on its own account; a handler's own exception passes through.
 *
 * @param opts - exclusions, a route function for non-Express apps, the route cap, metric names.
 * @returns `(req, res, next) => void`.
 */
export function traceRequests(
  opts: TraceRequestsOptions = {},
): (req: IncomingMessage, res: ServerResponse, next: Next) => void {
  const excluded = opts.excludePaths ?? [];
  // A bare string is one path, not its characters.
  // Compared the way Express routes: case-insensitively and without a trailing slash, so a probe of
  // `/Healthz/` is excluded when `/healthz` is.
  const exclude = new Set(
    [...(typeof excluded === "string" ? [excluded] : excluded)].map(excludeKey),
  );
  const routeOf = opts.route ?? expressRoute;
  const maxRoutes = opts.maxRoutes ?? 200;
  const countName = opts.countName ?? "http.request.count";
  const durationName = opts.durationName ?? "http.request.duration";
  const seen = new Set<string>();

  const bounded = (route: string | undefined): string => {
    if (typeof route !== "string" || route === "") return UNMATCHED;
    if (seen.has(route)) return route;
    if (seen.size >= maxRoutes) return OVERFLOW;
    seen.add(route);
    return route;
  };

  return function ozyTraceRequests(
    req: IncomingMessage,
    res: ServerResponse,
    next: Next,
  ): void {
    let span: Span = NOOP_SPAN;
    let path = "/";
    let method = "OTHER";
    let started = 0;
    // `next()` is called only after the setup below, never inside it: a plain handler that
    // throws synchronously would otherwise land in the `catch` and be run a second time.
    let passThrough = false;
    try {
      const r = req as IncomingMessage & ExpressFields;
      const statsd = globalState().statsd;
      path = pathOnly(
        typeof r.originalUrl === "string" ? r.originalUrl : req.url,
      );
      if (integrationOff("http") || r[CLAIMED]) {
        passThrough = true;
      } else if (exclude.has(excludeKey(path))) {
        r[CLAIMED] = true; // excluded here is excluded for any middleware further in
        passThrough = true;
      } else {
        span = tracer.startSpan("http.request", {
          type: "web",
          childOf: tracer.extract(req.headers as Record<string, unknown>),
          tags: { "span.kind": "server" },
        });
        if (span === NOOP_SPAN && !statsd) passThrough = true;
      }
      if (!passThrough) {
        r[CLAIMED] = true;
        method =
          typeof req.method === "string" && METHODS.has(req.method)
            ? req.method
            : "OTHER";
        started = performance.now();
        let done = false;
        const finish = (): void => {
          if (done) return;
          done = true;
          try {
            end(r, res, span, path, method, performance.now() - started);
          } catch {
            // Recording is best-effort.
          }
        };
        res.once("finish", finish);
        res.once("close", finish);
      }
    } catch {
      passThrough = true;
    }
    if (passThrough || span === NOOP_SPAN) {
      next();
      return;
    }
    try {
      tracer.scope().activate(span, () => next());
    } catch (err) {
      // Express catches its handlers' errors itself; a plain handler's
      // reaches here. It is the app's, so it goes on unchanged, and the span
      // names it if the response turns out to be a 5xx.
      (req as IncomingMessage & ExpressFields)[ERROR] = err;
      throw err;
    }
  };

  function end(
    r: IncomingMessage & ExpressFields,
    res: ServerResponse,
    span: Span,
    path: string,
    method: string,
    elapsedMs: number,
  ): void {
    // The connection closing before any header went out is the client
    // leaving; after the headers, the status sent stands.
    const status =
      !res.writableFinished && !res.headersSent ? CLIENT_CLOSED : statusOf(res);
    let route: string;
    try {
      route = bounded(routeOf(r));
    } catch {
      route = UNMATCHED;
    }
    if (span !== NOOP_SPAN) {
      try {
        const known = route !== UNMATCHED && route !== OVERFLOW;
        span.setResource(`${method} ${known ? route : normalizePath(path)}`);
        span.setTag("http.method", method);
        span.setTag("http.url", path);
        if (known) span.setTag("http.route", route);
        span.setTag("http.status_code", status);
        const swallowed = r[ERROR];
        if (status >= SERVER_ERROR) {
          span.setError(
            swallowed instanceof Error
              ? swallowed
              : new Error(`HTTP ${status}`),
          );
        }
      } catch {
        // Tagging is best-effort; the span is still finished below.
      }
      span.finish();
    }
    const statsd = globalState().statsd;
    if (!statsd) return;
    try {
      const tags = [
        `route:${route}`,
        `method:${method}`,
        `status:${status}`,
        `status_class:${Math.floor(status / 100)}xx`,
      ];
      statsd.metric("c", countName, 1, { tags });
      statsd.metric("d", durationName, elapsedMs, { tags });
    } catch (err) {
      statsd.recordError(err);
    }
  }
}

/**
 * Returns an Express error handler that hands the error to the request's span
 * and passes it on unchanged (`next(err)`), so the span of a 500 says why.
 * Add it after the routes and before your own error handlers. It has four
 * parameters because that is how Express recognises an error handler.
 *
 * @returns `(err, req, res, next) => void`.
 */
export function traceErrors(): (
  err: unknown,
  req: IncomingMessage,
  res: ServerResponse,
  next: Next,
) => void {
  return function ozyTraceErrors(
    err: unknown,
    req: IncomingMessage,
    _res: ServerResponse,
    next: Next,
  ): void {
    try {
      (req as IncomingMessage & ExpressFields)[ERROR] = err;
    } catch {
      // A frozen request is not our problem to fix.
    }
    next(err);
  };
}
