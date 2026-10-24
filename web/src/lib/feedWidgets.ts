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
import type { MonitorSummaryFilter } from "./dashboard";
import { severity, type GroupState, type Status, type StoredMonitor } from "./monitorsApi";
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
export function selectMonitors(ms: readonly StoredMonitor[], filter: MonitorSummaryFilter | undefined, limit = 0): MonitorRows {
  const want = filter?.status ?? [];
  const kept = want.length === 0 ? [...ms] : ms.filter((m) => m.status !== null && want.includes(m.status));
  const sorted = sortMonitors(kept);
  const n = limit > 0 ? limit : DEFAULT_SUMMARY_LIMIT;
  return { rows: sorted.slice(0, n), more: Math.max(0, sorted.length - n) };
}

/** The server-side part of a filter, as the monitor list takes it. */
export function listParams(filter: MonitorSummaryFilter | undefined): { tag?: string; q?: string } {
  const out: { tag?: string; q?: string } = {};
  if (filter?.tag) out.tag = filter.tag;
  if (filter?.q) out.q = filter.q;
  return out;
}

/** At most this many tagged monitors have their states fetched for one widget. */
export const MAX_STATUS_MONITORS = 50;

/** The monitors tagged `service:<name>` for any of the services shown, at most `cap`. */
export function taggedMonitors(ms: readonly StoredMonitor[], services: ReadonlySet<string>, cap = MAX_STATUS_MONITORS): StoredMonitor[] {
  return ms.filter((m) => serviceTags(m).some((s) => services.has(s))).slice(0, cap);
}

function serviceTags(m: StoredMonitor): string[] {
  return (m.tags ?? []).filter((t) => t.startsWith("service:")).map((t) => t.slice("service:".length));
}

/**
 * One card's dot: the worst status among the groups of the monitors tagged
 * `service:<name>` that belong to the card's environment.
 *
 * By group, not by monitor: a monitor grouped `by {env}` is ALERT when staging
 * is, and a prod card must not turn red for it. A group belongs to the card
 * when its tags name the card's env, or name no env at all (an ungrouped
 * monitor speaks for every env). A card with no env (spans without one) takes
 * every group.
 *
 * `undefined`: no monitor is tagged with the service, said as "no monitors",
 * never as OK. `null`: monitors are tagged but none has a group for this env
 * yet. `states` maps a monitor id to its groups; a tagged monitor missing
 * from it (not loaded) is skipped, and the caller says so.
 */
export function serviceEnvStatus(
  tagged: readonly StoredMonitor[],
  states: ReadonlyMap<number, readonly GroupState[]>,
  service: string,
  env: string | undefined,
): Status | null | undefined {
  const mine = tagged.filter((m) => serviceTags(m).includes(service));
  if (mine.length === 0) return undefined;
  let worst: Status | null = null;
  for (const m of mine) {
    if (m.status === null) continue; // disabled, not running or never evaluated: no claim either way
    for (const g of states.get(m.id) ?? []) {
      const ge = g.tags.env;
      if (env && ge !== undefined && ge !== env) continue;
      if (severity(g.status) > severity(worst)) worst = g.status;
    }
  }
  return worst;
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
