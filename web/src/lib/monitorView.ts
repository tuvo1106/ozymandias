/**
 * Pure helpers for the monitor pages: mute wording, the status timeline, and
 * reading an event for a person.
 */
import type { GroupState, MonitorEvent, Status } from "./monitorsApi";
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
  /** null: before the first transition we know of, so not known. */
  status: Status | null;
}

/**
 * Each group's status over [from, to], from its transitions and its current
 * state.
 *
 * Read backwards from now: a group's status at the end is its current state,
 * and each transition (newest first) says what it was before. Before the
 * oldest transition in range the status is the oldest one's `from`, when we
 * have it. A group whose state began inside the range (`since`) with no
 * transition to explain it is unknown before then: it was created, or reset by
 * a query change, and what came before is not this monitor's answer. The
 * same goes for everything before a `reset` event: the states were cleared
 * because they answered a different query.
 *
 * Groups appear in `states` order (worst first), then any group known only
 * from events (expired since).
 */
export function statusTimeline(
  events: readonly MonitorEvent[],
  states: readonly GroupState[],
  from: number,
  to: number,
): { group: string; segments: Segment[] }[] {
  let start = from;
  for (const e of events) {
    const ts = Date.parse(e.ts) / 1000;
    if (e.kind === "reset" && ts > start && ts <= to) start = ts;
  }
  const transitions = new Map<string, { ts: number; from: Status; to: Status }[]>();
  for (const e of events) {
    if (e.kind !== "transition" || !e.group || !e.from || !e.to) continue;
    const ts = Date.parse(e.ts) / 1000;
    if (ts < start || ts > to) continue;
    const list = transitions.get(e.group) ?? [];
    list.push({ ts, from: e.from, to: e.to });
    transitions.set(e.group, list);
  }
  const groups = [...states.map((s) => s.group), ...[...transitions.keys()].filter((g) => !states.some((s) => s.group === g)).sort()];
  return groups.map((group) => {
    const state = states.find((s) => s.group === group);
    const ts = (transitions.get(group) ?? []).sort((a, b) => b.ts - a.ts); // newest first
    const segments: Segment[] = [];
    let end = to;
    let status: Status | null = state?.status ?? ts[0]?.to ?? null;
    for (const t of ts) {
      segments.unshift({ from: t.ts, to: end, status });
      end = t.ts;
      status = t.from;
    }
    // Before the oldest transition: its `from`, unless the state began after
    // the range did and nothing explains what came before.
    const since = state ? Date.parse(state.since) / 1000 : NaN;
    if (ts.length === 0 && Number.isFinite(since) && since > start) {
      segments.unshift({ from: since, to: end, status });
      end = since;
      status = null;
    }
    if (end > start) {
      segments.unshift({ from: start, to: end, status });
      end = start;
    }
    if (end > from) segments.unshift({ from, to: end, status: null });
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

/** Event kinds that are about delivering, for the notification log. */
export const NOTIFICATION_KINDS = ["notify_ok", "notify_failed", "test_notification"];
