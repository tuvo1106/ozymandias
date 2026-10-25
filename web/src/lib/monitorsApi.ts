/**
 * Client for ozyd's monitor endpoints (docs/api.md#monitors).
 *
 * Same contract as dashboardsApi.ts: every response is checked for the shape
 * the UI reads before it reaches a component, so a server that has drifted is
 * a sentence on the page, not a crash in a table. The fetch implementation is
 * an argument so tests need no server.
 *
 * Two answers here are not what they look like, and the types say so:
 *
 * - **A monitor's `status` is `null` more often than "OK".** No groups yet,
 *   disabled, or a stored definition this build cannot compile (`problem`):
 *   each of those is "unknown", and a page that shows them as OK tells someone
 *   their alerting works when it does not.
 * - **`validate` answers 200 for an invalid monitor.** "Here is why it is
 *   invalid" is its successful answer, so it resolves with `valid: false`
 *   rather than rejecting; only a failed *request* rejects.
 */
import { ApiError, deleteJSON, getJSON, postJSON, putJSON } from "./metricsApi";

type FetchLike = typeof fetch;

/** A group's status. The order is severity, worst last (see `severity`). */
export const STATUSES = ["OK", "WARN", "NO_DATA", "ALERT"] as const;
/** A group's status: what the state machine says about it now. */
export type Status = (typeof STATUSES)[number];

/** How bad a status is, for sorting: higher is worse. Unknown is below OK. */
export function severity(s: Status | null | undefined): number {
  return s == null ? -1 : STATUSES.indexOf(s);
}

/** The thresholds a definition may set; any may be left to its default. */
export interface Thresholds {
  critical?: number;
  warning?: number;
  critical_recovery?: number;
  warning_recovery?: number;
}

/** A monitor's options, as stored. Absent fields take the server's defaults. */
export interface MonitorOptions {
  thresholds?: Thresholds;
  evaluation_interval_s?: number;
  evaluation_delay_s?: number;
  require_full_window?: boolean;
  notify_no_data?: boolean;
  no_data_timeframe_m?: number;
  renotify_interval_m?: number;
  min_consecutive_failures?: number;
}

/** Which store a monitor's query reads. */
export type MonitorType = "metric" | "log";

/** What a person writes: the body of a create, update, validate or backtest. */
export interface MonitorDefinition {
  uid?: string;
  name: string;
  type: MonitorType;
  query: string;
  options: MonitorOptions;
  message: string;
  tags: string[];
  enabled?: boolean;
}

/** A stored monitor, as the list and the detail page read it. */
export interface StoredMonitor extends MonitorDefinition {
  id: number;
  enabled: boolean;
  provisioned: boolean;
  created_at: string;
  updated_at: string;
  /** Unix seconds; null when not muted. */
  muted_until: number | null;
  /** How many groups are in each status. Empty when status is null. */
  groups: Partial<Record<Status, number>>;
  /** The worst group's status, or null: no groups yet, disabled, or `problem`. */
  status: Status | null;
  /** Why the stored definition does not compile; the monitor is not running. */
  problem?: string;
  /**
   * Each group, worst first: only when the list was asked `?states=1`, and
   * only for a monitor with a status (a disabled one's states are stale).
   */
  group_states?: GroupStatus[];
}

/** A group in the list's `?states=1` answer: `/states` without the timestamps. */
export interface GroupStatus {
  group: string;
  tags: Record<string, string>;
  status: Status;
  value: number | null;
}

/** One group's state, from `/states`. */
export interface GroupState {
  group: string;
  tags: Record<string, string>;
  status: Status;
  since: string;
  value: number | null;
  last_eval: string | null;
  last_data: string | null;
  last_notified: string | null;
}

/** The kinds of history entry (docs/api.md#get-apiv1events). */
export type EventKind =
  | "transition"
  | "renotify"
  | "notify_ok"
  | "notify_failed"
  | "eval_error"
  | "eval_recovered"
  | "group_expired"
  | "reset"
  | "released"
  | "muted"
  | "unmuted"
  | "test_notification";

/** One history entry, from `/events`. */
export interface MonitorEvent {
  id: number;
  ts: string;
  kind: EventKind | string;
  monitor_id: number;
  group?: string;
  from?: Status;
  to?: Status;
  value: number | null;
  text?: string;
}

/** The thresholds with every default applied: what the editor draws. */
export interface Levels {
  op: ">" | ">=" | "<" | "<=";
  critical: number;
  critical_recovery: number;
  warning?: number;
  warning_recovery?: number;
}

/** A dry run's value for one group. */
export interface DryRunGroup {
  group: string;
  tags: Record<string, string>;
  value: number | null;
  skipped?: boolean;
  status: Status;
}

/** What validate answers: the dry run and levels, or every reason it is invalid. */
export type Validation =
  | {
      valid: true;
      query: string;
      grouped: boolean;
      levels: Levels;
      mentions: string[];
      /** Unix seconds: what an evaluation at now reads. */
      window: { from: number; to: number };
      interval_s: number;
      groups?: DryRunGroup[];
      eval_error?: string;
    }
  | { valid: false; errors: string[] };

/** One transition or notification a backtest found. */
export interface BacktestEvent {
  ts: string;
  group: string;
  from: Status;
  to: Status;
  value: number | null;
  notify: boolean;
  reason?: string;
}

/** One group's evaluated values over a backtest: the editor's line. */
export interface BacktestSeries {
  group: string;
  tags: Record<string, string>;
  /** [unix seconds, value or null], one per evaluation. */
  points: [number, number | null][];
}

/** A backtest's answer: what the monitor would have done over the range. */
export interface Backtest {
  from: string;
  to: string;
  evaluations: number;
  events: BacktestEvent[];
  final: Record<string, Status>;
  series: BacktestSeries[];
  series_omitted: number;
  /** Every status change and notification, including events past the cap. */
  transitions: number;
  notifications: number;
  /** Events past the cap: counted above, not in `events`. */
  events_omitted: number;
}

/** The scheduler's health and the counts behind the nav badge. */
export interface MonitorsStatus {
  running: boolean;
  monitors: number;
  stalled: boolean;
  loading: boolean;
  last_eval_age_seconds: number;
  monitors_by_status: Partial<Record<Status, number>>;
}

/** A configured notifier, as far as the UI may know: never its URL. */
export interface Notifier {
  name: string;
  type: string;
  ready: boolean;
  problem?: string;
}

/** One notifier's answer to a test notification. */
export interface TestResult {
  notifier: string;
  ok: boolean;
  error?: string;
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

const isNum = (v: unknown): v is number => typeof v === "number";
const isStr = (v: unknown): v is string => typeof v === "string";
const isNumOrNull = (v: unknown): v is number | null => v === null || isNum(v);
const isStrOrNull = (v: unknown): v is string | null => v === null || isStr(v);
const isStrings = (v: unknown): v is string[] => Array.isArray(v) && v.every(isStr);
const isStringMap = (v: unknown): v is Record<string, string> => isRecord(v) && Object.values(v).every(isStr);
const isCounts = (v: unknown): v is Record<string, number> => isRecord(v) && Object.values(v).every(isNum);

function isStatus(v: unknown): v is Status {
  return (STATUSES as readonly unknown[]).includes(v);
}

function isStoredMonitor(v: unknown): v is StoredMonitor {
  return (
    isRecord(v) &&
    isNum(v.id) &&
    isStr(v.name) &&
    (v.type === "metric" || v.type === "log") &&
    isStr(v.query) &&
    isRecord(v.options) &&
    isStr(v.message) &&
    isStrings(v.tags) &&
    typeof v.enabled === "boolean" &&
    typeof v.provisioned === "boolean" &&
    isNumOrNull(v.muted_until) &&
    isCounts(v.groups) &&
    (v.status === null || isStatus(v.status)) &&
    (v.problem === undefined || isStr(v.problem)) &&
    (v.group_states === undefined || (Array.isArray(v.group_states) && v.group_states.every(isGroupStatus)))
  );
}

function isGroupStatus(v: unknown): v is GroupStatus {
  return isRecord(v) && isStr(v.group) && isRecord(v.tags) && isStatus(v.status) && isNumOrNull(v.value);
}

function isGroupState(v: unknown): v is GroupState {
  return (
    isRecord(v) &&
    isStr(v.group) &&
    isStringMap(v.tags) &&
    isStatus(v.status) &&
    isStr(v.since) &&
    isNumOrNull(v.value) &&
    isStrOrNull(v.last_eval) &&
    isStrOrNull(v.last_data) &&
    isStrOrNull(v.last_notified)
  );
}

function isEvent(v: unknown): v is MonitorEvent {
  return isRecord(v) && isNum(v.id) && isStr(v.ts) && isStr(v.kind) && isNum(v.monitor_id) && isNumOrNull(v.value);
}

function isLevels(v: unknown): v is Levels {
  return isRecord(v) && isStr(v.op) && isNum(v.critical) && isNum(v.critical_recovery);
}

function isValidation(v: unknown): v is Validation {
  if (!isRecord(v)) return false;
  if (v.valid === false) return isStrings(v.errors);
  return (
    v.valid === true &&
    isStr(v.query) &&
    isLevels(v.levels) &&
    isStrings(v.mentions) &&
    isRecord(v.window) &&
    isNum(v.window.from) &&
    isNum(v.window.to) &&
    isNum(v.interval_s)
  );
}

function isBacktest(v: unknown): v is Backtest {
  return (
    isRecord(v) &&
    isNum(v.evaluations) &&
    Array.isArray(v.events) &&
    v.events.every((e) => isRecord(e) && isStr(e.ts) && isStr(e.group) && isStatus(e.to) && isNumOrNull(e.value)) &&
    Array.isArray(v.series) &&
    v.series.every(
      (s) =>
        isRecord(s) &&
        isStr(s.group) &&
        Array.isArray(s.points) &&
        s.points.every((p) => Array.isArray(p) && p.length === 2 && isNum(p[0]) && isNumOrNull(p[1])),
    ) &&
    isNum(v.series_omitted) &&
    isNum(v.transitions) &&
    isNum(v.notifications) &&
    isNum(v.events_omitted)
  );
}

function isStatusDoc(v: unknown): v is MonitorsStatus {
  return (
    isRecord(v) &&
    typeof v.running === "boolean" &&
    isNum(v.monitors) &&
    typeof v.stalled === "boolean" &&
    typeof v.loading === "boolean" &&
    isCounts(v.monitors_by_status)
  );
}

function unexpected(endpoint: string): ApiError {
  return new ApiError(`ozyd sent an unexpected ${endpoint} response`);
}

/** Filters for the list; every one optional. */
export interface MonitorFilter {
  tag?: string;
  status?: Status;
  q?: string;
  /** "1" adds each monitor's group_states. */
  states?: "1";
}

/** Lists monitors, by name. */
export async function fetchMonitors(filter: MonitorFilter = {}, fetchImpl: FetchLike = fetch, signal?: AbortSignal): Promise<StoredMonitor[]> {
  const params = new URLSearchParams();
  for (const [k, v] of Object.entries(filter)) if (v) params.set(k, v);
  const qs = params.toString();
  const body = await getJSON(`/api/v1/monitors${qs ? `?${qs}` : ""}`, fetchImpl, signal);
  if (!isRecord(body) || !Array.isArray(body.monitors) || !body.monitors.every(isStoredMonitor)) {
    throw unexpected("/api/v1/monitors");
  }
  return body.monitors;
}

/** Fetches one monitor. */
export async function fetchMonitor(id: number, fetchImpl: FetchLike = fetch, signal?: AbortSignal): Promise<StoredMonitor> {
  const body = await getJSON(`/api/v1/monitors/${id}`, fetchImpl, signal);
  if (!isStoredMonitor(body)) throw unexpected(`/api/v1/monitors/${id}`);
  return body;
}

/** Creates a monitor. Rejects with the server's sentence when it does not validate. */
export async function createMonitor(def: MonitorDefinition, fetchImpl: FetchLike = fetch): Promise<StoredMonitor> {
  const body = await postJSON("/api/v1/monitors", def, fetchImpl);
  if (!isStoredMonitor(body)) throw unexpected("POST /api/v1/monitors");
  return body;
}

/** Replaces a monitor's definition. */
export async function updateMonitor(id: number, def: MonitorDefinition, fetchImpl: FetchLike = fetch): Promise<StoredMonitor> {
  const body = await putJSON(`/api/v1/monitors/${id}`, def, fetchImpl);
  if (!isStoredMonitor(body)) throw unexpected(`PUT /api/v1/monitors/${id}`);
  return body;
}

/** Deletes a monitor with its states, events and undelivered notifications. */
export async function deleteMonitor(id: number, fetchImpl: FetchLike = fetch): Promise<void> {
  await deleteJSON(`/api/v1/monitors/${id}`, fetchImpl);
}

/** Mutes until `until` (unix seconds), or until unmuted when omitted. */
export async function muteMonitor(id: number, until?: number, fetchImpl: FetchLike = fetch): Promise<StoredMonitor> {
  const body = await postJSON(`/api/v1/monitors/${id}/mute`, until === undefined ? {} : { until }, fetchImpl);
  if (!isStoredMonitor(body)) throw unexpected("mute");
  return body;
}

/** Unmutes. */
export async function unmuteMonitor(id: number, fetchImpl: FetchLike = fetch): Promise<StoredMonitor> {
  const body = await postJSON(`/api/v1/monitors/${id}/unmute`, {}, fetchImpl);
  if (!isStoredMonitor(body)) throw unexpected("unmute");
  return body;
}

/** Each group's state, worst first. */
export async function fetchStates(id: number, fetchImpl: FetchLike = fetch, signal?: AbortSignal): Promise<GroupState[]> {
  const body = await getJSON(`/api/v1/monitors/${id}/states`, fetchImpl, signal);
  if (!isRecord(body) || !Array.isArray(body.states) || !body.states.every(isGroupState)) {
    throw unexpected("states");
  }
  return body.states;
}

/** Filters for the event history. */
export interface EventFilter {
  monitor_id?: number;
  /** Unix seconds. */
  from?: number;
  to?: number;
  kind?: string[];
  limit?: number;
}

/** The history, newest first. */
export async function fetchEvents(filter: EventFilter, fetchImpl: FetchLike = fetch, signal?: AbortSignal): Promise<MonitorEvent[]> {
  const params = new URLSearchParams();
  if (filter.monitor_id !== undefined) params.set("monitor_id", String(filter.monitor_id));
  if (filter.from !== undefined) params.set("from", String(Math.floor(filter.from)));
  if (filter.to !== undefined) params.set("to", String(Math.ceil(filter.to)));
  if (filter.kind?.length) params.set("kind", filter.kind.join(","));
  if (filter.limit !== undefined) params.set("limit", String(filter.limit));
  const body = await getJSON(`/api/v1/events?${params.toString()}`, fetchImpl, signal);
  if (!isRecord(body) || !Array.isArray(body.events) || !body.events.every(isEvent)) {
    throw unexpected("/api/v1/events");
  }
  return body.events;
}

/** Validates a definition and dry-runs it at now. Resolves for an invalid one. */
export async function validateMonitor(def: MonitorDefinition, fetchImpl: FetchLike = fetch, signal?: AbortSignal): Promise<Validation> {
  const body = await postJSON("/api/v1/monitors/validate", def, fetchImpl, signal);
  if (!isValidation(body)) throw unexpected("validate");
  return body;
}

/**
 * Replays a definition over a range (unix seconds; the server defaults to the
 * last day). Pass the saved monitor's id so the ticks fall where the live
 * ones do.
 */
export async function backtestMonitor(
  def: MonitorDefinition,
  opts: { id?: number; from?: number; to?: number } = {},
  fetchImpl: FetchLike = fetch,
  signal?: AbortSignal,
): Promise<Backtest> {
  const body = await postJSON("/api/v1/monitors/backtest", { monitor: def, ...opts }, fetchImpl, signal);
  if (!isBacktest(body)) throw unexpected("backtest");
  return body;
}

/** Sends the message as an ALERT now; the answer is each notifier's outcome. */
export async function testNotification(id: number, fetchImpl: FetchLike = fetch): Promise<TestResult[]> {
  const body = await postJSON(`/api/v1/monitors/${id}/test-notification`, {}, fetchImpl);
  if (!isRecord(body) || !Array.isArray(body.results)) throw unexpected("test-notification");
  return body.results.filter((r): r is TestResult => isRecord(r) && isStr(r.notifier) && typeof r.ok === "boolean");
}

/** The nav badge and the stall banner. */
export async function fetchMonitorsStatus(fetchImpl: FetchLike = fetch, signal?: AbortSignal): Promise<MonitorsStatus> {
  const body = await getJSON("/api/v1/monitors/status", fetchImpl, signal);
  if (!isStatusDoc(body)) throw unexpected("/api/v1/monitors/status");
  return body;
}

/** What a message can @mention. Names and types only, never a URL. */
export async function fetchNotifiers(fetchImpl: FetchLike = fetch, signal?: AbortSignal): Promise<Notifier[]> {
  const body = await getJSON("/api/v1/notifiers", fetchImpl, signal);
  if (!isRecord(body) || !Array.isArray(body.notifiers)) throw unexpected("/api/v1/notifiers");
  return body.notifiers.filter((n): n is Notifier => isRecord(n) && isStr(n.name) && isStr(n.type) && typeof n.ready === "boolean");
}
