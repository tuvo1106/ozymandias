/**
 * Client for ozyd's log API: search pages, the histogram, facets and the
 * live-tail event stream (docs/api.md).
 *
 * Times on this API are unix MILLISECONDS, unlike the metrics API's seconds: a
 * log's timestamp, a histogram bar and a paging cursor are all milliseconds,
 * and a page fed back as a window needs no conversion. Every response is
 * checked against the contract before it reaches a component, so a drifted
 * server is a readable error rather than `undefined` inside a row renderer.
 */
import { ApiError, getJSON } from "./metricsApi";

type FetchLike = typeof fetch;

/** One stored log as the API returns it. */
export interface LogEntry {
  ts: number;
  message: string;
  status: string;
  service: string;
  source?: string;
  host?: string;
  tags?: string[];
  attrs?: Record<string, unknown>;
  trace_id?: string;
  span_id?: string;
}

/** What a query cost, as the server reports it. */
export interface LogStats {
  streams: number;
  blocks_read: number;
  blocks_skipped: number;
  bytes_read: number;
  entries_examined: number;
}

/** One page of search results. */
export interface LogPage {
  logs: LogEntry[];
  /** Absent on the last page. */
  cursor?: string;
  /** The scan budget ran out: a correct prefix, not everything. */
  truncated: boolean;
  stats?: LogStats;
}

/** One histogram bar: counts by the `by` value ("" without one). */
export interface HistogramBucket {
  ts: number;
  counts: Record<string, number>;
}

/** The histogram above the list. */
export interface Histogram {
  /** The window that was asked for (unix ms): bars are laid out over it, not over the data's own span. */
  from: number;
  to: number;
  interval_ms: number;
  buckets: HistogramBucket[];
  truncated: boolean;
}

/** One facet value and how many matching logs have it. */
export interface FacetValue {
  value: string;
  count: number;
}

/** The sidebar's facets, by key. */
export interface Facets {
  facets: Record<string, FacetValue[]>;
  capped: string[];
  truncated: boolean;
}

/** The window and filter every logs request shares. */
export interface LogsQuery {
  q: string;
  /** Unix milliseconds, inclusive. */
  from: number;
  to: number;
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function bad(endpoint: string): ApiError {
  return new ApiError(`ozyd sent an unexpected ${endpoint} response`);
}

function isLogEntry(v: unknown): v is LogEntry {
  return (
    isRecord(v) &&
    typeof v.ts === "number" &&
    typeof v.message === "string" &&
    typeof v.status === "string" &&
    typeof v.service === "string" &&
    (v.attrs === undefined || isRecord(v.attrs))
  );
}

function params(query: LogsQuery, extra: Record<string, string> = {}): URLSearchParams {
  const p = new URLSearchParams({ from: String(query.from), to: String(query.to), ...extra });
  if (query.q) p.set("q", query.q);
  return p;
}

/** Fetches one page of logs, newest first; pass the previous page's cursor to continue. */
export async function fetchLogs(
  query: LogsQuery,
  opts: { limit?: number; cursor?: string } = {},
  fetchImpl: FetchLike = fetch,
  signal?: AbortSignal,
): Promise<LogPage> {
  const extra: Record<string, string> = { limit: String(opts.limit ?? 200) };
  if (opts.cursor) extra.cursor = opts.cursor;
  const body = await getJSON(`/api/v1/logs?${params(query, extra)}`, fetchImpl, signal);
  if (!isRecord(body) || !Array.isArray(body.logs) || !body.logs.every(isLogEntry)) throw bad("/api/v1/logs");
  return {
    logs: body.logs,
    cursor: typeof body.cursor === "string" && body.cursor ? body.cursor : undefined,
    truncated: body.truncated === true,
    stats: isRecord(body.stats) ? (body.stats as unknown as LogStats) : undefined,
  };
}

/** Fetches the histogram, split by `by` (a label or `@attr`). */
export async function fetchHistogram(
  query: LogsQuery,
  by: string,
  fetchImpl: FetchLike = fetch,
  signal?: AbortSignal,
): Promise<Histogram> {
  const body = await getJSON(`/api/v1/logs/aggregate?${params(query, { by })}`, fetchImpl, signal);
  if (!isRecord(body) || typeof body.interval_ms !== "number" || !Array.isArray(body.buckets)) throw bad("/api/v1/logs/aggregate");
  const buckets = body.buckets.filter((b): b is HistogramBucket => isRecord(b) && typeof b.ts === "number" && isRecord(b.counts));
  if (buckets.length !== body.buckets.length) throw bad("/api/v1/logs/aggregate");
  return { from: query.from, to: query.to, interval_ms: body.interval_ms, buckets, truncated: body.truncated === true };
}

/** Fetches the most frequent values of `keys` among the matching logs. */
export async function fetchFacets(
  query: LogsQuery,
  keys: readonly string[],
  fetchImpl: FetchLike = fetch,
  signal?: AbortSignal,
): Promise<Facets> {
  const body = await getJSON(`/api/v1/logs/facets?${params(query, { keys: keys.join(","), limit: "8" })}`, fetchImpl, signal);
  if (!isRecord(body) || !isRecord(body.facets)) throw bad("/api/v1/logs/facets");
  const facets: Record<string, FacetValue[]> = {};
  for (const [k, vs] of Object.entries(body.facets)) {
    if (!Array.isArray(vs) || !vs.every((v) => isRecord(v) && typeof v.value === "string" && typeof v.count === "number")) {
      throw bad("/api/v1/logs/facets");
    }
    facets[k] = vs as FacetValue[];
  }
  return {
    facets,
    capped: Array.isArray(body.capped) ? body.capped.filter((c): c is string => typeof c === "string") : [],
    truncated: body.truncated === true,
  };
}

/** The slice of EventSource the tail uses, so tests can drive it without a server. */
export interface TailSource {
  addEventListener(type: string, fn: (e: MessageEvent) => void): void;
  onerror: ((e: Event) => void) | null;
  onopen: ((e: Event) => void) | null;
  close(): void;
}

/** What the tail reports. */
export interface TailHandlers {
  onLog: (log: LogEntry) => void;
  /** The server dropped this many logs because the reader fell behind. */
  onDropped: (n: number) => void;
  onOpen: () => void;
  /** The stream broke; EventSource itself reconnects, so this is a status, not an end. */
  onError: () => void;
}

/**
 * Opens the live tail for a query and returns a function that closes it.
 * Malformed events are ignored (a tail that throws on one bad frame would
 * stop showing logs).
 */
export function openTail(
  q: string,
  handlers: TailHandlers,
  make: (url: string) => TailSource = (url) => new EventSource(url),
): () => void {
  const src = make(`/api/v1/logs/tail${q ? `?${new URLSearchParams({ q })}` : ""}`);
  src.addEventListener("log", (e) => {
    try {
      const v: unknown = JSON.parse(String(e.data));
      if (isLogEntry(v)) handlers.onLog(v);
    } catch {
      /* one bad frame must not end the tail */
    }
  });
  src.addEventListener("dropped", (e) => {
    try {
      const v: unknown = JSON.parse(String(e.data));
      if (isRecord(v) && typeof v.dropped === "number") handlers.onDropped(v.dropped);
    } catch {
      /* ignore */
    }
  });
  src.onopen = () => handlers.onOpen();
  src.onerror = () => handlers.onError();
  return () => src.close();
}
