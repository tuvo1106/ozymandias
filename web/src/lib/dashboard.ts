/**
 * The dashboard definition, as the UI sees it, and the pure functions that
 * turn one into something drawable.
 *
 * These types mirror `internal/dashboard` field for field, because the schema
 * is normative in docs/dashboards.md and the server refuses anything it does
 * not recognise. They are deliberately *structural* rather than validated
 * here: the server validates on the way in, so a definition the UI has
 * received has already been through a stricter check than TypeScript can
 * express. What this file adds is the arithmetic the server has no opinion
 * about — where a widget sits on the grid, which queries a dashboard is going
 * to ask for, and how one number is picked out of a line.
 */

/** The grid is twelve columns wide, as `dashboard.Columns` says. */
export const COLUMNS = 12;

/** Widget kinds this build draws. */
export type WidgetType =
  | "timeseries"
  | "query_value"
  | "toplist"
  | "table"
  | "heatmap"
  | "note";

/** How a timeseries draws a line. */
export type Display = "line" | "area" | "bars" | "points";

/** How a line collapses to the one number a query_value or cell shows. */
export type Reducer = "last" | "avg" | "sum" | "min" | "max";

/** A conditional format's comparison. */
export type Op = ">" | ">=" | "<" | "<=" | "=" | "!=";

/** Where a widget sits, in grid cells rather than pixels. */
export interface Layout {
  x: number;
  y: number;
  w: number;
  h: number;
}

/** One thing a widget asks for. */
export interface DashboardQuery {
  q: string;
  name?: string;
  display?: Display;
  reducer?: Reducer;
}

/** A chart's vertical axis. */
export interface YAxis {
  min?: number;
  max?: number;
  /** A label, not a conversion: "req/s", "ms", "%". */
  unit?: string;
  /** "linear" (the default) or "log". */
  scale?: string;
}

/** Colours a value that compares true. */
export interface ConditionalFormat {
  op: Op;
  value: number;
  /** A palette name, not CSS — see [[conditionalColor]]. */
  color: string;
}

/** One widget of a dashboard. */
export interface Widget {
  id: string;
  type: WidgetType;
  title?: string;
  layout: Layout;
  queries?: DashboardQuery[];
  yaxis?: YAxis;
  precision?: number;
  conditional_formats?: ConditionalFormat[];
  limit?: number;
  markdown?: string;
}

/** One selector on the variable bar. */
export interface TemplateVar {
  /** What queries write as `$name`. */
  name: string;
  /** The tag key whose values the selector offers. */
  tag: string;
  /** Selected when the URL says nothing; "*" or empty means every value. */
  default?: string;
}

/** A dashboard definition. */
export interface Dashboard {
  uid?: string;
  title: string;
  description?: string;
  template?: boolean;
  template_vars?: TemplateVar[];
  widgets: Widget[];
}

/** A dashboard as the API serves it: the definition plus the database's columns. */
export interface StoredDashboard extends Dashboard {
  id: number;
  provisioned: boolean;
  created_at: string;
  updated_at: string;
}

/**
 * The widgets in the order they should be drawn and tabbed through: top to
 * bottom, then left to right.
 *
 * The definition's own order is the author's and means nothing visually — two
 * widgets can be written in either order and land in the same places — so
 * reading order has to be derived from the layout. Without this, tab order and
 * the DOM order a screen reader follows would be whatever order the JSON
 * happened to be in.
 */
export function widgetsInReadingOrder(widgets: readonly Widget[]): Widget[] {
  return [...widgets].sort(
    (a, b) =>
      a.layout.y - b.layout.y ||
      a.layout.x - b.layout.x ||
      a.id.localeCompare(b.id),
  );
}

/**
 * The CSS grid placement for a widget: 1-based column and row lines, since
 * that is what `grid-column` wants and the definition counts from 0.
 *
 * Out-of-range values are clamped rather than trusted. The server refuses a
 * layout past the right edge, so this only bites on a definition that arrived
 * some other way (a paste into the JSON editor before it is saved), and a
 * widget drawn half off-screen is easier to fix than one that broke the grid
 * for everything after it.
 */
export function gridArea(layout: Layout): {
  gridColumn: string;
  gridRow: string;
} {
  const w = Math.min(Math.max(layout.w, 1), COLUMNS);
  const x = Math.min(Math.max(layout.x, 0), COLUMNS - w);
  const h = Math.max(layout.h, 1);
  const y = Math.max(layout.y, 0);
  return {
    gridColumn: `${x + 1} / span ${w}`,
    gridRow: `${y + 1} / span ${h}`,
  };
}

/** How many rows the grid needs to hold every widget. */
export function gridRows(widgets: readonly Widget[]): number {
  return widgets.reduce(
    (rows, w) =>
      Math.max(rows, Math.max(w.layout.y, 0) + Math.max(w.layout.h, 1)),
    0,
  );
}

/**
 * Reduces a line to one number, the way a query_value, toplist or table cell
 * shows it.
 *
 * Nulls are skipped rather than counted as zero: an empty bucket is "we did
 * not measure", and averaging it in as a zero drags the answer toward the
 * floor in exactly the situation — a sparse series — where the reader is
 * least likely to notice. A line that is *entirely* null reduces to null, not
 * to 0, for the same reason.
 */
export function reduceSeries(
  points: readonly (number | null)[],
  reducer: Reducer,
): number | null {
  const values: number[] = [];
  for (const p of points) if (p !== null && Number.isFinite(p)) values.push(p);
  if (values.length === 0) return null;
  switch (reducer) {
    case "last":
      return values[values.length - 1] ?? null;
    case "sum":
      return values.reduce((a, b) => a + b, 0);
    case "avg":
      return values.reduce((a, b) => a + b, 0) / values.length;
    case "min":
      return Math.min(...values);
    case "max":
      return Math.max(...values);
    default:
      // `Reducer` is this bundle's idea of the set, and a definition is served
      // back from the store without being re-validated — so a hand-edited row,
      // or an `ozyd` that has learnt a sixth reducer, delivers one this switch
      // has never heard of. Falling off the end of a switch returns undefined,
      // which is not `number | null` however firmly the signature says it is,
      // and the first `.toFixed` on it takes the page down.
      return null;
  }
}

/**
 * The first conditional format that matches, or undefined.
 *
 * First match wins, in the author's order — so the rules read as a cascade
 * ("red over 5, else yellow over 1, else green") rather than as a set whose
 * outcome depends on how the UI happens to sort them.
 */
export function matchConditionalFormat(
  value: number | null,
  formats: readonly ConditionalFormat[] | undefined,
): ConditionalFormat | undefined {
  if (value === null || !formats) return undefined;
  return formats.find((f) => {
    switch (f.op) {
      case ">":
        return value > f.value;
      case ">=":
        return value >= f.value;
      case "<":
        return value < f.value;
      case "<=":
        return value <= f.value;
      case "=":
        return value === f.value;
      case "!=":
        return value !== f.value;
    }
  });
}

/**
 * The metric a query selects, read off the text without parsing it.
 *
 * A **heuristic on purpose**. The UI has no metricql parser and should not
 * grow one for this: the only caller is the variable bar, which needs some
 * metric to ask `/api/v1/tags/values` about, and being wrong costs an empty
 * dropdown rather than a wrong answer. The real parse happens on the server,
 * which is also what refuses a query this would mis-read.
 *
 * Matches `agg:metric{` — the shape every leaf query has, since the
 * aggregator prefix is mandatory.
 */
export function queryMetric(q: string): string | undefined {
  return /(?:^|[\s(])[a-z0-9_]+:([a-zA-Z0-9._\-/]+)\s*\{/.exec(q)?.[1];
}

/**
 * The first metric a dashboard's widgets query, for the variable bar's
 * dropdowns.
 *
 * The first rather than a union: a dashboard's variables are tag keys its
 * queries share, so any one of its metrics answers "what values does this key
 * take", and a union across metrics would offer combinations that select
 * nothing.
 */
export function firstMetric(widgets: readonly Widget[]): string | undefined {
  for (const w of widgetsInReadingOrder(widgets)) {
    for (const q of w.queries ?? []) {
      const metric = queryMetric(q.q);
      if (metric) return metric;
    }
  }
  return undefined;
}
