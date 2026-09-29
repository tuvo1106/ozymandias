/**
 * Client for the cardinality endpoints behind the Metric Summary page:
 * series per metric (`/api/v1/metrics/cardinality`) and, for one metric, the
 * tag keys that make them (`/api/v1/tags/cardinality`). See ADR-0023 for
 * what is counted.
 *
 * As everywhere in lib/, a response is checked before it is used, so a
 * drifted server is a readable error on the page rather than a TypeError in a
 * table.
 */
import { ApiError, getJSON } from "./metricsApi";

type FetchLike = typeof fetch;

/** One metric and its series. */
export interface MetricCardinality {
  name: string;
  /** The kind it was first seen with, or null: ozyd has no record of one. */
  type: string | null;
  series: number;
}

/** A page of metrics, highest series count first. */
export interface MetricCardinalityList {
  metrics: MetricCardinality[];
  /** Every metric that matched; more than `metrics.length` when truncated. */
  total: number;
  truncated: boolean;
}

/** One tag key of a metric. */
export interface TagCardinality {
  key: string;
  /** Series of the metric that carry the key. */
  series: number;
  /** Distinct values; a bare tag adds to series and not to this. */
  values: number;
}

/** One metric's tag keys, most values first. */
export interface MetricTags {
  metric: string;
  type: string | null;
  /** The metric's own series; 0 means this store does not have it. */
  series: number;
  keys: TagCardinality[];
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

const isCount = (v: unknown): v is number => typeof v === "number" && Number.isInteger(v) && v >= 0;
const isType = (v: unknown): v is string | null => v === null || typeof v === "string";

function isMetricCardinality(v: unknown): v is MetricCardinality {
  return isRecord(v) && typeof v.name === "string" && isType(v.type) && isCount(v.series);
}

function isTagCardinality(v: unknown): v is TagCardinality {
  return isRecord(v) && typeof v.key === "string" && isCount(v.series) && isCount(v.values);
}

/** Reports whether a value is a well-formed metrics cardinality list. */
export function isMetricCardinalityList(v: unknown): v is MetricCardinalityList {
  return (
    isRecord(v) &&
    Array.isArray(v.metrics) &&
    v.metrics.every(isMetricCardinality) &&
    isCount(v.total) &&
    typeof v.truncated === "boolean"
  );
}

/** Reports whether a value is a well-formed tags cardinality answer. */
export function isMetricTags(v: unknown): v is MetricTags {
  return (
    isRecord(v) &&
    typeof v.metric === "string" &&
    isType(v.type) &&
    isCount(v.series) &&
    Array.isArray(v.keys) &&
    v.keys.every(isTagCardinality)
  );
}

/** Metrics starting with `prefix`, highest series count first, at most `limit`. */
export async function fetchMetricCardinality(
  prefix: string,
  limit: number,
  fetchImpl: FetchLike = fetch,
  signal?: AbortSignal,
): Promise<MetricCardinalityList> {
  const body = await getJSON(`/api/v1/metrics/cardinality?${new URLSearchParams({ prefix, limit: String(limit) })}`, fetchImpl, signal);
  if (!isMetricCardinalityList(body)) throw new ApiError("ozyd sent an unexpected /api/v1/metrics/cardinality response");
  return body;
}

/** One metric's tag keys. */
export async function fetchMetricTags(metric: string, fetchImpl: FetchLike = fetch, signal?: AbortSignal): Promise<MetricTags> {
  const body = await getJSON(`/api/v1/tags/cardinality?${new URLSearchParams({ metric })}`, fetchImpl, signal);
  if (!isMetricTags(body)) throw new ApiError("ozyd sent an unexpected /api/v1/tags/cardinality response");
  return body;
}
