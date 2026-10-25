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
import type { MonitorSummaryFilter } from "./dashboard";
import { severity, type MonitorFilter, type Status, type StoredMonitor } from "./monitorsApi";
import { sortMonitors } from "./monitorView";

/**
 * The one monitor list every feed widget reads, groups included, so a page
 * with several (and the editor's monitor picker) polls it once.
 */
export const FEED_LIST: MonitorFilter = { states: "1" };

/** How many rows a summary shows when its definition sets no limit. */
export const DEFAULT_SUMMARY_LIMIT = 10;

/** A monitor_summary's rows, and how many matched beyond its limit. */
export interface MonitorRows {
  rows: StoredMonitor[];
  more: number;
}

/**
 * The monitors a filter keeps, worst first, cut at the limit. Everything is
 * applied here, from the one list every feed widget shares (`?states=1`), so a
 * page with three of them polls the list once. The status set has
 * the Monitors page's meaning of `?status=`: a monitor is kept when any of its
 * groups is in a status asked for, not only its worst one. A monitor with no
 * status (no data yet, disabled, not running) has no groups counted, so a
 * filter that names statuses drops it.
 */
export function selectMonitors(ms: readonly StoredMonitor[], filter: MonitorSummaryFilter | undefined, limit = 0): MonitorRows {
  const matching = ms.filter((m) => matchesFilter(m, filter));
  const want = filter?.status ?? [];
  const kept = want.length === 0 ? matching : matching.filter((m) => want.some((st) => (m.groups[st] ?? 0) > 0));
  const sorted = sortMonitors(kept);
  const n = limit > 0 ? limit : DEFAULT_SUMMARY_LIMIT;
  return { rows: sorted.slice(0, n), more: Math.max(0, sorted.length - n) };
}

/**
 * Whether a monitor matches a filter's tag and name, with the list API's
 * meaning: `tag` is one exact tag, `q` a case-insensitive name substring.
 */
export function matchesFilter(m: StoredMonitor, filter: MonitorSummaryFilter | undefined): boolean {
  if (filter?.tag && !(m.tags ?? []).includes(filter.tag)) return false;
  if (filter?.q && !m.name.toLowerCase().includes(filter.q.toLowerCase())) return false;
  return true;
}

/** The tag and name part of a filter, as the Monitors page's URL takes it. */
export function listParams(filter: MonitorSummaryFilter | undefined): { tag?: string; q?: string } {
  const out: { tag?: string; q?: string } = {};
  if (filter?.tag) out.tag = filter.tag;
  if (filter?.q) out.q = filter.q;
  return out;
}

function serviceTags(m: StoredMonitor): string[] {
  return (m.tags ?? []).filter((t) => t.startsWith("service:")).map((t) => t.slice("service:".length));
}

/**
 * A group's environment: its `env` tag, or `@env` for a log monitor grouped
 * by the attribute (logs carry env as an attribute, not a tag).
 */
function groupEnv(tags: Readonly<Record<string, string>>): string | undefined {
  return tags.env ?? tags["@env"];
}

/**
 * One card's dot: the worst status among the groups of the monitors tagged
 * `service:<name>` that belong to the card's environment. The monitors must
 * come from the list asked `?states=1`, which carries every group.
 *
 * By group, not by monitor: a monitor grouped `by {env}` is ALERT when staging
 * is, and a prod card must not turn red for it. A group belongs to the card
 * when its env (groupEnv) is the card's, or when it has none (an ungrouped
 * monitor speaks for every env). A card with no env (spans without one) takes
 * every group.
 *
 * `undefined`: no monitor is tagged with the service, said as "no monitors",
 * never as OK. `null`: monitors are tagged but none has a group for this env
 * yet, or none is running.
 */
export function serviceEnvStatus(ms: readonly StoredMonitor[], service: string, env: string | undefined): Status | null | undefined {
  const mine = ms.filter((m) => serviceTags(m).includes(service));
  if (mine.length === 0) return undefined;
  let worst: Status | null = null;
  for (const m of mine) {
    for (const g of m.group_states ?? []) {
      const ge = groupEnv(g.tags);
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
 * The Monitors page for the rest of a summary's list: its tag and name, and
 * its status when it names exactly one (the page filters on one status; for
 * a set it shows them all, which still holds every row the widget cut).
 */
export function monitorsLink(filter: MonitorSummaryFilter | undefined): string {
  const p = new URLSearchParams(listParams(filter));
  if (filter?.status?.length === 1) p.set("status", filter.status[0]!);
  const qs = p.toString();
  return qs ? `/monitors?${qs}` : "/monitors";
}
