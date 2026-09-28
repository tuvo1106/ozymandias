/**
 * Client for ozyd's dashboard endpoints and for the batch query a dashboard
 * runs on.
 *
 * Same contract as metricsApi.ts and for the same reason: every response is
 * checked for shape before it reaches a component, so a server that has
 * drifted shows up as a sentence in the page rather than as a crash inside a
 * chart. The fetch implementation is an argument so tests need no server.
 *
 * The one thing worth knowing about the batch: **a failed query is not a
 * failed request**. `/api/v1/query/batch` answers 200 with a per-query status,
 * because one typo in one widget must not blank the other eleven (ADR-0017).
 * So `fetchBatch` rejects only when the *request* failed, and a caller reads
 * each result's own `status`.
 */
import { ApiError, getJSON, postJSON, type Series } from "./metricsApi";
import type { Dashboard, StoredDashboard } from "./dashboard";
import type { ResolvedRange } from "./timeRange";

type FetchLike = typeof fetch;

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function isPoint(v: unknown): v is [number, number | null] {
  return Array.isArray(v) && v.length === 2 && typeof v[0] === "number" && (typeof v[1] === "number" || v[1] === null);
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

/**
 * A definition is checked only as far as the UI depends on it: a title and an
 * array of widgets with the fields the renderer reads.
 *
 * Not a full schema check, deliberately. The server validates on the way in
 * against rules TypeScript cannot express (every `$var` declared, the grid,
 * the aggregator a heatmap needs), so re-implementing half of them here would
 * be a second source of truth that drifts. What this catches is the case the
 * renderer cannot survive: something that is not a dashboard at all.
 */
function isDashboardish(v: unknown): v is Dashboard {
  return (
    isRecord(v) &&
    typeof v.title === "string" &&
    Array.isArray(v.widgets) &&
    v.widgets.every((w) => isRecord(w) && typeof w.id === "string" && typeof w.type === "string" && isRecord(w.layout))
  );
}

function isStoredDashboard(v: unknown): v is StoredDashboard {
  return isDashboardish(v) && isRecord(v) && typeof v.id === "number" && typeof v.provisioned === "boolean";
}

/** The dashboard list, with the rows whose definitions could not be read. */
export interface DashboardList {
  dashboards: StoredDashboard[];
  /** Ids of rows the server could not encode; always present, often empty. */
  unreadable: number[];
}

/** Lists every stored dashboard, definitions included. */
export async function fetchDashboards(fetchImpl: FetchLike = fetch, signal?: AbortSignal): Promise<DashboardList> {
  const body = await getJSON("/api/v1/dashboards", fetchImpl, signal);
  if (!isRecord(body) || !Array.isArray(body.dashboards) || !body.dashboards.every(isStoredDashboard)) {
    throw new ApiError("ozyd sent an unexpected /api/v1/dashboards response");
  }
  const unreadable = Array.isArray(body.unreadable) ? body.unreadable.filter((x): x is number => typeof x === "number") : [];
  return { dashboards: body.dashboards, unreadable };
}

/** Fetches one stored dashboard by id. */
export async function fetchDashboard(id: number, fetchImpl: FetchLike = fetch, signal?: AbortSignal): Promise<StoredDashboard> {
  const body = await getJSON(`/api/v1/dashboards/${id}`, fetchImpl, signal);
  if (!isStoredDashboard(body)) throw new ApiError(`ozyd sent an unexpected /api/v1/dashboards/${id} response`);
  return body;
}

/** The services a template dashboard can be instantiated for. */
export interface ServiceList {
  services: string[];
  /** True when the server stopped before covering everything. */
  truncated: boolean;
}

/** Lists the services any template dashboard covers. */
export async function fetchServices(fetchImpl: FetchLike = fetch, signal?: AbortSignal): Promise<ServiceList> {
  const body = await getJSON("/api/v1/dashboards/services", fetchImpl, signal);
  if (!isRecord(body) || !Array.isArray(body.services) || !body.services.every((s) => typeof s === "string")) {
    throw new ApiError("ozyd sent an unexpected /api/v1/dashboards/services response");
  }
  return { services: body.services as string[], truncated: body.truncated === true };
}

/** One template instantiated for one service. */
export interface ServiceDashboard {
  template_id: number;
  template_uid?: string;
  service: string;
  dashboard: Dashboard;
}

/**
 * Fetches every template instantiated for one service.
 *
 * A list, because nothing says a deployment has one template — an app repo
 * mounting its own provisioning directory beside the stock one is the case
 * provisioning exists for.
 */
export async function fetchServiceDashboards(
  service: string,
  fetchImpl: FetchLike = fetch,
  signal?: AbortSignal,
): Promise<ServiceDashboard[]> {
  const body = await getJSON(`/api/v1/dashboards/service/${encodeURIComponent(service)}`, fetchImpl, signal);
  if (!isRecord(body) || !Array.isArray(body.dashboards)) {
    throw new ApiError("ozyd sent an unexpected /api/v1/dashboards/service response");
  }
  const out: ServiceDashboard[] = [];
  for (const entry of body.dashboards) {
    if (!isRecord(entry) || typeof entry.template_id !== "number" || !isDashboardish(entry.dashboard)) {
      throw new ApiError("ozyd sent an unexpected /api/v1/dashboards/service response");
    }
    out.push({
      template_id: entry.template_id,
      template_uid: typeof entry.template_uid === "string" ? entry.template_uid : undefined,
      service,
      dashboard: entry.dashboard,
    });
  }
  return out;
}

/** One query's answer inside a batch: ok with series, or its own failure. */
export interface BatchResult {
  index: number;
  status: "ok" | "error";
  query: string;
  /** Bucket width this result was evaluated on; 0 on a failure. */
  interval: number;
  series: Series[];
  warnings: string[];
  /** Present only on a failure: the status the same query would have had alone. */
  code?: number;
  error?: string;
}

/** The body of a successful `/api/v1/query/batch`. */
export interface BatchResponse {
  from: number;
  to: number;
  results: BatchResult[];
}

function isBatchResult(v: unknown): v is BatchResult {
  return (
    isRecord(v) &&
    typeof v.index === "number" &&
    (v.status === "ok" || v.status === "error") &&
    typeof v.query === "string" &&
    typeof v.interval === "number" &&
    Array.isArray(v.series) &&
    v.series.every(isSeries) &&
    Array.isArray(v.warnings)
  );
}

/** How many queries one batch may carry, as the server's limit says. */
export const MAX_QUERIES_PER_BATCH = 50;

/**
 * Runs a batch of queries over one window.
 *
 * `vars` binds each template variable to zero or more `key:value` tags — zero
 * meaning "every value", which is what a cleared selector means. The window
 * and the variables belong to the batch rather than to a query, because a
 * dashboard has one time picker and two queries on different grids share no
 * work (ADR-0016, ADR-0018).
 */
export async function fetchBatch(
  queries: readonly string[],
  range: ResolvedRange,
  vars: Record<string, string[]>,
  fetchImpl: FetchLike = fetch,
  signal?: AbortSignal,
): Promise<BatchResponse> {
  const body = { queries: queries.map((q) => ({ q })), from: range.from, to: range.to, vars };
  const parsed = await postJSON("/api/v1/query/batch", body, fetchImpl, signal);
  if (!isRecord(parsed) || typeof parsed.from !== "number" || typeof parsed.to !== "number" || !Array.isArray(parsed.results) || !parsed.results.every(isBatchResult)) {
    throw new ApiError("ozyd sent an unexpected /api/v1/query/batch response");
  }
  return { from: parsed.from, to: parsed.to, results: parsed.results };
}

/**
 * One bin of a sketch: `[lower, upper, count]` — the half-open value range
 * `(lower, upper]` and how many observations fell in it.
 *
 * Resolved bounds rather than the sketch's bucket index, because an index is
 * meaningless without γ (api.md). A tuple rather than an object because there
 * are up to 200000 of them in one answer and three keys repeated that many
 * times is most of the payload.
 */
export type SketchBin = [lower: number, upper: number, count: number];

/** One time bucket's distribution. */
export interface SketchBucket {
  /** Bucket start, unix **milliseconds** — unlike the window, which is seconds. */
  t: number;
  /**
   * The ratio between this bucket's bin bounds: the relative accuracy of the
   * sketches that merged into it is α = (γ-1)/(γ+1). Per bucket, not per
   * response — two buckets need not agree (api.md).
   */
  gamma: number;
  /** Exact, not estimated. Null where the server could not encode the number. */
  count: number | null;
  sum: number | null;
  min: number | null;
  max: number | null;
  /** Value order, negatives first; zero is its own bin `[0, 0, n]`. */
  bins: SketchBin[];
}

/** One group's distribution over time. */
export interface SketchSeries {
  metric: string;
  tags: Record<string, string>;
  scope: string;
  /** Only the buckets something landed in: an absent bucket is a gap. */
  buckets: SketchBucket[];
}

/** The body of a successful `/api/v1/query/sketch`. */
export interface SketchResponse {
  from: number;
  to: number;
  /** Bucket width in seconds, which is how wide one column of a heatmap is. */
  interval: number;
  /** Total bins across every series, so a caller can see the limit coming. */
  bins: number;
  series: SketchSeries[];
  warnings: string[];
}

function isSketchBin(v: unknown): v is SketchBin {
  return Array.isArray(v) && v.length === 3 && v.every((x) => typeof x === "number");
}

/**
 * `count`, `sum`, `min` and `max` are read as nullable because the server
 * writes null for a value that is not finite — an empty sketch reports its min
 * as +Inf. Reading them as numbers would make that a silent zero, which is a
 * tooltip claiming the fastest request took no time at all.
 */
function nullableNumber(v: unknown): number | null {
  return typeof v === "number" && Number.isFinite(v) ? v : null;
}

function isSketchBucket(v: unknown): v is SketchBucket {
  return isRecord(v) && typeof v.t === "number" && typeof v.gamma === "number" && Array.isArray(v.bins) && v.bins.every(isSketchBin);
}

function isSketchSeries(v: unknown): v is SketchSeries {
  return (
    isRecord(v) &&
    typeof v.metric === "string" &&
    isRecord(v.tags) &&
    Array.isArray(v.buckets) &&
    v.buckets.every(isSketchBucket)
  );
}

/**
 * Runs one `dist:` query and returns the sketch behind it.
 *
 * Unlike the batch, **a refused query here is a refused request**: this
 * endpoint answers one query, so there is no per-query status to put a failure
 * in and a 400 is the whole answer. The heatmap widget therefore draws the
 * thrown error, which is the same picture a batch widget draws from its
 * result's own `status` (ADR-0017) by a different route.
 */
export async function fetchSketch(
  q: string,
  range: ResolvedRange,
  vars: Record<string, string[]>,
  fetchImpl: FetchLike = fetch,
  signal?: AbortSignal,
): Promise<SketchResponse> {
  const parsed = await postJSON("/api/v1/query/sketch", { q, from: range.from, to: range.to, vars }, fetchImpl, signal);
  if (
    !isRecord(parsed) ||
    typeof parsed.from !== "number" ||
    typeof parsed.to !== "number" ||
    typeof parsed.interval !== "number" ||
    !Array.isArray(parsed.series) ||
    !parsed.series.every(isSketchSeries)
  ) {
    throw new ApiError("ozyd sent an unexpected /api/v1/query/sketch response");
  }
  return {
    from: parsed.from,
    to: parsed.to,
    interval: parsed.interval,
    bins: typeof parsed.bins === "number" ? parsed.bins : 0,
    series: (parsed.series as SketchSeries[]).map((s) => ({
      metric: s.metric,
      tags: s.tags,
      scope: typeof s.scope === "string" ? s.scope : "",
      buckets: s.buckets.map((b) => ({
        t: b.t,
        gamma: b.gamma,
        count: nullableNumber(b.count),
        sum: nullableNumber(b.sum),
        min: nullableNumber(b.min),
        max: nullableNumber(b.max),
        bins: b.bins,
      })),
    })),
    warnings: Array.isArray(parsed.warnings) ? parsed.warnings.filter((w): w is string => typeof w === "string") : [],
  };
}
