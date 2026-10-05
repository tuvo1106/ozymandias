/**
 * Pure helpers behind the Log Explorer: building queries from clicks, merging
 * the live tail into the list, flattening attributes for the detail panel,
 * and saved views. No React, no network: everything here is tested directly.
 */
import type { LogEntry } from "./logsApi";

/** Status values and the colour class that draws each, from least to most severe. */
export const STATUS_ORDER = ["debug", "info", "warn", "error", "critical"] as const;

/** Tailwind background class for a status; unknown values are neutral. */
export function statusColor(status: string): string {
  switch (status) {
    case "critical":
      return "bg-fuchsia-600";
    case "error":
      return "bg-red-500";
    case "warn":
      return "bg-amber-500";
    case "info":
      return "bg-sky-500";
    case "debug":
      return "bg-zinc-400";
    default:
      return "bg-zinc-500";
  }
}

const LABELS = new Set(["service", "source", "host", "env", "status", "trace_id"]);

/** Quotes a value for the query language when it has anything but plain characters. */
export function quoteValue(v: string): string {
  if (/^[A-Za-z0-9_./*][A-Za-z0-9_.\-/*]*$/.test(v)) return v;
  const esc = v.replace(/\\/g, "\\\\").replace(/"/g, '\\"').replace(/\n/g, "\\n").replace(/\t/g, "\\t").replace(/\r/g, "\\r");
  return `"${esc}"`;
}

/**
 * Adds `key:value` (or `@key:value` for an attribute) to a query, or its
 * negation. Clicking a facet value is how people build most queries, so this
 * must never produce a query that does not parse: values are quoted, and a
 * term already present is not repeated.
 */
export function addTerm(q: string, key: string, value: string, negate = false): string {
  const name = LABELS.has(key) ? key : `@${key.replace(/^@/, "")}`;
  const term = `${negate ? "-" : ""}${name}:${quoteValue(value)}`;
  const parts = q.trim();
  if (parts === term || parts.split(/\s+/).includes(term)) return parts;
  return parts ? `${parts} ${term}` : term;
}

/** Flattens nested attributes to dotted paths: `{a:{b:1}}` becomes `a.b`. Arrays stay values. */
export function flattenAttrs(attrs: Record<string, unknown> | undefined, prefix = ""): [string, unknown][] {
  const out: [string, unknown][] = [];
  for (const [k, v] of Object.entries(attrs ?? {})) {
    const path = prefix ? `${prefix}.${k}` : k;
    if (typeof v === "object" && v !== null && !Array.isArray(v)) out.push(...flattenAttrs(v as Record<string, unknown>, path));
    else out.push([path, v]);
  }
  return out.sort(([a], [b]) => a.localeCompare(b));
}

/** The value at a dotted path as display text; "" when absent. */
export function attrText(log: LogEntry, path: string): string {
  const hit = flattenAttrs(log.attrs).find(([p]) => p === path);
  if (!hit) return "";
  return typeof hit[1] === "string" ? hit[1] : JSON.stringify(hit[1]);
}

/** A stable identity for a row: the tail and a page can both hold the same log. */
export function rowKey(l: LogEntry): string {
  return `${l.ts}\u0000${l.service}\u0000${l.host ?? ""}\u0000${l.message}`;
}

/**
 * Puts tailed logs above a list, newest first, without repeating one the list
 * already has (once per copy) (the API says a log may be seen both in the page and the tail),
 * and keeps at most `cap` rows so a long tail cannot grow memory forever.
 */
export function mergeTail(list: readonly LogEntry[], incoming: readonly LogEntry[], cap: number): LogEntry[] {
  // A multiset, not a set: two distinct logs can share a timestamp, service, host and
  // message (a retry loop), and collapsing them would hide real logs. Each log already
  // in the list cancels at most one incoming log of the same key.
  const have = new Map<string, number>();
  for (const l of list) have.set(rowKey(l), (have.get(rowKey(l)) ?? 0) + 1);
  const fresh: LogEntry[] = [];
  for (const l of incoming) {
    const k = rowKey(l);
    const n = have.get(k) ?? 0;
    if (n > 0) have.set(k, n - 1);
    else fresh.push(l);
  }
  // Incoming is oldest first (arrival order); the list is newest first.
  return [...fresh.reverse(), ...list].slice(0, cap);
}

/**
 * Which rows of a fixed-height list to render: the visible window plus
 * `overscan` rows each side. A virtual list is this and a spacer.
 */
export function visibleRange(scrollTop: number, viewport: number, rowHeight: number, total: number, overscan = 8): { start: number; end: number } {
  const start = Math.max(0, Math.floor(scrollTop / rowHeight) - overscan);
  const end = Math.min(total, Math.ceil((scrollTop + viewport) / rowHeight) + overscan);
  return { start, end: Math.max(start, end) };
}

/** A saved query + range + columns. */
export interface SavedView {
  name: string;
  /** The URL search string that restores it. */
  search: string;
}

const KEY = "ozy.logs.views";

/** Reads saved views; a missing, unreadable or malformed store is an empty list. */
export function loadViews(storage: Pick<Storage, "getItem"> = localStorage): SavedView[] {
  try {
    const v: unknown = JSON.parse(storage.getItem(KEY) ?? "[]");
    return Array.isArray(v)
      ? v.filter((x): x is SavedView => typeof x?.name === "string" && typeof x?.search === "string")
      : [];
  } catch {
    return [];
  }
}

/** Saves a view, replacing one of the same name; returns the new list. Storage failures are swallowed. */
export function saveView(view: SavedView, storage: Pick<Storage, "getItem" | "setItem"> = localStorage): SavedView[] {
  const next = [...loadViews(storage).filter((v) => v.name !== view.name), view];
  try {
    storage.setItem(KEY, JSON.stringify(next));
  } catch {
    /* private window or quota: the view just is not kept */
  }
  return next;
}

/** Removes a saved view by name; returns the new list. */
export function deleteView(name: string, storage: Pick<Storage, "getItem" | "setItem"> = localStorage): SavedView[] {
  const next = loadViews(storage).filter((v) => v.name !== name);
  try {
    storage.setItem(KEY, JSON.stringify(next));
  } catch {
    /* ignore */
  }
  return next;
}

/** The keys a query can name without `@`. */
export const LABEL_KEYS: readonly string[] = ["service", "status", "host", "source", "env", "trace_id"];

/**
 * Completions for the token being typed, as whole replacement queries (the
 * query box swaps its text for the one picked). A token with no colon
 * completes to a key; `key:prefix` completes to a value the facets know. Only
 * the last token completes: the rest of the query is left as typed.
 */
export function completions(draft: string, facets: Record<string, readonly { value: string }[]>, limit = 8): string[] {
  const m = /(^|\s)(-?)(@?[A-Za-z0-9_.]*)(:?)([^\s:"]*)$/.exec(draft);
  if (!m || draft.endsWith(" ")) return [];
  const head = draft.slice(0, m.index + m[1]!.length);
  const neg = m[2]!;
  const key = m[3]!;
  if (m[4] === "") {
    if (key === "" || key.startsWith("@")) return [];
    return LABEL_KEYS.filter((k) => k.startsWith(key) && k !== key).map((k) => `${head}${neg}${k}:`).slice(0, limit);
  }
  const prefix = m[5]!.toLowerCase();
  const values = facets[key.replace(/^@/, "")] ?? [];
  return values
    .map((v) => v.value)
    .filter((v) => v.toLowerCase().startsWith(prefix) && v.toLowerCase() !== prefix)
    .slice(0, limit)
    .map((v) => `${head}${neg}${key}:${quoteValue(v)}`);
}
