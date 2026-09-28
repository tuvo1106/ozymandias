/**
 * The five widget types that draw from `/api/v1/query/batch`.
 *
 * Each takes the widget's definition and the results for it — one per query
 * the definition wrote, with a hole where a query produced nothing — and draws
 * that and nothing else. None of them fetches: a dashboard is one request, so
 * a widget that fetched for itself would be the thing ADR-0018's shared
 * selection exists to avoid.
 *
 * The heatmap lives in its own file, because it draws a distribution from a
 * different endpoint (ADR-0019).
 */
import { useMemo } from "react";
import { TimeseriesChart } from "../../charts/TimeseriesChart";
import { alignSeries, formatValue, seriesLabel } from "../../lib/chartData";
import {
  isReducer,
  matchConditionalFormat,
  reduceSeries,
  type DashboardQuery,
  type Widget,
} from "../../lib/dashboard";
import type { BatchResult } from "../../lib/dashboardsApi";
import type { SketchState } from "../../lib/useDashboards";
import type { Series } from "../../lib/metricsApi";
import { WidgetFrame } from "./WidgetFrame";

/** What every widget renderer is given. */
export interface WidgetProps {
  widget: Widget;
  /** One entry per query the definition wrote; undefined where none arrived. */
  results: (BatchResult | undefined)[];
  /** The window the server evaluated, for a chart's x-axis. */
  xRange?: [number, number];
  /** Charts sharing this key share a cursor. */
  syncKey?: string;
  /**
   * The sketch behind a heatmap, from `/api/v1/query/sketch`. Only a heatmap
   * has one — it is on the shared props rather than on a second interface
   * because the grid hands every widget the same object, and a renderer table
   * keyed by type (see [[DashboardGrid]]) needs every entry to take it.
   */
  sketch?: SketchState;
}

/**
 * The first error across a widget's queries, or undefined.
 *
 * First rather than all: the widget has one small box, and two stacked
 * messages in it are read as neither. A widget whose queries all failed the
 * same way says it once.
 */
function firstError(
  results: readonly (BatchResult | undefined)[],
): string | undefined {
  for (const r of results)
    if (r?.status === "error") return r.error ?? "this query was refused";
  return undefined;
}

/**
 * Whether this widget has heard back.
 *
 * The distinction the frame needs: "No data" before anything has arrived tells
 * the reader their service is silent, and a dashboard saying that by accident
 * during a slow first load is worse than saying nothing for a moment.
 *
 * A widget whose queries were all blank is *answered* even though nothing came
 * back for it, because nothing was ever sent — the batch skips a blank query
 * rather than letting the server refuse it — so no answer is coming and "No
 * data" is the true statement rather than a wait that never ends.
 */
function answered(
  widget: Widget,
  results: readonly (BatchResult | undefined)[],
): boolean {
  if (results.some((r) => r !== undefined)) return true;
  return !(widget.queries ?? []).some((q) => q.q.trim() !== "");
}

/**
 * The widget's own complaint about a reducer this build cannot apply, if any.
 *
 * An **error** rather than a warning, which is two decisions.
 *
 * It is the widget's problem and not the query's: the server answered, and
 * this build cannot do what the definition asks of the answer. That is the
 * same thing [[UnknownWidget]] says about a type it cannot draw, and it earns
 * the same treatment — including suppressing "No data", because a toplist
 * whose every reducer is unknown has no rows *and* has series, and telling the
 * reader a reporting service is silent is the failure this whole file keeps
 * circling.
 *
 * And the wording has to hold for all three widgets that take a reducer. The
 * row really is gone from a toplist, but a table keeps the column and fills it
 * with dashes, and a query_value shows one — so "left out" was true of one of
 * them and a lie about a full column of dashes.
 */
function reducerProblem(widget: Widget): string | undefined {
  const unknown = new Set<string>();
  for (const q of widget.queries ?? []) {
    if (q.reducer !== undefined && !isReducer(q.reducer))
      unknown.add(q.reducer);
  }
  if (unknown.size === 0) return undefined;
  const names = [...unknown].map((r) => `"${r}"`).join(", ");
  return `This build cannot apply ${unknown.size === 1 ? "a" : "the"} ${names} reducer${unknown.size === 1 ? "" : "s"}, so nothing is shown for ${unknown.size === 1 ? "that query" : "those queries"}.`;
}

/** Every warning across a widget's queries, deduplicated. */
function allWarnings(results: readonly (BatchResult | undefined)[]): string[] {
  return [...new Set(results.flatMap((r) => r?.warnings ?? []))];
}

/** A row of one query's answer: the series, labelled the way a legend wants. */
interface Line {
  label: string;
  series: Series;
  query: DashboardQuery;
}

/**
 * Flattens a widget's results into lines.
 *
 * A query that asked for a group-by comes back as several series, and the
 * label has to distinguish them — but a query that produced exactly one
 * ungrouped series is better labelled by the author's `name` than by
 * `metric{*}`, which is what the reader already knows.
 */
function linesOf(
  widget: Widget,
  results: readonly (BatchResult | undefined)[],
): Line[] {
  const lines: Line[] = [];
  (widget.queries ?? []).forEach((query, i) => {
    const result = results[i];
    if (!result || result.status !== "ok") return;
    const single =
      result.series.length === 1 &&
      Object.keys(result.series[0]?.tags ?? {}).length === 0;
    for (const series of result.series) {
      lines.push({
        label: single && query.name ? query.name : seriesLabel(series),
        series,
        query,
      });
    }
  });
  return lines;
}

/** A line chart of every series the widget's queries returned. */
export function TimeseriesWidget({
  widget,
  results,
  xRange,
  syncKey,
}: WidgetProps) {
  const lines = useMemo(() => linesOf(widget, results), [widget, results]);
  const data = useMemo(() => alignSeries(lines.map((l) => l.series)), [lines]);
  const error = firstError(results);
  return (
    <WidgetFrame
      title={widget.title}
      error={error}
      warnings={allWarnings(results)}
      empty={answered(widget, results) && lines.length === 0}
    >
      <TimeseriesChart
        data={data}
        labels={lines.map((l) => l.label)}
        xRange={xRange}
        syncKey={syncKey}
      />
    </WidgetFrame>
  );
}

/** The colours a conditional format may name, mapped to this build's palette. */
const FORMAT_COLORS: Record<string, string> = {
  red: "text-red-600 dark:text-red-400",
  yellow: "text-amber-600 dark:text-amber-400",
  green: "text-emerald-600 dark:text-emerald-400",
  blue: "text-sky-600 dark:text-sky-400",
  grey: "text-zinc-500",
  gray: "text-zinc-500",
};

/**
 * Formats one reduced number, honouring the widget's `precision`.
 *
 * Precision is a pointer in the definition because 0 is a real answer, so
 * `undefined` means "pick something sensible" and falls through to the same
 * formatter the charts use — which is what makes a query_value and the chart
 * beside it agree about what 1234.5 looks like.
 */
export function formatWidgetValue(
  value: number | null,
  precision: number | undefined,
): string {
  if (value === null) return "—";
  if (precision === undefined) return formatValue(value);
  return value.toFixed(precision);
}

/** One number, reduced from one query's line. */
export function QueryValueWidget({ widget, results }: WidgetProps) {
  const error = firstError(results) ?? reducerProblem(widget);
  const lines = linesOf(widget, results);
  // A query_value shows one number, so a query that grouped has more answers
  // than the widget has room for. Showing the first would be a lie by
  // omission; the reducer is applied to the first line and the count is shown.
  //
  // The reducer comes from the line's *own* query, not from `queries[0]`: when
  // the first query errors or selects nothing, the first line belongs to the
  // second query, and reducing it by the first query's rule would answer a
  // question nobody asked.
  const first = lines[0];
  const value = first?.query.reducer
    ? reduceSeries(
        first.series.points.map((p) => p[1]),
        first.query.reducer,
      )
    : null;
  const format = matchConditionalFormat(value, widget.conditional_formats);
  const color = format
    ? (FORMAT_COLORS[format.color] ?? "text-zinc-900 dark:text-zinc-100")
    : "text-zinc-900 dark:text-zinc-100";
  return (
    <WidgetFrame
      title={widget.title}
      error={error}
      warnings={allWarnings(results)}
      empty={answered(widget, results) && lines.length === 0}
    >
      <div className="flex h-full flex-col items-center justify-center">
        <span className={`text-4xl font-semibold tabular-nums ${color}`}>
          {formatWidgetValue(value, widget.precision)}
        </span>
        {lines.length > 1 ? (
          <span className="mt-1 text-xs text-zinc-500">
            of {lines.length} series
          </span>
        ) : null}
      </div>
    </WidgetFrame>
  );
}

/** Rows ranked by their reduced value, biggest first. */
export function ToplistWidget({ widget, results }: WidgetProps) {
  const error = firstError(results) ?? reducerProblem(widget);
  // Each line by its own query's reducer. A toplist may carry several queries
  // and each carries its own rule, so one taken from `queries[0]` and applied
  // to all of them ranks the rows by two different questions.
  const rows = linesOf(widget, results)
    .map((l, i) => ({
      key: `${i}:${l.label}`,
      label: l.label,
      value: reduceSeries(
        l.series.points.map((p) => p[1]),
        l.query.reducer ?? "last",
      ),
    }))
    .filter(
      (r): r is { key: string; label: string; value: number } =>
        r.value !== null,
    )
    .sort((a, b) => b.value - a.value)
    .slice(0, widget.limit && widget.limit > 0 ? widget.limit : 10);
  return (
    <WidgetFrame
      title={widget.title}
      error={error}
      warnings={allWarnings(results)}
      empty={answered(widget, results) && rows.length === 0}
    >
      <ol className="h-full overflow-auto text-sm">
        {rows.map((row) => (
          // Keyed by position as well as label: two queries on one toplist can
          // legitimately return the same group, and a duplicate key makes
          // React reuse the wrong row.
          <li
            key={row.key}
            className="flex items-baseline justify-between gap-2 border-b border-zinc-100 py-1 last:border-0 dark:border-zinc-800"
          >
            <span
              className="truncate text-zinc-600 dark:text-zinc-300"
              title={row.label}
            >
              {row.label}
            </span>
            <span className="shrink-0 tabular-nums">
              {formatWidgetValue(row.value, widget.precision)}
            </span>
          </li>
        ))}
      </ol>
    </WidgetFrame>
  );
}

/**
 * One row per group, one column per query.
 *
 * Grouped on the series label, so a table of "requests" and "errors" by route
 * lines the two queries up on the route rather than on the order they came
 * back in — which is what makes the row mean one thing.
 */
export function TableWidget({ widget, results }: WidgetProps) {
  const error = firstError(results) ?? reducerProblem(widget);
  const queries = widget.queries ?? [];
  const rows = new Map<string, (number | null)[]>();
  queries.forEach((query, i) => {
    const result = results[i];
    if (!result || result.status !== "ok") return;
    for (const series of result.series) {
      const label = seriesLabel(series);
      let row = rows.get(label);
      if (!row) {
        row = queries.map(() => null);
        rows.set(label, row);
      }
      row[i] = reduceSeries(
        series.points.map((p) => p[1]),
        query.reducer ?? "last",
      );
    }
  });
  return (
    <WidgetFrame
      title={widget.title}
      error={error}
      warnings={allWarnings(results)}
      empty={answered(widget, results) && rows.size === 0}
    >
      <div className="h-full overflow-auto">
        <table className="w-full text-left text-sm">
          <thead className="text-xs text-zinc-500">
            <tr>
              <th scope="col" className="py-1 pr-2 font-medium">
                Group
              </th>
              {queries.map((q, i) => (
                <th
                  key={i}
                  scope="col"
                  className="py-1 pl-2 text-right font-medium"
                >
                  {q.name ?? q.q}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {[...rows].map(([label, cells]) => (
              <tr
                key={label}
                className="border-t border-zinc-100 dark:border-zinc-800"
              >
                <th
                  scope="row"
                  className="max-w-0 truncate py-1 pr-2 font-normal text-zinc-600 dark:text-zinc-300"
                  title={label}
                >
                  {label}
                </th>
                {cells.map((cell, i) => (
                  <td key={i} className="py-1 pl-2 text-right tabular-nums">
                    {formatWidgetValue(cell, widget.precision)}
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </WidgetFrame>
  );
}

/**
 * A note: the author's markdown, rendered as text.
 *
 * Deliberately *not* rendered as HTML. A dashboard can be created by anyone
 * who can POST one, and turning stored text into markup is how a monitoring
 * page becomes a way to run script in an operator's browser. Paragraphs and
 * line breaks are honoured because that is what the whitespace class does;
 * anything richer waits for a sanitizer we have chosen on purpose.
 */
export function NoteWidget({ widget }: WidgetProps) {
  return (
    <WidgetFrame title={widget.title}>
      <div className="h-full overflow-auto whitespace-pre-wrap text-sm text-zinc-700 dark:text-zinc-300">
        {widget.markdown}
      </div>
    </WidgetFrame>
  );
}
