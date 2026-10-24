/**
 * What the feed widgets draw, as pure functions: which monitors a
 * monitor_summary lists, which monitor an alert_value shows, and each
 * service's status dot.
 *
 * The widgets draw from the monitor list and the APM tables rather than from
 * metric queries (docs/dashboards.md). Every "nothing here" they can show has
 * a different cause (no monitors match, none is triggered, the uid names no
 * monitor, a service has no monitors), and each is said in words, because a
 * blank widget on Home reads as "all is well" whichever it is.
 */
import type { RedRow } from "./apmApi";
import type { MonitorFilter } from "./dashboard";
import { severity, type Status, type StoredMonitor } from "./monitorsApi";
import { sortMonitors } from "./monitorView";

/** How many rows a summary shows when its definition sets no limit. */
export const DEFAULT_SUMMARY_LIMIT = 10;

/** A monitor_summary's rows, and how many matched beyond its limit. */
export interface MonitorRows {
  rows: StoredMonitor[];
  more: number;
}

/**
 * The monitors a filter keeps, worst first, cut at the limit. `tag` and `q`
 * were already applied by the server; the status set is applied here. A
 * monitor with no status (no data yet, disabled, not running) is in no
 * status, so a filter that names statuses drops it.
 */
export function selectMonitors(ms: readonly StoredMonitor[], filter: MonitorFilter | undefined, limit = 0): MonitorRows {
  const want = filter?.status ?? [];
  const kept = want.length === 0 ? [...ms] : ms.filter((m) => m.status !== null && want.includes(m.status));
  const sorted = sortMonitors(kept);
  const n = limit > 0 ? limit : DEFAULT_SUMMARY_LIMIT;
  return { rows: sorted.slice(0, n), more: Math.max(0, sorted.length - n) };
}

/** The server-side part of a filter, as the monitor list takes it. */
export function listParams(filter: MonitorFilter | undefined): { tag?: string; q?: string } {
  const out: { tag?: string; q?: string } = {};
  if (filter?.tag) out.tag = filter.tag;
  if (filter?.q) out.q = filter.q;
  return out;
}

/**
 * Each service's worst monitor status, from monitors tagged `service:<name>`.
 * A service with no tagged monitor is absent from the map, which the card
 * says as "no monitors", not as OK. Monitors with no status count as tagged
 * but do not set a status: "has monitors, none has data yet".
 */
export function serviceStatuses(ms: readonly StoredMonitor[]): Map<string, Status | null> {
  const out = new Map<string, Status | null>();
  for (const m of ms) {
    for (const tag of m.tags ?? []) {
      if (!tag.startsWith("service:")) continue;
      const svc = tag.slice("service:".length);
      const prev = out.get(svc) ?? null;
      out.set(svc, severity(m.status) > severity(prev) ? m.status : prev);
    }
  }
  return out;
}

/** The monitor an alert_value names, or undefined when no monitor has that uid. */
export function monitorByUid(ms: readonly StoredMonitor[], uid: string | undefined): StoredMonitor | undefined {
  if (!uid) return undefined;
  return ms.find((m) => m.uid === uid);
}

/**
 * Service cards busiest first, then by name. The API answers in name order and
 * includes services known from earlier windows with no request in this one;
 * those stay, last, because a service gone silent is worth seeing, but they
 * must not push the busy ones off a small widget.
 */
export function serviceCards(rows: readonly RedRow[]): RedRow[] {
  return [...rows].sort((a, b) => b.requests - a.requests || a.service.localeCompare(b.service) || a.name.localeCompare(b.name));
}
