/**
 * Client for ozyd's metrics query API (M1 spec §3.4): metric-name and
 * tag autocomplete, and the structured `/api/v1/query` endpoint.
 *
 * Every function validates the response shape before returning it. The UI is
 * built against a contract, not against whatever the server happens to send,
 * so a drifted server shows up as a readable error in the page rather than as
 * `undefined is not a function` deep inside the chart. Like health.ts, the
 * fetch implementation is an argument so tests run without a server.
 */
import { formatFilters, type ExplorerState } from "./explorerState";
import type { ResolvedRange } from "./timeRange";

/**
 * An error the UI can show as-is. `status` is the HTTP status when the server
 * answered, and undefined when it could not be reached; callers use it to
 * decide whether retrying could help (a 400 never gets better).
 */
export class ApiError extends Error {
  readonly status: number | undefined;

  /** Creates an error carrying the HTTP status, if there was one. */
  constructor(message: string, status?: number, options?: ErrorOptions) {
    super(message, options);
    this.name = "ApiError";
    this.status = status;
  }
}

/** One series of a query result. */
export interface Series {
  metric: string;
  /** The group-by tag values identifying this series; empty without `by`. */
  tags: Record<string, string>;
  /** `[unix ms, value]`, ascending; a null value is an empty bucket. */
  points: [number, number | null][];
}

/** The body of a successful `/api/v1/query`. */
export interface QueryResult {
  status: "ok";
  /** Window actually evaluated, unix seconds. */
  from: number;
  to: number;
  /** Bucket width the server chose (or was given), seconds. */
  interval: number;
  series: Series[];
}

type FetchLike = typeof fetch;

/**
 * GETs a JSON document and turns every failure into an ApiError with a
 * message fit for the page: the server's own `{"error": …}` text when it sent
 * one, otherwise the status line or the network error. Aborts pass through
 * untouched so callers (TanStack Query) can recognise and ignore them.
 */
export async function getJSON(url: string, fetchImpl: FetchLike = fetch, signal?: AbortSignal): Promise<unknown> {
  let res: Response;
  try {
    res = await fetchImpl(url, { signal, headers: { Accept: "application/json" } });
  } catch (err) {
    if (err instanceof DOMException && err.name === "AbortError") throw err;
    throw new ApiError(`ozyd is unreachable: ${err instanceof Error ? err.message : String(err)}`, undefined, {
      cause: err,
    });
  }
  const body: unknown = await res.json().catch(() => undefined);
  if (!res.ok) {
    const msg = isRecord(body) && typeof body.error === "string" && body.error ? body.error : undefined;
    throw new ApiError(msg ?? `ozyd answered ${res.status} ${res.statusText}`.trim(), res.status);
  }
  return body;
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function isStringArray(v: unknown): v is string[] {
  return Array.isArray(v) && v.every((x) => typeof x === "string");
}

function stringList(body: unknown, field: string, endpoint: string): string[] {
  const v = isRecord(body) ? body[field] : undefined;
  if (!isStringArray(v)) throw new ApiError(`ozyd sent an unexpected ${endpoint} response`);
  return v;
}

/** Lists metric names starting with `prefix` (all names when empty). */
export async function fetchMetricNames(
  prefix: string,
  limit: number,
  fetchImpl: FetchLike = fetch,
  signal?: AbortSignal,
): Promise<string[]> {
  const q = new URLSearchParams({ prefix, limit: String(limit) });
  return stringList(await getJSON(`/api/v1/metrics?${q}`, fetchImpl, signal), "metrics", "/api/v1/metrics");
}

/** Lists the tag keys seen on a metric. */
export async function fetchTagKeys(metric: string, fetchImpl: FetchLike = fetch, signal?: AbortSignal): Promise<string[]> {
  const q = new URLSearchParams({ metric });
  return stringList(await getJSON(`/api/v1/tags?${q}`, fetchImpl, signal), "keys", "/api/v1/tags");
}

/** Lists values seen for one tag key on a metric. */
export async function fetchTagValues(
  metric: string,
  key: string,
  limit: number,
  fetchImpl: FetchLike = fetch,
  signal?: AbortSignal,
): Promise<string[]> {
  const q = new URLSearchParams({ metric, key, limit: String(limit) });
  return stringList(await getJSON(`/api/v1/tags/values?${q}`, fetchImpl, signal), "values", "/api/v1/tags/values");
}

/**
 * Builds the `/api/v1/query` search string for a state and a resolved
 * window. Empty `filter`/`by` are omitted rather than sent blank, and
 * `interval` is left to the server (range/300 rounded to 10 s) unless given.
 */
export function buildQueryParams(state: ExplorerState, range: ResolvedRange, interval?: number): URLSearchParams {
  const q = new URLSearchParams({ metric: state.metric });
  if (state.filters.length) q.set("filter", formatFilters(state.filters));
  if (state.by.length) q.set("by", state.by.join(","));
  q.set("agg", state.agg);
  q.set("from", String(range.from));
  q.set("to", String(range.to));
  if (interval !== undefined) q.set("interval", String(interval));
  return q;
}

function isPoint(v: unknown): v is [number, number | null] {
  return (
    Array.isArray(v) && v.length === 2 && typeof v[0] === "number" && (typeof v[1] === "number" || v[1] === null)
  );
}

function isSeries(v: unknown): v is Series {
  return (
    isRecord(v) &&
    typeof v.metric === "string" &&
    isRecord(v.tags) &&
    Object.values(v.tags).every((t) => typeof t === "string") &&
    Array.isArray(v.points) &&
    v.points.every(isPoint)
  );
}

/** Reports whether a value is a well-formed QueryResult. */
export function isQueryResult(v: unknown): v is QueryResult {
  return (
    isRecord(v) &&
    v.status === "ok" &&
    typeof v.from === "number" &&
    typeof v.to === "number" &&
    typeof v.interval === "number" &&
    Array.isArray(v.series) &&
    v.series.every(isSeries)
  );
}

/** Runs a query and validates the result. */
export async function fetchQuery(
  state: ExplorerState,
  range: ResolvedRange,
  fetchImpl: FetchLike = fetch,
  signal?: AbortSignal,
): Promise<QueryResult> {
  const body = await getJSON(`/api/v1/query?${buildQueryParams(state, range)}`, fetchImpl, signal);
  if (!isQueryResult(body)) throw new ApiError("ozyd sent an unexpected /api/v1/query response");
  return body;
}
