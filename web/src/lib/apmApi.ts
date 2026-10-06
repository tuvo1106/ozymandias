/**
 * Client for ozyd's tracing API: trace search, one trace, the service map and
 * the service / resource RED tables (docs/api.md, "Tracing (APM)").
 *
 * Two units meet here and must not be confused. Request windows (`from`, `to`)
 * are unix MILLISECONDS, like the logs API. Times *inside* a trace or span
 * (`start`, `duration`) are MICROSECONDS, as on the wire; only the latency
 * percentiles of a service table are in milliseconds (`p95_ms`). Two sources
 * too: traces and the map come from the sampled trace store, the tables from
 * metrics computed before sampling, so a table's request count is exact while
 * a count of listed traces is a sample. Every response is checked against the
 * contract, so drift is a readable error rather than `undefined` in a chart.
 */
import { ApiError, getJSON } from "./metricsApi";

type FetchLike = typeof fetch;

/** The window every APM request shares: unix milliseconds, inclusive. */
export interface ApmWindow {
  from: number;
  to: number;
}

/** Filters of the trace search; an absent field is not sent. */
export interface TraceFilter {
  env?: string;
  service?: string;
  /** Matched ignoring case, so a lower-cased metric tag finds the span's own spelling. */
  resource?: string;
  name?: string;
  error?: boolean;
  minDurationMs?: number;
  maxDurationMs?: number;
  statusCode?: number;
}

/** One entry-span summary in the search results. */
export interface TraceSummary {
  trace_id: string;
  span_id: string;
  env: string;
  service: string;
  name: string;
  resource: string;
  /** Unix microseconds. */
  start: number;
  /** Microseconds. */
  duration: number;
  /** The entry span itself failed. */
  error: boolean;
  /** Some span of the trace failed (that arrived in the same request). */
  trace_error: boolean;
  status_code?: number;
}

/** One page of the search. */
export interface TracePage {
  traces: TraceSummary[];
  /** Absent on the last page. */
  cursor?: string;
  /** Index entries examined: the cost of a selective filter over a long window. */
  examined: number;
}

/** A span as stored; the same shape the SDKs sent. */
export interface TraceSpan {
  trace_id: string;
  span_id: string;
  parent_id?: string | null;
  service: string;
  name: string;
  resource: string;
  type: string;
  /** Unix microseconds. */
  start: number;
  /** Microseconds. */
  duration: number;
  error: number;
  meta?: Record<string, string>;
  metrics?: Record<string, number>;
}

/** One whole trace, as stored. */
export interface TraceDetail {
  trace_id: string;
  spans: TraceSpan[];
  services: string[];
  start: number;
  duration: number;
  span_count: number;
  errors: number;
  /** Span ids whose parent is not in the store. */
  orphans: string[];
}

/** A service in the map. */
export interface MapNode {
  service: string;
  calls_in: number;
  errors_in: number;
}

/** A call from one service to another's entry span. */
export interface MapEdge {
  parent: string;
  child: string;
  env?: string;
  calls: number;
  errors: number;
  /** Microseconds. */
  avg_duration: number;
}

/** The service map. Counts come from stored spans, so under sampling they show shape, not rate. */
export interface ServiceMap {
  nodes: MapNode[];
  edges: MapEdge[];
}

/** One row of a service or resource table. */
export interface RedRow {
  service: string;
  env?: string;
  /** The entry span's name (`http.request`, `arq.job`): kinds are not merged. */
  name: string;
  /** Lower-cased (a metric tag); absent in the service table. */
  resource?: string;
  requests: number;
  requests_per_second: number;
  errors: number;
  error_pct: number;
  /** Null when the window has no latency sketches: "no data", not 0 ms. */
  p50_ms: number | null;
  p95_ms: number | null;
  p99_ms: number | null;
  /** 30 request counts across the window. */
  sparkline: number[];
}

/** A RED table with the window it covers (unix ms). */
export interface RedTable {
  services: RedRow[];
  from: number;
  to: number;
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function bad(endpoint: string): ApiError {
  return new ApiError(`ozyd sent an unexpected ${endpoint} response`);
}

const num = (v: unknown): v is number => typeof v === "number" && Number.isFinite(v);
const str = (v: unknown): v is string => typeof v === "string";
const nullableNum = (v: unknown): v is number | null => v === null || num(v);

function isSummary(v: unknown): v is TraceSummary {
  return (
    isRecord(v) &&
    str(v.trace_id) &&
    str(v.span_id) &&
    str(v.service) &&
    str(v.name) &&
    str(v.resource) &&
    num(v.start) &&
    num(v.duration) &&
    typeof v.error === "boolean"
  );
}

function isSpan(v: unknown): v is TraceSpan {
  return (
    isRecord(v) &&
    str(v.trace_id) &&
    str(v.span_id) &&
    str(v.service) &&
    str(v.name) &&
    str(v.resource) &&
    num(v.start) &&
    num(v.duration) &&
    num(v.error) &&
    (v.parent_id === undefined || v.parent_id === null || str(v.parent_id)) &&
    (v.meta === undefined || v.meta === null || isRecord(v.meta))
  );
}

function isRed(v: unknown): v is RedRow {
  return (
    isRecord(v) &&
    str(v.service) &&
    str(v.name) &&
    num(v.requests) &&
    num(v.requests_per_second) &&
    num(v.errors) &&
    num(v.error_pct) &&
    nullableNum(v.p50_ms) &&
    nullableNum(v.p95_ms) &&
    nullableNum(v.p99_ms)
  );
}

/** Builds the query string of the trace search. */
export function searchParams(f: TraceFilter, w: ApmWindow, opts: { limit?: number; cursor?: string } = {}): URLSearchParams {
  const p = new URLSearchParams({ from: String(w.from), to: String(w.to), limit: String(opts.limit ?? 50) });
  if (f.env) p.set("env", f.env);
  if (f.service) p.set("service", f.service);
  if (f.resource) p.set("resource", f.resource);
  if (f.name) p.set("name", f.name);
  if (f.error) p.set("error", "true");
  if (f.minDurationMs !== undefined) p.set("min_duration_ms", String(f.minDurationMs));
  if (f.maxDurationMs !== undefined) p.set("max_duration_ms", String(f.maxDurationMs));
  if (f.statusCode !== undefined) p.set("status_code", String(f.statusCode));
  if (opts.cursor) p.set("cursor", opts.cursor);
  return p;
}

/** Fetches one page of entry spans, newest first; pass the previous cursor to continue. */
export async function fetchTraces(
  f: TraceFilter,
  w: ApmWindow,
  opts: { limit?: number; cursor?: string } = {},
  fetchImpl: FetchLike = fetch,
  signal?: AbortSignal,
): Promise<TracePage> {
  const body = await getJSON(`/api/v1/traces?${searchParams(f, w, opts)}`, fetchImpl, signal);
  if (!isRecord(body) || !Array.isArray(body.traces) || !body.traces.every(isSummary)) throw bad("/api/v1/traces");
  return {
    traces: body.traces,
    cursor: str(body.cursor) && body.cursor ? body.cursor : undefined,
    examined: num(body.examined) ? body.examined : 0,
  };
}

/** Fetches every stored span of one trace. A 404 is an ApiError with `status` 404: never sampled, or past retention. */
export async function fetchTrace(traceId: string, fetchImpl: FetchLike = fetch, signal?: AbortSignal): Promise<TraceDetail> {
  const body = await getJSON(`/api/v1/traces/${encodeURIComponent(traceId)}`, fetchImpl, signal);
  if (!isRecord(body) || !Array.isArray(body.spans) || !body.spans.every(isSpan) || !num(body.start) || !num(body.duration)) {
    throw bad("/api/v1/traces/{id}");
  }
  return {
    trace_id: str(body.trace_id) ? body.trace_id : traceId,
    spans: body.spans,
    services: Array.isArray(body.services) ? body.services.filter(str) : [],
    start: body.start,
    duration: body.duration,
    span_count: num(body.span_count) ? body.span_count : body.spans.length,
    errors: num(body.errors) ? body.errors : 0,
    orphans: Array.isArray(body.orphans) ? body.orphans.filter(str) : [],
  };
}

/** Fetches the service map for a window. */
export async function fetchServiceMap(env: string, w: ApmWindow, fetchImpl: FetchLike = fetch, signal?: AbortSignal): Promise<ServiceMap> {
  const p = new URLSearchParams({ from: String(w.from), to: String(w.to) });
  if (env) p.set("env", env);
  const body = await getJSON(`/api/v1/service-map?${p}`, fetchImpl, signal);
  if (!isRecord(body)) throw bad("/api/v1/service-map");
  const nodes = Array.isArray(body.nodes) ? body.nodes : [];
  const edges = Array.isArray(body.edges) ? body.edges : [];
  const okNode = (n: unknown): n is MapNode => isRecord(n) && str(n.service) && num(n.calls_in) && num(n.errors_in);
  const okEdge = (e: unknown): e is MapEdge => isRecord(e) && str(e.parent) && str(e.child) && num(e.calls) && num(e.errors) && num(e.avg_duration);
  if (!nodes.every(okNode) || !edges.every(okEdge)) throw bad("/api/v1/service-map");
  return { nodes, edges };
}

async function fetchRed(url: string, endpoint: string, fetchImpl: FetchLike, signal?: AbortSignal): Promise<RedTable> {
  const body = await getJSON(url, fetchImpl, signal);
  if (!isRecord(body) || !Array.isArray(body.services) || !body.services.every(isRed) || !num(body.from) || !num(body.to)) throw bad(endpoint);
  const services = (body.services as RedRow[]).map((r) => ({ ...r, sparkline: Array.isArray(r.sparkline) ? r.sparkline.filter(num) : [] }));
  return { services, from: body.from, to: body.to };
}

function redParams(env: string, w: ApmWindow): string {
  const p = new URLSearchParams({ from: String(w.from), to: String(w.to) });
  if (env) p.set("env", env);
  return p.toString();
}

/** Fetches the service table: one row per (service, env, entry span name). */
export function fetchServices(env: string, w: ApmWindow, fetchImpl: FetchLike = fetch, signal?: AbortSignal): Promise<RedTable> {
  return fetchRed(`/api/v1/services?${redParams(env, w)}`, "/api/v1/services", fetchImpl, signal);
}

/** Fetches one service's resource table. */
export function fetchResources(service: string, env: string, w: ApmWindow, fetchImpl: FetchLike = fetch, signal?: AbortSignal): Promise<RedTable> {
  return fetchRed(`/api/v1/services/${encodeURIComponent(service)}/resources?${redParams(env, w)}`, "/api/v1/services/{service}/resources", fetchImpl, signal);
}
