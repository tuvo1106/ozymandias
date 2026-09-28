/**
 * The twelve-column grid a dashboard is laid out on.
 *
 * CSS grid rather than absolute positioning: the definition already speaks in
 * cells (`{x, y, w, h}`), which is the same model `grid-column` and `grid-row`
 * use, so the translation is arithmetic rather than layout code. It also means
 * the browser handles what happens when the window is narrower than twelve
 * usable columns, and a dashboard prints.
 *
 * Rows are a fixed pixel height because charts need one: uPlot draws to a
 * canvas of a given size, so "as tall as the content" is not available to us.
 * The grid stops at the cell, though: how much of a cell is left for a canvas
 * depends on whether that widget has a title, a warnings row and a legend, and
 * a constant here that tried to guess clipped the legend off every chart with
 * a warning above it. Each chart measures the box it was given instead.
 */
import {
  gridArea,
  gridRows,
  widgetsInReadingOrder,
  type Widget,
} from "../../lib/dashboard";
import type { BatchResult } from "../../lib/dashboardsApi";
import type { SketchState } from "../../lib/useDashboards";
import { widgetResults, widgetSketch, type Answer } from "../../lib/dashboardQueries";
import { HeatmapWidget } from "./HeatmapWidget";
import { WidgetFrame } from "./WidgetFrame";
import {
  NoteWidget,
  QueryValueWidget,
  TableWidget,
  TimeseriesWidget,
  ToplistWidget,
  type WidgetProps,
} from "./widgets";

/** Height of one grid row in CSS pixels. */
export const ROW_HEIGHT = 64;

/** Props for DashboardGrid. */
export interface DashboardGridProps {
  widgets: readonly Widget[];
  /** Results by widget id, then by the query's index within that widget. */
  byWidget: Map<string, Map<number, Answer<BatchResult>>>;
  /** Sketches by widget id; only heatmaps have one. */
  sketches: Map<string, SketchState>;
  /** The evaluated window, so every chart's x-axis covers it. */
  xRange?: [number, number];
  /** Charts sharing this key share a cursor; one per dashboard. */
  syncKey: string;
  /** Warnings said once above the grid; see [[sharedWarnings]]. */
  hiddenWarnings?: ReadonlySet<string>;
}

/**
 * A widget this build does not know how to draw.
 *
 * The table below is total over the union, but the union is this bundle's idea
 * of the types — and the client validates `type` only as "a string", because
 * the server is the one that validates definitions. A server newer than the
 * page, or a definition that reached the store some other way, therefore
 * produces a type with no renderer. Without this, `<undefined />` throws during
 * render and React Router's default boundary replaces the whole application,
 * sidebar included, with "Unexpected Application Error" — one unrecognised
 * widget destroying the page that the rest of this file works to keep isolated
 * (ADR-0017).
 */
function UnknownWidget({ widget }: WidgetProps) {
  return (
    <WidgetFrame
      title={widget.title}
      error={`This build cannot draw a "${widget.type}" widget.`}
    />
  );
}

/**
 * One renderer per widget type, as a table rather than a switch: the compiler
 * then requires an entry for every member of the union, so adding a type to
 * the definition fails here instead of rendering a blank square.
 */
const RENDERERS: Record<
  Widget["type"],
  (props: WidgetProps) => React.ReactElement
> = {
  timeseries: TimeseriesWidget,
  query_value: QueryValueWidget,
  toplist: ToplistWidget,
  table: TableWidget,
  heatmap: HeatmapWidget,
  note: NoteWidget,
};

/** Draws every widget in its place. */
export function DashboardGrid({
  widgets,
  byWidget,
  sketches,
  xRange,
  syncKey,
  hiddenWarnings,
}: DashboardGridProps) {
  const ordered = widgetsInReadingOrder(widgets);
  return (
    <div
      data-testid="dashboard-grid"
      className="grid gap-3"
      style={{
        gridTemplateColumns: "repeat(12, minmax(0, 1fr))",
        gridTemplateRows: `repeat(${Math.max(gridRows(widgets), 1)}, ${ROW_HEIGHT}px)`,
      }}
    >
      {ordered.map((widget) => {
        const Renderer = RENDERERS[widget.type] ?? UnknownWidget;
        return (
          <div
            key={widget.id}
            style={gridArea(widget.layout)}
            className="min-h-0 min-w-0"
          >
            <Renderer
              widget={widget}
              results={widgetResults(widget, byWidget)}
              xRange={xRange}
              syncKey={syncKey}
              sketch={widgetSketch(widget, sketches)}
              hiddenWarnings={hiddenWarnings}
            />
          </div>
        );
      })}
    </div>
  );
}
