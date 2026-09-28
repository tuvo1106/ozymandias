/**
 * Turning a dashboard into requests, and the answers back into widgets.
 *
 * A dashboard is one time picker over many widgets, so its queries go to
 * `/api/v1/query/batch` together: queries sharing a selector are selected and
 * bucketized once for the whole batch, which is the saving ADR-0018 measured.
 * That makes this file's job bookkeeping — which query belongs to which
 * widget, and how to put a flat array of results back where it came from.
 *
 * Two things stop it being a one-liner:
 *
 *   - **A dashboard can hold more queries than a batch can carry.** A
 *     definition may have 100 widgets of 10 queries and the server refuses
 *     more than 50 per request, so the queries are chunked and the chunks
 *     issued together. The sharing is per request, so chunking costs some of
 *     it — which is an argument for smaller dashboards, not for a bigger
 *     limit, since the batch deadline is per request too.
 *   - **A heatmap does not go in the batch at all.** It draws a distribution,
 *     which `/api/v1/query/sketch` answers and `/api/v1/query` refuses; the
 *     two endpoints reject each other's queries on purpose (ADR-0019). So
 *     heatmap widgets are collected separately and asked for one at a time.
 */
import type { Widget } from "./dashboard";
import { MAX_QUERIES_PER_BATCH } from "./dashboardsApi";

/** Where one query in a flattened batch came from. */
export interface QuerySlot {
  widgetId: string;
  /** Index within that widget's own `queries` array. */
  queryIndex: number;
  q: string;
}

/** What a dashboard needs to ask for. */
export interface DashboardRequests {
  /** Queries for `/api/v1/query/batch`, already split into legal chunks. */
  chunks: QuerySlot[][];
  /** Heatmap widgets, each needing its own `/api/v1/query/sketch`. */
  heatmaps: { widgetId: string; q: string }[];
}

/**
 * Collects everything a dashboard's widgets ask for.
 *
 * A note has no queries, and a widget whose `q` is blank is skipped rather
 * than sent: the server would refuse it, and one unfinished widget should not
 * cost its neighbours a request.
 */
export function collectRequests(widgets: readonly Widget[]): DashboardRequests {
  const slots: QuerySlot[] = [];
  const heatmaps: { widgetId: string; q: string }[] = [];
  for (const w of widgets) {
    if (w.type === "note") continue;
    (w.queries ?? []).forEach((query, queryIndex) => {
      const q = query.q.trim();
      if (q === "") return;
      if (w.type === "heatmap") heatmaps.push({ widgetId: w.id, q });
      else slots.push({ widgetId: w.id, queryIndex, q });
    });
  }
  const chunks: QuerySlot[][] = [];
  for (let i = 0; i < slots.length; i += MAX_QUERIES_PER_BATCH) {
    chunks.push(slots.slice(i, i + MAX_QUERIES_PER_BATCH));
  }
  return { chunks, heatmaps };
}

/**
 * A stable key for a dashboard's batch, so that refetching is a refetch and
 * editing a query is a new cache entry.
 *
 * The queries themselves, in order — not the dashboard's id, which does not
 * change when a widget is edited, and not a hash, which would make a cache
 * miss impossible to explain while debugging.
 */
export function requestsKey(requests: DashboardRequests): string {
  return JSON.stringify([requests.chunks.map((c) => c.map((s) => s.q)), requests.heatmaps.map((h) => h.q)]);
}

/**
 * Puts a chunk's results back against the slots that asked for them.
 *
 * Matched on the result's own `index` rather than on array position: the
 * server repeats the index precisely so a result survives being handed
 * around, and trusting position instead would mean a server that ever
 * reordered or dropped one would silently show a widget somebody else's
 * numbers — the worst failure a dashboard has.
 */
export function pairResults<T extends { index: number }>(slots: readonly QuerySlot[], results: readonly T[]): Map<string, Map<number, T>> {
  const byWidget = new Map<string, Map<number, T>>();
  for (const result of results) {
    const slot = slots[result.index];
    if (!slot) continue;
    let widget = byWidget.get(slot.widgetId);
    if (!widget) {
      widget = new Map<number, T>();
      byWidget.set(slot.widgetId, widget);
    }
    widget.set(slot.queryIndex, result);
  }
  return byWidget;
}

/** Merges the per-chunk maps into one, so a widget's results are in one place. */
export function mergeWidgetResults<T>(maps: readonly Map<string, Map<number, T>>[]): Map<string, Map<number, T>> {
  const out = new Map<string, Map<number, T>>();
  for (const m of maps) {
    for (const [widgetId, byIndex] of m) {
      const existing = out.get(widgetId);
      if (!existing) out.set(widgetId, new Map(byIndex));
      else for (const [i, v] of byIndex) existing.set(i, v);
    }
  }
  return out;
}

/**
 * One widget's results in the order its queries are written, with a hole
 * where a query produced nothing.
 *
 * Ordered by the definition rather than by what came back, because a table's
 * columns and a chart's legend are in the author's order and a missing answer
 * must not shift the rest along.
 */
export function widgetResults<T>(widget: Widget, byWidget: Map<string, Map<number, T>>): (T | undefined)[] {
  const byIndex = byWidget.get(widget.id);
  return (widget.queries ?? []).map((_, i) => byIndex?.get(i));
}
