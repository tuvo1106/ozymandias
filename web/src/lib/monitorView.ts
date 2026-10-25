/**
 * Pure helpers for the monitor pages: mute wording, the status timeline, and
 * reading an event for a person.
 */
import { severity, type GroupState, type MonitorEvent, type Status, type StoredMonitor } from "./monitorsApi";
import { formatUptime } from "./health";

/**
 * Mute without an end is stored as a date in the year 9999 (docs/api.md), so
 * any mute past this is "until unmuted" rather than "for 2.7 million days".
 */
const FOREVER_AFTER = Date.UTC(9000, 0, 1) / 1000;

/** How a mute reads: null when not muted. `now` in unix seconds. */
export function muteLabel(mutedUntil: number | null, now: number): string | null {
  if (mutedUntil === null || mutedUntil <= now) return null;
  if (mutedUntil >= FOREVER_AFTER) return "muted until unmuted";
  return `muted for ${formatUptime(mutedUntil - now)}`;
}

/** One stretch of a group's timeline. Times in unix seconds. */
export interface Segment {
  from: number;
  to: number;
  /** null: not known (before the group existed, or after it was forgotten). */
  status: Status | null;
}

interface Transition {
  ts: number;
  from: Status;
  to: Status;
}

/**
 * One life of a group over [lo, hi]: read backwards from `end`, the status at
 * hi, through its transitions (newest first). With no transition to explain
 * it, a state that began inside the life (`since`) was unknown before then.
 */
function life(transitions: Transition[], end: Status | null, lo: number, hi: number, since?: number): Segment[] {
  const out: Segment[] = [];
  let at = hi;
  let status = end;
  for (const t of [...transitions].sort((x, y) => y.ts - x.ts)) {
    out.unshift({ from: t.ts, to: at, status });
    at = t.ts;
    status = t.from;
  }
  if (transitions.length === 0 && since !== undefined && since > lo && since < at) {
    out.unshift({ from: since, to: at, status });
    at = since;
    status = null;
  }
  if (at > lo) out.unshift({ from: lo, to: at, status });
  return out;
}

/**
 * Each group's status over [from, to], from its transitions and its current
 * state.
 *
 * Read backwards from now: a group's status at the end is its current state,
 * and each transition (newest first) says what it was before. Two events end
 * what is known:
 *
 * - `reset` (the query changed): every group's earlier states answered a
 *   different query, so the timeline is unknown before it.
 * - `group_expired` (forgotten after a day without data): that group's life
 *   ends there. Before it, the status is the one the event says it was
 *   forgotten in, read back through any transitions before it; after it,
 *   unknown until the group is seen again (a later state's `since`, or a
 *   later transition).
 *
 * A state that began inside the range with no transition to explain it is
 * unknown before then: the group was created, and what came before is not
 * this monitor's answer.
 *
 * Groups appear in `states` order (worst first), then any group known only
 * from its transitions or its expiry.
 */
export function statusTimeline(
  events: readonly MonitorEvent[],
  states: readonly GroupState[],
  from: number,
  to: number,
): { group: string; segments: Segment[] }[] {
  const ts = (e: MonitorEvent) => Date.parse(e.ts) / 1000;
  let start = from;
  for (const e of events) if (e.kind === "reset" && ts(e) > start && ts(e) <= to) start = ts(e);
  const transitions = new Map<string, Transition[]>();
  // The latest expiry per group, with the status it was forgotten in (the
  // event's own `from`: the transition into it is usually a day earlier, out
  // of range).
  const expired = new Map<string, { ts: number; from: Status | null }>();
  for (const e of events) {
    const t = ts(e);
    if (!e.group || t < start || t > to) continue;
    if (e.kind === "group_expired" && t > (expired.get(e.group)?.ts ?? -Infinity)) expired.set(e.group, { ts: t, from: e.from ?? null });
    if (e.kind !== "transition" || !e.from || !e.to) continue;
    const list = transitions.get(e.group) ?? [];
    list.push({ ts: t, from: e.from, to: e.to });
    transitions.set(e.group, list);
  }
  const others = new Set([...transitions.keys(), ...expired.keys()].filter((g) => !states.some((s) => s.group === g)));
  const groups = [...states.map((s) => s.group), ...[...others].sort()];
  return groups.map((group) => {
    const state = states.find((s) => s.group === group);
    const all = transitions.get(group) ?? [];
    const since = state ? Date.parse(state.since) / 1000 : undefined;
    const expiry = expired.get(group);
    const exp = expiry?.ts;
    const newestTo = (ts: Transition[]) => [...ts].sort((x, y) => y.ts - x.ts)[0]?.to ?? null;
    const segments: Segment[] = [];
    if (start > from) segments.push({ from, to: start, status: null });
    if (exp !== undefined) {
      const before = all.filter((t) => t.ts <= exp);
      const after = all.filter((t) => t.ts > exp);
      // Before expiry: the status it was forgotten in, as the event says.
      segments.push(...life(before, expiry?.from ?? newestTo(before), start, exp));
      const end = state?.status ?? newestTo(after);
      // After it, unknown until seen again: a transition, or the state's since.
      const seen = after.length ? Math.min(...after.map((t) => t.ts)) : since !== undefined && since > exp ? since : to;
      if (seen > exp) segments.push({ from: exp, to: seen, status: null });
      if (seen < to) segments.push(...life(after, end, seen, to, since));
    } else {
      segments.push(...life(all, state?.status ?? newestTo(all), start, to, since));
    }
    return { group, segments: segments.filter((s) => s.to > s.from) };
  });
}

/** A sentence for an event, for the history table. */
export function describeEvent(e: MonitorEvent): string {
  switch (e.kind) {
    case "transition":
      return `${e.from} → ${e.to}`;
    case "renotify":
      return `still ${e.to}, reminded`;
    case "notify_ok":
      return `delivered: ${e.text ?? ""}`.trim();
    case "notify_failed":
      return `delivery failed: ${e.text ?? ""}`.trim();
    case "eval_error":
      return `query failed: ${e.text ?? ""}`.trim();
    case "eval_recovered":
      return "query works again";
    case "group_expired":
      return "forgotten after 24h without data";
    case "reset":
      return "query changed; group states cleared";
    case "released":
      return "provisioning file gone; now edited through the UI";
    case "muted":
    case "unmuted":
      return e.text ? `${e.kind}: ${e.text}` : e.kind;
    case "test_notification":
      return e.text ?? "test notification";
    default:
      return e.text ? `${e.kind}: ${e.text}` : e.kind;
  }
}

/** Event kinds the status timeline reads. */
export const TIMELINE_KINDS = ["transition", "reset", "group_expired"];

/** Event kinds that are about delivering, for the notification log. */
export const NOTIFICATION_KINDS = ["notify_ok", "notify_failed", "test_notification"];

/** Worst status first, then name. A monitor with no status sorts below OK. */
export function sortMonitors(ms: readonly StoredMonitor[]): StoredMonitor[] {
  return [...ms].sort((a, b) => severity(b.status) - severity(a.status) || a.name.localeCompare(b.name));
}
